// Command mcpcall is a small real MCP client used for smoke tests and demos.
// It connects to WalletScope over stdio (by spawning the binary) or over
// Streamable HTTP, lists the tools, and optionally calls one.
//
//	go run ./scripts/mcpcall -bin ./bin/walletscope -tool get_address_info -args '{"address":"0x..."}'
//	go run ./scripts/mcpcall -url http://127.0.0.1:8080/mcp -token $TOKEN -tool check_sanctions -args '{...}'
//	go run ./scripts/mcpcall -bin ./bin/walletscope -calls calls.json -out dir/   # several calls, one session
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	bin := flag.String("bin", "", "path to walletscope binary (stdio mode)")
	url := flag.String("url", "", "Streamable HTTP endpoint (http mode)")
	token := flag.String("token", "", "bearer token for http mode")
	tool := flag.String("tool", "", "tool to call (omit to only list tools)")
	args := flag.String("args", "{}", "JSON tool arguments")
	calls := flag.String("calls", "", `JSON file with [{"tool":"...","args":{...}}, ...] to run in one session`)
	outDir := flag.String("out", "", "with -calls: directory to write <tool>.json results into")
	timeout := flag.Duration("timeout", 3*time.Minute, "overall timeout")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	var transport mcp.Transport
	switch {
	case *bin != "":
		cmd := exec.Command(*bin, "-transport", "stdio")
		cmd.Stderr = os.Stderr
		transport = &mcp.CommandTransport{Command: cmd}
	case *url != "":
		hc := &http.Client{}
		if *token != "" {
			hc.Transport = bearer{token: *token, base: http.DefaultTransport}
		}
		transport = &mcp.StreamableClientTransport{Endpoint: *url, HTTPClient: hc, DisableStandaloneSSE: true}
	default:
		fail(fmt.Errorf("one of -bin or -url is required"))
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "walletscope-mcpcall", Version: "1"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		fail(fmt.Errorf("connect: %w", err))
	}
	defer session.Close()
	init := session.InitializeResult()
	fmt.Fprintf(os.Stderr, "connected: server=%s %s protocol=%s\n", init.ServerInfo.Name, init.ServerInfo.Version, init.ProtocolVersion)

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		fail(fmt.Errorf("list tools: %w", err))
	}
	for _, t := range tools.Tools {
		fmt.Fprintf(os.Stderr, "tool: %s\n", t.Name)
	}
	if *calls != "" {
		runCalls(ctx, session, *calls, *outDir)
		return
	}
	if *tool == "" {
		return
	}
	var arguments map[string]any
	if err := json.Unmarshal([]byte(*args), &arguments); err != nil {
		fail(fmt.Errorf("-args is not a JSON object: %w", err))
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: *tool, Arguments: arguments})
	if err != nil {
		fail(fmt.Errorf("call %s: %w", *tool, err))
	}
	if res.IsError {
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				fmt.Fprintln(os.Stderr, "tool error:", tc.Text)
			}
		}
		os.Exit(2)
	}
	out, _ := json.MarshalIndent(res.StructuredContent, "", "  ")
	fmt.Println(string(out))
}

func runCalls(ctx context.Context, session *mcp.ClientSession, path, outDir string) {
	data, err := os.ReadFile(path)
	if err != nil {
		fail(err)
	}
	var list []struct {
		Tool string         `json:"tool"`
		Args map[string]any `json:"args"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		fail(fmt.Errorf("-calls: %w", err))
	}
	for _, c := range list {
		start := time.Now()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: c.Tool, Arguments: c.Args})
		if err != nil {
			fail(fmt.Errorf("call %s: %w", c.Tool, err))
		}
		fmt.Fprintf(os.Stderr, "called %s in %s (isError=%v)\n", c.Tool, time.Since(start).Round(time.Millisecond), res.IsError)
		var out []byte
		if res.IsError {
			out, _ = json.MarshalIndent(res.Content, "", "  ")
		} else {
			out, _ = json.MarshalIndent(res.StructuredContent, "", "  ")
		}
		if outDir == "" {
			fmt.Println(string(out))
			continue
		}
		if err := os.WriteFile(filepath.Join(outDir, c.Tool+".json"), append(out, '\n'), 0o644); err != nil {
			fail(err)
		}
	}
}

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "mcpcall:", err)
	os.Exit(1)
}
