package risk

import (
	"encoding/json"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/amnariaz007/walletscope-mcp/internal/blockchain"
	"github.com/amnariaz007/walletscope-mcp/internal/investigation"
	"github.com/amnariaz007/walletscope-mcp/internal/sanctions"
)

// Synthetic, obviously artificial addresses.
const (
	wallet     = "0x1111111111111111111111111111111111111111"
	other      = "0x2222222222222222222222222222222222222222"
	sanctioned = "0x9999999999999999999999999999999999999999"
	mixer      = "0x7777777777777777777777777777777777777777"
)

var now = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func wei(eth string) *big.Int {
	v, err := blockchainParse(eth)
	if err != nil {
		panic(err)
	}
	return v
}

func blockchainParse(eth string) (*big.Int, error) {
	r, ok := new(big.Rat).SetString(eth)
	if !ok {
		return nil, fmt.Errorf("bad %s", eth)
	}
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)))
	return new(big.Int).Div(r.Num(), r.Denom()), nil
}

type builder struct {
	n    int
	acts []blockchain.Activity
}

// eth adds a confirmed native transfer at now - ago.
func (b *builder) eth(from, to, amount string, ago time.Duration) string {
	return b.add(blockchain.KindNativeTransfer, from, to, amount, ago, true)
}

func (b *builder) add(kind, from, to, amount string, ago time.Duration, ok bool) string {
	b.n++
	blk := uint64(1_000_000 + b.n)
	ts := now.Add(-ago)
	hash := fmt.Sprintf("0x%064x", b.n)
	a := blockchain.Activity{Kind: kind, TxHash: hash, BlockNumber: &blk, Timestamp: &ts,
		From: blockchain.AddressRef{Address: from}, To: &blockchain.AddressRef{Address: to},
		Asset: blockchain.NativeAsset(), Status: blockchain.StatusSuccess}
	if !ok {
		a.Status = blockchain.StatusFailed
	}
	a.SetValue(wei(amount))
	a.TransferConfirmed = ok && kind != blockchain.KindContractCall && a.Value().Sign() > 0
	b.acts = append(b.acts, a)
	return hash
}

func history(acts []blockchain.Activity, complete bool) *investigation.History {
	cov := []investigation.SourceCoverage{
		{Source: investigation.SourceTransactions, Pages: 1, Items: len(acts), Complete: complete},
		{Source: investigation.SourceInternal, Pages: 1, Complete: true},
		{Source: investigation.SourceTokens, Pages: 1, Complete: true},
	}
	h := &investigation.History{Address: wallet, Activities: append([]blockchain.Activity(nil), acts...), Coverage: cov}
	investigation.SortNewestFirst(h.Activities)
	return h
}

var sdnRecord = sanctions.Record{UID: "9999001", Name: "SYNTHETIC TEST ENTITY", Type: "Entity", Programs: []string{"TEST-PROGRAM"},
	IDType: "Digital Currency Address - ETH", ListedAsset: "ETH", ListedValue: sanctioned}

func evidence(acts []blockchain.Activity, complete bool) *Evidence {
	loadedAt := now.Add(-time.Hour)
	return &Evidence{
		Address: wallet, Now: now,
		Direct:        sanctions.Result{Address: wallet, Status: sanctions.StatusNoMatch, Matches: []sanctions.Record{}},
		SanctionsInfo: sanctions.DatasetInfo{Name: "OFAC SDN", PublishDate: "10/28/2026", LastSuccessfulLoad: &loadedAt},
		Lookup: func(a string) []sanctions.Record {
			if a == sanctioned {
				return []sanctions.Record{sdnRecord}
			}
			return nil
		},
		History: history(acts, complete), HistoryRead: true,
		ProviderName: "blockscout", ProviderURL: "https://example.invalid/api/v2", HistoryFetched: now,
	}
}

