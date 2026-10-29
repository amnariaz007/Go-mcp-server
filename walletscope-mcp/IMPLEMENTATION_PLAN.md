# WalletScope MCP — Implementation Plan

Status: living document, updated as behaviour changes.

## 1. Verified environment and external interfaces (Phase 0, 2026-10-29)

| Item | Verified fact |
|---|---|
| Go | go1.27.3 darwin/amd64 (official tarball, `~/sdk/go`); module targets `go 1.26.0` (minimum required by `golang.org/x/crypto`) |
| MCP SDK | `github.com/modelcontextprotocol/go-sdk` v1.8.0. `mcp.AddTool[In,Out]` infers 2020-12 JSON Schema from `json`/`jsonschema` tags, validates input, fills `StructuredContent`. `mcp.StdioTransport{}`, `mcp.NewStreamableHTTPHandler(getServer, *StreamableHTTPOptions)` (localhost DNS-rebinding protection on by default), `mcp.NewInMemoryTransports()`, `mcp.CommandTransport`, `mcp.StreamableClientTransport` for tests. |
| Blockscout | `https://eth.blockscout.com/api/v2`. Endpoints used: `GET /addresses/{a}`, `GET /addresses/{a}/counters`, `GET /addresses/{a}/transactions?filter=from\|to`, `GET /addresses/{a}/internal-transactions?filter=from\|to`, `GET /addresses/{a}/token-transfers?type=ERC-20&filter=from\|to`. 50 items/page; cursor = `next_page_params` object echoed back as query params; `null` = last page. Invalid address → HTTP 422. Unknown address → 200 with zero balance (so "no data" ≠ "no activity" must be handled by counters). Responses carry `x-ratelimit-limit/remaining/reset`. Amounts are decimal strings in base units. No first-seen field exists. |
| OFAC | Official SDN XML `https://sanctionslistservice.ofac.treas.gov/api/PublicationPreview/exports/SDN.XML` (302 → S3, ~29 MB). Addresses are `<id><idType>Digital Currency Address - ETH</idType><idNumber>0x…</idNumber></id>` inside `<sdnEntry>`. 124 ETH ids on 2026-10-28; EVM-format ids also appear under USDT, USDC, ARB, BSC, ETC. |
| Tornado Cash | **Not present in the current SDN file** (delisted March 2025). Mixer list therefore comes from a separate, documented config list. Default entries verified by Blockscout verified-contract name: 0.1/1/10/100 ETH pools, TornadoRouter, TornadoProxy. |
| Docker | CLI 28.3.2 present; daemon not running at discovery time. |

## 2. Architecture

```
cmd/walletscope            entrypoint: flags, config, wiring, transports, healthcheck subcommand
internal/config            env parsing + validation
internal/observability     slog setup, per-call stats (context), IDs
internal/cache             generic bounded TTL+LRU cache
internal/ratelimit         token bucket shared by all workers of an upstream
internal/blockchain        domain types, Provider interface, Blockscout client (retry, cache, limiter)
internal/sanctions         Screener interface, OFAC SDN XML loader with refresh
internal/investigation     address info, activity listing, fund tracing (bounded worker pool)
internal/risk              rules, deterministic engine, report assembly
internal/mcpserver         tool schemas/handlers, audit wrapper, stdio + Streamable HTTP
```

Dependency direction: mcpserver → investigation/risk → blockchain/sanctions → cache/ratelimit/observability.

## 3. Key decisions

