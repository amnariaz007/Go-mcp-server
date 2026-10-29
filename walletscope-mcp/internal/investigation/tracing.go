package investigation

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amnariaz007/walletscope-mcp/internal/blockchain"
	"github.com/amnariaz007/walletscope-mcp/internal/config"
)

// Node stop reasons.
const (
	StopMaxHops       = "max_hops_reached"
	StopContractLeaf  = "contract_not_expanded"
	StopNodeLimit     = "node_limit_reached"
	StopFetchFailed   = "fetch_failed"
	StopBudget        = "api_budget_exhausted"
	StopCancelled     = "cancelled_or_timed_out"
	StopNoOutflows    = "no_qualifying_outflows"
	StopFlaggedLeaf   = "flagged_address_not_expanded"
	maxEdgeTxHashes   = 20
	tracePagesPerNode = 1
)

// TraceRequest is a validated fund-trace request.
type TraceRequest struct {
	Address   string
	Hops      int
	MinAmount string // whole units of the traced asset, e.g. "0.1"
	Asset     string // "native"/"" or an ERC-20 contract address
}

// TraceNode is an address in the fund-flow graph.
type TraceNode struct {
	Address           string   `json:"address"`
	Hop               int      `json:"hop" jsonschema:"hop at which the address was first reached (0 = start)"`
	IsContract        *bool    `json:"is_contract,omitempty"`
	Name              string   `json:"name,omitempty"`
	ENS               string   `json:"ens,omitempty"`
	Labels            []string `json:"labels,omitempty"`
	Flags             []string `json:"flags,omitempty" jsonschema:"local dataset matches, e.g. ofac_sdn or mixer:<label>"`
	Expanded          bool     `json:"expanded" jsonschema:"true when the address's outgoing transfers were fetched"`
	StopReason        string   `json:"stop_reason,omitempty"`
	OutflowsTruncated bool     `json:"outflows_truncated,omitempty" jsonschema:"more recipients qualified than the per-node child limit"`
	HistoryTruncated  bool     `json:"history_truncated,omitempty" jsonschema:"older outgoing activity exists beyond the page read"`
	parent            string
}

// TraceEdge aggregates all qualifying transfers from one address to another.
type TraceEdge struct {
	From              string            `json:"from"`
	To                string            `json:"to"`
	Hop               int               `json:"hop"`
	Total             blockchain.Amount `json:"total" jsonschema:"exact sum of the qualifying transfers on this edge"`
	TransferCount     int               `json:"transfer_count"`
	TxHashes          []string          `json:"tx_hashes" jsonschema:"supporting transaction hashes (first 20)"`
	TxHashesTruncated bool              `json:"tx_hashes_truncated,omitempty"`
	FirstSeen         *time.Time        `json:"first_seen,omitempty"`
	LastSeen          *time.Time        `json:"last_seen,omitempty"`
	ToVisitedEarlier  bool              `json:"to_visited_earlier,omitempty" jsonschema:"recipient was already in the graph (cycle or convergence)"`
}

// FlaggedNode highlights a flagged address and the discovery path to it.
type FlaggedNode struct {
	Address string   `json:"address"`
	Hop     int      `json:"hop"`
	Flags   []string `json:"flags"`
	Path    []string `json:"path" jsonschema:"addresses from the start to this node along first-discovery edges"`
}

// TraceStats describes resource usage.
type TraceStats struct {
	APICalls        int64 `json:"api_calls"`
	NodesExpanded   int   `json:"nodes_expanded"`
	SelfTransfers   int   `json:"self_transfers_skipped"`
	BelowMinimum    int   `json:"transfers_below_minimum"`
	UnconfirmedSeen int   `json:"unconfirmed_transfers_skipped"`
	DurationMillis  int64 `json:"duration_ms"`
}

