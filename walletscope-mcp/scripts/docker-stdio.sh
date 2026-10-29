#!/usr/bin/env bash
# Wrapper so stdio MCP clients (and scripts/mcpcall -bin) can launch the container.
exec docker run -i --rm -e WALLETSCOPE_TRANSPORT=stdio "${WALLETSCOPE_IMAGE:-walletscope-mcp:local}" -transport stdio
