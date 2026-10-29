package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/amnariaz007/walletscope-mcp/internal/blockchain"
)

func edgeMap(r *TraceResult) map[string]TraceEdge {
	m := map[string]TraceEdge{}
	for _, e := range r.Edges {
		m[e.From+">"+e.To] = e
	}
	return m
}

func nodeMap(r *TraceResult) map[string]TraceNode {
	m := map[string]TraceNode{}
	for _, n := range r.Nodes {
		m[n.Address] = n
	}
	return m
}

func TestTraceAggregatesAndRespectsDepth(t *testing.T) {
	f := newFake()
	h1 := f.tx(addrA, addrB, "2", true)
	h2 := f.tx(addrA, addrB, "3", true)
	f.tx(addrA, addrC, "1", false) // failed: must not appear
	f.tx(addrB, addrD, "4", true)
	f.tx(addrD, addrE, "4", true) // hop 3, beyond hops=2
	s := testService(f)

	r, err := s.TraceFunds(context.Background(), TraceRequest{Address: addrA, Hops: 2, MinAmount: "0.1"})
	if err != nil {
		t.Fatal(err)
	}
	edges := edgeMap(r)
	ab, ok := edges[addrA+">"+addrB]
	if !ok || ab.Total.Formatted != "5" || ab.TransferCount != 2 || ab.Hop != 1 {
		t.Fatalf("A>B edge wrong: %+v", ab)
	}
	if len(ab.TxHashes) != 2 || !(ab.TxHashes[0] == h1 || ab.TxHashes[1] == h1) || !(ab.TxHashes[0] == h2 || ab.TxHashes[1] == h2) {
		t.Fatalf("evidence hashes missing: %v", ab.TxHashes)
	}
	if _, ok := edges[addrA+">"+addrC]; ok {
		t.Fatal("failed transfer must not create an edge")
	}
	if bd := edges[addrB+">"+addrD]; bd.Hop != 2 || bd.Total.Formatted != "4" {
		t.Fatalf("B>D: %+v", bd)
	}
	if _, ok := edges[addrD+">"+addrE]; ok {
		t.Fatal("trace exceeded requested hops")
	}
	nodes := nodeMap(r)
	if nodes[addrD].StopReason != StopMaxHops || nodes[addrD].Hop != 2 {
		t.Fatalf("D node: %+v", nodes[addrD])
	}
	if r.Stats.UnconfirmedSeen != 1 || r.Partial {
		t.Fatalf("stats=%+v partial=%v", r.Stats, r.Partial)
	}
}

