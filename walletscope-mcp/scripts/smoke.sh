#!/usr/bin/env bash
# Live smoke test: real Blockscout + real OFAC download over stdio and HTTP.
# Requires network access. Exits non-zero on the first failure.
set -euo pipefail
cd "$(dirname "$0")/.."
BIN=./bin/walletscope
CALL=./bin/mcpcall
LAZARUS=0x098B716B8Aaf21512996dC57EB0615e2383E2f96   # OFAC SDN: LAZARUS GROUP (DPRK3)
VITALIK=0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045

check() { local name=$1; shift; if "$@" >/dev/null; then echo "PASS $name"; else echo "FAIL $name"; exit 1; fi; }
json_has() { python3 -c "import json,sys; d=json.load(sys.stdin); sys.exit(0 if eval(sys.argv[1]) else 1)" "$1"; }

echo "== stdio =="
$CALL -bin $BIN -tool get_address_info -args "{\"address\":\"$VITALIK\"}" 2>/dev/null | json_has "d['ens']=='vitalik.eth' and d['balance'] is not None" && echo "PASS get_address_info" || { echo "FAIL:get_address_info"; exit 1; }
$CALL -bin $BIN -tool get_transactions -args "{\"address\":\"$VITALIK\",\"limit\":5}" 2>/dev/null | json_has "len(d['items'])==5" && echo "PASS get_transactions" || { echo "FAIL:get_transactions"; exit 1; }
$CALL -bin $BIN -tool check_sanctions -args "{\"address\":\"$LAZARUS\"}" 2>/dev/null | json_has "d['status']=='match'" && echo "PASS check_sanctions" || { echo "FAIL:check_sanctions"; exit 1; }
$CALL -bin $BIN -tool trace_funds -args "{\"address\":\"$LAZARUS\",\"hops\":1,\"min_amount\":\"1\"}" 2>/dev/null | json_has "len(d['nodes'])>=1" && echo "PASS trace_funds" || { echo "FAIL:trace_funds"; exit 1; }

echo "== http =="
TOKEN=smoke-test-token-$RANDOM$RANDOM
WALLETSCOPE_HTTP_AUTH_TOKEN=$TOKEN WALLETSCOPE_HTTP_ADDR=127.0.0.1:18181 $BIN -transport http 2>/dev/null &
PID=$!
trap 'kill $PID 2>/dev/null || true' EXIT
for _ in $(seq 1 50); do curl -fs http://127.0.0.1:18181/healthz >/dev/null && break; sleep 0.2; done
check healthz curl -fs http://127.0.0.1:18181/healthz
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:18181/mcp -H 'Content-Type: application/json' -d '{}')
[ "$code" = 401 ] && echo "PASS unauthenticated request rejected (401)" || { echo "FAIL expected 401 got $code"; exit 1; }
$CALL -url http://127.0.0.1:18181/mcp -token "$TOKEN" -tool risk_report -args "{\"address\":\"$LAZARUS\"}" 2>/dev/null | json_has "d['score']==100" && echo "PASS risk_report over HTTP" || { echo "FAIL:risk_report over HTTP"; exit 1; }
echo "smoke: all checks passed"
