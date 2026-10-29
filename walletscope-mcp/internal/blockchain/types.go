// Package blockchain defines WalletScope's normalized blockchain data contracts
// and the Provider interface, plus a Blockscout implementation.
package blockchain

import (
	"math/big"
	"strings"
	"time"
)

// Native asset constants for Ethereum mainnet.
const (
	NativeSymbol   = "ETH"
	NativeDecimals = 18
)

// Activity kinds. A single transaction can yield several activities (for
// example a native value transfer plus ERC-20 Transfer events); they are kept
// separate because they move different assets.
const (
	KindNativeTransfer   = "native_transfer"   // top-level tx with value > 0
	KindContractCall     = "contract_call"     // top-level tx with value == 0 (an interaction, not a transfer)
	KindInternalTransfer = "internal_transfer" // ETH moved by a contract call trace
	KindTokenTransfer    = "token_transfer"    // ERC-20 Transfer event
)

// Statuses of the underlying transaction.
const (
	StatusSuccess = "success"
	StatusFailed  = "failed"
	StatusPending = "pending"
)

// Directions relative to the queried address.
const (
	DirIn   = "in"
	DirOut  = "out"
	DirSelf = "self"
)

// Asset types.
const (
	AssetNative = "native"
	AssetERC20  = "erc20"
)

// Amount is an exact token quantity. Raw is the integer amount in base units
// (wei for ETH); Formatted is the exact decimal rendering when decimals are known.
type Amount struct {
	Raw       string `json:"raw" jsonschema:"integer amount in base units (wei for ETH)"`
	Decimals  *int   `json:"decimals,omitempty" jsonschema:"token decimals; absent when the provider did not report them"`
	Formatted string `json:"formatted,omitempty" jsonschema:"exact decimal amount in whole units"`
	Symbol    string `json:"symbol,omitempty"`
}

// NewAmount builds an Amount; decimals < 0 means unknown.
func NewAmount(v *big.Int, decimals int, symbol string) Amount {
	if v == nil {
		v = new(big.Int)
	}
	a := Amount{Raw: v.String(), Symbol: symbol}
	if decimals >= 0 {
		d := decimals
		a.Decimals = &d
		a.Formatted = FormatUnits(v, decimals)
	}
	return a
}

// ETH builds a native ETH amount from wei.
func ETH(wei *big.Int) Amount { return NewAmount(wei, NativeDecimals, NativeSymbol) }

// FormatUnits renders v / 10^decimals exactly, trimming trailing zeros.
func FormatUnits(v *big.Int, decimals int) string {
	neg := v.Sign() < 0
	s := new(big.Int).Abs(v).String()
	if decimals > 0 {
		if len(s) <= decimals {
			s = strings.Repeat("0", decimals-len(s)+1) + s
		}
		whole, frac := s[:len(s)-decimals], strings.TrimRight(s[len(s)-decimals:], "0")
		s = whole
		if frac != "" {
			s += "." + frac
		}
	}
	if neg {
		s = "-" + s
	}
	return s
}

// AddressRef describes a participant as reported inline by the provider.
type AddressRef struct {
	Address    string   `json:"address"`
	IsContract *bool    `json:"is_contract,omitempty"`
	Name       string   `json:"name,omitempty" jsonschema:"contract or provider-supplied name"`
	ENS        string   `json:"ens,omitempty"`
	Labels     []string `json:"labels,omitempty" jsonschema:"public labels supplied by the data provider (not independently verified)"`
}

// Asset identifies what moved.
type Asset struct {
	Type       string `json:"type" jsonschema:"native or erc20"`
	Symbol     string `json:"symbol,omitempty"`
	Name       string `json:"name,omitempty"`
	Contract   string `json:"contract,omitempty" jsonschema:"ERC-20 contract address (absent for native ETH)"`
	Decimals   *int   `json:"decimals,omitempty"`
	Reputation string `json:"reputation,omitempty" jsonschema:"provider token reputation, e.g. ok or scam"`
}

// Key uniquely identifies the asset for aggregation.
func (a Asset) Key() string {
	if a.Type == AssetNative {
		return AssetNative
	}
	return a.Contract
}

