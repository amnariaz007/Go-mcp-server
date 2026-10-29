package risk

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amnariaz007/walletscope-mcp/internal/blockchain"
	"github.com/amnariaz007/walletscope-mcp/internal/investigation"
	"github.com/amnariaz007/walletscope-mcp/internal/sanctions"
)

// Evaluate applies every rule to the evidence and aggregates the score. It is
// a pure function: identical evidence and configuration produce identical output.
func Evaluate(ev *Evidence, cfg *Config) Report {
	var acts []blockchain.Activity
	if ev.History != nil {
		acts = ev.History.Activities
	}
	rep := Report{
		Chain:            blockchain.ChainEthereum,
		Address:          ev.Address,
		GeneratedAt:      ev.Now.UTC(),
		ModelVersion:     ModelVersion,
		Findings:         []Finding{},
		EvidenceTxHashes: []string{},
		Warnings:         []string{},
		Coverage:         []investigation.SourceCoverage{},
	}

	direct := ruleSanctionsDirect(ev)
	oneHop, sanctionedCPs := ruleSanctionsOneHop(ev, acts)
	results := []ruleResult{
		direct,
		oneHop,
		ruleMixer(ev, cfg, acts, sanctionedCPs),
		ruleStructuring(ev, cfg, acts),
		ruleNewWallet(ev, cfg, acts),
	}
	sum := 0
	hashes := map[string]bool{}
	for _, r := range results {
		rep.RuleEvaluations = append(rep.RuleEvaluations, r.eval)
		if r.finding == nil {
			continue
		}
		rep.Findings = append(rep.Findings, *r.finding)
		sum += r.finding.Points
		for _, h := range r.finding.EvidenceTxHashes {
			hashes[h] = true
		}
	}
	sort.SliceStable(rep.Findings, func(i, j int) bool {
		if rep.Findings[i].Points != rep.Findings[j].Points {
			return rep.Findings[i].Points > rep.Findings[j].Points
		}
		return rep.Findings[i].RuleID < rep.Findings[j].RuleID
	})
	for h := range hashes {
		rep.EvidenceTxHashes = append(rep.EvidenceTxHashes, h)
	}
	sort.Strings(rep.EvidenceTxHashes)

	switch {
	case direct.eval.Status == EvalTriggered:
		rep.Score = 100
	case sum > 100:
		rep.Score = 100
	default:
		rep.Score = sum
	}

	// Completeness.
	sanctionsOK := ev.Direct.Status != sanctions.StatusInconclusive
	historyOK := ev.History != nil && !ev.History.AllSourcesFailed()
	switch {
	case !sanctionsOK || !historyOK:
		rep.AssessmentStatus = AssessmentInsufficient
	case ev.History.Complete() && ev.AddressError == "":
		rep.AssessmentStatus = AssessmentComplete
	default:
		rep.AssessmentStatus = AssessmentPartial
	}
	rep.ScoreIsLowerBound = rep.AssessmentStatus != AssessmentComplete && rep.Score < 100
	rep.RiskBand, rep.RiskBandLabel = band(rep.Score, rep.AssessmentStatus)

	// Provenance and coverage.
	if ev.History != nil {
		rep.Coverage = ev.History.Coverage
		rep.ActivitiesReviewed = len(ev.History.Activities)
		rep.ReviewedWindow = window(ev.History.Activities)
	}
	fetched := ev.HistoryFetched.UTC()
	rep.DataSources = []DataSource{
		{Name: ev.ProviderName + " (blockchain data)", URL: ev.ProviderURL, RetrievedAt: &fetched,
			Detail: "address profile and activity history (transactions, internal transactions, ERC-20 transfers)"},
		{Name: ev.SanctionsInfo.Name, URL: ev.SanctionsInfo.SourceURL, RetrievedAt: ev.SanctionsInfo.LastSuccessfulLoad,
			Detail: sanctionsDetail(ev.SanctionsInfo)},
	}
	if len(cfg.Mixers) > 0 {
		rep.DataSources = append(rep.DataSources, DataSource{Name: "WalletScope configured mixer list",
			Detail: fmt.Sprintf("%d addresses (WALLETSCOPE_MIXER_ADDRESSES)", len(cfg.Mixers))})
	}

	// Warnings explain every reason the score could be understated.
	if !sanctionsOK {
		rep.Warnings = append(rep.Warnings, "Sanctions dataset unavailable: direct and one-hop sanctions rules were not evaluated. "+ev.Direct.Reason)
	} else if ev.SanctionsInfo.Stale {
		rep.Warnings = append(rep.Warnings, "Sanctions dataset is stale (refresh failing); recent designations may be missing.")
	}
	if ev.History == nil || ev.History.AllSourcesFailed() {
		rep.Warnings = append(rep.Warnings, "Activity history could not be retrieved: transaction-based rules were not evaluated.")
	} else {
		for _, c := range ev.History.Coverage {
			switch {
			case c.Error != "":
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("Source %s failed (%s); its activity was not assessed.", c.Source, c.Error))
			case !c.Complete:
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("Source %s was truncated at %d pages (%d items); older activity was not assessed.", c.Source, c.Pages, c.Items))
			}
		}
	}
	if ev.AddressError != "" {
		rep.Warnings = append(rep.Warnings, "Address profile unavailable: "+ev.AddressError)
	}
	for _, e := range rep.RuleEvaluations {
		if e.Status == EvalNotEvaluated {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("Rule %s not evaluated: %s.", e.RuleID, e.Reason))
		}
	}
	rep.Limitations = []string{
		"Scores are rule-based indicators requiring analyst review, not probabilities or legal determinations.",
		"Only the configured rules are evaluated; see docs/risk-model.md for definitions and thresholds.",
		"Exposure is assessed one hop from the wallet; use trace_funds for multi-hop flows.",
		"Blockscout index coverage may lag or omit activity.",
	}
	rep.Summary = summarize(&rep)
	return rep
}