func cfg() *Config {
	return &Config{
		Mixers:                 map[string]string{mixer: "Test mixer"},
		StructuringMinDeposits: 5, StructuringWindow: 72 * time.Hour, StructuringMultiple: 3, StructuringCoveragePct: 50,
		NewWalletAge: 30 * 24 * time.Hour, LargeTransferWei: wei("10"),
	}
}

func finding(r Report, id string) *Finding {
	for i := range r.Findings {
		if r.Findings[i].RuleID == id {
			return &r.Findings[i]
		}
	}
	return nil
}

func evalOf(r Report, id string) RuleEvaluation {
	for _, e := range r.RuleEvaluations {
		if e.RuleID == id {
			return e
		}
	}
	return RuleEvaluation{}
}

// Regression 1: a direct sanctions match triggers the expected finding and score 100.
func TestDirectSanctionsMatch(t *testing.T) {
	ev := evidence(nil, true)
	ev.Address = sanctioned
	ev.Direct = sanctions.Result{Address: sanctioned, Status: sanctions.StatusMatch, Matches: []sanctions.Record{sdnRecord}}
	r := Evaluate(ev, cfg())
	f := finding(r, RuleSanctionsDirect)
	if f == nil || r.Score != 100 || r.RiskBand != "severe" || f.Severity != "critical" || len(f.SanctionsRecords) != 1 {
		t.Fatalf("score=%d finding=%+v", r.Score, f)
	}
}

// Regression 2: one-hop exposure is distinguished from a direct match.
func TestOneHopIsNotDirect(t *testing.T) {
	var b builder
	hOut := b.eth(wallet, sanctioned, "1", 48*time.Hour)
	r := Evaluate(evidence(b.acts, true), cfg())
	if finding(r, RuleSanctionsDirect) != nil || evalOf(r, RuleSanctionsDirect).Status != EvalNotTriggered {
		t.Fatal("one-hop exposure must not produce a direct match")
	}
	f := finding(r, RuleSanctionsOneHop)
	if f == nil || f.Points != PointsOneHopOutgoing || r.Score != 60 || f.EvidenceTxHashes[0] != hOut {
		t.Fatalf("score=%d f=%+v", r.Score, f)
	}
	if len(f.Counterparties) != 1 || f.Counterparties[0].Direction != "sent_to" || len(f.Counterparties[0].Records) != 1 {
		t.Fatalf("counterparties: %+v", f.Counterparties)
	}

	// Incoming-only exposure is weighted lower.
	var b2 builder
	b2.eth(sanctioned, wallet, "0.01", 48*time.Hour)
	r2 := Evaluate(evidence(b2.acts, true), cfg())
	if f := finding(r2, RuleSanctionsOneHop); f == nil || f.Points != PointsOneHopIncoming || f.Severity != "medium" {
		t.Fatalf("incoming: %+v", f)
	}
}

// Regression 3: mixer interaction is detected only for configured addresses.
func TestMixerOnlyForConfiguredAddresses(t *testing.T) {
	var b builder
	b.eth(wallet, other, "1", 24*time.Hour)
	r := Evaluate(evidence(b.acts, true), cfg())
	if finding(r, RuleMixer) != nil {
		t.Fatal("unconfigured counterparty flagged as mixer")
	}
	h := b.eth(wallet, mixer, "1", 12*time.Hour)
	r = Evaluate(evidence(b.acts, true), cfg())
	f := finding(r, RuleMixer)
	if f == nil || f.Points != PointsMixerOutgoing || f.EvidenceTxHashes[0] != h || f.Counterparties[0].Role != "Test mixer" {
		t.Fatalf("mixer: %+v", f)
	}
	c := cfg()
	c.Mixers = nil
	if e := evalOf(Evaluate(evidence(b.acts, true), c), RuleMixer); e.Status != EvalNotEvaluated {
		t.Fatalf("no mixer list must be not_evaluated: %+v", e)
	}
}

