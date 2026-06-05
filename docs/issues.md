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

## Report hand-off follow-ups

- **Validate report quality vs. the raw dump (the deferred head-to-head).**
  The explorer now hands the synthesizer a curated report instead of the
  raw tool transcript (`extractExplorerReport`), ~7-15% of the bytes. The
  explorer-report experiment (`internal/server/explorer_report_experiment_test.go`)
  validated the report *in isolation* and looked good, but the decisive
  test — same question, pro fed (a) raw transcript vs (b) report, compare
  the two final answers — was deferred. Run it before trusting the report
  path on hard questions; it's the evidence that flash's curation isn't
  dropping needles. (The experiment file's prompts/comments still name the
  retired `stripExplorerSynthesis`; refresh or retire it when you do this.)

- **Report is invisible in `dpal://session`.** Sessions persist lean
  `[user, answer]` now; the per-turn report is ephemeral. There's a
  `dpal.explorer_report_bytes` span attribute but no way to read the
  report itself after the fact. If that visibility matters, surface the
  last report via a debug resource rather than bloating durable history.

- **Synth fetches are pro-priced.** The synthesizer can now `read_file`/
  `search_project` to fill gaps in the report (Option-2 hybrid), but each
  fetch is a `deepseek-v4-pro` tool iteration — expensive. Fine as a
  safety net; watch the counts. If pro over-fetches, tighten the synth
  system-prompt nudge or give the synth tool loop its own (smaller)
  iteration cap separate from the explorer's 100.

## Productivity / DX

- **No streaming.** `CreateChatCompletion` blocks until the full
  response is back. V4-Pro in thinking mode can take minute-plus on
  reasoning-heavy prompts with no visible progress. deepseek-go
  exposes streaming; need to thread `Server.chat` through it and add
  an MCP progress-notification path. Especially painful now that the
  two-phase default doubles the silent wait time — and the synth phase
  can add its own tool-loop round-trips on top.

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

- **`sess.reasoning` duplicates `sess.messages[*].ReasoningContent`.**
  After the V4 rule flip, `reasoning_content` lives on the assistant
  message itself for replay. The parallel `sess.reasoning` array is
  redundant; only `dpal://session/{id}` reads from it. Refactor:
  reconstruct the reasoning view by walking `sess.messages` and drop
  the parallel array.

- **Explorer-loop behavior on broad-scope prompts.** Open-ended scopes
  push tool-call counts up; a broad commit review blew the old
  10-iteration cap. Shipped, in order: cap 10->25; a `project_tree` tool
  (one-call orientation — killed navigation: search_project 12->~1,
  list_directory 13->1); a "reading policy" prompt nudge; and explorer
  thinking-on so file selection is reasoned rather than swept. read_file
  on the same broad review fell 29 -> 25 (nudge) -> 19 (thinking), and
  the 19 are now the genuinely relevant files (it skips ~14 unrelated
  ones). The explorer is now scope-sensitive: a focused prompt ("review
  just the tool_choice change") read 6 files (project_tree -> 4 targeted
  search_project -> 6 reads) vs 19 for the broad one — thinking lets it
  scale effort to scope, which the non-thinking explorer couldn't.
  Since then: cap 25->100 (it's a runaway backstop, not a ration); the
  cap-hit now winds down to a report (forced tool_choice:"none" + a
  per-turn budget note) instead of erroring and discarding the load; and a
  `read_files` batch tool so a working set loads in one round-trip rather
  than one read per turn. Still open: (b) code-side repeat-detection (skip
  duplicate tool calls) — `read_files` reduces the round-trip pressure that
  motivated it but doesn't dedupe.

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

