package blockchain

import (
	"context"
	"errors"
	"fmt"
)

// Provider is the minimal set of upstream operations WalletScope needs.
// Implementations must be safe for concurrent use.
type Provider interface {
	// Name is a short provider identifier, e.g. "blockscout".
	Name() string
	// BaseURL identifies the configured deployment for provenance.
	BaseURL() string
	GetAddress(ctx context.Context, addr string) (*AddressInfo, error)
	GetCounters(ctx context.Context, addr string) (*Counters, error)
	// Transactions lists top-level transactions (native transfers and contract calls).
	Transactions(ctx context.Context, addr string, f Filter, cursor string) (Page, error)
	// InternalTransfers lists ETH movements from call traces.
	InternalTransfers(ctx context.Context, addr string, f Filter, cursor string) (Page, error)
	// TokenTransfers lists ERC-20 Transfer events.
	TokenTransfers(ctx context.Context, addr string, f Filter, cursor string) (Page, error)
}

// Sentinel errors returned by providers.
var (
	ErrNotFound     = errors.New("not found at provider")
	ErrInvalidInput = errors.New("provider rejected the request as invalid")
	ErrRateLimited  = errors.New("provider rate limit exceeded")
	ErrUnavailable  = errors.New("provider unavailable")
	ErrBadResponse  = errors.New("malformed provider response")
)

// UpstreamError carries details about a failed upstream call without exposing
// configuration secrets.
type UpstreamError struct {
	Category string // endpoint category, e.g. "address", "transactions"
	Status   int    // HTTP status, 0 for transport errors
	Err      error  // one of the sentinel errors, possibly wrapped
}

func (e *UpstreamError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s request failed (HTTP %d): %v", e.Category, e.Status, e.Err)
	}
	return fmt.Sprintf("%s request failed: %v", e.Category, e.Err)
}

func (e *UpstreamError) Unwrap() error { return e.Err }