func TestFailedTransactionsAreNotEvidence(t *testing.T) {
	var b builder
	b.add(blockchain.KindNativeTransfer, wallet, mixer, "1", time.Hour, false)
	b.add(blockchain.KindNativeTransfer, wallet, sanctioned, "1", time.Hour, false)
	r := Evaluate(evidence(b.acts, true), cfg())
	if len(r.Findings) != 0 || r.Score != 0 || r.RiskBand != "none_detected" {
		t.Fatalf("failed txs produced findings: %+v", r.Findings)
	}
}

func TestOverlapIsCountedOnce(t *testing.T) {
	var b builder
	b.eth(wallet, sanctioned, "1", time.Hour)
	c := cfg()
	c.Mixers[sanctioned] = "Sanctioned mixer"
	r := Evaluate(evidence(b.acts, true), c)
	m := finding(r, RuleMixer)
	if m == nil || m.Points != 0 || m.SuppressedReason == "" {
		t.Fatalf("overlapping mixer evidence must be shown with 0 points: %+v", m)
	}
	if r.Score != 60 {
		t.Fatalf("score = %d, want 60 (no double counting)", r.Score)
	}
}

// Regression 4: deposit-then-outflow triggers only when every condition holds.
func TestStructuringConditions(t *testing.T) {
	build := func(n int, depositETH, outETH string, spread time.Duration) []blockchain.Activity {
		var b builder
		for i := 0; i < n; i++ {
			b.eth(fmt.Sprintf("0x%040x", 0xa00+i), wallet, depositETH, 10*time.Hour+time.Duration(i)*spread)
		}
		b.eth(wallet, other, outETH, time.Hour)
		return b.acts
	}
	cases := []struct {
		name string
		acts []blockchain.Activity
		want bool
	}{
		{"pattern holds", build(6, "1", "5", time.Hour), true},
		{"too few deposits", build(4, "1", "4", time.Hour), false},
		{"deposits not small enough", build(6, "2", "5", time.Hour), false}, // 2*3 > 5
		{"coverage below 50%", build(5, "1", "11", time.Hour), false},       // 5 < 5.5
		{"deposits outside window", build(6, "1", "5", 20*time.Hour), false},
	}
	for _, c := range cases {
		r := Evaluate(evidence(c.acts, true), cfg())
		f := finding(r, RuleStructuring)
		if (f != nil) != c.want {
			t.Errorf("%s: triggered=%v want %v (%+v)", c.name, f != nil, c.want, evalOf(r, RuleStructuring))
		}
		if f != nil && (len(f.EvidenceTxHashes) != 7 || f.Metrics["deposit_count"] != "6" || f.Metrics["deposit_total"] != "6") {
			t.Errorf("%s: evidence %+v", c.name, f)
		}
	}
}

func TestStructuringDoesNotMixAssets(t *testing.T) {
	var b builder
	for i := 0; i < 6; i++ {
		b.eth(other, wallet, "1", 10*time.Hour)
	}
	// Large outflow in a token, not ETH.
	dec := 6
	blk := uint64(9)
	ts := now.Add(-time.Hour)
	tok := blockchain.Activity{Kind: blockchain.KindTokenTransfer, TxHash: "0xtok", LogIndex: new(int), BlockNumber: &blk, Timestamp: &ts,
		From: blockchain.AddressRef{Address: wallet}, To: &blockchain.AddressRef{Address: other}, Status: blockchain.StatusSuccess, TransferConfirmed: true,
		Asset: blockchain.Asset{Type: blockchain.AssetERC20, Symbol: "USDC", Contract: "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48", Decimals: &dec, Reputation: "ok"}}
	tok.SetValue(big.NewInt(100_000_000_000))
	r := Evaluate(evidence(append(b.acts, tok), true), cfg())
	if finding(r, RuleStructuring) != nil {
		t.Fatal("ETH deposits must not be matched against a token outflow")
	}
}