func band(score int, status string) (string, string) {
	switch {
	case score >= 75:
		return "severe", "Severe indicators requiring priority review"
	case score >= 50:
		return "high", "High indicators requiring review"
	case score >= 25:
		return "moderate", "Elevated indicators requiring review"
	case score >= 1:
		return "low", "Low indicators; review if other context warrants"
	}
	switch status {
	case AssessmentComplete:
		return "none_detected", "No indicators detected by the configured rules"
	case AssessmentPartial:
		return "none_detected_in_reviewed_window", "No indicators in the reviewed window; older activity not assessed"
	}
	return "indeterminate", "Indeterminate: required data could not be obtained"
}

func summarize(r *Report) string {
	var b strings.Builder
	switch r.RiskBand {
	case "indeterminate":
		b.WriteString("Assessment incomplete: required data could not be obtained, so no conclusion about risk can be drawn. ")
	case "none_detected":
		b.WriteString("No configured risk indicators were detected across the full indexed history. ")
	case "none_detected_in_reviewed_window":
		b.WriteString("No configured risk indicators were detected in the reviewed activity window; older activity was not assessed. ")
	default:
		lb := ""
		if r.ScoreIsLowerBound {
			lb = " (lower bound: some data was missing)"
		}
		fmt.Fprintf(&b, "Score %d/100%s: %s. ", r.Score, lb, strings.ToLower(r.RiskBandLabel))
	}
	for _, f := range r.Findings {
		pts := fmt.Sprintf("+%d", f.Points)
		if f.SuppressedReason != "" {
			pts = "+0, overlap"
		}
		fmt.Fprintf(&b, "%s (%s): %s ", f.Title, pts, f.Explanation)
	}
	if r.AssessmentStatus != AssessmentComplete {
		fmt.Fprintf(&b, "Assessment status: %s; see warnings.", r.AssessmentStatus)
	}
	return strings.TrimSpace(b.String())
}

func window(acts []blockchain.Activity) *TimeRange {
	var tr TimeRange
	for i := range acts {
		ts := acts[i].Timestamp
		if ts == nil {
			continue
		}
		if tr.First == nil || ts.Before(*tr.First) {
			tr.First = ts
		}
		if tr.Last == nil || ts.After(*tr.Last) {
			tr.Last = ts
		}
	}
	if tr.First == nil {
		return nil
	}
	return &tr
}

func sanctionsDetail(i sanctions.DatasetInfo) string {
	if i.LastSuccessfulLoad == nil {
		return "not loaded"
	}
	return fmt.Sprintf("publish date %s, %d entries, %d EVM address identifiers", i.PublishDate, i.Entries, i.EVMAddresses)
}

// Reporter collects evidence and evaluates it.
type Reporter struct {
	Investigation *investigation.Service
	Sanctions     sanctions.Screener
	Config        Config
	Now           func() time.Time
}

// Report builds a risk report for a normalized address. Upstream failures
// degrade the assessment status instead of failing the call.
func (rp *Reporter) Report(ctx context.Context, addr string) Report {
	now := time.Now().UTC()
	if rp.Now != nil {
		now = rp.Now().UTC()
	}
	ev := &Evidence{
		Address:      addr,
		Now:          now,
		ProviderName: rp.Investigation.Provider.Name(),
		ProviderURL:  rp.Investigation.Provider.BaseURL(),
		HistoryRead:  true,
	}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		ev.Direct = rp.Sanctions.Check(ctx, addr)
	}()
	go func() {
		defer wg.Done()
		ev.History = rp.Investigation.CollectHistory(ctx, addr, blockchain.FilterAll, rp.Investigation.HistoryPages, 0)
		ev.HistoryFetched = time.Now().UTC()
		if rp.Now != nil {
			ev.HistoryFetched = now
		}
	}()
	go func() {
		defer wg.Done()
		info, err := rp.Investigation.Provider.GetAddress(ctx, addr)
		if err != nil {
			ev.AddressError = "address lookup failed"
			return
		}
		ev.AddressInfo = info
	}()
	wg.Wait()

	ev.SanctionsInfo = ev.Direct.Dataset
	if ev.Direct.Status != sanctions.StatusInconclusive {
		ev.Lookup = func(a string) []sanctions.Record {
			recs, _ := rp.Sanctions.Lookup(a)
			return recs
		}
	}
	return Evaluate(ev, &rp.Config)
}
