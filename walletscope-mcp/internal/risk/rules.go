package risk

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/amnariaz007/walletscope-mcp/internal/blockchain"
	"github.com/amnariaz007/walletscope-mcp/internal/sanctions"
)

// ruleResult is what each rule returns to the engine.
type ruleResult struct {
	eval    RuleEvaluation
	finding *Finding
}

// usable reports whether an activity is confirmed evidence of value movement
// or of a successful interaction with the counterparty.
func usable(a *blockchain.Activity) bool {
	if a.To == nil {
		return false
	}
	if a.Kind == blockchain.KindContractCall {
		return a.Status == blockchain.StatusSuccess
	}
	return a.TransferConfirmed
}

// exposure groups confirmed activity with flagged counterparties.
type exposure struct {
	cp       map[string]*Counterparty
	hashes   map[string]bool
	outgoing bool
	tr       TimeRange
}

func newExposure() *exposure {
	return &exposure{cp: map[string]*Counterparty{}, hashes: map[string]bool{}}
}

func (x *exposure) add(a *blockchain.Activity, counterparty, dir, role string, recs []sanctions.Record) {
	c := x.cp[counterparty]
	if c == nil {
		c = &Counterparty{Address: counterparty, Role: role, Direction: dir, Records: recs}
		x.cp[counterparty] = c
	} else if c.Direction != dir {
		c.Direction = "both"
	}
	c.TxCount++
	x.hashes[a.TxHash] = true
	if dir == "sent_to" {
		x.outgoing = true
	}
	if ts := a.Timestamp; ts != nil {
		if x.tr.First == nil || ts.Before(*x.tr.First) {
			x.tr.First = ts
		}
		if x.tr.Last == nil || ts.After(*x.tr.Last) {
			x.tr.Last = ts
		}
	}
}