// Regression 5: a new wallet with a large transfer triggers the indicator.
func TestNewWalletLargeTransfer(t *testing.T) {
	var b builder
	b.eth(other, wallet, "50", 5*24*time.Hour)
	h := b.eth(wallet, other, "49", 4*24*time.Hour)
	r := Evaluate(evidence(b.acts, true), cfg())
	f := finding(r, RuleNewWallet)
	if f == nil || f.Points != PointsNewWalletTransfer || f.Metrics["largest_transfer"] != "50 ETH" {
		t.Fatalf("new wallet: %+v", f)
	}
	if !contains(f.EvidenceTxHashes, h) {
		t.Fatal("evidence must include large transfers")
	}

	// Old wallet: not triggered.
	var old builder
	old.eth(other, wallet, "1", 400*24*time.Hour)
	old.eth(other, wallet, "50", 24*time.Hour)
	if e := evalOf(Evaluate(evidence(old.acts, true), cfg()), RuleNewWallet); e.Status != EvalNotTriggered {
		t.Fatalf("old wallet: %+v", e)
	}
	// Truncated history with only recent activity: age unknown -> not evaluated.
	if e := evalOf(Evaluate(evidence(b.acts, false), cfg()), RuleNewWallet); e.Status != EvalNotEvaluated {
		t.Fatalf("truncated history: %+v", e)
	}
	// Truncated history that already shows old activity: provably not new.
	if e := evalOf(Evaluate(evidence(old.acts, false), cfg()), RuleNewWallet); e.Status != EvalNotTriggered {
		t.Fatalf("truncated but old: %+v", e)
	}
}

// Regression 6: missing data never produces an unjustified low-risk result.
func TestMissingDataIsNotLowRisk(t *testing.T) {
	ev := evidence(nil, true)
	ev.Direct = sanctions.Result{Status: sanctions.StatusInconclusive, Reason: "dataset not loaded", Matches: []sanctions.Record{}}
	ev.Lookup = nil
	r := Evaluate(ev, cfg())
	if r.AssessmentStatus != AssessmentInsufficient || r.RiskBand != "indeterminate" || !r.ScoreIsLowerBound {
		t.Fatalf("sanctions unavailable: status=%s band=%s", r.AssessmentStatus, r.RiskBand)
	}
	if evalOf(r, RuleSanctionsDirect).Status != EvalNotEvaluated || evalOf(r, RuleSanctionsOneHop).Status != EvalNotEvaluated {
		t.Fatal("sanctions rules must be not_evaluated")
	}

	ev = evidence(nil, true)
	ev.History.Coverage = []investigation.SourceCoverage{{Source: "transactions", Error: "provider unavailable"}, {Source: "internal_transactions", Error: "x"}, {Source: "token_transfers", Error: "x"}}
	r = Evaluate(ev, cfg())
	if r.RiskBand != "indeterminate" || len(r.Warnings) == 0 {
		t.Fatalf("history unavailable: band=%s warnings=%v", r.RiskBand, r.Warnings)
	}

	var b builder
	b.eth(other, wallet, "1", time.Hour)
	r = Evaluate(evidence(b.acts, false), cfg())
	if r.AssessmentStatus != AssessmentPartial || r.RiskBand != "none_detected_in_reviewed_window" || !r.ScoreIsLowerBound {
		t.Fatalf("truncated: status=%s band=%s", r.AssessmentStatus, r.RiskBand)
	}
}

