<p align="center">
  <img src="assets/banner.svg" alt="dpal — Your Pal DeepSeek" width="800"/>
</p>

# dpal

dpal is an MCP server providing access to DeepSeek V4 models, surfacing
thinking-mode reasoning as a separate output channel.

Sibling of [gpal](https://github.com/tobert/gpal) (Gemini) and cpal (Claude),
implemented in Go on top of the official
[modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk).

## Tools

| tool | description |
| --- | --- |
| `consult_deepseek` | **Agentic, two-phase, stateful.** Explorer (`deepseek-v4-flash`, non-thinking) reads the codebase via tool calls, then synthesizer (`deepseek-v4-pro` with thinking) writes the answer. Keyed by `session_id`. **Both models are billed.** |
| `consult_deepseek_oneshot` | Stateless, single direct call. No tools, no explore phase. Defaults to `deepseek-v4-pro` with thinking enabled. One prompt in, one answer out. Use when you've already curated context. |

Both surface V4 thinking-mode `reasoning_content` separately from the
final answer. Up to 100 in-process sessions for `consult_deepseek`,
evicted LRU when full. Sessions persist for the life of the process
(no TTL); use a fresh `session_id` to start a new conversation.

> **Model migration note.** The legacy `deepseek-chat` and
> `deepseek-reasoner` aliases route to V4-Flash today but DeepSeek is
> retiring them on **2026-07-24**. dpal addresses V4 models by explicit
> ID (`deepseek-v4-pro`, `deepseek-v4-flash`) so it survives the
> retirement and so `deepseek-v4-pro` can serve as the strong
> synthesizer instead of V4-Flash-with-thinking.

### Two-phase consultation (the agentic default)

`consult_deepseek` is the flagship — it's why dpal exists as a separate
project rather than a config tweak on top of a generic OpenAI-API shim.
Each call runs two phases against the same session history:

1. **Explore.** The explorer model (default `deepseek-v4-flash`, no
   thinking) sees the user prompt and the project's exploration tools.
   It reads files, searches code, and stops as soon as it has loaded
   what the synthesizer will need. Its own final text answer is
   **stripped** so it doesn't anchor the synthesizer. Thinking mode is
   off here — exploration is pattern-match-and-load, not deep reasoning.
2. **Synthesize.** The synthesizer model (default `deepseek-v4-pro` with
   thinking) sees the user prompt plus all of the explorer's tool
   exchanges (but not the explorer's answer) and writes the response.
   Tools are not advertised on this call — the synthesizer reads what
   the explorer loaded rather than running its own tool loop.

This is the same pattern as gpal's auto mode (Gemini Flash Lite explores,
Pro/Flash synthesizes), surfaced through DeepSeek V4's thinking channel,
function calling, and cache visibility.

Per-call overrides on `consult_deepseek`:

| field | default | what it does |
| --- | --- | --- |
| `model` | `deepseek-v4-pro` | synthesizer model |
| `explorer_model` | `deepseek-v4-flash` | explore-phase model |
| `system_prompt` | configured default | synthesizer's system prompt for this call |
| `explorer_system_prompt` | built-in explorer prompt | explorer's system prompt for this call |
| `disable_explore` | `false` | skip the explore phase entirely for this call |
| `thinking` | `true` | V4 thinking mode on the synth call; set `false` for fast non-thinking responses. The explorer phase never uses thinking regardless of this setting. |

If you want a stateful conversation without the explore phase, set
`disable_explore: true` per call. If you want a stateless, direct call,
use `consult_deepseek_oneshot` instead.

### Exploration tools

When `--root` points at a project (default: CWD) and `--no-explore` is
not set, the explorer phase gets three function-calling tools:

- `list_directory(path)` — list entries under a directory, sorted alphabetically with sizes
- `read_file(path)` — read a file (capped at 100 KiB; truncated with a trailing marker if larger)
- `search_project(pattern, glob)` — RE2 regex search across files, optional basename glob; skips `.git`, `node_modules`, `vendor`

All paths are resolved relative to `--root` and validated against `..`
traversal and symlink escapes. The loop caps at 10 tool iterations per
explore phase.

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
| `--root` | — | `.` (fallback when MCP client doesn't advertise roots/list; if it does, dpal uses the first advertised root and logs any extras) |
| `--no-explore` | — | `false` |
| `--config` | — | `$XDG_CONFIG_HOME/dpal/config.toml` (or `~/.config/dpal/config.toml`) |
| `--system-prompt` | — | (repeatable; appends file contents to the system prompt) |
| `--no-default-prompt` | — | `false` (suppresses the built-in DeepSeek-shaped prompt) |
| `--otel-endpoint` | `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | empty (OTel disabled) |
| `--otel-endpoint-file` | — | (re-read every startup; pairs well with collectors on ephemeral ports) |
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
