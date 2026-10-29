package app

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

//go:embed ui/index.html
var uiPage []byte

// uiTools is the allow-list of tools the browser may call.
var uiTools = map[string]bool{
	"get_address_info": true, "get_transactions": true, "trace_funds": true,
	"check_sanctions": true, "risk_report": true,
}

// UIHandler serves the local web interface. Browser requests reach the tools
// through an in-process MCP client session, so they get exactly the same
// schema validation, normalization and evidence as Claude does.
func (a *App) UIHandler(ctx context.Context, shutdown func()) (http.Handler, error) {
	ct, st := mcp.NewInMemoryTransports()
	if _, err := a.Server.Connect(ctx, st, nil); err != nil {
		return nil, err
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "walletscope-ui", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex // the session is shared; calls are serialized for predictable rate-limit use

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		w.Write(uiPage)
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, _ *http.Request) {
		info := a.Sanctions.Info()
		writeJSON(w, http.StatusOK, map[string]any{
			"sanctions_loaded": a.Sanctions.Ready(), "sanctions_publish_date": info.PublishDate,
			"sanctions_error": info.LastError, "provider": a.Config.BlockscoutURL,
		})
	})
	mux.HandleFunc("POST /api/call", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tool string         `json:"tool"`
			Args map[string]any `json:"args"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || !uiTools[req.Tool] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		mu.Lock()
		res, err := session.CallTool(r.Context(), &mcp.CallToolParams{Name: req.Tool, Arguments: req.Args})
		mu.Unlock()
		switch {
		case err != nil:
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		case res.IsError:
			msg := "tool error"
			if len(res.Content) > 0 {
				if tc, ok := res.Content[0].(*mcp.TextContent); ok {
					msg = tc.Text
				}
			}
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": msg})
		default:
			writeJSON(w, http.StatusOK, map[string]any{"result": res.StructuredContent})
		}
	})
	mux.HandleFunc("POST /api/shutdown", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "stopping"})
		go shutdown()
	})
	return localOnly(http.NewCrossOriginProtection().Handler(securityHeaders(mux))), nil
}

// localOnly rejects requests whose Host is not a loopback name, blocking DNS
// rebinding attacks from web pages against this unauthenticated local UI.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != "localhost" && host != "127.0.0.1" && host != "::1" && host != "[::1]" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RunUI serves the web interface on a loopback address until ctx is done or
// the user clicks "Stop server". If openBrowser is set, the page is opened.
func (a *App) RunUI(ctx context.Context, addr string, openBrowser bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("the web UI has no authentication and only binds to loopback addresses (e.g. 127.0.0.1:8090)")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	handler, err := a.UIHandler(ctx, cancel)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	url := "http://" + ln.Addr().String() + "/"
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	a.Logger.Info("web UI listening", "url", url)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	if openBrowser {
		openURL(url)
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, c2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer c2()
	a.Logger.Info("web UI stopping")
	return srv.Shutdown(shutdownCtx)
}

func openURL(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
