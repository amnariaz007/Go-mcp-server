package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/amnariaz007/walletscope-mcp/internal/config"
)

// Scenario (synthetic wallet, real SDN/mixer addresses):
//   - wallet sends 1 ETH to a Lazarus Group address listed in the SDN fixture
//   - wallet deposits 10 ETH into the Tornado Cash 10 ETH pool
//   - wallet attempts a 2 ETH transfer that fails
//   - wallet receives 9.9 ETH from the Tornado Cash 100 ETH pool (internal tx)
const (
	wallet  = "0xaAaAaAaaAaAaAaaAaAAAAAAAAaaaAaAaAaaAaaAa"
	lazarus = "0x098B716B8Aaf21512996dC57EB0615e2383E2f96"
	tornado = "0x910Cbd523D972eb0a6f4cAe4618aD62622b39DbF"
	t100    = "0xA160cdAB225685dA1d56aa342Ad8841c3b53f291"
	plain   = "0xbBbBBBBbbBBBbbbBbbBbbbbBBbBbbbbBbBbbBBbB"
)

type row map[string]any

func ref(addr string, contract bool) row {
	return row{"hash": addr, "is_contract": contract, "name": nil, "ens_domain_name": nil, "metadata": nil, "public_tags": []any{}}
}

func mockBlockscout(t *testing.T) *httptest.Server {
	now := time.Now().UTC()
	ts := func(ago time.Duration) string { return now.Add(-ago).Format("2006-01-02T15:04:05.000000Z") }
	txs := []row{
		{"hash": "0x" + strings.Repeat("1", 64), "block_number": 103, "timestamp": ts(1 * time.Hour), "status": "ok", "value": "1000000000000000000",
			"from": ref(wallet, false), "to": ref(lazarus, false), "method": nil, "created_contract": nil},
		{"hash": "0x" + strings.Repeat("2", 64), "block_number": 102, "timestamp": ts(2 * time.Hour), "status": "ok", "value": "10000000000000000000",
			"from": ref(wallet, false), "to": ref(tornado, true), "method": "deposit", "created_contract": nil},
		{"hash": "0x" + strings.Repeat("3", 64), "block_number": 101, "timestamp": ts(3 * time.Hour), "status": "error", "value": "2000000000000000000",
			"from": ref(wallet, false), "to": ref(plain, false), "method": nil, "created_contract": nil},
	}
	internal := []row{
		{"transaction_hash": "0x" + strings.Repeat("4", 64), "index": 3, "block_number": 100, "timestamp": ts(4 * time.Hour), "success": true,
			"value": "9900000000000000000", "type": "call", "from": ref(t100, true), "to": ref(wallet, false), "created_contract": nil},
	}
	filter := func(items []row, addr, f string) []row {
		var out []row
		for _, it := range items {
			from := strings.EqualFold(it["from"].(row)["hash"].(string), addr)
			to := strings.EqualFold(it["to"].(row)["hash"].(string), addr)
			if (f == "from" && from) || (f == "to" && to) || (f == "" && (from || to)) {
				out = append(out, it)
			}
		}
		if out == nil {
			out = []row{}
		}
		return out
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/addresses/{addr}", func(w http.ResponseWriter, r *http.Request) {
		a := r.PathValue("addr")
		json.NewEncoder(w).Encode(row{"hash": a, "coin_balance": "123000000000000000", "is_contract": strings.EqualFold(a, tornado),
			"block_number_balance_updated_at": 104, "name": nil, "ens_domain_name": nil, "metadata": nil, "public_tags": []any{}, "proxy_type": nil})
	})
	mux.HandleFunc("/api/v2/addresses/{addr}/counters", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(row{"transactions_count": "3", "token_transfers_count": "0"})
	})
	list := func(items []row) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(row{"items": filter(items, r.PathValue("addr"), r.URL.Query().Get("filter")), "next_page_params": nil})
		}
	}
	mux.HandleFunc("/api/v2/addresses/{addr}/transactions", list(txs))
	mux.HandleFunc("/api/v2/addresses/{addr}/internal-transactions", list(internal))
	mux.HandleFunc("/api/v2/addresses/{addr}/token-transfers", list(nil))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func testConfig(t *testing.T, blockscoutURL string, extra map[string]string) *config.Config {
	t.Helper()
	env := map[string]string{
		"WALLETSCOPE_BLOCKSCOUT_URL":     blockscoutURL + "/api/v2",
		"WALLETSCOPE_OFAC_SOURCE":        filepath.Join("..", "..", "testdata", "sanctions", "sdn_sample.xml"),
		"WALLETSCOPE_UPSTREAM_RPS":       "1000",
		"WALLETSCOPE_UPSTREAM_BURST":     "1000",
		"WALLETSCOPE_OFAC_WAIT_ON_QUERY": "5s",
	}
	for k, v := range extra {
		env[k] = v
	}
	cfg, err := config.LoadFrom(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func startApp(t *testing.T, extra map[string]string) *App {
	a := Build(testConfig(t, mockBlockscout(t).URL, extra), quiet())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Start(ctx)
	return a
}

func connectInMemory(t *testing.T, a *App) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := a.Server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("%s: tool error: %v", tool, res.Content[0].(*mcp.TextContent).Text)
	}
	b, _ := json.Marshal(res.StructuredContent)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAllToolsEndToEnd(t *testing.T) {
	cs := connectInMemory(t, startApp(t, nil))
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
		if tl.Description == "" || tl.InputSchema == nil || tl.OutputSchema == nil {
			t.Errorf("tool %s lacks description or schemas", tl.Name)
		}
	}
	if got := strings.Join(names, ","); got != "check_sanctions,get_address_info,get_transactions,risk_report,trace_funds" {
		t.Fatalf("tools = %s", got)
	}

	info := call(t, cs, "get_address_info", map[string]any{"address": strings.ToLower(wallet)})
	if info["address"] != wallet || info["first_observed"] == nil || info["complete"] != true {
		t.Fatalf("address info: %v", info)
	}

	txs := call(t, cs, "get_transactions", map[string]any{"address": wallet, "direction": "out", "limit": 10})
	if items := txs["items"].([]any); len(items) != 3 {
		t.Fatalf("expected 3 outgoing items, got %d", len(items))
	}
	if sum := txs["summary"].(map[string]any); sum["native_out_confirmed_eth"] != "11" || sum["unconfirmed"].(float64) != 1 {
		t.Fatalf("failed tx must be excluded from totals: %v", sum)
	}

	sanc := call(t, cs, "check_sanctions", map[string]any{"address": lazarus})
	if sanc["status"] != "match" || !strings.Contains(fmt.Sprint(sanc["matches"]), "LAZARUS GROUP") {
		t.Fatalf("sanctions: %v", sanc)
	}
	if s := call(t, cs, "check_sanctions", map[string]any{"address": wallet}); s["status"] != "no_match" {
		t.Fatalf("wallet should not match: %v", s)
	}

	trace := call(t, cs, "trace_funds", map[string]any{"address": wallet, "hops": 2, "min_amount": "0.5"})
	flagged := fmt.Sprint(trace["flagged_nodes"])
	if !strings.Contains(flagged, lazarus) || !strings.Contains(flagged, "ofac_sdn:LAZARUS GROUP") || !strings.Contains(flagged, "mixer:Tornado Cash 10 ETH pool") {
		t.Fatalf("trace flags: %s", flagged)
	}
	if strings.Contains(fmt.Sprint(trace["edges"]), plain) {
		t.Fatal("failed transfer appeared in trace")
	}

	rep := call(t, cs, "risk_report", map[string]any{"address": wallet, "investigation_id": "inv-test"})
	if rep["score"].(float64) != 100 || rep["risk_band"] != "severe" || rep["assessment_status"] != "complete" {
		t.Fatalf("risk: score=%v band=%v status=%v warnings=%v", rep["score"], rep["risk_band"], rep["assessment_status"], rep["warnings"])
	}
	rules := map[string]float64{}
	for _, f := range rep["findings"].([]any) {
		fm := f.(map[string]any)
		rules[fm["rule_id"].(string)] = fm["points"].(float64)
		if len(fm["evidence_tx_hashes"].([]any)) == 0 {
			t.Errorf("%s has no evidence", fm["rule_id"])
		}
	}
	want := map[string]float64{"SANCTIONS_ONE_HOP": 60, "MIXER_INTERACTION": 40, "NEW_WALLET_LARGE_TRANSFER": 20}
	for k, v := range want {
		if rules[k] != v {
			t.Errorf("rule %s points = %v want %v (all: %v)", k, rules[k], v, rules)
		}
	}
	if _, ok := rules["SANCTIONS_DIRECT"]; ok {
		t.Error("one-hop exposure reported as direct match")
	}
}