func (x *exposure) counterparties() []Counterparty {
	out := make([]Counterparty, 0, len(x.cp))
	for _, c := range x.cp {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

// sortedHashes returns up to maxEvidenceHashes hashes in a stable order.
func sortedHashes(set map[string]bool) ([]string, bool) {
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	if len(out) > maxEvidenceHashes {
		return out[:maxEvidenceHashes], true
	}
	return out, false
}

// counterpartyOf returns the other side of a for the wallet and the direction.
// An outgoing token event in a transaction the wallet provably did not sign is
// reported as "unsigned_token_event" and weighted like passive receipt: token
// contracts can emit Transfer events naming any sender (address poisoning).
func counterpartyOf(a *blockchain.Activity, wallet string) (string, string) {
	switch a.DirectionFor(wallet) {
	case blockchain.DirOut:
		if a.InitiatedByWallet != nil && !*a.InitiatedByWallet {
			return a.ToAddress(), "unsigned_token_event"
		}
		return a.ToAddress(), "sent_to"
	case blockchain.DirIn:
		return a.From.Address, "received_from"
	}
	return "", ""
}

func ruleSanctionsDirect(ev *Evidence) ruleResult {
	r := ruleResult{eval: RuleEvaluation{RuleID: RuleSanctionsDirect}}
	switch ev.Direct.Status {
	case sanctions.StatusInconclusive:
		r.eval.Status, r.eval.Reason = EvalNotEvaluated, "sanctions dataset unavailable: "+ev.Direct.Reason
		return r
	case sanctions.StatusNoMatch:
		r.eval.Status = EvalNotTriggered
		return r
	}
	r.eval.Status = EvalTriggered
	r.finding = &Finding{
		RuleID: RuleSanctionsDirect, Title: "Address listed in the OFAC SDN dataset", Severity: "critical",
		Points: PointsSanctionsDirect, MaxPoints: PointsSanctionsDirect,
		Explanation: fmt.Sprintf("The address exactly matches %d digital currency identifier(s) in the loaded OFAC SDN list (%s).",
			len(ev.Direct.Matches), recordNames(ev.Direct.Matches)),
		EvidenceTxHashes: []string{},
		SanctionsRecords: ev.Direct.Matches,
		Sources:          []string{sanctionsSource(ev)},
		Confidence:       "high: exact identifier match in the official dataset",
		Limitations: []string{
			"Screening reflects the loaded dataset version; confirm against the current official list before acting.",
			"This is a screening hit, not a legal determination.",
		},
	}
	return r
}

func ruleSanctionsOneHop(ev *Evidence, acts []blockchain.Activity) (ruleResult, map[string]bool) {
	r := ruleResult{eval: RuleEvaluation{RuleID: RuleSanctionsOneHop}}
	if ev.Lookup == nil {
		r.eval.Status, r.eval.Reason = EvalNotEvaluated, "sanctions dataset unavailable"
		return r, nil
	}
	if len(acts) == 0 {
		return notEvaluatedNoHistory(r, ev), nil
	}
	x := newExposure()
	for i := range acts {
		a := &acts[i]
		if !usable(a) {
			continue
		}
		cp, dir := counterpartyOf(a, ev.Address)
		if cp == "" {
			continue
		}
		if recs := ev.Lookup(cp); len(recs) > 0 {
			x.add(a, cp, dir, "OFAC SDN listed: "+recordNames(recs), recs)
		}
	}
	flagged := map[string]bool{}
	for a := range x.cp {
		flagged[a] = true
	}
	if len(x.cp) == 0 {
		r.eval.Status = EvalNotTriggered
		return r, flagged
	}
	r.eval.Status = EvalTriggered
	f := &Finding{
		RuleID: RuleSanctionsOneHop, Title: "Direct counterparty of a sanctioned address", MaxPoints: PointsOneHopOutgoing,
		Counterparties: x.counterparties(), EvidenceTimeRange: &x.tr,
		Sources: []string{historySource(ev), sanctionsSource(ev)},
		Limitations: []string{
			"Exposure is limited to the reviewed activity window.",
			"One-hop exposure does not mean the queried wallet is itself sanctioned or controlled by a sanctioned party.",
		},
	}
	f.EvidenceTxHashes, f.EvidenceTruncated = sortedHashes(x.hashes)
	if x.outgoing {
		f.Points, f.Severity = PointsOneHopOutgoing, "high"
		f.Explanation = fmt.Sprintf("The wallet sent value to or called %d sanctioned address(es) in %d transaction(s).", len(x.cp), len(x.hashes))
		f.Confidence = "high: confirmed on-chain transfers to an exactly matched SDN identifier"
	} else {
		f.Points, f.Severity = PointsOneHopIncoming, "medium"
		f.Explanation = fmt.Sprintf("The wallet received value from, or appeared as sender in unsigned token events with, %d sanctioned address(es) in %d transaction(s); it did not itself send to them in the reviewed window.", len(x.cp), len(x.hashes))
		f.Confidence = "medium: incoming transfers and unsigned token events can be unsolicited (dusting, address poisoning)"
		f.Limitations = append(f.Limitations, "Incoming transfers and token events in transactions the wallet did not sign do not require its consent.")
	}
	r.finding = f
	return r, flagged
}

func ruleMixer(ev *Evidence, cfg *Config, acts []blockchain.Activity, sanctionedCPs map[string]bool) ruleResult {
	r := ruleResult{eval: RuleEvaluation{RuleID: RuleMixer}}
	if len(cfg.Mixers) == 0 {
		r.eval.Status, r.eval.Reason = EvalNotEvaluated, "no mixer addresses configured"
		return r
	}
	if len(acts) == 0 {
		return notEvaluatedNoHistory(r, ev)
	}
	x := newExposure()
	suppressed := newExposure()
	for i := range acts {
		a := &acts[i]
		if !usable(a) {
			continue
		}
		cp, dir := counterpartyOf(a, ev.Address)
		label, ok := cfg.Mixers[cp]
		if !ok {
			continue
		}
		if sanctionedCPs[cp] {
			suppressed.add(a, cp, dir, label, nil)
			continue
		}
		x.add(a, cp, dir, label, nil)
	}
	if len(x.cp) == 0 && len(suppressed.cp) == 0 {
		r.eval.Status = EvalNotTriggered
		return r
	}
	r.eval.Status = EvalTriggered
	f := &Finding{
		RuleID: RuleMixer, Title: "Interaction with a configured mixer contract", MaxPoints: PointsMixerOutgoing,
		Sources: []string{historySource(ev), "WalletScope mixer list (WALLETSCOPE_MIXER_ADDRESSES)"},
		Limitations: []string{
			"Mixers have legitimate privacy uses; interaction alone is not evidence of wrongdoing.",
			"Only configured mixer addresses are recognised; other mixers are not detected.",
		},
	}
	if len(x.cp) == 0 {
		// All mixer evidence overlaps sanctions one-hop exposure: show, but do not double count.
		x = suppressed
		f.SuppressedReason = "every mixer counterparty is also sanctioned; counted once under " + RuleSanctionsOneHop
	}
	f.Counterparties, f.EvidenceTimeRange = x.counterparties(), &x.tr
	f.EvidenceTxHashes, f.EvidenceTruncated = sortedHashes(x.hashes)
	if x.outgoing {
		f.Points, f.Severity = PointsMixerOutgoing, "high"
		f.Explanation = fmt.Sprintf("The wallet sent funds to or called configured mixer contract(s) in %d transaction(s).", len(x.hashes))
		f.Confidence = "high: confirmed transactions to a verified mixer contract address"
	} else {
		f.Points, f.Severity = PointsMixerIncoming, "medium"
		f.Explanation = fmt.Sprintf("The wallet received funds from configured mixer contract(s) (withdrawals) or appeared in unsigned token events with them, in %d transaction(s).", len(x.hashes))
		f.Confidence = "high: confirmed transfers from a verified mixer contract address"
	}
	if f.SuppressedReason != "" {
		f.Points = 0
	}
	r.finding = f
	return r
}

func ruleStructuring(ev *Evidence, cfg *Config, acts []blockchain.Activity) ruleResult {
	r := ruleResult{eval: RuleEvaluation{RuleID: RuleStructuring}}
	if len(acts) == 0 {
		return notEvaluatedNoHistory(r, ev)
	}
	type tr struct {
		a *blockchain.Activity
		t time.Time
	}
	in, out := map[string][]tr{}, map[string][]tr{}
	for i := range acts {
		a := &acts[i]
		if !a.TransferConfirmed || a.Timestamp == nil || a.Value().Sign() <= 0 {
			continue
		}
		if a.Asset.Type == blockchain.AssetERC20 && a.Asset.Reputation != "" && a.Asset.Reputation != "ok" {
			continue
		}
		switch a.DirectionFor(ev.Address) {
		case blockchain.DirIn:
			in[a.Asset.Key()] = append(in[a.Asset.Key()], tr{a, *a.Timestamp})
		case blockchain.DirOut:
			if a.InitiatedByWallet != nil && !*a.InitiatedByWallet {
				continue // not the wallet's own outflow
			}
			out[a.Asset.Key()] = append(out[a.Asset.Key()], tr{a, *a.Timestamp})
		}
	}
	type candidate struct {
		out      *blockchain.Activity
		deposits []*blockchain.Activity
		sum      *big.Int
	}
	var best *candidate
	better := func(c, b *candidate) bool {
		if b == nil {
			return true
		}
		if len(c.deposits) != len(b.deposits) {
			return len(c.deposits) > len(b.deposits)
		}
		if cmp := c.out.Value().Cmp(b.out.Value()); cmp != 0 {
			return cmp > 0
		}
		if !c.out.Timestamp.Equal(*b.out.Timestamp) {
			return c.out.Timestamp.Before(*b.out.Timestamp)
		}
		return c.out.ID() < b.out.ID()
	}
	mult := big.NewInt(int64(cfg.StructuringMultiple))
	for asset, outs := range out {
		for _, o := range outs {
			amt := o.a.Value()
			c := &candidate{out: o.a, sum: new(big.Int)}
			for _, d := range in[asset] {
				if !d.t.Before(o.t) || d.t.Before(o.t.Add(-cfg.StructuringWindow)) {
					continue
				}
				if new(big.Int).Mul(d.a.Value(), mult).Cmp(amt) > 0 {
					continue // deposit not small relative to the outflow
				}
				c.deposits = append(c.deposits, d.a)
				c.sum.Add(c.sum, d.a.Value())
			}
			if len(c.deposits) < cfg.StructuringMinDeposits {
				continue
			}
			// sum*100 >= pct*amt
			lhs := new(big.Int).Mul(c.sum, big.NewInt(100))
			rhs := new(big.Int).Mul(amt, big.NewInt(int64(cfg.StructuringCoveragePct)))
			if lhs.Cmp(rhs) < 0 {
				continue
			}
			if better(c, best) {
				best = c
			}
		}
	}
	if best == nil {
		r.eval.Status = EvalNotTriggered
		return r
	}
	r.eval.Status = EvalTriggered
	sort.Slice(best.deposits, func(i, j int) bool { return best.deposits[i].ID() < best.deposits[j].ID() })
	hashes := map[string]bool{best.out.TxHash: true}
	first := *best.out.Timestamp
	for _, d := range best.deposits {
		hashes[d.TxHash] = true
		if d.Timestamp.Before(first) {
			first = *d.Timestamp
		}
	}
	dec := -1
	if best.out.Asset.Decimals != nil {
		dec = *best.out.Asset.Decimals
	}
	sym := best.out.Asset.Symbol
	sumAmt := blockchain.NewAmount(best.sum, dec, sym)
	f := &Finding{
		RuleID: RuleStructuring, Title: "Many small deposits followed by a much larger outflow", Severity: "medium",
		Points: PointsStructuring, MaxPoints: PointsStructuring,
		Explanation: fmt.Sprintf("%d incoming %s transfers, each at most 1/%d of the outflow and totalling %s %s, arrived within %s before a %s %s outgoing transfer (tx %s).",
			len(best.deposits), sym, cfg.StructuringMultiple, sumAmt.Formatted, sym, cfg.StructuringWindow, best.out.Amount.Formatted, sym, best.out.TxHash),
		Counterparties:    []Counterparty{{Address: best.out.ToAddress(), Role: "recipient of the large outflow", Direction: "sent_to", TxCount: 1}},
		EvidenceTimeRange: &TimeRange{First: &first, Last: best.out.Timestamp},
		Metrics: map[string]string{
			"asset":            assetLabel(best.out.Asset),
			"deposit_count":    fmt.Sprint(len(best.deposits)),
			"deposit_total":    sumAmt.Formatted,
			"outflow_amount":   best.out.Amount.Formatted,
			"outflow_tx":       best.out.TxHash,
			"window":           cfg.StructuringWindow.String(),
			"min_deposits":     fmt.Sprint(cfg.StructuringMinDeposits),
			"size_multiple":    fmt.Sprint(cfg.StructuringMultiple),
			"min_coverage_pct": fmt.Sprint(cfg.StructuringCoveragePct),
		},
		Sources:    []string{historySource(ev)},
		Confidence: "low: the pattern is consistent with aggregation before transfer but also with payroll, merchants and exchange sweeps",
		Limitations: []string{
			"Pattern evaluated only within the reviewed activity window.",
			"Amounts are compared per asset; cross-asset aggregation is not considered.",
		},
	}
	f.EvidenceTxHashes, f.EvidenceTruncated = sortedHashes(hashes)
	r.finding = f
	return r
}

func ruleNewWallet(ev *Evidence, cfg *Config, acts []blockchain.Activity) ruleResult {
	r := ruleResult{eval: RuleEvaluation{RuleID: RuleNewWallet}}
	if len(acts) == 0 {
		return notEvaluatedNoHistory(r, ev)
	}
	var oldest *time.Time
	for i := range acts {
		if ts := acts[i].Timestamp; ts != nil && (oldest == nil || ts.Before(*oldest)) {
			oldest = ts
		}
	}
	if oldest == nil {
		r.eval.Status, r.eval.Reason = EvalNotEvaluated, "no timestamped activity available"
		return r
	}
	age := ev.Now.Sub(*oldest)
	if !ev.History.Complete() {
		if age > cfg.NewWalletAge {
			r.eval.Status = EvalNotTriggered
			r.eval.Reason = fmt.Sprintf("activity older than %s was observed, so the wallet is not new", humanDuration(cfg.NewWalletAge))
			return r
		}
		r.eval.Status, r.eval.Reason = EvalNotEvaluated, "full history not read; wallet age cannot be established"
		return r
	}
	if age > cfg.NewWalletAge {
		r.eval.Status = EvalNotTriggered
		r.eval.Reason = fmt.Sprintf("first activity %s is older than %s", oldest.Format(time.RFC3339), humanDuration(cfg.NewWalletAge))
		return r
	}
	var large []*blockchain.Activity
	for i := range acts {
		a := &acts[i]
		if a.TransferConfirmed && a.Asset.Type == blockchain.AssetNative && a.DirectionFor(ev.Address) != "" &&
			a.DirectionFor(ev.Address) != blockchain.DirSelf && a.Value().Cmp(cfg.LargeTransferWei) >= 0 {
			large = append(large, a)
		}
	}
	if len(large) == 0 {
		r.eval.Status = EvalNotTriggered
		r.eval.Reason = "wallet is new but no native transfer reached the large-transfer threshold"
		return r
	}
	sort.Slice(large, func(i, j int) bool {
		if c := large[i].Value().Cmp(large[j].Value()); c != 0 {
			return c > 0
		}
		return large[i].ID() < large[j].ID()
	})
	if len(large) > 5 {
		large = large[:5]
	}
	hashes := map[string]bool{}
	var cps []Counterparty
	for _, a := range large {
		hashes[a.TxHash] = true
		cp, dir := counterpartyOf(a, ev.Address)
		cps = append(cps, Counterparty{Address: cp, Role: "counterparty of large transfer (" + a.Amount.Formatted + " ETH)", Direction: dir, TxCount: 1})
	}
	r.eval.Status = EvalTriggered
	f := &Finding{
		RuleID: RuleNewWallet, Title: "New wallet with a large ETH transfer", Severity: "low",
		Points: PointsNewWalletTransfer, MaxPoints: PointsNewWalletTransfer,
		Explanation: fmt.Sprintf("The wallet's first indexed activity was %s (%s before this report) and it moved %s ETH in a single transfer, at or above the %s ETH threshold.",
			oldest.Format(time.RFC3339), humanDuration(age), large[0].Amount.Formatted, blockchain.FormatUnits(cfg.LargeTransferWei, 18)),
		Counterparties:    cps,
		EvidenceTimeRange: &TimeRange{First: oldest, Last: large[0].Timestamp},
		Metrics: map[string]string{
			"first_activity":   oldest.Format(time.RFC3339),
			"wallet_age":       humanDuration(age),
			"age_threshold":    humanDuration(cfg.NewWalletAge),
			"largest_transfer": large[0].Amount.Formatted + " ETH",
			"threshold_eth":    blockchain.FormatUnits(cfg.LargeTransferWei, 18),
		},
		Sources:    []string{historySource(ev)},
		Confidence: "medium: wallet age is established from the complete indexed history",
		Limitations: []string{
			"New wallets moving large sums are common for legitimate users (e.g. fresh cold storage).",
			"Only native ETH is measured; token transfers are not valued.",
		},
	}
	f.EvidenceTxHashes, _ = sortedHashes(hashes)
	r.finding = f
	return r
}

func notEvaluatedNoHistory(r ruleResult, ev *Evidence) ruleResult {
	if ev.History == nil || ev.History.AllSourcesFailed() {
		r.eval.Status, r.eval.Reason = EvalNotEvaluated, "activity history unavailable"
		return r
	}
	r.eval.Status, r.eval.Reason = EvalNotTriggered, "no indexed activity"
	return r
}

func recordNames(recs []sanctions.Record) string {
	seen := map[string]bool{}
	var names []string
	for _, r := range recs {
		n := r.Name + " [SDN uid " + r.UID + ", " + strings.Join(r.Programs, "/") + "]"
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return strings.Join(names, "; ")
}

func sanctionsSource(ev *Evidence) string {
	s := "OFAC SDN list"
	if ev.SanctionsInfo.PublishDate != "" {
		s += " published " + ev.SanctionsInfo.PublishDate
	}
	if ev.SanctionsInfo.LastSuccessfulLoad != nil {
		s += ", loaded " + ev.SanctionsInfo.LastSuccessfulLoad.UTC().Format(time.RFC3339)
	}
	return s
}

func historySource(ev *Evidence) string {
	return fmt.Sprintf("%s activity history (%s), retrieved %s", ev.ProviderName, ev.ProviderURL, ev.HistoryFetched.UTC().Format(time.RFC3339))
}

func assetLabel(a blockchain.Asset) string {
	if a.Type == blockchain.AssetNative {
		return "ETH"
	}
	return a.Symbol + " (" + a.Contract + ")"
}

func humanDuration(d time.Duration) string {
	days := int(d.Hours() / 24)
	if days >= 1 {
		return fmt.Sprintf("%d days", days)
	}
	return d.Round(time.Minute).String()
}
