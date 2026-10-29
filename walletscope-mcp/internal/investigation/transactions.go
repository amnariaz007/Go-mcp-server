package investigation

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/amnariaz007/walletscope-mcp/internal/blockchain"
)

// Transaction listing bounds.
const (
	DefaultTxLimit = 20
	MaxTxLimit     = 100
	DirBoth        = "both"
)

// TransactionsResult is the get_transactions response.
type TransactionsResult struct {
	Provenance
	Address   string                `json:"address"`
	Direction string                `json:"direction"`
	Limit     int                   `json:"limit"`
	Items     []blockchain.Activity `json:"items" jsonschema:"newest first; each item is one asset movement or contract interaction"`
	Summary   ActivitySummary       `json:"summary"`
	HasMore   bool                  `json:"has_more" jsonschema:"true when older activity exists beyond what was returned"`
	Coverage  []SourceCoverage      `json:"coverage"`
	Complete  bool                  `json:"complete" jsonschema:"false when a source failed; items may then be missing"`
	Warnings  []string              `json:"warnings,omitempty"`
	Notes     []string              `json:"notes"`
}

// IsPartial reports whether a source failed.
func (r TransactionsResult) IsPartial() bool { return !r.Complete }

// ActivitySummary counts and totals the returned items.
type ActivitySummary struct {
	ByKind                map[string]int `json:"by_kind"`
	Unconfirmed           int            `json:"unconfirmed" jsonschema:"failed, pending or zero-value items that moved no value"`
	NativeInConfirmedETH  string         `json:"native_in_confirmed_eth" jsonschema:"sum of confirmed incoming ETH among returned items"`
	NativeOutConfirmedETH string         `json:"native_out_confirmed_eth" jsonschema:"sum of confirmed outgoing ETH among returned items"`
}

// ValidateTxQuery normalises direction and limit, applying documented defaults.
func ValidateTxQuery(direction string, limit int) (string, int, error) {
	switch direction {
	case "":
		direction = DirBoth
	case blockchain.DirIn, blockchain.DirOut, DirBoth:
	default:
		return "", 0, fmt.Errorf("direction must be \"in\", \"out\" or \"both\", got %q", direction)
	}
	if limit == 0 {
		limit = DefaultTxLimit
	}
	if limit < 1 || limit > MaxTxLimit {
		return "", 0, fmt.Errorf("limit must be between 1 and %d, got %d", MaxTxLimit, limit)
	}
	return direction, limit, nil
}

// GetTransactions returns the most recent activities merged across sources.
func (s *Service) GetTransactions(ctx context.Context, addr, direction string, limit int) (*TransactionsResult, error) {
	direction, limit, err := ValidateTxQuery(direction, limit)
	if err != nil {
		return nil, err
	}
	filter := blockchain.FilterAll
	switch direction {
	case blockchain.DirIn:
		filter = blockchain.FilterTo
	case blockchain.DirOut:
		filter = blockchain.FilterFrom
	}
	// Each source contributes its newest `limit` items, so the merged newest
	// `limit` are correct. Pages are 50 items; MaxTxLimit needs at most 2.
	maxPages := (limit + 49) / 50
	h := s.CollectHistory(ctx, addr, filter, maxPages, limit)
	if h.AllSourcesFailed() {
		return nil, errors.New("all activity sources failed: " + h.Coverage[0].Error)
	}

	res := &TransactionsResult{
		Provenance: s.provenance(),
		Address:    addr,
		Direction:  direction,
		Limit:      limit,
		Coverage:   h.Coverage,
		Complete:   !h.AnySourceFailed(),
		Summary:    ActivitySummary{ByKind: map[string]int{}},
		Notes: []string{
			"Direction is relative to the queried address: in = it received, out = it sent, self = both sides.",
			"contract_call items are interactions that moved no ETH; only items with transfer_confirmed=true represent value that actually moved.",
			"A transaction's ETH value and its ERC-20 events are listed separately because they are different assets.",
			"Failed or pending transactions are shown with transfer_confirmed=false and excluded from the totals.",
			"Outgoing token events with initiated_by_wallet=false were emitted in transactions the wallet did not sign (approved spender, relayer, or spoofed/address-poisoning tokens) and should not be read as the wallet's own actions.",
		},
	}
	if !res.Complete {
		res.Warnings = append(res.Warnings, "At least one activity source failed; the list may be missing items (see coverage).")
	}
	in, out := new(big.Int), new(big.Int)
	for _, a := range h.Activities {
		a.Direction = a.DirectionFor(addr)
		if a.Direction == "" {
			continue // provider returned an unrelated row; never attribute it to the wallet
		}
		if len(res.Items) == limit {
			res.HasMore = true
			break
		}
		res.Items = append(res.Items, a)
		res.Summary.ByKind[a.Kind]++
		if !a.TransferConfirmed {
			res.Summary.Unconfirmed++
			continue
		}
		if a.Asset.Type == blockchain.AssetNative {
			switch a.Direction {
			case blockchain.DirIn:
				in.Add(in, a.Value())
			case blockchain.DirOut:
				out.Add(out, a.Value())
			}
		}
	}
	for _, c := range h.Coverage {
		if !c.Complete && c.Error == "" {
			res.HasMore = true
		}
	}
	if res.Items == nil {
		res.Items = []blockchain.Activity{}
	}
	res.Summary.NativeInConfirmedETH = blockchain.FormatUnits(in, blockchain.NativeDecimals)
	res.Summary.NativeOutConfirmedETH = blockchain.FormatUnits(out, blockchain.NativeDecimals)
	return res, nil
}
