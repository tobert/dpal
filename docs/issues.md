# Open Issues

Live work items for dpal. Code is truth; this exists to track what's
*not* in the code yet. Shared by Amy, Claude, and (with growing
responsibility) DeepSeek itself via `consult_deepseek`.

Keep entries terse — link to `file:line` when a pointer makes the work
concrete. When an item ships, delete the entry.

---

## Prompting

- **No system prompt.** Today we send DeepSeek a single user message
  (oneshot) or accumulated session history (consult), plus the explorer
  tool schemas when `--no-explore` isn't set. There is no instructional
  preamble. The model has no idea it's running inside an MCP server, no
  encouragement to use the tools, no guidance on style. gpal layers
  base + config + CLI prompts; cpal accepts `--system-prompt`. dpal
  should at minimum support `--system-prompt` and probably ship a small
  default ("You can call `list_directory`/`read_file`/`search_project`
  to inspect the project rooted at the current directory before
  answering...").

- **No prompt customization at runtime.** Even with a system-prompt
  flag, individual `consult_deepseek` calls can't override or extend it
  per-session. cpal/gpal both let the caller append per-call. Open
  design question: parameter on the tool, or session-scoped resource?

## Sandbox & security

- **TOCTOU on symlinks.** `explorer.resolve()` calls `EvalSymlinks`
  before the actual `os.ReadFile`/`os.ReadDir`. An attacker with write
  access to the sandbox could swap a symlink between check and use.
  Mitigation is Linux-specific (`openat2(RESOLVE_BENEATH)`) and not
  worth the platform cost while the threat model is "model
  misbehaving" rather than "concurrent attacker with write access."
  Revisit if the threat model changes.

- **Search walk skips errors silently.**
  `internal/explorer/explorer.go:216` returns `nil` from the
  `WalkDirFunc` when the walk hits an unreadable entry. This is the
  right UX for a search tool, but we never even *count* the skipped
  files — a sandbox full of unreadable files looks like "0 hits" with
  no signal. Cheap improvement: tally skipped paths and report
  "(N files unreadable)" in the result footer.

## Reliability

- **No retry on rate limits or transient 5xx.** A 429 from DeepSeek
  surfaces immediately as an error. gpal uses tenacity with
  server-suggested delays parsed from the error metadata; dpal has
  nothing. Worth doing once we see real rate-limit hits in practice.
  Start simple: exponential backoff with jitter, bounded retry count,
  retry only on 429/5xx.

- **DeepSeek-R1 function calling not validated in e2e.**
  `e2e_test.go::TestE2E_ToolLoopReadsSandboxedFile` uses
  `deepseek-chat` (V3) explicitly because R1's tool-calling support
  has been historically spotty. Add a matched test against
  `deepseek-reasoner` so we know whether the default model can drive
  the explorer. If it can't, document the override in README and
  consider defaulting tool-loop calls to V3 even when R1 is the
  user-selected model.

- **Ephemeral OTel endpoint capture.** `claude mcp add` resolves
  `--otel-endpoint` at install time. For collectors like otlp-mcp that
  bind a random port at startup (Amy's setup: `127.0.0.1:46861`), the
  dpal config goes stale every time the collector restarts. Options:
  add `--otel-endpoint-file` that re-reads on dpal startup (matches the
  `--api-key-file` pattern) or document a stable-port collector setup.

- **`--otel-insecure` defaults to true.** Sane for local collectors;
  foot-gun for accidental remote OTLP. DeepSeek's self-review
  suggested flipping the default to false. Defer until we see a real
  case where this matters.

## Observability follow-ups

- **No per-model usage aggregation.** Tokens land as span attributes
  but never get rolled up. gpal exposes a 60s rolling window via its
  `info` resource. Add a token tally to `Server` and surface it in
  `dpal://info` (and consider rate-limiting at the boundary).

- **`dpal://session/{id}` doesn't include reasoning_content.**
  `sess.messages` correctly strips reasoning before sending back to
  DeepSeek, but the resource serves what's in `sess.messages`, so the
  transcript is lossy for R1 sessions. Keep a parallel `reasoning_log`
  per session for display, separate from the model-facing history.

- **`otelinit_test.go` is thin.** Two tests: no-op and bad-protocol.
  No coverage of actual exporter construction. Hard to test without a
  real collector; could mock the gRPC dialer or run a fake OTLP
  receiver in-process. Low priority until the otelinit package grows
  more logic.

## Productivity / DX

- **No two-phase "explore then synthesize" mode.** gpal's flagship is
  using cheap Gemini Flash Lite to autonomously read files, then
  handing the loaded context to Pro/Flash for the actual answer. dpal
  could do the same with `deepseek-chat` exploring and
  `deepseek-reasoner` synthesizing. Big lift; worth it once
  single-model tool loops feel limiting.

- **No streaming.** `CreateChatCompletion` blocks until the full
  response is back. R1 in particular can take minute-plus on
  reasoning-heavy prompts with no visible progress. deepseek-go
  exposes streaming; need to thread `Server.chat` through it and add
  an MCP progress-notification path.

- **No concurrent-access tests for Sessions.** The two-tier lock
  design is correct per inspection and the race detector passes on
  current tests, but no test actually fires concurrent goroutines at
  the same `Sessions` instance. A simple `t.Parallel` test that
  hammers `Acquire`/`Snapshot`/`Consult` from N goroutines would catch
  any future regression.

## Maintenance hygiene

- **No CLAUDE.md / AGENTS.md.** Next assistant joining the repo has
  only the README + commit history to orient on. Worth a short
  CLAUDE.md once the architecture stabilizes — package layout,
  testing conventions, "don't reintroduce session TTL," etc.

- **No CI.** No `.github/workflows`. `go test ./... -race` + `go vet`
  + `go build` on push would catch most regressions Amy would
  otherwise see locally. e2e tests stay behind the `e2e` build tag
  and would not run in CI.

- **`Snapshot()` staleness undocumented.** DeepSeek's self-review
  correctly noted that `Sessions.Snapshot` copies session pointers
  under the map lock then iterates after releasing it, so a
  concurrent `Acquire` can evict an entry mid-iteration. Safe (Go GC
  keeps the pointer valid; per-session lock still works), but the
  staleness is currently silent — add a sentence to the `Snapshot`
  doc comment.