func TestToolInputErrorsAreToolErrors(t *testing.T) {
	cs := connectInMemory(t, startApp(t, nil))
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"get_address_info", map[string]any{"address": "0x123"}, "pattern"},
		{"get_address_info", map[string]any{"address": wallet, "chain": "solana"}, "enum"},
		{"get_transactions", map[string]any{"address": wallet, "limit": 1000}, "maximum"},
		{"trace_funds", map[string]any{"address": wallet, "hops": 9}, "maximum"},
		{"trace_funds", map[string]any{"address": wallet, "min_amount": "-1"}, "pattern"},
		{"check_sanctions", map[string]any{"address": "0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96046"}, "checksum"},
	}
	for _, c := range cases {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: c.tool, Arguments: c.args})
		if err != nil {
			t.Fatalf("%s: protocol error instead of tool error: %v", c.tool, err)
		}
		if !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, c.want) {
			t.Errorf("%s %v: isError=%v content=%v", c.tool, c.args, res.IsError, res.Content)
		}
	}
}

func TestSanctionsUnavailableMakesRiskIndeterminate(t *testing.T) {
	a := startApp(t, map[string]string{
		"WALLETSCOPE_OFAC_SOURCE":        filepath.Join(t.TempDir(), "missing.xml"),
		"WALLETSCOPE_OFAC_WAIT_ON_QUERY": "50ms",
	})
	cs := connectInMemory(t, a)
	if s := call(t, cs, "check_sanctions", map[string]any{"address": lazarus}); s["status"] != "inconclusive" {
		t.Fatalf("expected inconclusive: %v", s)
	}
	rep := call(t, cs, "risk_report", map[string]any{"address": plain})
	if rep["assessment_status"] != "insufficient_data" || rep["risk_band"] != "indeterminate" || rep["score_is_lower_bound"] != true {
		t.Fatalf("risk with missing sanctions data: %v %v", rep["assessment_status"], rep["risk_band"])
	}
}

