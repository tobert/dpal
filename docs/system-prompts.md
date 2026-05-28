# System Prompts

dpal ships two built-in system prompts, both in `cmd/dpal/prompt.go`:

- **`defaultSystemPrompt`** — sent to the synthesizer in two-phase
  `consult_deepseek`, and to `consult_deepseek_oneshot`. Tuned for
  V4-Pro with thinking mode on. Carries **no** tool-use prose; the API's
  `tools` parameter conveys availability when oneshot attaches tools, and
  the synthesizer phase passes `tools=nil`.
- **`defaultExplorerSystemPrompt`** — sent to the explorer in two-phase
  `consult_deepseek`. Tuned for V4-Flash with thinking off. Describes
  the three sandbox tools and a fixed stopping condition.

Per-call `system_prompt` / `explorer_system_prompt` overrides win over
these. CLI flags and config-file prompts compose on top via
`composeSystemPrompt` (see `cmd/dpal/config.go`).

## How these were tuned

Both prompts were drafted by V4-Pro itself in a self-review session
through `consult_deepseek` with `disable_explore=true`. The session
gave Pro the full text of each prompt, the architecture context (what
gets `tools=nil`, what doesn't, who answers, who loads), and asked
for a diagnostic before a rewrite.

The first response was its own data point: Pro emitted a
`<tool_call>read_file</tool_call>` block into a tools=nil call. The
prompt had told it to use exploration tools that the synthesizer phase
can't reach, and the model dutifully tried. That live failure drove the
biggest single change — dropping the tool block from the shared prompt.

## Principles that emerged

Preserve these unless a future round of testing falsifies them:

- **Don't describe tools in a system prompt for a call that won't have
  them.** The API's `tools` parameter is the source of truth for tool
  availability. Prose tool descriptions in the system prompt are a V3-era
  pattern that misfires on V4 when the phase doesn't actually have tools.
- **Positive stopping framing beats negated failure patterns.** "Early
  stops are good stops" and a self-check question ("can the answering
  model answer from what I've loaded?") avoid the white-bear effect of
  "Do NOT re-read", "Do NOT loop", etc.
- **Reinforce hard bounds three ways.** The explorer's search policy
  states the same rule as (1) a numeric limit, (2) named pivot actions,
  (3) an epistemic principle. Different angles hit different
  interpretive paths and make the rule harder to squint past.
- **No threats in the budget reminder.** "Hitting the cap aborts both
  phases with an error" induces anxiety-driven decision paralysis or
  rushed abandonment in a smaller model. State the constraint, drop
  the consequence.
- **Anchor abstract instructions with concrete examples.** The synth's
  thinking-mode section names what belongs in reasoning ("weighing
  interpretations, tracing logic paths, verifying consistency") rather
  than relying on the model to infer the boundary.

## Running another tuning round

When the prompts feel stale or a real-world failure mode shows up:

1. Pick a session id (`prompt-tuning-$(date +%s)` works).
2. Call `consult_deepseek` with `disable_explore=true` and paste the
   current text of both prompts verbatim, with the architecture context
   (which phase gets which prompt, what `tools` is set to in each).
3. Ask for a **diagnostic pass first**, not a rewrite. Naming what's
   noise vs what's load-bearing surfaces disagreements you can resolve
   before any text changes.
4. Push back where the diagnostic feels off. The model is being asked
   to critique a prompt it's running under; deference is a real risk.
5. Only then ask for drafts. Feed any observed failure (e.g., this
   round's misfired tool call) back as ground truth — "this happened,
   account for it."
6. Ship as separate commits, one per prompt, with Pro co-authored.

If a future round changes a prompt in a way that contradicts one of
the principles above, update this file in the same commit. The
principles aren't sacred — they're load-bearing until the next
contradicting evidence.
