// Package observability provides structured logging setup, request-scoped
// operational counters and identifiers. Operational logs are written to stderr
// and are never part of the evidence returned to analysts.
package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"sync/atomic"
)

// NewLogger builds a slog logger writing to w (stderr in production, so that
// stdout stays reserved for MCP protocol traffic in stdio mode).
func NewLogger(w io.Writer, level, format string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// NewID returns a random 128-bit hex identifier with the given prefix.
func NewID(prefix string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// CallStats accumulates upstream and cache activity for one tool call. All
// methods are safe for concurrent use by tracing workers.
type CallStats struct {
	UpstreamRequests atomic.Int64
	UpstreamErrors   atomic.Int64
	Retries          atomic.Int64
	CacheHits        atomic.Int64
	CacheMisses      atomic.Int64
}

// LogAttrs returns the counters as slog attributes.
func (s *CallStats) LogAttrs() []any {
	return []any{
		slog.Int64("upstream_requests", s.UpstreamRequests.Load()),
		slog.Int64("upstream_errors", s.UpstreamErrors.Load()),
		slog.Int64("upstream_retries", s.Retries.Load()),
		slog.Int64("cache_hits", s.CacheHits.Load()),
		slog.Int64("cache_misses", s.CacheMisses.Load()),
	}
}

type statsKey struct{}

// WithStats attaches stats to ctx.
func WithStats(ctx context.Context, s *CallStats) context.Context {
	return context.WithValue(ctx, statsKey{}, s)
}

// Stats returns the stats attached to ctx, or a throwaway instance so callers
// never need nil checks.
func Stats(ctx context.Context) *CallStats {
	if s, ok := ctx.Value(statsKey{}).(*CallStats); ok {
		return s
	}
	return &CallStats{}
}
