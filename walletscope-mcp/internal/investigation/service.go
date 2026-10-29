// Package investigation answers address, activity and fund-flow questions on
// top of a blockchain.Provider.
package investigation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/amnariaz007/walletscope-mcp/internal/blockchain"
)

// ErrUnsupportedChain is returned for any chain other than Ethereum mainnet.
var ErrUnsupportedChain = errors.New("unsupported chain")

// Provenance records where and when data was collected.
type Provenance struct {
	Chain       string    `json:"chain"`
	Source      string    `json:"source" jsonschema:"data provider identifier"`
	SourceURL   string    `json:"source_url" jsonschema:"provider API base URL"`
	RetrievedAt time.Time `json:"retrieved_at" jsonschema:"UTC collection time"`
}

// SourceCoverage describes how much of one upstream listing was read.
type SourceCoverage struct {
	Source   string `json:"source" jsonschema:"transactions, internal_transactions or token_transfers"`
	Pages    int    `json:"pages_fetched"`
	Items    int    `json:"items"`
	Complete bool   `json:"complete" jsonschema:"true when the provider reported no further pages"`
	Error    string `json:"error,omitempty"`
}

// Annotator returns flags (e.g. sanctions or mixer membership) for an address
// using local, already-loaded datasets. It must not perform network I/O.
type Annotator func(addr string) []string

// TraceLimits bounds a fund trace.
type TraceLimits struct {
	Workers         int
	MaxHops         int
	MaxNodes        int
	MaxEdges        int
	MaxAPICalls     int
	MaxChildren     int
	Timeout         time.Duration
	ExpandContracts bool
}

// Service implements the investigation use cases.
type Service struct {
	Provider     blockchain.Provider
	Logger       *slog.Logger
	HistoryPages int
	Trace        TraceLimits
	Annotate     Annotator
	Now          func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) provenance() Provenance {
	return Provenance{Chain: blockchain.ChainEthereum, Source: s.Provider.Name(), SourceURL: s.Provider.BaseURL(), RetrievedAt: s.now()}
}

// ValidateChain accepts "" (default) or "ethereum" in any case.
func ValidateChain(chain string) (string, error) {
	c := strings.ToLower(strings.TrimSpace(chain))
	switch c {
	case "", blockchain.ChainEthereum, "eth", "mainnet", "ethereum-mainnet":
		return blockchain.ChainEthereum, nil
	}
	return "", fmt.Errorf("%w %q: only \"ethereum\" (mainnet) is supported in this version", ErrUnsupportedChain, chain)
}

// providerErrorMessage converts a provider error into an analyst-facing string
// that never contains internal URLs or configuration.
func providerErrorMessage(err error) string {
	switch {
	case errors.Is(err, blockchain.ErrRateLimited):
		return "provider rate limit exceeded after retries"
	case errors.Is(err, blockchain.ErrUnavailable):
		return "provider unavailable or timed out after retries"
	case errors.Is(err, blockchain.ErrBadResponse):
		return "provider returned a malformed response"
	case errors.Is(err, blockchain.ErrNotFound):
		return "provider returned not found"
	case errors.Is(err, blockchain.ErrInvalidInput):
		return "provider rejected the request"
	case errors.Is(err, errBudgetExhausted):
		return "API call budget exhausted"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "request cancelled or timed out"
	}
	var ue *blockchain.UpstreamError
	if errors.As(err, &ue) {
		return ue.Error()
	}
	return "provider request failed"
}

// errBudgetExhausted signals that a trace hit its upstream call budget.
var errBudgetExhausted = errors.New("API call budget exhausted")
