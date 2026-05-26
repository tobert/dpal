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
go install ./cmd/dpal
```

This drops the binary at `$(go env GOPATH)/bin/dpal` (or `$(go env GOBIN)`
if you've set it). Make sure that directory is on your `PATH` so the MCP
client can spawn `dpal` by name. `go build ./cmd/dpal` is also fine for
development; it leaves `./dpal` in the project.

## Install into Claude Code

Save your DeepSeek API key to `~/.deepseek-key` (`chmod 600` recommended)
and register dpal as a user-scope MCP server. `--api-key-file` re-reads
the file on every startup, so rotating the key is a one-line edit — no
need to re-run `claude mcp add`, and the key never lands in Claude
Code's MCP config or in `ps` output.

```sh
claude mcp add dpal \
    -s user \
    -- dpal --api-key-file ~/.deepseek-key
```

(If `dpal` isn't on the spawning shell's `PATH`, substitute an absolute
path — `$(command -v dpal)` resolves it at install time.)

With a local OTel collector listening on the default OTLP HTTP port
(4318) — useful with Jaeger, SigNoz, or `otelcol --config local`:

```sh
claude mcp add dpal \
    -s user \
    -- dpal \
       --api-key-file ~/.deepseek-key \
       --otel-endpoint localhost:4318 \
       --otel-protocol http/protobuf
```

Substitute your collector's actual address — `localhost:4317` for gRPC
(omit `--otel-protocol`, gRPC is the default), or whatever
`OTEL_EXPORTER_OTLP_ENDPOINT` your tooling reports.

Inspect or remove with `claude mcp list`, `claude mcp get dpal`, or
`claude mcp remove dpal`.

## Run standalone

```sh
export DEEPSEEK_API_KEY=...
dpal
```

Speaks MCP over stdio.

### Flags

| flag | env fallback | default |
| --- | --- | --- |
| `--api-key-file` | `DEEPSEEK_API_KEY` | (one of the three is required) |
| `--api-key` | `DEEPSEEK_API_KEY` | (visible via `ps`; prefer `--api-key-file`) |
| `--root` | — | `.` |
| `--no-explore` | — | `false` |
| `--otel-endpoint` | `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | empty (OTel disabled) |
| `--otel-protocol` | `OTEL_EXPORTER_OTLP_PROTOCOL` | `grpc` |
| `--otel-insecure` | — | `true` |
| `--version` | — | — |

### Tracing

When `--otel-endpoint` (or `OTEL_EXPORTER_OTLP_ENDPOINT`) is set, dpal
exports OTLP traces with one `deepseek.chat` span per upstream call
plus an `explorer.tool_call` child span per tool dispatch. Spans carry
`gen_ai.*` semantic attributes (system, operation, request/response
model, input/output tokens) plus DeepSeek-specific
`deepseek.usage.cache_hit_tokens` / `cache_miss_tokens`. Both gRPC
(default, port 4317) and HTTP (`--otel-protocol http/protobuf`, port
4318) transports are supported. `OTEL_SERVICE_NAME` overrides the
default `service.name=dpal`.
