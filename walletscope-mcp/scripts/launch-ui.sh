#!/bin/bash
# Starts the WalletScope web UI in the background (building first if needed)
# and opens it in the browser. Used by WalletScope.app; safe to run repeatedly.
set -euo pipefail
DIR="$(cd "$(dirname "$0")/.." && pwd)"
export PATH="$HOME/sdk/go/bin:/usr/local/go/bin:/opt/homebrew/bin:$PATH"
ADDR="${WALLETSCOPE_UI_ADDR:-127.0.0.1:8090}"
URL="http://$ADDR/"
LOG="$HOME/Library/Logs/WalletScope.log"

if curl -fs -o /dev/null --max-time 2 "${URL}api/status"; then
  open "$URL"   # already running
  exit 0
fi
BIN="$DIR/bin/walletscope"
if [ ! -x "$BIN" ] || [ -n "$(find "$DIR/cmd" "$DIR/internal" \( -name '*.go' -o -name '*.html' \) -newer "$BIN" | head -1)" ]; then
  (cd "$DIR" && go build -o bin/walletscope ./cmd/walletscope) >>"$LOG" 2>&1
fi
nohup "$BIN" ui -addr "$ADDR" >>"$LOG" 2>&1 &
