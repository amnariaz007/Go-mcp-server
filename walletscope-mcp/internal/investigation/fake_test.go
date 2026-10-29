package investigation

import (
	"context"
	"io"
	"log/slog"
	"math/big"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amnariaz007/walletscope-mcp/internal/blockchain"
)

// fakeProvider serves synthetic activity from memory. Addresses used in tests
// are obviously artificial (0x1111…, 0x2222…).
type fakeProvider struct {
	mu        sync.Mutex
	acts      []blockchain.Activity
	info      map[string]*blockchain.AddressInfo
	counters  map[string]*blockchain.Counters
	fail      map[string]error // address -> error for listing calls
	failSrc   map[string]error // source name -> error
	pageSize  int
	delay     time.Duration
	calls     atomic.Int64
	inFlight  atomic.Int64
	maxFlight atomic.Int64
}

func newFake() *fakeProvider {
	return &fakeProvider{info: map[string]*blockchain.AddressInfo{}, counters: map[string]*blockchain.Counters{},
		fail: map[string]error{}, failSrc: map[string]error{}, pageSize: 50}
}

func (f *fakeProvider) Name() string    { return "fake" }
func (f *fakeProvider) BaseURL() string { return "memory://fake" }

func (f *fakeProvider) GetAddress(_ context.Context, addr string) (*blockchain.AddressInfo, error) {
	f.calls.Add(1)
	if i, ok := f.info[addr]; ok {
		return i, nil
	}
	return &blockchain.AddressInfo{Address: addr, BalanceWei: big.NewInt(0)}, nil
}

func (f *fakeProvider) GetCounters(_ context.Context, addr string) (*blockchain.Counters, error) {
	f.calls.Add(1)
	if c, ok := f.counters[addr]; ok {
		return c, nil
	}
	return nil, blockchain.ErrUnavailable
}

func (f *fakeProvider) list(ctx context.Context, src, addr string, flt blockchain.Filter, cursor string, match func(blockchain.Activity) bool) (blockchain.Page, error) {
	f.calls.Add(1)
	n := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		m := f.maxFlight.Load()
		if n <= m || f.maxFlight.CompareAndSwap(m, n) {
			break
		}
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return blockchain.Page{}, ctx.Err()
		}
	}
	if err := f.fail[addr]; err != nil {
		return blockchain.Page{}, err
	}
	if err := f.failSrc[src]; err != nil {
		return blockchain.Page{}, err
	}
	f.mu.Lock()
	var rows []blockchain.Activity
	for _, a := range f.acts {
		if !match(a) {
			continue
		}
		from, to := a.From.Address == addr, a.ToAddress() == addr
		if (flt == blockchain.FilterFrom && from) || (flt == blockchain.FilterTo && to) || (flt == blockchain.FilterAll && (from || to)) {
			rows = append(rows, a)
		}
	}
	f.mu.Unlock()
	SortNewestFirst(rows)
	off := 0
	if cursor != "" {
		off, _ = strconv.Atoi(cursor)
	}
	end := min(off+f.pageSize, len(rows))
	p := blockchain.Page{Items: rows[min(off, len(rows)):end]}
	if end < len(rows) {
		p.NextCursor = strconv.Itoa(end)
	}
	return p, nil
}

func (f *fakeProvider) Transactions(ctx context.Context, addr string, flt blockchain.Filter, c string) (blockchain.Page, error) {
	return f.list(ctx, SourceTransactions, addr, flt, c, func(a blockchain.Activity) bool {
		return a.Kind == blockchain.KindNativeTransfer || a.Kind == blockchain.KindContractCall
	})
}

func (f *fakeProvider) InternalTransfers(ctx context.Context, addr string, flt blockchain.Filter, c string) (blockchain.Page, error) {
	return f.list(ctx, SourceInternal, addr, flt, c, func(a blockchain.Activity) bool { return a.Kind == blockchain.KindInternalTransfer })
}

func (f *fakeProvider) TokenTransfers(ctx context.Context, addr string, flt blockchain.Filter, c string) (blockchain.Page, error) {
	return f.list(ctx, SourceTokens, addr, flt, c, func(a blockchain.Activity) bool { return a.Kind == blockchain.KindTokenTransfer })
}

var block uint64 = 1000

func eth(s string) *big.Int {
	v, _ := new(big.Int).SetString(s, 10)
	return new(big.Int).Mul(v, new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
}

// tx adds a native transfer of whole ETH. Each call gets a new block number.
func (f *fakeProvider) tx(from, to string, wholeETH string, ok bool) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	block++
	b := block
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(b) * time.Minute)
	hash := "0x" + strconv.FormatUint(b, 16)
	a := blockchain.Activity{Kind: blockchain.KindNativeTransfer, TxHash: hash, BlockNumber: &b, Timestamp: &ts,
		From: blockchain.AddressRef{Address: from}, To: &blockchain.AddressRef{Address: to}, Asset: blockchain.NativeAsset(), Status: blockchain.StatusSuccess}
	if !ok {
		a.Status = blockchain.StatusFailed
	}
	a.SetValue(eth(wholeETH))
	a.TransferConfirmed = ok
	f.acts = append(f.acts, a)
	return hash
}

func (f *fakeProvider) markContract(addr string) {
	t := true
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.acts {
		if f.acts[i].ToAddress() == addr {
			f.acts[i].To.IsContract = &t
		}
	}
}

func testService(p blockchain.Provider) *Service {
	return &Service{
		Provider:     p,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		HistoryPages: 4,
		Trace: TraceLimits{Workers: 4, MaxHops: 3, MaxNodes: 100, MaxEdges: 100, MaxAPICalls: 100, MaxChildren: 10,
			Timeout: 5 * time.Second},
		Now: func() time.Time { return time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC) },
	}
}

const (
	addrA = "0x1111111111111111111111111111111111111111"
	addrB = "0x2222222222222222222222222222222222222222"
	addrC = "0x3333333333333333333333333333333333333333"
	addrD = "0x4444444444444444444444444444444444444444"
	addrE = "0x5555555555555555555555555555555555555555"
)
