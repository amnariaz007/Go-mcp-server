#!/usr/bin/env bash
# Smoke-test the container image over both transports (network required).
set -euo pipefail
cd "$(dirname "$0")/.."
IMAGE=${1:-walletscope-mcp:local}
TOKEN=docker-smoke-token-$RANDOM$RANDOM
LAZARUS=0x098B716B8Aaf21512996dC57EB0615e2383E2f96
json_has() { python3 -c "import json,sys; d=json.load(sys.stdin); sys.exit(0 if eval(sys.argv[1]) else 1)" "$1"; }

echo "== refuses public bind without token =="
if docker run --rm "$IMAGE" >/dev/null 2>&1; then echo "FAIL started without token"; exit 1; fi
echo "PASS container refuses to start on 0.0.0.0 without WALLETSCOPE_HTTP_AUTH_TOKEN"

echo "== http =="
CID=$(docker run -d --rm -p 127.0.0.1:18282:8080 -e WALLETSCOPE_HTTP_AUTH_TOKEN="$TOKEN" --read-only --cap-drop ALL "$IMAGE")
trap 'docker stop "$CID" >/dev/null 2>&1 || true' EXIT
for _ in $(seq 1 50); do curl -fs http://127.0.0.1:18282/healthz >/dev/null && break; sleep 0.3; done
curl -fs http://127.0.0.1:18282/healthz && echo " PASS healthz" || { echo "FAIL:healthz"; exit 1; }
[ "$(docker exec "$CID" /walletscope healthcheck >/dev/null 2>&1; echo $?)" = 0 ] && echo "PASS in-container healthcheck" || { echo "FAIL:in-container healthcheck"; exit 1; }
[ "$(docker inspect -f '{{.Config.User}}' "$CID")" = "nonroot:nonroot" ] && echo "PASS runs as non-root" || { echo "FAIL:runs as non-root"; exit 1; }
./bin/mcpcall -url http://127.0.0.1:18282/mcp -token "$TOKEN" -tool check_sanctions -args "{\"address\":\"$LAZARUS\"}" 2>/dev/null | json_has "d['status']=='match'" && echo "PASS check_sanctions over HTTP" || { echo "FAIL:check_sanctions over HTTP"; exit 1; }

echo "== stdio =="
WALLETSCOPE_IMAGE=$IMAGE ./bin/mcpcall -bin ./scripts/docker-stdio.sh -tool get_transactions -args "{\"address\":\"$LAZARUS\",\"limit\":3}" 2>/dev/null | json_has "len(d['items'])==3" && echo "PASS get_transactions over docker stdio (stdout is protocol-only)" || { echo "FAIL:get_transactions over docker stdio (stdout is protocol-only)"; exit 1; }
echo "docker-smoke: all checks passed"
