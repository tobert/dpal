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

- **No streaming.** `CreateChatCompletion` blocks until the full
  response is back. V4-Pro in thinking mode can take minute-plus on
  reasoning-heavy prompts with no visible progress. deepseek-go
  exposes streaming; need to thread `Server.chat` through it and add
  an MCP progress-notification path. Especially painful now that the
  two-phase default doubles the silent wait time.

- **No stateful + non-agentic tool.** Today the matrix has
  `consult_deepseek` (stateful + agentic) and `consult_deepseek_oneshot`
  (stateless + direct), but no "multi-turn V4 chat without auto-explore."
  gpal has the same gap. The per-call `disable_explore` flag covers most
  cases; add a third tool only if someone actually asks. (If you do, name
  it so the distinction from `consult_deepseek` is obvious — not
  `consult_deepseek_direct`, which is too close to `oneshot`.)

- **Two-phase loses the prefix cache on model switch.** DeepSeek caches
  per-model, so switching from `deepseek-v4-flash` (explore) to
  `deepseek-v4-pro` (synth) starts the synthesizer cold every turn.
  Acceptable today — the agentic value outweighs the cache cost — but
  worth measuring if a heavy user complains.

- **No `reasoning_effort` knob.** V4 supports `reasoning_effort` =
  `"high"`/`"max"` on thinking-mode calls; deepseek-go exposes it via
  `ExtraFields`. We hardcode the model default (whatever it is). Add a
  per-call `reasoning_effort` field to `ConsultInput`/`OneshotInput`
  when someone wants to tune for deeper or shallower thinking.

- **`sess.reasoning` duplicates `sess.messages[*].ReasoningContent`.**
  After the V4 rule flip, `reasoning_content` lives on the assistant
  message itself for replay. The parallel `sess.reasoning` array is
  redundant; only `dpal://session/{id}` reads from it. Refactor:
  reconstruct the reasoning view by walking `sess.messages` and drop
  the parallel array.

- **Investigate explorer-loop behavior on broad-scope prompts.** Even
  with the V4-aware explorer prompt, an open-ended "review everything"
  scope could push past the 10-iteration cap. We confirmed the new
  prompt holds for focused tasks (14 tool calls inside the cap on a
  5-file scope). If broad scopes become a real workflow, options:
  (a) raise the cap, (b) add code-side repeat-detection (skip
  duplicate tool calls), (c) further tighten the prompt with a hard
  numeric file budget.

- **OTel not on by default for the install command.** README's
  `claude mcp add` example doesn't pass `--otel-endpoint`. When the
  explorer hits the iteration cap or behaves strangely, there are no
  traces to inspect. Consider either flipping the install example to
  include a localhost OTLP target, or adding an in-memory ring buffer
  of recent tool calls/responses for the `dpal://debug` resource.

## Model migration

- **V4 alias retirement: 2026-07-24.** `deepseek-chat` and
  `deepseek-reasoner` stop working. dpal already moved to explicit V4
  IDs (`ModelV4Pro`/`ModelV4Flash`), so this is informational. Watch
  for the deepseek-go SDK to add V4 constants; replace dpal's locals
  when they do.

