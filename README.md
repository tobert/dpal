# dpal

An MCP server providing access to DeepSeek models, including R1's separate
reasoning channel.

Sibling of [gpal](https://github.com/tobert/gpal) (Gemini) and cpal (Claude),
implemented in Go on top of the official
[modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk).

## Tools

| tool | description |
| --- | --- |
| `consult_deepseek_oneshot` | Stateless single prompt. Returns answer + `reasoning_content`. |
| `consult_deepseek` | Stateful conversation keyed by `session_id`. Up to 100 in-process sessions, evicted LRU when full. |

Both tools surface DeepSeek-R1's `reasoning_content` separately from the
final answer. Sessions persist for the life of the process (no TTL); use a
fresh `session_id` to start a new conversation.

## Build

```sh
go build ./cmd/dpal
```

## Run

```sh
export DEEPSEEK_API_KEY=...
./dpal
```

Speaks MCP over stdio.

### Flags

| flag | env fallback | default |
| --- | --- | --- |
| `--api-key` | `DEEPSEEK_API_KEY` | (required) |
| `--otel-endpoint` | `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | empty (OTel disabled) |
| `--otel-insecure` | — | `true` |
| `--version` | — | — |

### Tracing

When `--otel-endpoint` (or `OTEL_EXPORTER_OTLP_ENDPOINT`) is set, dpal
exports OTLP gRPC traces with one `deepseek.chat` span per upstream call,
carrying `gen_ai.*` semantic attributes plus DeepSeek-specific cache
hit/miss token counts. `OTEL_SERVICE_NAME` overrides the default
`service.name=dpal`.