func TestTraceMinimumAmountAndSelfTransfers(t *testing.T) {
	f := newFake()
	f.tx(addrA, addrB, "1", true)
	f.tx(addrA, addrC, "5", true)
	f.tx(addrA, addrA, "9", true)
	r, err := testService(f).TraceFunds(context.Background(), TraceRequest{Address: addrA, Hops: 1, MinAmount: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Edges) != 1 || r.Edges[0].To != addrC {
		t.Fatalf("edges: %+v", r.Edges)
	}
	if r.Stats.BelowMinimum != 1 || r.Stats.SelfTransfers != 1 {
		t.Fatalf("stats: %+v", r.Stats)
	}
	if r.MinAmount.Raw != "2000000000000000000" {
		t.Fatalf("min amount: %+v", r.MinAmount)
	}
}

func TestTraceHandlesCycles(t *testing.T) {
	f := newFake()
	f.tx(addrA, addrB, "5", true)
	f.tx(addrB, addrC, "5", true)
	f.tx(addrC, addrA, "5", true) // cycle back to start
	f.tx(addrC, addrB, "1", true) // and back to B
	r, err := testService(f).TraceFunds(context.Background(), TraceRequest{Address: addrA, Hops: 3, MinAmount: "0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Nodes) != 3 {
		t.Fatalf("nodes must be unique: %+v", r.Nodes)
	}
	edges := edgeMap(r)
	if ca := edges[addrC+">"+addrA]; !ca.ToVisitedEarlier || ca.Hop != 3 {
		t.Fatalf("cycle edge: %+v", ca)
	}
	if len(r.Edges) != 4 {
		t.Fatalf("edges: %d", len(r.Edges))
	}
	if r.Stats.NodesExpanded != 3 {
		t.Fatalf("each node must be expanded once, got %d", r.Stats.NodesExpanded)
	}
}

func TestTraceContractsAreLeavesAndFlagsReported(t *testing.T) {
	f := newFake()
	f.tx(addrA, addrB, "5", true)
	f.tx(addrA, addrC, "5", true)
	f.tx(addrB, addrD, "5", true)
	f.markContract(addrB)
	s := testService(f)
	s.Annotate = func(a string) []string {
		if a == addrC {
			return []string{"ofac_sdn"}
		}
		return nil
	}
	r, err := s.TraceFunds(context.Background(), TraceRequest{Address: addrA, Hops: 3, MinAmount: "0"})
	if err != nil {
		t.Fatal(err)
	}
	nodes := nodeMap(r)
	if nodes[addrB].StopReason != StopContractLeaf || nodes[addrB].Expanded {
		t.Fatalf("contract must be a leaf: %+v", nodes[addrB])
	}
	if nodes[addrC].StopReason != StopFlaggedLeaf {
		t.Fatalf("flagged node: %+v", nodes[addrC])
	}
	if len(r.FlaggedNodes) != 1 || r.FlaggedNodes[0].Address != addrC || fmt.Sprint(r.FlaggedNodes[0].Path) != fmt.Sprint([]string{addrA, addrC}) {
		t.Fatalf("flagged: %+v", r.FlaggedNodes)
	}
}

func TestTraceNodeLimitProducesPartial(t *testing.T) {
	f := newFake()
	for i := 0; i < 8; i++ {
		f.tx(addrA, fmt.Sprintf("0x%040x", 0x100+i), "1", true)
	}
	s := testService(f)
	s.Trace.MaxNodes = 4
	r, err := s.TraceFunds(context.Background(), TraceRequest{Address: addrA, Hops: 1, MinAmount: "0"})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Partial || len(r.Nodes) != 4 || len(r.Edges) != 3 || len(r.Warnings) == 0 {
		t.Fatalf("partial=%v nodes=%d edges=%d warnings=%v", r.Partial, len(r.Nodes), len(r.Edges), r.Warnings)
	}
}

func TestTraceChildLimitKeepsLargest(t *testing.T) {
	f := newFake()
	for i := 1; i <= 5; i++ {
		f.tx(addrA, fmt.Sprintf("0x%040x", 0x200+i), fmt.Sprint(i), true)
	}
	s := testService(f)
	s.Trace.MaxChildren = 2
	r, _ := s.TraceFunds(context.Background(), TraceRequest{Address: addrA, Hops: 1, MinAmount: "0"})
	if len(r.Edges) != 2 || r.Edges[0].Total.Formatted != "5" || r.Edges[1].Total.Formatted != "4" {
		t.Fatalf("edges: %+v", r.Edges)
	}
	if !nodeMap(r)[addrA].OutflowsTruncated {
		t.Fatal("truncation must be reported")
	}
}

func TestTraceAPIBudget(t *testing.T) {
	f := newFake()
	f.tx(addrA, addrB, "1", true)
	f.tx(addrB, addrC, "1", true)
	s := testService(f)
	s.Trace.MaxAPICalls = 2 // enough for A only (transactions + internal)
	r, err := s.TraceFunds(context.Background(), TraceRequest{Address: addrA, Hops: 2, MinAmount: "0"})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Partial || nodeMap(r)[addrB].StopReason != StopBudget || r.Stats.APICalls != 2 {
		t.Fatalf("partial=%v B=%+v calls=%d", r.Partial, nodeMap(r)[addrB], r.Stats.APICalls)
	}
}

func TestTraceProviderFailureIsPartialNotFatal(t *testing.T) {
	f := newFake()
	f.tx(addrA, addrB, "1", true)
	f.tx(addrA, addrC, "1", true)
	f.tx(addrC, addrD, "1", true)
	f.fail[addrB] = &blockchain.UpstreamError{Category: "transactions", Status: 503, Err: blockchain.ErrUnavailable}
	r, err := testService(f).TraceFunds(context.Background(), TraceRequest{Address: addrA, Hops: 2, MinAmount: "0"})
	if err != nil {
		t.Fatal(err)
	}
	nodes := nodeMap(r)
	if !r.Partial || nodes[addrB].Expanded || nodes[addrB].StopReason == "" || !nodes[addrC].Expanded {
		t.Fatalf("partial=%v B=%+v C=%+v", r.Partial, nodes[addrB], nodes[addrC])
	}
	if _, ok := edgeMap(r)[addrC+">"+addrD]; !ok {
		t.Fatal("healthy branch must still be traced")
	}
}

func TestTraceCancellationAndWorkerBound(t *testing.T) {
	f := newFake()
	for i := 0; i < 10; i++ {
		f.tx(addrA, fmt.Sprintf("0x%040x", 0x300+i), "1", true)
	}
	f.delay = 50 * time.Millisecond
	s := testService(f)
	s.Trace.Workers = 3
	r, err := s.TraceFunds(context.Background(), TraceRequest{Address: addrA, Hops: 2, MinAmount: "0"})
	if err != nil {
		t.Fatal(err)
	}
	if f.maxFlight.Load() > 3 {
		t.Fatalf("worker bound exceeded: %d concurrent calls", f.maxFlight.Load())
	}
	if r.Stats.NodesExpanded != 11 {
		t.Fatalf("expanded %d", r.Stats.NodesExpanded)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	f.delay = 200 * time.Millisecond
	start := time.Now()
	r, err = s.TraceFunds(ctx, TraceRequest{Address: addrA, Hops: 3, MinAmount: "0"})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancellation not propagated promptly")
	}
	if !r.Partial || nodeMap(r)[addrA].StopReason != StopCancelled {
		t.Fatalf("partial=%v root=%+v", r.Partial, nodeMap(r)[addrA])
	}
}

func TestTraceIsDeterministic(t *testing.T) {
	f := newFake()
	for i := 0; i < 6; i++ {
		mid := fmt.Sprintf("0x%040x", 0x400+i)
		f.tx(addrA, mid, "2", true)
		f.tx(mid, fmt.Sprintf("0x%040x", 0x500+i%3), "1", true)
	}
	s := testService(f)
	graph := func() string {
		r, _ := s.TraceFunds(context.Background(), TraceRequest{Address: addrA, Hops: 2, MinAmount: "0"})
		b, _ := json.Marshal([]any{r.Nodes, r.Edges})
		return string(b)
	}
	first := graph()
	for i := 0; i < 5; i++ {
		if graph() != first {
			t.Fatal("trace output depends on goroutine scheduling")
		}
	}
}

func TestValidateTrace(t *testing.T) {
	s := testService(newFake())
	bad := []TraceRequest{
		{Address: addrA, Hops: 0, MinAmount: "1"},
		{Address: addrA, Hops: 4, MinAmount: "1"},
		{Address: addrA, Hops: 1, MinAmount: "-1"},
		{Address: addrA, Hops: 1, MinAmount: "abc"},
		{Address: addrA, Hops: 1, MinAmount: "0.0000000000000000001"},
		{Address: addrA, Hops: 1, MinAmount: "1", Asset: "USDT"},
	}
	for _, r := range bad {
		if _, err := s.ValidateTrace(r); err == nil {
			t.Errorf("expected error for %+v", r)
		}
	}
	if c, err := s.ValidateTrace(TraceRequest{Address: addrA, Hops: 3, MinAmount: "0.5", Asset: "0xdac17f958d2ee523a2206206994597c13d831ec7"}); err != nil || c != "0xdAC17F958D2ee523a2206206994597C13D831ec7" {
		t.Fatalf("token asset: %q %v", c, err)
	}
}

func TestTraceTokenAssetSeparation(t *testing.T) {
	f := newFake()
	usdt := "0xdAC17F958D2ee523a2206206994597C13D831ec7"
	dec := 6
	b := uint64(5000)
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tok := blockchain.Activity{Kind: blockchain.KindTokenTransfer, TxHash: "0xt1", LogIndex: new(int), BlockNumber: &b, Timestamp: &ts,
		From: blockchain.AddressRef{Address: addrA}, To: &blockchain.AddressRef{Address: addrB}, Status: blockchain.StatusSuccess, TransferConfirmed: true,
		Asset: blockchain.Asset{Type: blockchain.AssetERC20, Symbol: "USDT", Contract: usdt, Decimals: &dec}}
	tok.SetValue(eth("0").SetInt64(2_500_000)) // 2.5 USDT
	f.acts = append(f.acts, tok)
	f.tx(addrA, addrC, "1", true) // ETH must not appear in a USDT trace
	r, err := testService(f).TraceFunds(context.Background(), TraceRequest{Address: addrA, Hops: 1, MinAmount: "1", Asset: usdt})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Edges) != 1 || r.Edges[0].To != addrB || r.Edges[0].Total.Formatted != "2.5" || r.Edges[0].Total.Symbol != "USDT" {
		t.Fatalf("edges: %+v", r.Edges)
	}
	if r.MinAmount.Raw != "1000000" {
		t.Fatalf("min: %+v", r.MinAmount)
	}
	_ = errors.New
}
