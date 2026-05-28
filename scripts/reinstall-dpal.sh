#!/usr/bin/env bash
# Reinstall dpal in Claude Code with OTel pointed at a local collector
# on the standard OTLP gRPC port (4317). --otel-insecure is dpal's
# default but spelled out here for the avoidance of doubt.
set -euo pipefail

claude mcp remove dpal 2>/dev/null || true
claude mcp add dpal -s user -- \
    dpal \
    --api-key-file "$HOME/.deepseek-key" \
    --otel-endpoint localhost:4317 \
    --otel-insecure
