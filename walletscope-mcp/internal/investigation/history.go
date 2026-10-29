package investigation

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/amnariaz007/walletscope-mcp/internal/blockchain"
)

// Source names used in coverage reports.
const (
	SourceTransactions = "transactions"
	SourceInternal     = "internal_transactions"
	SourceTokens       = "token_transfers"
)

// History is a bounded, deduplicated sample of an address's activity, newest first.
type History struct {
	Address    string
	Activities []blockchain.Activity
	Coverage   []SourceCoverage
}

// Complete reports whether every source was read to its last page without error.
func (h *History) Complete() bool {
	for _, c := range h.Coverage {
		if !c.Complete || c.Error != "" {
			return false
		}
	}
	return len(h.Coverage) > 0
}

// AnySourceFailed reports whether at least one source returned an error.
func (h *History) AnySourceFailed() bool {
	for _, c := range h.Coverage {
		if c.Error != "" {
			return true
		}
	}
	return false
}

// AllSourcesFailed reports whether no source returned data successfully.
func (h *History) AllSourcesFailed() bool {
	for _, c := range h.Coverage {
		if c.Error == "" {
			return false
		}
	}
	return true
}

type listFn func(ctx context.Context, addr string, f blockchain.Filter, cursor string) (blockchain.Page, error)

func (s *Service) sources() []struct {
	name string
	fn   listFn
} {
	return []struct {
		name string
		fn   listFn
	}{
		{SourceTransactions, s.Provider.Transactions},
		{SourceInternal, s.Provider.InternalTransfers},
		{SourceTokens, s.Provider.TokenTransfers},
	}
}

// CollectHistory reads up to maxPages pages from each activity source in
// parallel (one goroutine per source, three in total). Source failures are
// recorded in Coverage rather than aborting the whole collection.
func (s *Service) CollectHistory(ctx context.Context, addr string, f blockchain.Filter, maxPages, wantItems int) *History {
	srcs := s.sources()
	results := make([][]blockchain.Activity, len(srcs))
	coverage := make([]SourceCoverage, len(srcs))
	var wg sync.WaitGroup
	for i, src := range srcs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cov := SourceCoverage{Source: src.name}
			cursor := ""
			for cov.Pages < maxPages {
				page, err := src.fn(ctx, addr, f, cursor)
				if err != nil {
					cov.Error = providerErrorMessage(err)
					break
				}
				cov.Pages++
				results[i] = append(results[i], page.Items...)
				cursor = page.NextCursor
				if cursor == "" {
					cov.Complete = true
					break
				}
				if wantItems > 0 && len(results[i]) >= wantItems {
					break
				}
			}
			cov.Items = len(results[i])
			coverage[i] = cov
		}()
	}
	wg.Wait()

	seen := map[string]bool{}
	var all []blockchain.Activity
	for _, items := range results {
		for _, a := range items {
			if id := a.ID(); !seen[id] {
				seen[id] = true
				all = append(all, a)
			}
		}
	}
	SortNewestFirst(all)
	if f != blockchain.FilterTo {
		MarkInitiation(addr, all, coverage[0].Complete && coverage[0].Error == "")
	}
	return &History{Address: addr, Activities: all, Coverage: coverage}
}

// SortNewestFirst orders activities deterministically: pending first, then by
// block descending, then by tx hash and intra-transaction index.
func SortNewestFirst(items []blockchain.Activity) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		ab, bb := blockOf(a), blockOf(b)
		if ab != bb {
			return ab > bb
		}
		if a.TxHash != b.TxHash {
			return a.TxHash < b.TxHash
		}
		return a.ID() < b.ID()
	})
}

func blockOf(a blockchain.Activity) uint64 {
	if a.BlockNumber == nil {
		return ^uint64(0) // pending sorts first
	}
	return *a.BlockNumber
}

// MarkInitiation sets InitiatedByWallet on outgoing ERC-20 events of addr.
// Token contracts can emit Transfer events naming any "from" address, so an
// outgoing event alone does not prove the wallet acted. The flag is only set
// when the wallet is provably an EOA (it sent at least one top-level
// transaction in items): true if its signed transaction carries the event,
// false if the transactions read cover the event's time but do not include
// its hash. txComplete reports whether the transactions source was read to
// its last page.
func MarkInitiation(addr string, items []blockchain.Activity, txComplete bool) {
	signed := map[string]bool{}
	var oldest *time.Time
	for i := range items {
		a := &items[i]
		if a.Kind != blockchain.KindNativeTransfer && a.Kind != blockchain.KindContractCall {
			continue
		}
		if a.From.Address == addr {
			signed[a.TxHash] = true
		}
		if a.Timestamp != nil && (oldest == nil || a.Timestamp.Before(*oldest)) {
			oldest = a.Timestamp
		}
	}
	if len(signed) == 0 {
		return // cannot tell an EOA from a contract wallet (e.g. a Safe)
	}
	yes, no := true, false
	for i := range items {
		a := &items[i]
		if a.Kind != blockchain.KindTokenTransfer || a.From.Address != addr || a.ToAddress() == addr {
			continue
		}
		switch {
		case signed[a.TxHash]:
			a.InitiatedByWallet = &yes
		case txComplete || (oldest != nil && a.Timestamp != nil && !a.Timestamp.Before(*oldest)):
			a.InitiatedByWallet = &no
		}
	}
}