1. **Amounts** are `math/big` integers in base units, exposed as `{raw, decimals, formatted}`; formatting is exact string arithmetic. No floats in accounting.
2. **Activity kinds** are explicit: `native_transfer`, `contract_call` (value 0), `internal_transfer`, `token_transfer`. A tx's ETH value and its ERC-20 events are different assets, never summed together.
3. **Failed / pending** transactions are returned but flagged `transfer_confirmed=false` and excluded from tracing and risk arithmetic.
4. **Tracing** is single-asset per call (`asset`: `native` or an ERC-20 contract), BFS by hop, bounded worker pool, global budgets (nodes, edges, upstream calls, wall time), deterministic merge order, cycle-safe. It is graph tracing, not provenance of specific coins.
5. **Contracts are leaves by default** in tracing (configurable) to avoid exploding through exchanges/routers.
6. **Sanctions** index every EVM-format (`0x`+40 hex) digital-currency id, keeping the OFAC asset label. Status is `match`, `no_match`, or `inconclusive` (dataset not loaded).
7. **Risk score** = sum of per-rule maxima, capped at 100; a direct sanctions match forces 100. Overlapping evidence (same counterparty for one-hop sanctions and mixer) counts once. Missing data → `score_is_lower_bound=true` and an incomplete status, never a "low risk" claim.
8. **HTTP security**: bind 127.0.0.1 by default; optional static bearer token; refuse to start on a non-loopback bind without a token unless explicitly overridden.
9. **No persistent store**; caches are in memory.

10. **Address poisoning** (added after the real Claude Code session): outgoing ERC-20 events in transactions the wallet did not sign are tagged `initiated_by_wallet=false` and scored at the passive tier only.
11. **Initial OFAC load retries quickly** (2s, 4s, 8s, 16s, 32s) until the first success, then at most every 5 min on failure (added after a live TLS handshake timeout).
12. **`internal/app`** owns wiring so `main` and integration tests share one construction path.

## 4. Phases and exit gates

| Phase | Deliverable | Exit gate |
|---|---|---|
| 0 | Discovery, this plan | Plan matches real SDK/provider |
| 1 | Module, SDK, entrypoint, config, stdio, test tool | Real stdio client calls the tool |
| 2 | Blockscout client, `get_address_info`, `get_transactions` | Offline tests + live smoke |
| 3 | `trace_funds` | Cycles, limits, failures, cancellation tested |
| 4 | OFAC loader, `check_sanctions` | match / no_match / inconclusive tested |
| 5 | Risk engine, `risk_report`, `docs/risk-model.md` | Deterministic, explainable, incomplete-data safe |
| 6 | Streamable HTTP, hardening | Both transports invoke tools |
| 7 | Verification, Docker, docs, demo | DoD checklist evaluated |

## 5. Risks

- Blockscout public instance rate limits / indexing gaps → limiter, retries, partial flags, documented coverage.
- OFAC download size/latency → background load, streaming parse, local file override, inconclusive status while loading.
- Hub addresses (exchanges) explode tracing → contract leaves, per-node fan-out cap, global budgets.
- Heuristic false positives (dusting from mixers to famous wallets) → incoming-only exposure weighted lower, limitations stated in every finding.

## 6. Status (2026-10-29)

| Phase | Status | Evidence |
|---|---|---|
| 0 | done | §1 facts verified against live endpoints and SDK source |
| 1 | done | `mcpcall` (official SDK client) → stdio `ping` tool; later replaced by the five tools |
| 2 | done | offline fixture tests; live `get_address_info` / `get_transactions` on vitalik.eth |
| 3 | done | trace tests (cycles, budgets, cancellation, determinism, `-race`); live 2-hop trace |
| 4 | done | SDN excerpt tests; full official file parsed (19,484 entries, 137 EVM ids); live Lazarus match |
| 5 | done | rule regressions incl. the 7 required scenarios; live reports (30/100 mixer withdrawal; 100/100 Lazarus) |
| 6 | done | in-memory, stdio-pipe, compiled-binary stdio and Streamable HTTP integration tests; live HTTP with bearer auth |
| 7 | done except GIF and push | race/vet/build, live smoke, Docker build + smoke, real Claude Code session |

## 7. Acceptance criteria

The definition-of-done checklist from the build brief, evaluated against actual command output, is in the final delivery report.