// TraceLimitsApplied echoes the effective bounds.
type TraceLimitsApplied struct {
	MaxNodes           int  `json:"max_nodes"`
	MaxEdges           int  `json:"max_edges"`
	MaxAPICalls        int  `json:"max_api_calls"`
	MaxChildrenPerNode int  `json:"max_children_per_node"`
	ItemsPerSourcePage int  `json:"items_per_source_page"`
	PagesPerNode       int  `json:"pages_per_source_per_node"`
	Workers            int  `json:"workers"`
	TimeoutSeconds     int  `json:"timeout_seconds"`
	ContractsExpanded  bool `json:"contracts_expanded"`
}

// TraceResult is the trace_funds response.
type TraceResult struct {
	Provenance
	Start        string             `json:"start"`
	Asset        blockchain.Asset   `json:"asset"`
	Hops         int                `json:"hops"`
	MinAmount    blockchain.Amount  `json:"min_amount" jsonschema:"per-transfer minimum; smaller transfers are ignored"`
	Nodes        []TraceNode        `json:"nodes"`
	Edges        []TraceEdge        `json:"edges"`
	FlaggedNodes []FlaggedNode      `json:"flagged_nodes"`
	Stats        TraceStats         `json:"stats"`
	Limits       TraceLimitsApplied `json:"limits"`
	Partial      bool               `json:"partial"`
	Warnings     []string           `json:"warnings"`
	Methodology  []string           `json:"methodology"`
}

// IsPartial implements partial reporting for audit logs.
func (r TraceResult) IsPartial() bool { return r.Partial }

// ValidateTrace checks hops, minimum amount and asset and returns the asset
// filter: "" for native ETH or a checksummed ERC-20 contract.
func (s *Service) ValidateTrace(req TraceRequest) (assetContract string, err error) {
	maxHops := s.Trace.MaxHops
	if maxHops < 1 || maxHops > 3 {
		maxHops = 3
	}
	if req.Hops < 1 || req.Hops > maxHops {
		return "", fmt.Errorf("hops must be between 1 and %d, got %d", maxHops, req.Hops)
	}
	if _, err := config.ParseDecimalUnits(req.MinAmount, 77); err != nil {
		return "", fmt.Errorf("min_amount must be a non-negative decimal such as \"0.1\": %v", err)
	}
	switch strings.ToLower(strings.TrimSpace(req.Asset)) {
	case "", "native", "eth":
		if _, err := config.ParseDecimalUnits(req.MinAmount, blockchain.NativeDecimals); err != nil {
			return "", fmt.Errorf("min_amount for ETH: %v", err)
		}
		return "", nil
	}
	c, err := blockchain.NormalizeAddress(req.Asset)
	if err != nil {
		return "", fmt.Errorf("asset must be \"native\" or an ERC-20 contract address: %w", err)
	}
	return c, nil
}

// expansion is one worker's output for one frontier node.
type expansion struct {
	children          []*childAgg
	selfTransfers     int
	belowMin          int
	unconfirmed       int
	historyTruncated  bool
	outflowsTruncated bool
	err               error
	asset             *blockchain.Asset
}

type childAgg struct {
	ref       blockchain.AddressRef
	total     *big.Int
	count     int
	hashes    []string
	hashSet   map[string]bool
	truncated bool
	first     *time.Time
	last      *time.Time
}

