package investigation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/amnariaz007/walletscope-mcp/internal/blockchain"
)

func i64(v int64) *int64 { return &v }

func TestAddressInfoFirstSeenOnlyWithCompleteHistory(t *testing.T) {
	f := newFake()
	first := f.tx(addrB, addrA, "3", true)
	f.tx(addrA, addrC, "1", true)
	last := f.tx(addrA, addrD, "1", true)
	f.counters[addrA] = &blockchain.Counters{TransactionsCount: i64(3), TokenTransfersCount: i64(0)}
	r, err := testService(f).GetAddressInfo(context.Background(), addrA)
	if err != nil {
		t.Fatal(err)
	}
	if r.FirstObserved == nil || r.FirstObserved.TxHash != first || r.LastObserved.TxHash != last {
		t.Fatalf("first=%+v last=%+v", r.FirstObserved, r.LastObserved)
	}
	if !r.Complete || r.AccountType != AccountEOA || r.Balance == nil {
		t.Fatalf("%+v", r)
	}

	// Same wallet but with more history than one page: first-seen must be withheld.
	f.pageSize = 2
	f.counters[addrA] = &blockchain.Counters{TransactionsCount: i64(100000), TokenTransfersCount: i64(0)}
	r, err = testService(f).GetAddressInfo(context.Background(), addrA)
	if err != nil {
		t.Fatal(err)
	}
	if r.FirstObserved != nil || r.Complete || r.LastObserved == nil {
		t.Fatalf("first-seen must not be guessed from a partial history: %+v", r)
	}
}

func TestAddressInfoMissingCountersIsPartial(t *testing.T) {
	f := newFake() // no counters configured -> provider error
	r, err := testService(f).GetAddressInfo(context.Background(), addrA)
	if err != nil {
		t.Fatal(err)
	}
	if r.Complete || r.TransactionCount != nil {
		t.Fatalf("missing counters must be partial and absent, not zero: %+v", r)
	}
}

func TestGetTransactionsDirectionLimitAndTotals(t *testing.T) {
	f := newFake()
	f.tx(addrB, addrA, "2", true)
	f.tx(addrC, addrA, "1", false) // failed incoming
	f.tx(addrA, addrD, "5", true)
	f.tx(addrA, addrA, "1", true) // self
	s := testService(f)

	in, err := s.GetTransactions(context.Background(), addrA, "in", 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range in.Items {
		if it.Direction != blockchain.DirIn && it.Direction != blockchain.DirSelf {
			t.Fatalf("direction filter leaked %q", it.Direction)
		}
	}
	if in.Summary.NativeInConfirmedETH != "2" || in.Summary.Unconfirmed != 1 {
		t.Fatalf("failed transfers must not count: %+v", in.Summary)
	}

	both, _ := s.GetTransactions(context.Background(), addrA, "", 2)
	if both.Direction != DirBoth || len(both.Items) != 2 || !both.HasMore {
		t.Fatalf("limit/has_more: %+v", both)
	}
	if blockOf(both.Items[0]) < blockOf(both.Items[1]) {
		t.Fatal("items must be newest first")
	}
}

func TestGetTransactionsPaginatesToLimit(t *testing.T) {
	f := newFake()
	f.pageSize = 50
	for i := 0; i < 70; i++ {
		f.tx(addrB, addrA, "1", true)
	}
	r, err := testService(f).GetTransactions(context.Background(), addrA, "in", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Items) != 60 || !r.HasMore {
		t.Fatalf("items=%d has_more=%v", len(r.Items), r.HasMore)
	}
	seen := map[string]bool{}
	for _, it := range r.Items {
		if seen[it.ID()] {
			t.Fatal("duplicate item across pages")
		}
		seen[it.ID()] = true
	}
}

func TestGetTransactionsPartialAndTotalFailure(t *testing.T) {
	f := newFake()
	f.tx(addrB, addrA, "2", true)
	f.failSrc[SourceTokens] = blockchain.ErrRateLimited
	s := testService(f)
	r, err := s.GetTransactions(context.Background(), addrA, "both", 10)
	if err != nil {
		t.Fatal(err)
	}
	if r.Complete || len(r.Warnings) == 0 || len(r.Items) != 1 {
		t.Fatalf("one failed source must yield a partial result: %+v", r)
	}
	f.fail[addrA] = blockchain.ErrUnavailable
	if _, err := s.GetTransactions(context.Background(), addrA, "both", 10); err == nil {
		t.Fatal("all sources failing must be an error, not an empty list")
	}
}

func TestValidateInputs(t *testing.T) {
	if _, _, err := ValidateTxQuery("up", 5); err == nil {
		t.Error("bad direction accepted")
	}
	for _, l := range []int{-1, 101} {
		if _, _, err := ValidateTxQuery("in", l); err == nil {
			t.Errorf("limit %d accepted", l)
		}
	}
	if d, l, _ := ValidateTxQuery("", 0); d != DirBoth || l != DefaultTxLimit {
		t.Errorf("defaults: %s %d", d, l)
	}
	if c, err := ValidateChain(""); err != nil || c != "ethereum" {
		t.Error("chain default")
	}
	if _, err := ValidateChain("polygon"); !errors.Is(err, ErrUnsupportedChain) {
		t.Error("unsupported chain accepted")
	}
}

func TestMarkInitiation(t *testing.T) {
	f := newFake()
	signedHash := f.tx(addrA, addrB, "1", true)
	tok := func(hash string) blockchain.Activity {
		b := block
		ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(b+5) * time.Minute)
		return blockchain.Activity{Kind: blockchain.KindTokenTransfer, TxHash: hash, LogIndex: new(int), BlockNumber: &b, Timestamp: &ts,
			From: blockchain.AddressRef{Address: addrA}, To: &blockchain.AddressRef{Address: addrC}, Asset: blockchain.Asset{Type: blockchain.AssetERC20}}
	}
	items := append(f.acts, tok(signedHash), tok("0xnotsigned"))
	MarkInitiation(addrA, items, true)
	if v := items[1].InitiatedByWallet; v == nil || !*v {
		t.Fatal("token event inside the wallet's own tx must be initiated")
	}
	if v := items[2].InitiatedByWallet; v == nil || *v {
		t.Fatal("token event in a tx the wallet did not send must be marked not initiated")
	}
	// A wallet with no top-level transactions (could be a contract wallet) is left unknown.
	only := []blockchain.Activity{tok("0xx")}
	MarkInitiation(addrA, only, true)
	if only[0].InitiatedByWallet != nil {
		t.Fatal("must not guess for wallets that never sent a top-level transaction")
	}
}
