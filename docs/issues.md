# Open Issues

Live work items for dpal. Code is truth; this exists to track what's
*not* in the code yet. Shared by Amy, Claude, and (with growing
responsibility) DeepSeek itself via `consult_deepseek`.

Keep entries terse — link to `file:line` when a pointer makes the work
concrete. When an item ships, delete the entry.

---

## Sandbox & security

- **TOCTOU on symlinks.** `explorer.resolve()` calls `EvalSymlinks`
  before the actual `os.ReadFile`/`os.ReadDir`. An attacker with write
  access to the sandbox could swap a symlink between check and use.
  Mitigation is Linux-specific (`openat2(RESOLVE_BENEATH)`) and not
  worth the platform cost while the threat model is "model
  misbehaving" rather than "concurrent attacker with write access."
  Revisit if the threat model changes.

## Reliability

- **`--otel-insecure` defaults to true.** Sane for local collectors;
  foot-gun for accidental remote OTLP. DeepSeek's self-review
  suggested flipping the default to false. Defer until we see a real
  case where this matters.

## Observability follow-ups

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

