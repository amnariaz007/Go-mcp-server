// Package app wires configuration into services and transports. It is shared
// by the command entry point and the integration tests so both exercise the
// same construction path.
package app

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/amnariaz007/walletscope-mcp/internal/blockchain"
	"github.com/amnariaz007/walletscope-mcp/internal/cache"
	"github.com/amnariaz007/walletscope-mcp/internal/config"
	"github.com/amnariaz007/walletscope-mcp/internal/investigation"
	"github.com/amnariaz007/walletscope-mcp/internal/mcpserver"
	"github.com/amnariaz007/walletscope-mcp/internal/ratelimit"
	"github.com/amnariaz007/walletscope-mcp/internal/risk"
	"github.com/amnariaz007/walletscope-mcp/internal/sanctions"
)

const maxHTTPRequestBytes = 1 << 20

// App holds the constructed services.
type App struct {
	Config    *config.Config
	Logger    *slog.Logger
	Server    *mcp.Server
	Sanctions *sanctions.OFAC
	Mixers    map[string]string
}

// Build constructs every service from cfg. It performs no network I/O; call
// Start to begin loading the sanctions dataset.
func Build(cfg *config.Config, logger *slog.Logger) *App {
	provider := blockchain.NewBlockscout(blockchain.BlockscoutOptions{
		BaseURL:        cfg.BlockscoutURL,
		HTTPClient:     &http.Client{},
		RequestTimeout: cfg.UpstreamTimeout,
		Limiter:        ratelimit.New(cfg.UpstreamRPS, cfg.UpstreamBurst),
		Cache:          cache.New[[]byte](cfg.CacheTTL, cfg.CacheMaxEntries),
		MaxRetries:     cfg.UpstreamRetries,
		Logger:         logger,
	})
	ofac := sanctions.NewOFAC(sanctions.OFACOptions{
		Source:      cfg.OFACSource,
		HTTPClient:  &http.Client{},
		Refresh:     cfg.OFACRefresh,
		LoadTimeout: cfg.OFACLoadTimeout,
		WaitOnQuery: cfg.OFACWaitOnQuery,
		Logger:      logger,
	})
	mixers := map[string]string{}
	for _, m := range cfg.Mixers {
		mixers[blockchain.MustNormalize(strings.ToLower(m.Address))] = m.Label
	}
	inv := &investigation.Service{
		Provider:     provider,
		Logger:       logger,
		HistoryPages: cfg.RiskHistoryPages,
		Trace: investigation.TraceLimits{
			Workers:         cfg.TraceWorkers,
			MaxHops:         cfg.TraceMaxHops,
			MaxNodes:        cfg.TraceMaxNodes,
			MaxEdges:        cfg.TraceMaxEdges,
			MaxAPICalls:     cfg.TraceMaxAPICalls,
			MaxChildren:     cfg.TraceMaxChildren,
			Timeout:         cfg.TraceTimeout,
			ExpandContracts: cfg.TraceExpandContracts,
		},
		Annotate: annotator(ofac, mixers),
	}
	reporter := &risk.Reporter{
		Investigation: inv,
		Sanctions:     ofac,
		Config: risk.Config{
			Mixers:                 mixers,
			StructuringMinDeposits: cfg.StructuringMinDeposits,
			StructuringWindow:      cfg.StructuringWindow,
			StructuringMultiple:    cfg.StructuringMultiple,
			StructuringCoveragePct: cfg.StructuringCoveragePct,
			NewWalletAge:           cfg.NewWalletAge,
			LargeTransferWei:       cfg.LargeTransferWei,
		},
	}
	server := mcpserver.New(mcpserver.Deps{
		Logger:        logger,
		Investigation: inv,
		Sanctions:     ofac,
		Risk:          reporter,
		ToolTimeout:   cfg.ToolTimeout,
		RiskTimeout:   cfg.RiskTimeout,
	})
	return &App{Config: cfg, Logger: logger, Server: server, Sanctions: ofac, Mixers: mixers}
}

// annotator flags addresses using local datasets only (no network I/O).
func annotator(s sanctions.Screener, mixers map[string]string) investigation.Annotator {
	return func(addr string) []string {
		var flags []string
		if recs, _ := s.Lookup(addr); len(recs) > 0 {
			flags = append(flags, "ofac_sdn:"+recs[0].Name)
		}
		if label, ok := mixers[addr]; ok {
			flags = append(flags, "mixer:"+label)
		}
		return flags
	}
}

// Start begins background work (sanctions loading and refresh).
func (a *App) Start(ctx context.Context) { a.Sanctions.Start(ctx) }

// RunStdio serves MCP over stdin/stdout until the client disconnects or ctx ends.
func (a *App) RunStdio(ctx context.Context) error {
	err := a.Server.Run(ctx, &mcp.StdioTransport{})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// HTTPHandler returns the Streamable HTTP handler with health endpoints,
// bearer-token authentication (when configured), cross-origin protection and
// request size limits.
func (a *App) HTTPHandler() http.Handler {
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return a.Server }, &mcp.StreamableHTTPOptions{
		SessionTimeout:               30 * time.Minute,
		MaxRequestBodyBytes:          maxHTTPRequestBytes,
		PropagateRequestCancellation: true,
		Logger:                       a.Logger,
	})
	mux := http.NewServeMux()
	mux.Handle("/mcp", a.requireToken(http.NewCrossOriginProtection().Handler(mcpHandler)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		info := a.Sanctions.Info()
		body := map[string]any{"status": "ready", "sanctions_loaded": a.Sanctions.Ready(), "sanctions_publish_date": info.PublishDate}
		if !a.Sanctions.Ready() {
			// The server can answer non-sanctions tools, but screening would be inconclusive.
			body["status"] = "degraded"
			body["sanctions_error"] = info.LastError
			writeJSON(w, http.StatusServiceUnavailable, body)
			return
		}
		writeJSON(w, http.StatusOK, body)
	})
	return securityHeaders(mux)
}

// requireToken enforces a static bearer token when one is configured.
func (a *App) requireToken(next http.Handler) http.Handler {
	token := a.Config.HTTPAuthToken
	if token == "" {
		return next
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="walletscope"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// RunHTTP serves Streamable HTTP until ctx is cancelled, then shuts down
// gracefully, giving in-flight requests time to finish.
func (a *App) RunHTTP(ctx context.Context) error {
	srv := &http.Server{
		Addr:              a.Config.HTTPAddr,
		Handler:           a.HTTPHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
		// No WriteTimeout: tool calls may legitimately stream for up to the
		// trace/risk timeouts, which are enforced per call instead.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	ln, err := net.Listen("tcp", a.Config.HTTPAddr)
	if err != nil {
		return err
	}
	a.Logger.Info("http transport listening", "addr", ln.Addr().String(), "endpoint", "/mcp",
		"auth", a.Config.HTTPAuthToken != "")
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a.Logger.Info("http transport shutting down")
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