// Regression 7: every transaction-based finding carries evidence hashes.
func TestFindingsCarryEvidence(t *testing.T) {
	var b builder
	b.eth(wallet, sanctioned, "1", 2*time.Hour)
	b.eth(wallet, mixer, "1", 3*time.Hour)
	for i := 0; i < 6; i++ {
		b.eth(fmt.Sprintf("0x%040x", 0xb00+i), wallet, "2", 10*time.Hour)
	}
	b.eth(wallet, other, "12", time.Hour)
	r := Evaluate(evidence(b.acts, true), cfg())
	if len(r.Findings) != 4 {
		t.Fatalf("expected 4 findings, got %d: %+v", len(r.Findings), r.RuleEvaluations)
	}
	for _, f := range r.Findings {
		if len(f.EvidenceTxHashes) == 0 || len(f.Sources) == 0 || f.Confidence == "" || len(f.Limitations) == 0 || f.EvidenceTimeRange == nil {
			t.Errorf("%s lacks evidence/provenance: %+v", f.RuleID, f)
		}
		for _, h := range f.EvidenceTxHashes {
			if !contains(r.EvidenceTxHashes, h) {
				t.Errorf("report evidence list missing %s", h)
			}
		}
	}
	// 60 + 40 + 25 + 20 = 145 -> capped.
	if r.Score != 100 || r.ScoreIsLowerBound {
		t.Fatalf("score = %d", r.Score)
	}
	if r.Findings[0].RuleID != RuleSanctionsOneHop {
		t.Fatalf("findings must be ordered by points: %s", r.Findings[0].RuleID)
	}
}

func TestBands(t *testing.T) {
	cases := map[int]string{100: "severe", 75: "severe", 74: "high", 50: "high", 49: "moderate", 25: "moderate", 24: "low", 1: "low"}
	for s, want := range cases {
		if got, _ := band(s, AssessmentComplete); got != want {
			t.Errorf("band(%d) = %s want %s", s, got, want)
		}
	}
}

func TestDeterminism(t *testing.T) {
	var b builder
	b.eth(wallet, sanctioned, "1", 2*time.Hour)
	b.eth(mixer, wallet, "1", 3*time.Hour)
	for i := 0; i < 7; i++ {
		b.eth(fmt.Sprintf("0x%040x", 0xc00+i), wallet, "1", 10*time.Hour)
	}
	b.eth(wallet, other, "6", time.Hour)
	first, _ := json.Marshal(Evaluate(evidence(b.acts, true), cfg()))
	for i := 0; i < 20; i++ {
		// Shuffle input order: results must not depend on it.
		acts := append([]blockchain.Activity(nil), b.acts...)
		for j := range acts {
			k := (j*7 + i) % len(acts)
			acts[j], acts[k] = acts[k], acts[j]
		}
		again, _ := json.Marshal(Evaluate(evidence(acts, true), cfg()))
		if string(again) != string(first) {
			t.Fatal("identical evidence produced a different report")
		}
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// Address-poisoning regression: a token contract can emit Transfer(from=wallet)
// in a transaction the wallet never signed. Such events must not count as the
// wallet sending to a sanctioned address.
func TestUnsignedTokenEventIsNotOutgoingExposure(t *testing.T) {
	var b builder
	b.eth(wallet, other, "0.1", 3*time.Hour) // wallet's own signed tx: proves it is an EOA
	dec := 18
	blk := uint64(5)
	ts := now.Add(-time.Hour)
	spoof := blockchain.Activity{Kind: blockchain.KindTokenTransfer, TxHash: "0xspoof", LogIndex: new(int), BlockNumber: &blk, Timestamp: &ts,
		From: blockchain.AddressRef{Address: wallet}, To: &blockchain.AddressRef{Address: sanctioned}, Status: blockchain.StatusSuccess, TransferConfirmed: true,
		Asset: blockchain.Asset{Type: blockchain.AssetERC20, Symbol: "ETH", Contract: "0x37eD000000000000000000000000000000000F6F", Decimals: &dec, Reputation: "ok"}}
	spoof.SetValue(wei("9.9"))
	acts := append(b.acts, spoof)
	investigation.MarkInitiation(wallet, acts, true)
	if acts[1].InitiatedByWallet == nil || *acts[1].InitiatedByWallet {
		t.Fatalf("spoofed event must be marked not initiated: %+v", acts[1].InitiatedByWallet)
	}
	r := Evaluate(evidence(acts, true), cfg())
	f := finding(r, RuleSanctionsOneHop)
	if f == nil || f.Points != PointsOneHopIncoming || f.Counterparties[0].Direction != "unsigned_token_event" {
		t.Fatalf("unsigned token event must be passive-tier exposure: %+v", f)
	}
}