func TestStdioFraming(t *testing.T) {
	a := startApp(t, nil)
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Server.Run(ctx, &mcp.IOTransport{Reader: sr, Writer: sw}) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: cr, Writer: cw}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := call(t, cs, "check_sanctions", map[string]any{"address": lazarus}); s["status"] != "match" {
		t.Fatalf("%v", s)
	}
	cs.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not exit after client EOF")
	}
}

// TestStdioBinary builds the real binary and drives it over stdio, verifying
// that stdout carries only protocol traffic (any stray output would break the client).
func TestStdioBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "walletscope")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, "../../cmd/walletscope")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	srv := mockBlockscout(t)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"WALLETSCOPE_BLOCKSCOUT_URL="+srv.URL+"/api/v2",
		"WALLETSCOPE_OFAC_SOURCE="+filepath.Join("..", "..", "testdata", "sanctions", "sdn_sample.xml"),
		"WALLETSCOPE_LOG_LEVEL=debug")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if cs.InitializeResult().ServerInfo.Name != "walletscope" {
		t.Fatal("unexpected server info")
	}
	rep := call(t, cs, "risk_report", map[string]any{"address": wallet})
	if rep["score"].(float64) != 100 {
		t.Fatalf("score = %v", rep["score"])
	}
}

func TestStreamableHTTPWithAuth(t *testing.T) {
	const token = "test-token-0123456789abcdef"
	a := startApp(t, map[string]string{"WALLETSCOPE_TRANSPORT": "http", "WALLETSCOPE_HTTP_AUTH_TOKEN": token})
	srv := httptest.NewServer(a.HTTPHandler())
	defer srv.Close()

	// Health is unauthenticated; readiness reflects the sanctions dataset.
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("healthz: %v %v", err, resp)
	}
	resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !a.Sanctions.Ready() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	resp, _ = http.Get(srv.URL + "/readyz")
	if resp.StatusCode != 200 {
		t.Fatalf("readyz = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Missing token is rejected before reaching MCP.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request: %v %v", err, resp.StatusCode)
	}
	resp.Body.Close()

	// Authenticated MCP session: initialize, list tools, call a tool.
	hc := &http.Client{Transport: bearerRT{token: token}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: hc, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 5 {
		t.Fatalf("tools over http: %v %d", err, len(tools.Tools))
	}
	if s := call(t, cs, "check_sanctions", map[string]any{"address": lazarus}); s["status"] != "match" {
		t.Fatalf("%v", s)
	}
	if r := call(t, cs, "risk_report", map[string]any{"address": wallet}); r["score"].(float64) != 100 {
		t.Fatalf("%v", r["score"])
	}
}

