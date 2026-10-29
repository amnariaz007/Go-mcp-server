// Command walletscope runs the WalletScope MCP server.
//
//	walletscope                      # stdio transport (default)
//	walletscope -transport http      # Streamable HTTP on WALLETSCOPE_HTTP_ADDR
//	walletscope healthcheck [url]    # exit 0 if the HTTP server reports healthy
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/amnariaz007/walletscope-mcp/internal/app"
	"github.com/amnariaz007/walletscope-mcp/internal/config"
	"github.com/amnariaz007/walletscope-mcp/internal/mcpserver"
	"github.com/amnariaz007/walletscope-mcp/internal/observability"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(os.Args[2:]))
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "walletscope:", err)
		os.Exit(1)
	}
}

func run() error {
	transport := flag.String("transport", "", "override WALLETSCOPE_TRANSPORT (stdio or http)")
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *version {
		fmt.Println(mcpserver.Version)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}
	if *transport != "" {
		cfg.Transport = *transport
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("invalid configuration:\n%w", err)
		}
	}
	// Logs always go to stderr: in stdio mode stdout carries only MCP messages.
	logger := observability.NewLogger(os.Stderr, cfg.LogLevel, cfg.LogFormat)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a := app.Build(cfg, logger)
	a.Start(ctx)
	logger.Info("starting walletscope", "version", mcpserver.Version, "transport", cfg.Transport,
		"blockscout_url", cfg.BlockscoutURL, "ofac_source", cfg.OFACSource, "mixers", len(cfg.Mixers))
	if cfg.Transport == config.TransportHTTP {
		return a.RunHTTP(ctx)
	}
	return a.RunStdio(ctx)
}

func healthcheck(args []string) int {
	url := "http://127.0.0.1:8080/healthz"
	if len(args) > 0 {
		url = args[0]
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy: HTTP", resp.StatusCode)
		return 1
	}
	return 0
}