// TraceFunds performs bounded breadth-first tracing of outgoing transfers.
func (s *Service) TraceFunds(ctx context.Context, req TraceRequest) (*TraceResult, error) {
	assetContract, err := s.ValidateTrace(req)
	if err != nil {
		return nil, err
	}
	lim := s.Trace
	if lim.Workers < 1 {
		lim.Workers = 1
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, lim.Timeout)
	defer cancel()

	res := &TraceResult{
		Provenance:   s.provenance(),
		Start:        req.Address,
		Hops:         req.Hops,
		Nodes:        []TraceNode{},
		Edges:        []TraceEdge{},
		FlaggedNodes: []FlaggedNode{},
		Warnings:     []string{},
		Limits: TraceLimitsApplied{
			MaxNodes: lim.MaxNodes, MaxEdges: lim.MaxEdges, MaxAPICalls: lim.MaxAPICalls, MaxChildrenPerNode: lim.MaxChildren,
			ItemsPerSourcePage: 50, PagesPerNode: tracePagesPerNode, Workers: lim.Workers,
			TimeoutSeconds: int(lim.Timeout / time.Second), ContractsExpanded: lim.ExpandContracts,
		},
		Methodology: []string{
			"Graph tracing: each edge aggregates confirmed outgoing transfers of the traced asset from one address to another, each transfer >= min_amount.",
			"This shows where value was sent, hop by hop. It does not prove that specific coins received earlier were the ones sent later.",
			"Failed, pending and zero-value transactions are excluded. Self-transfers are skipped.",
			"Only the most recent page (up to 50 items) of outgoing activity per source is read for each address; older outflows may be missing (history_truncated).",
			"Contracts (exchanges, routers, mixers) are graph leaves unless contract expansion is enabled, because their outflows belong to many unrelated users.",
		},
	}
	if assetContract == "" {
		res.Asset = blockchain.NativeAsset()
		res.Methodology = append(res.Methodology, "Native ETH includes top-level transfers and internal (contract-initiated) transfers.")
	} else {
		res.Asset = blockchain.Asset{Type: blockchain.AssetERC20, Contract: assetContract}
	}

	var calls atomic.Int64
	minUnits := map[int]*big.Int{} // decimals -> min in base units (computed once per decimals value)
	var minMu sync.Mutex
	minFor := func(decimals int) (*big.Int, error) {
		minMu.Lock()
		defer minMu.Unlock()
		if v, ok := minUnits[decimals]; ok {
			return v, nil
		}
		v, err := config.ParseDecimalUnits(req.MinAmount, decimals)
		if err != nil {
			return nil, err
		}
		minUnits[decimals] = v
		return v, nil
	}

	nodes := map[string]*TraceNode{}
	var order []string
	addNode := func(ref blockchain.AddressRef, hop int, parent string) *TraceNode {
		n := &TraceNode{Address: ref.Address, Hop: hop, IsContract: ref.IsContract, Name: ref.Name, ENS: ref.ENS, Labels: ref.Labels, parent: parent}
		if s.Annotate != nil {
			n.Flags = s.Annotate(ref.Address)
		}
		nodes[ref.Address] = n
		order = append(order, ref.Address)
		return n
	}
	addNode(blockchain.AddressRef{Address: req.Address}, 0, "")
	edgeCount := 0
	frontier := []string{req.Address}
	limitHit := map[string]bool{}
	warn := func(key, msg string) {
		if !limitHit[key] {
			limitHit[key] = true
			res.Warnings = append(res.Warnings, msg)
		}
	}

	for hop := 1; hop <= req.Hops && len(frontier) > 0; hop++ {
		sort.Strings(frontier)
		results := make([]expansion, len(frontier))
		jobs := make(chan int)
		var wg sync.WaitGroup
		for w := 0; w < min(lim.Workers, len(frontier)); w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					results[i] = s.expand(ctx, frontier[i], assetContract, &calls, lim, minFor)
				}
			}()
		}
	feed:
		for i := range frontier {
			select {
			case jobs <- i:
			case <-ctx.Done():
				for j := i; j < len(frontier); j++ {
					results[j] = expansion{err: ctx.Err()}
				}
				break feed
			}
		}
		close(jobs)
		wg.Wait()

		var next []string
		for i, addr := range frontier {
			n, ex := nodes[addr], results[i]
			res.Stats.SelfTransfers += ex.selfTransfers
			res.Stats.BelowMinimum += ex.belowMin
			res.Stats.UnconfirmedSeen += ex.unconfirmed
			if ex.asset != nil && res.Asset.Type == blockchain.AssetERC20 && res.Asset.Symbol == "" {
				res.Asset = *ex.asset
			}
			if ex.err != nil {
				res.Partial = true
				switch {
				case errors.Is(ex.err, errBudgetExhausted):
					n.StopReason = StopBudget
					warn("budget", fmt.Sprintf("API call budget (%d) exhausted; some addresses were not expanded.", lim.MaxAPICalls))
				case ctx.Err() != nil:
					n.StopReason = StopCancelled
					warn("ctx", "Trace cancelled or timed out; the graph is incomplete.")
				default:
					n.StopReason = StopFetchFailed + ": " + providerErrorMessage(ex.err)
					warn("fetch:"+addr, fmt.Sprintf("Could not fetch outgoing transfers for %s: %s.", addr, providerErrorMessage(ex.err)))
				}
				continue
			}
			n.Expanded = true
			n.HistoryTruncated = ex.historyTruncated
			n.OutflowsTruncated = ex.outflowsTruncated
			res.Stats.NodesExpanded++
			if ex.historyTruncated {
				warn("hist", "Some expanded addresses have older outgoing activity beyond the page read (history_truncated); outflows may be incomplete.")
			}
			if ex.outflowsTruncated {
				warn("children", fmt.Sprintf("Some addresses had more than %d qualifying recipients; only the largest were kept (outflows_truncated).", lim.MaxChildren))
			}
			if len(ex.children) == 0 {
				n.StopReason = StopNoOutflows
			}
			for _, c := range ex.children {
				if edgeCount >= lim.MaxEdges {
					res.Partial = true
					warn("edges", fmt.Sprintf("Edge limit (%d) reached; remaining transfers omitted.", lim.MaxEdges))
					break
				}
				existing, visited := nodes[c.ref.Address]
				if !visited {
					if len(nodes) >= lim.MaxNodes {
						res.Partial = true
						warn("nodes", fmt.Sprintf("Node limit (%d) reached; remaining recipients omitted.", lim.MaxNodes))
						n.StopReason = StopNodeLimit
						break
					}
					existing = addNode(c.ref, hop, addr)
					if s.shouldExpand(existing, lim) {
						next = append(next, existing.Address)
					}
				}
				edgeCount++
				dec := -1
				if res.Asset.Decimals != nil {
					dec = *res.Asset.Decimals
				}
				res.Edges = append(res.Edges, TraceEdge{
					From: addr, To: c.ref.Address, Hop: hop,
					Total:         blockchain.NewAmount(c.total, dec, res.Asset.Symbol),
					TransferCount: c.count, TxHashes: c.hashes, TxHashesTruncated: c.truncated,
					FirstSeen: c.first, LastSeen: c.last, ToVisitedEarlier: visited,
				})
			}
		}
		frontier = next
	}

	// Classify why unexpanded nodes stopped.
	for _, addr := range order {
		n := nodes[addr]
		if n.Expanded || n.StopReason != "" {
			continue
		}
		switch {
		case n.IsContract != nil && *n.IsContract && !lim.ExpandContracts:
			n.StopReason = StopContractLeaf
		case len(n.Flags) > 0 && n.Hop > 0:
			n.StopReason = StopFlaggedLeaf
		default:
			n.StopReason = StopMaxHops
		}
	}
	for _, addr := range order {
		n := nodes[addr]
		res.Nodes = append(res.Nodes, *n)
		if len(n.Flags) > 0 {
			res.FlaggedNodes = append(res.FlaggedNodes, FlaggedNode{Address: addr, Hop: n.Hop, Flags: n.Flags, Path: pathTo(nodes, addr)})
		}
	}
	if res.Asset.Decimals != nil {
		mu, _ := config.ParseDecimalUnits(req.MinAmount, *res.Asset.Decimals)
		res.MinAmount = blockchain.NewAmount(mu, *res.Asset.Decimals, res.Asset.Symbol)
	} else {
		res.MinAmount = blockchain.Amount{Formatted: req.MinAmount, Symbol: res.Asset.Symbol}
		if res.Asset.Type == blockchain.AssetERC20 {
			res.Warnings = append(res.Warnings, "Token decimals were not observed (no matching transfers); min_amount could not be converted to base units.")
		}
	}
	res.Stats.APICalls = calls.Load()
	res.Stats.DurationMillis = time.Since(start).Milliseconds()
	return res, nil
}

