// Package mcpserver exposes WalletScope capabilities as MCP tools and serves
// them over stdio or Streamable HTTP using the official MCP Go SDK.
package mcpserver

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/amnariaz007/walletscope-mcp/internal/investigation"
	"github.com/amnariaz007/walletscope-mcp/internal/observability"
	"github.com/amnariaz007/walletscope-mcp/internal/risk"
	"github.com/amnariaz007/walletscope-mcp/internal/sanctions"
)

// Version is the server version reported during MCP initialization.
var Version = "0.1.0"

// Deps holds the services the tools depend on.
type Deps struct {
	Logger        *slog.Logger
	Investigation *investigation.Service
	Sanctions     sanctions.Screener
	Risk          *risk.Reporter
	ToolTimeout   time.Duration // default per-call deadline
	RiskTimeout   time.Duration
}

const instructions = `WalletScope investigates Ethereum mainnet addresses with evidence.
Typical workflow: get_address_info -> get_transactions -> check_sanctions -> trace_funds -> risk_report.
Every result carries provenance (source, retrieval time) and completeness flags; treat
complete=false or partial=true as "data missing", never as evidence of safety.
Risk scores are rule-based indicators requiring analyst review, not legal determinations.
Pass the same investigation_id to related calls to correlate them in audit logs.`

// New builds an MCP server with all WalletScope tools registered.
func New(d Deps) *mcp.Server {
	if d.ToolTimeout <= 0 {
		d.ToolTimeout = 45 * time.Second
	}
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "walletscope",
		Title:   "WalletScope wallet investigation",
		Version: Version,
	}, &mcp.ServerOptions{
		Instructions: instructions,
		Logger:       d.Logger,
		Capabilities: &mcp.ServerCapabilities{},
	})
	registerTools(s, &d)
	return s
}

// partialReporter lets results declare partial success for audit logging.
type partialReporter interface{ IsPartial() bool }

// addTool registers a typed handler wrapped with deadline, correlation and
// audit logging. Handler errors become MCP tool errors (isError=true) whose
// text is safe to show to the analyst.
func addTool[In, Out any](s *mcp.Server, d *Deps, tool *mcp.Tool, timeout time.Duration,
	meta func(In) (address, investigationID string),
	h func(ctx context.Context, in In) (Out, error)) {

	if tool.InputSchema == nil {
		schema, err := jsonschema.For[In](nil)
		if err != nil {
			panic(err)
		}
		tool.InputSchema = schema
	}
	if timeout <= 0 {
		timeout = d.ToolTimeout
	}
	mcp.AddTool(s, tool, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		start := time.Now()
		requestID := observability.NewID("req_")
		addr, invID := meta(in)
		stats := &observability.CallStats{}
		ctx = observability.WithStats(ctx, stats)
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		out, err := h(ctx, in)

		outcome := "success"
		if err != nil {
			outcome = "error"
		} else if pr, ok := any(out).(partialReporter); ok && pr.IsPartial() {
			outcome = "partial"
		}
		attrs := []any{
			"tool", tool.Name, "request_id", requestID, "investigation_id", invID, "address", addr,
			"duration_ms", time.Since(start).Milliseconds(), "outcome", outcome,
		}
		attrs = append(attrs, stats.LogAttrs()...)
		if err != nil {
			attrs = append(attrs, "error", err.Error())
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				attrs = append(attrs, "deadline_exceeded", true)
			}
			d.Logger.Warn("tool call", attrs...)
			var zero Out
			return nil, zero, err
		}
		d.Logger.Info("tool call", attrs...)
		return nil, out, nil
	})
}

func ptr[T any](v T) *T { return &v }
