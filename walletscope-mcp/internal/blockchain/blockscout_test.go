package blockchain

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amnariaz007/walletscope-mcp/internal/cache"
	"github.com/amnariaz007/walletscope-mcp/internal/observability"
)

const vitalik = "0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "blockscout", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newTestClient(t *testing.T, h http.Handler, opts ...func(*BlockscoutOptions)) *Blockscout {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	o := BlockscoutOptions{BaseURL: srv.URL + "/api/v2", RequestTimeout: 2 * time.Second, MaxRetries: 2}
	for _, f := range opts {
		f(&o)
	}
	c := NewBlockscout(o)
	c.sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() } // no real waiting in tests
	return c
}

func TestGetAddressAndCounters(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/addresses/" + vitalik:
			w.Write(fixture(t, "address_vitalik.json"))
		case "/api/v2/addresses/" + vitalik + "/counters":
			w.Write(fixture(t, "counters_vitalik.json"))
		default:
			http.NotFound(w, r)
		}
	}))
	info, err := c.GetAddress(context.Background(), vitalik)
	if err != nil {
		t.Fatal(err)
	}
	if info.Address != vitalik || info.ENS != "vitalik.eth" || info.ProxyType != "eip7702" || info.BalanceWei == nil {
		t.Fatalf("unexpected info: %+v", info)
	}
	if got := info.BalanceWei.String(); got != "5777859622599202671" {
		t.Fatalf("balance = %s", got)
	}
	cnt, err := c.GetCounters(context.Background(), vitalik)
	if err != nil {
		t.Fatal(err)
	}
	if cnt.TransactionsCount == nil || *cnt.TransactionsCount != 78421 {
		t.Fatalf("counters = %+v", cnt)
	}
}

func TestTransactionsParsingAndPagination(t *testing.T) {
	var gotQuery url.Values
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Write(fixture(t, "transactions_page1.json"))
	}))
	page, err := c.Transactions(context.Background(), vitalik, FilterFrom, "")
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery.Get("filter") != "from" {
		t.Fatalf("filter not sent: %v", gotQuery)
	}
	if len(page.Items) != 3 {
		t.Fatalf("items = %d", len(page.Items))
	}
	if page.Items[0].Kind != KindContractCall || page.Items[0].TransferConfirmed {
		t.Fatalf("zero-value tx must be a contract_call without confirmed transfer: %+v", page.Items[0])
	}
	tx := page.Items[2]
	if tx.Kind != KindNativeTransfer || !tx.TransferConfirmed || tx.Amount.Formatted != "0.041534" || tx.DirectionFor(vitalik) != DirOut {
		t.Fatalf("native transfer parsed wrong: %+v", tx)
	}
	if page.NextCursor == "" {
		t.Fatal("expected next cursor")
	}
	// The cursor must be replayed verbatim as query parameters.
	if _, err := c.Transactions(context.Background(), vitalik, FilterFrom, page.NextCursor); err != nil {
		t.Fatal(err)
	}
	if gotQuery.Get("block_number") != "25535494" || gotQuery.Get("index") != "23" || gotQuery.Get("filter") != "from" {
		t.Fatalf("cursor not replayed: %v", gotQuery)
	}
}

func TestTransactionEdgeCases(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture(t, "transactions_edge_cases.json"))
	}))
	page, err := c.Transactions(context.Background(), "0x1111111111111111111111111111111111111111", FilterAll, "")
	if err != nil {
		t.Fatal(err)
	}
	pending, failed, creation := page.Items[0], page.Items[1], page.Items[2]
	if pending.Status != StatusPending || pending.TransferConfirmed {
		t.Errorf("pending: %+v", pending)
	}
	if failed.Status != StatusFailed || failed.TransferConfirmed || failed.Kind != KindNativeTransfer {
		t.Errorf("failed tx must not be a confirmed transfer: %+v", failed)
	}
	if creation.To == nil || creation.To.Address != "0x3333333333333333333333333333333333333333" || creation.Method != "contract_creation" {
		t.Errorf("creation: %+v", creation)
	}
	if page.NextCursor != "" {
		t.Errorf("null next_page_params must end pagination")
	}
}

func TestTokenAndInternalTransfers(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token-transfers") {
			if r.URL.Query().Get("type") != "ERC-20" {
				t.Errorf("token type filter missing")
			}
			w.Write(fixture(t, "token_transfers_page1.json"))
			return
		}
		w.Write(fixture(t, "internal_transactions_tornado10.json"))
	}))
	tok, err := c.TokenTransfers(context.Background(), vitalik, FilterTo, "")
	if err != nil {
		t.Fatal(err)
	}
	fork := tok.Items[0]
	if fork.Asset.Type != AssetERC20 || fork.Asset.Symbol != "FORK" || *fork.Asset.Decimals != 9 ||
		fork.Amount.Formatted != "88888888" || fork.Amount.Raw != "88888888000000000" || fork.LogIndex == nil {
		t.Fatalf("token transfer: %+v", fork)
	}
	itx, err := c.InternalTransfers(context.Background(), "0x910Cbd523D972eb0a6f4cAe4618aD62622b39DbF", FilterFrom, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(itx.Items) != 2 || itx.Items[1].Amount.Formatted != "9.96231799666325" || itx.Items[0].Kind != KindInternalTransfer {
		t.Fatalf("internal: %+v", itx.Items)
	}
	if itx.Items[0].ID() == itx.Items[1].ID() {
		t.Fatal("internal transfers in one tx must have distinct IDs")
	}
}