func TestOversizedHTTPBodyRejected(t *testing.T) {
	a := startApp(t, map[string]string{"WALLETSCOPE_TRANSPORT": "http"})
	srv := httptest.NewServer(a.HTTPHandler())
	defer srv.Close()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"pad":"` + strings.Repeat("x", 2<<20) + `"}}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestRunHTTPGracefulShutdown(t *testing.T) {
	a := startApp(t, map[string]string{"WALLETSCOPE_TRANSPORT": "http", "WALLETSCOPE_HTTP_ADDR": "127.0.0.1:0"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.RunHTTP(ctx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not shut down")
	}
}

type bearerRT struct{ token string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestWebUI(t *testing.T) {
	a := startApp(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := a.UIHandler(ctx, cancel)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil || resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("index: %v %v", err, resp)
	}
	resp.Body.Close()

	post := func(body string) (int, map[string]any) {
		resp, err := http.Post(srv.URL+"/api/call", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	code, m := post(`{"tool":"check_sanctions","args":{"address":"` + lazarus + `"}}`)
	if code != 200 || m["result"].(map[string]any)["status"] != "match" {
		t.Fatalf("call: %d %v", code, m)
	}
	if code, _ := post(`{"tool":"shutdown_everything","args":{}}`); code != 400 {
		t.Fatalf("unknown tool must be rejected, got %d", code)
	}
	if code, m := post(`{"tool":"get_address_info","args":{"address":"0x12"}}`); code != 422 || m["error"] == nil {
		t.Fatalf("invalid input must surface the tool error: %d %v", code, m)
	}

	// DNS-rebinding guard: a non-loopback Host header is refused.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/status", nil)
	req.Host = "evil.example"
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host: %v %d", err, resp.StatusCode)
	}
	resp.Body.Close()
}

func TestUIRefusesNonLoopback(t *testing.T) {
	a := startApp(t, nil)
	if err := a.RunUI(context.Background(), "0.0.0.0:0", false); err == nil {
		t.Fatal("UI must refuse to bind to a public address")
	}
}
