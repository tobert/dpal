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

### Autonomous exploration

When `--root` points at a project (default: CWD) and `--no-explore` is not
set, both tools hand DeepSeek three function-calling tools so the model
can inspect the codebase on its own during reasoning:

- `list_directory(path)` — list entries under a directory, sorted alphabetically with sizes
- `read_file(path)` — read a file (capped at 100 KiB; `.git`, `node_modules`, `vendor` skipped)
- `search_project(pattern, glob)` — RE2 regex search across files, optional basename glob

All paths are resolved relative to `--root` and validated against `..`
traversal and symlink escapes. The loop caps at 10 tool iterations per
turn.

## Resources

| URI | content |
| --- | --- |
| `dpal://info` | service info (version, default model, session cap/count) |
| `dpal://sessions` | list of active sessions with message/turn counts |
| `dpal://session/{id}` | full transcript of one session |

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
| `--root` | — | `.` |
| `--no-explore` | — | `false` |
| `--otel-endpoint` | `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | empty (OTel disabled) |
| `--otel-insecure` | — | `true` |
| `--version` | — | — |

### Tracing

When `--otel-endpoint` (or `OTEL_EXPORTER_OTLP_ENDPOINT`) is set, dpal
exports OTLP gRPC traces with one `deepseek.chat` span per upstream call,
carrying `gen_ai.*` semantic attributes plus DeepSeek-specific cache
hit/miss token counts. `OTEL_SERVICE_NAME` overrides the default
`service.name=dpal`.