func TestRetryOn429ThenSuccess(t *testing.T) {
	var hits atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write(fixture(t, "counters_vitalik.json"))
	}))
	var slept time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { slept = d; return nil }
	stats := &observability.CallStats{}
	ctx := observability.WithStats(context.Background(), stats)
	if _, err := c.GetCounters(ctx, vitalik); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 || stats.Retries.Load() != 1 || stats.UpstreamRequests.Load() != 2 {
		t.Fatalf("hits=%d retries=%d", hits.Load(), stats.Retries.Load())
	}
	if slept != time.Second {
		t.Fatalf("Retry-After not honoured, slept %v", slept)
	}
}

func TestRetriesAreBoundedOn5xx(t *testing.T) {
	var hits atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	_, err := c.GetCounters(context.Background(), vitalik)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if hits.Load() != 3 { // 1 + MaxRetries(2)
		t.Fatalf("expected 3 attempts, got %d", hits.Load())
	}
}

func TestPermanentErrorsAreNotRetried(t *testing.T) {
	for _, code := range []int{http.StatusUnprocessableEntity, http.StatusNotFound, http.StatusForbidden} {
		var hits atomic.Int32
		c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.WriteHeader(code)
		}))
		_, err := c.GetAddress(context.Background(), vitalik)
		if err == nil || hits.Load() != 1 {
			t.Fatalf("status %d: err=%v hits=%d", code, err, hits.Load())
		}
	}
}

func TestMalformedJSONIsErrorAndNotCached(t *testing.T) {
	var hits atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"items": [`))
	}), func(o *BlockscoutOptions) { o.Cache = cache.New[[]byte](time.Minute, 10) })
	for i := 0; i < 2; i++ {
		_, err := c.Transactions(context.Background(), vitalik, FilterAll, "")
		if !errors.Is(err, ErrBadResponse) {
			t.Fatalf("err = %v", err)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("malformed response must not be cached; hits=%d", hits.Load())
	}
}

func TestMissingRequiredFieldIsBadResponse(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[{"hash":"0xabc","value":"1"}],"next_page_params":null}`))
	}))
	if _, err := c.Transactions(context.Background(), vitalik, FilterAll, ""); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("err = %v", err)
	}
}

func TestSuccessfulResponsesAreCached(t *testing.T) {
	var hits atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(fixture(t, "counters_vitalik.json"))
	}), func(o *BlockscoutOptions) { o.Cache = cache.New[[]byte](time.Minute, 10) })
	stats := &observability.CallStats{}
	ctx := observability.WithStats(context.Background(), stats)
	for i := 0; i < 3; i++ {
		if _, err := c.GetCounters(ctx, vitalik); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 1 || stats.CacheHits.Load() != 2 || stats.CacheMisses.Load() != 1 {
		t.Fatalf("hits=%d cacheHits=%d", hits.Load(), stats.CacheHits.Load())
	}
}

func TestRequestTimeoutIsTransient(t *testing.T) {
	var hits atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-time.After(time.Second):
		case <-r.Context().Done():
		}
	}), func(o *BlockscoutOptions) { o.RequestTimeout = 50 * time.Millisecond; o.MaxRetries = 1 })
	_, err := c.GetCounters(context.Background(), vitalik)
	if !errors.Is(err, ErrUnavailable) || hits.Load() != 2 {
		t.Fatalf("err=%v hits=%d", err, hits.Load())
	}
}

func TestCancellationStopsRetries(t *testing.T) {
	var hits atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}), func(o *BlockscoutOptions) { o.MaxRetries = 5 })
	ctx, cancel := context.WithCancel(context.Background())
	c.sleep = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }
	_, err := c.GetCounters(ctx, vitalik)
	if !errors.Is(err, context.Canceled) || hits.Load() != 1 {
		t.Fatalf("err=%v hits=%d", err, hits.Load())
	}
}

func TestRedactURLStripsQuery(t *testing.T) {
	err := redactURL(&url.Error{Op: "Get", URL: "https://x.example/api?apikey=SECRET", Err: errors.New("boom")})
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("secret leaked: %v", err)
	}
}

func TestBackoffIsBounded(t *testing.T) {
	c := NewBlockscout(BlockscoutOptions{BaseURL: "http://x", BaseBackoff: 100 * time.Millisecond, MaxBackoff: time.Second})
	for attempt := 0; attempt < 40; attempt++ {
		if d := c.backoff(attempt); d < 0 || d > time.Second {
			t.Fatalf("attempt %d backoff %v out of bounds", attempt, d)
		}
	}
}
