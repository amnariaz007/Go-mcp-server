package investigation

import (
	"context"
	"fmt"
	"time"

	"github.com/amnariaz007/walletscope-mcp/internal/blockchain"
)

// Account types.
const (
	AccountEOA          = "eoa"
	AccountEOADelegated = "eoa_eip7702_delegated"
	AccountContract     = "contract"
	AccountUnknown      = "unknown"
)

// ObservedActivity is a timestamped activity reference.
type ObservedActivity struct {
	Timestamp time.Time `json:"timestamp"`
	TxHash    string    `json:"tx_hash"`
	Kind      string    `json:"kind"`
}

// AddressInfoResult is the get_address_info response.
type AddressInfoResult struct {
	Provenance
	Address             string             `json:"address" jsonschema:"EIP-55 checksummed address"`
	Balance             *blockchain.Amount `json:"balance,omitempty" jsonschema:"native ETH balance; absent if the provider did not report one"`
	BalanceAsOfBlock    *uint64            `json:"balance_as_of_block,omitempty"`
	AccountType         string             `json:"account_type" jsonschema:"eoa, eoa_eip7702_delegated, contract or unknown"`
	ContractName        string             `json:"contract_name,omitempty"`
	ENS                 string             `json:"ens,omitempty"`
	Labels              []string           `json:"labels,omitempty" jsonschema:"public labels from the provider's metadata service; not independently verified"`
	ProviderFlaggedScam bool               `json:"provider_flagged_scam"`
	CreationTxHash      string             `json:"creation_tx_hash,omitempty"`
	Creator             string             `json:"creator,omitempty"`
	TransactionCount    *int64             `json:"transaction_count,omitempty" jsonschema:"provider counter of top-level transactions sent or received"`
	TokenTransferCount  *int64             `json:"token_transfer_count,omitempty" jsonschema:"provider counter of token transfer events"`
	FirstObserved       *ObservedActivity  `json:"first_observed,omitempty" jsonschema:"earliest activity; present only when the full indexed history was read"`
	LastObserved        *ObservedActivity  `json:"last_observed,omitempty" jsonschema:"most recent activity across all sources read"`
	Coverage            []SourceCoverage   `json:"coverage"`
	Complete            bool               `json:"complete" jsonschema:"true when every requested field was obtained"`
	Limitations         []string           `json:"limitations"`
}

// IsPartial reports whether any requested field could not be obtained.
func (r AddressInfoResult) IsPartial() bool { return !r.Complete }

// GetAddressInfo assembles an address profile. Provider sub-request failures
// produce a partial result with explicit limitations rather than an error,
// unless the core address lookup itself fails.
func (s *Service) GetAddressInfo(ctx context.Context, addr string) (*AddressInfoResult, error) {
	info, err := s.Provider.GetAddress(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("address lookup failed: %s", providerErrorMessage(err))
	}
	res := &AddressInfoResult{
		Provenance:          s.provenance(),
		Address:             addr,
		BalanceAsOfBlock:    info.BalanceBlock,
		ContractName:        info.Name,
		ENS:                 info.ENS,
		Labels:              info.Labels,
		ProviderFlaggedScam: info.IsScam,
		CreationTxHash:      info.CreationTxHash,
		Creator:             info.CreatorAddress,
		Complete:            true,
		Limitations: []string{
			"Data reflects the configured Blockscout instance's index, which may lag the chain or be incomplete.",
			"Labels are provider-supplied metadata and do not establish ownership or identity.",
		},
	}
	switch {
	case info.ProxyType == "eip7702":
		res.AccountType = AccountEOADelegated
		res.ContractName = ""
	case info.IsContract:
		res.AccountType = AccountContract
	default:
		res.AccountType = AccountEOA
	}
	if info.BalanceWei != nil {
		b := blockchain.ETH(info.BalanceWei)
		res.Balance = &b
	} else {
		res.Complete = false
		res.Limitations = append(res.Limitations, "Provider did not report a balance; it is unknown, not zero.")
	}

	counters, err := s.Provider.GetCounters(ctx, addr)
	if err != nil {
		res.Complete = false
		res.Limitations = append(res.Limitations, "Activity counters unavailable: "+providerErrorMessage(err))
	} else {
		res.TransactionCount, res.TokenTransferCount = counters.TransactionsCount, counters.TokenTransfersCount
	}

	// Read the full history only when the counters say it fits in the page
	// budget; otherwise one page per source is enough for "last observed".
	pages := 1
	if counters != nil && counters.TransactionsCount != nil && counters.TokenTransfersCount != nil &&
		*counters.TransactionsCount+*counters.TokenTransfersCount <= int64(s.HistoryPages*50) {
		pages = s.HistoryPages
	}
	h := s.CollectHistory(ctx, addr, blockchain.FilterAll, pages, 0)
	res.Coverage = h.Coverage
	if h.AnySourceFailed() {
		res.Complete = false
		res.Limitations = append(res.Limitations, "One or more activity sources failed; first/last observed may be inaccurate (see coverage).")
	}
	res.LastObserved = observed(h.Activities, false)
	switch {
	case h.Complete():
		res.FirstObserved = observed(h.Activities, true)
	case h.AnySourceFailed():
		res.Complete = false
		res.Limitations = append(res.Limitations, "First observed activity not determined because an activity source failed.")
	default:
		res.Complete = false
		res.Limitations = append(res.Limitations, fmt.Sprintf(
			"First observed activity not determined: the indexed history exceeds the %d-page read budget per source. Blockscout lists newest first and offers no first-seen field.", pages))
	}
	if len(h.Activities) == 0 && !h.AnySourceFailed() {
		res.Limitations = append(res.Limitations, "No indexed activity was returned; this is not proof the address was never used on-chain.")
	}
	return res, nil
}

// observed picks the newest (or oldest) timestamped activity.
func observed(items []blockchain.Activity, oldest bool) *ObservedActivity {
	var best *blockchain.Activity
	for i := range items {
		a := &items[i]
		if a.Timestamp == nil {
			continue
		}
		if best == nil || (oldest && a.Timestamp.Before(*best.Timestamp)) || (!oldest && a.Timestamp.After(*best.Timestamp)) {
			best = a
		}
	}
	if best == nil {
		return nil
	}
	return &ObservedActivity{Timestamp: *best.Timestamp, TxHash: best.TxHash, Kind: best.Kind}
}
