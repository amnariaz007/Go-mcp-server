#!/usr/bin/env bash
# Reproducible five-tool investigation.
#   scripts/demo.sh [address]        live: public Blockscout + official OFAC data
#   scripts/demo.sh --offline        offline: synthetic scenario served from a mock
#                                    Blockscout and the OFAC fixture excerpt (labelled)
set -euo pipefail
cd "$(dirname "$0")/.."

if [ "${1:-}" = "--offline" ]; then
  echo "OFFLINE DEMO: synthetic wallet + recorded OFAC excerpt (testdata/). Not live data."
  exec go test -count=1 -run 'TestAllToolsEndToEnd' -v ./internal/app/
fi

ADDR=${1:-0x212B8e1f935B779bBa6DD6Ca7c29c30aAa6e37BB}
INV=demo-$(date -u +%Y%m%dT%H%M%SZ)
OUT=demo-output/$INV
mkdir -p "$OUT"
echo "LIVE DEMO: address=$ADDR investigation_id=$INV (outputs in $OUT)"
# One MCP session for all five calls, as an AI client would use the server.
cat > "$OUT/calls.json" <<JSON
[
  {"tool": "get_address_info", "args": {"address": "$ADDR", "investigation_id": "$INV"}},
  {"tool": "get_transactions", "args": {"address": "$ADDR", "limit": 10, "direction": "both", "investigation_id": "$INV"}},
  {"tool": "check_sanctions",  "args": {"address": "$ADDR", "investigation_id": "$INV"}},
  {"tool": "trace_funds",      "args": {"address": "$ADDR", "hops": 2, "min_amount": "0.1", "investigation_id": "$INV"}},
  {"tool": "risk_report",      "args": {"address": "$ADDR", "investigation_id": "$INV"}}
]
JSON
./bin/mcpcall -bin ./bin/walletscope -calls "$OUT/calls.json" -out "$OUT" 2>"$OUT/server.log"
grep '^called' "$OUT/server.log"
python3 - "$OUT" <<'PY'
import json, sys, os
o = sys.argv[1]
r = json.load(open(os.path.join(o, "risk_report.json")))
t = json.load(open(os.path.join(o, "trace_funds.json")))
s = json.load(open(os.path.join(o, "check_sanctions.json")))
print(f"\nsanctions: {s['status']} (OFAC SDN published {s['dataset'].get('publish_date')})")
print(f"trace: {len(t['nodes'])} nodes, {len(t['edges'])} edges, partial={t['partial']}, flagged={[f['address'] for f in t['flagged_nodes']]}")
print(f"risk: {r['score']}/100 {r['risk_band']} ({r['assessment_status']})")
print(r["summary"])
for f in r["findings"]:
    print(f"  - {f['rule_id']} +{f['points']}: evidence {f['evidence_tx_hashes'][:3]}")
PY
echo "audit log lines for $INV:"; grep -c "\"investigation_id\":\"$INV\"" "$OUT/server.log"