// NativeAsset is ETH.
func NativeAsset() Asset {
	d := NativeDecimals
	return Asset{Type: AssetNative, Symbol: NativeSymbol, Name: "Ether", Decimals: &d}
}

// Activity is one normalized on-chain event involving an address.
type Activity struct {
	Kind        string      `json:"kind" jsonschema:"native_transfer, contract_call, internal_transfer or token_transfer"`
	TxHash      string      `json:"tx_hash"`
	LogIndex    *int        `json:"log_index,omitempty" jsonschema:"ERC-20 event log index"`
	TraceIndex  *int        `json:"trace_index,omitempty" jsonschema:"internal transaction index"`
	BlockNumber *uint64     `json:"block_number,omitempty"`
	Timestamp   *time.Time  `json:"timestamp,omitempty"`
	From        AddressRef  `json:"from"`
	To          *AddressRef `json:"to,omitempty" jsonschema:"absent for contract creation"`
	Asset       Asset       `json:"asset"`
	Amount      Amount      `json:"amount"`
	Status      string      `json:"status" jsonschema:"success, failed or pending"`
	// TransferConfirmed is true only when value actually moved: the
	// transaction succeeded and the amount is non-zero.
	TransferConfirmed bool   `json:"transfer_confirmed"`
	Method            string `json:"method,omitempty"`
	Direction         string `json:"direction,omitempty" jsonschema:"in, out or self relative to the queried address"`
	// InitiatedByWallet is set on outgoing ERC-20 events when WalletScope can
	// tell whether the queried wallet signed the carrying transaction. false
	// means the event was emitted in a transaction the wallet did not send
	// (approved spender, relayer, or a spoofed/address-poisoning token).
	InitiatedByWallet *bool `json:"initiated_by_wallet,omitempty" jsonschema:"for outgoing token events: whether the queried wallet signed the transaction (absent when unknown)"`

	value *big.Int
}

// Value returns the exact base-unit amount.
func (a *Activity) Value() *big.Int {
	if a.value == nil {
		return new(big.Int)
	}
	return a.value
}

// SetValue sets the exact amount and its rendered form.
func (a *Activity) SetValue(v *big.Int) {
	a.value = v
	dec := -1
	if a.Asset.Decimals != nil {
		dec = *a.Asset.Decimals
	}
	a.Amount = NewAmount(v, dec, a.Asset.Symbol)
}

// ToAddress returns the recipient address or "".
func (a *Activity) ToAddress() string {
	if a.To == nil {
		return ""
	}
	return a.To.Address
}

// ID is a stable unique identifier for deduplication.
func (a *Activity) ID() string {
	switch {
	case a.LogIndex != nil:
		return a.TxHash + ":log:" + itoa(*a.LogIndex)
	case a.TraceIndex != nil:
		return a.TxHash + ":trace:" + itoa(*a.TraceIndex)
	default:
		return a.TxHash + ":tx"
	}
}

// DirectionFor computes direction relative to addr ("" if not involved).
func (a *Activity) DirectionFor(addr string) string {
	from, to := a.From.Address == addr, a.ToAddress() == addr
	switch {
	case from && to:
		return DirSelf
	case from:
		return DirOut
	case to:
		return DirIn
	}
	return ""
}

func itoa(i int) string { return big.NewInt(int64(i)).String() }

// AddressInfo is the provider's view of an address.
type AddressInfo struct {
	Address           string
	BalanceWei        *big.Int // nil when the provider did not report a balance
	BalanceBlock      *uint64
	IsContract        bool
	Name              string
	ENS               string
	Labels            []string
	IsScam            bool
	IsVerified        bool
	CreationTxHash    string
	CreatorAddress    string
	HasTokenTransfers bool
	ProxyType         string // e.g. "eip7702" for a delegated EOA
}

// Counters are provider-maintained activity counts.
type Counters struct {
	TransactionsCount   *int64
	TokenTransfersCount *int64
}

// Filter restricts listings by direction at the provider.
type Filter string

const (
	FilterAll  Filter = ""
	FilterFrom Filter = "from"
	FilterTo   Filter = "to"
)

// Page is one page of results plus an opaque cursor ("" = no more pages).
type Page struct {
	Items      []Activity
	NextCursor string
}