func (s *Service) shouldExpand(n *TraceNode, lim TraceLimits) bool {
	if n.IsContract != nil && *n.IsContract && !lim.ExpandContracts {
		return false
	}
	// Mixer and sanctioned addresses are reported, not traversed: their outflows
	// belong to many unrelated parties.
	return len(n.Flags) == 0
}

// expand fetches and aggregates one address's qualifying outgoing transfers.
func (s *Service) expand(ctx context.Context, addr, assetContract string, calls *atomic.Int64,
	lim TraceLimits, minFor func(int) (*big.Int, error)) expansion {

	var ex expansion
	type src struct {
		name string
		fn   listFn
	}
	srcs := []src{{SourceTransactions, s.Provider.Transactions}, {SourceInternal, s.Provider.InternalTransfers}}
	if assetContract != "" {
		srcs = []src{{SourceTokens, s.Provider.TokenTransfers}}
	}
	var items []blockchain.Activity
	for _, sr := range srcs {
		if err := ctx.Err(); err != nil {
			return expansion{err: err}
		}
		if calls.Add(1) > int64(lim.MaxAPICalls) {
			calls.Add(-1)
			return expansion{err: errBudgetExhausted}
		}
		page, err := sr.fn(ctx, addr, blockchain.FilterFrom, "")
		if err != nil {
			return expansion{err: err}
		}
		if page.NextCursor != "" {
			ex.historyTruncated = true
		}
		items = append(items, page.Items...)
	}

	agg := map[string]*childAgg{}
	seen := map[string]bool{}
	for _, a := range items {
		if seen[a.ID()] || a.From.Address != addr {
			continue
		}
		seen[a.ID()] = true
		if assetContract != "" && a.Asset.Contract != assetContract {
			continue
		}
		if a.Kind == blockchain.KindContractCall {
			continue // interaction without value
		}
		if !a.TransferConfirmed || a.To == nil {
			ex.unconfirmed++
			continue
		}
		if a.ToAddress() == addr {
			ex.selfTransfers++
			continue
		}
		dec := blockchain.NativeDecimals
		if a.Asset.Type == blockchain.AssetERC20 {
			if a.Asset.Decimals == nil {
				ex.unconfirmed++ // cannot compare against min_amount without decimals
				continue
			}
			dec = *a.Asset.Decimals
			if ex.asset == nil {
				as := a.Asset
				ex.asset = &as
			}
		}
		minV, err := minFor(dec)
		if err != nil {
			continue
		}
		if a.Value().Cmp(minV) < 0 {
			ex.belowMin++
			continue
		}
		to := *a.To
		c := agg[to.Address]
		if c == nil {
			c = &childAgg{ref: to, total: new(big.Int), hashSet: map[string]bool{}}
			agg[to.Address] = c
		}
		c.total.Add(c.total, a.Value())
		c.count++
		if !c.hashSet[a.TxHash] {
			c.hashSet[a.TxHash] = true
			if len(c.hashes) < maxEdgeTxHashes {
				c.hashes = append(c.hashes, a.TxHash)
			} else {
				c.truncated = true
			}
		}
		if ts := a.Timestamp; ts != nil {
			if c.first == nil || ts.Before(*c.first) {
				c.first = ts
			}
			if c.last == nil || ts.After(*c.last) {
				c.last = ts
			}
		}
	}
	for _, c := range agg {
		sort.Strings(c.hashes)
		ex.children = append(ex.children, c)
	}
	sort.Slice(ex.children, func(i, j int) bool {
		if cmp := ex.children[i].total.Cmp(ex.children[j].total); cmp != 0 {
			return cmp > 0
		}
		return ex.children[i].ref.Address < ex.children[j].ref.Address
	})
	if len(ex.children) > lim.MaxChildren {
		ex.children = ex.children[:lim.MaxChildren]
		ex.outflowsTruncated = true
	}
	return ex
}

func pathTo(nodes map[string]*TraceNode, addr string) []string {
	var path []string
	for cur := addr; cur != ""; cur = nodes[cur].parent {
		path = append([]string{cur}, path...)
		if len(path) > 10 {
			break
		}
	}
	return path
}
