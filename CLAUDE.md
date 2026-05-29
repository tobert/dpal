# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What dpal is

A stdio MCP server fronting DeepSeek V4. Sibling of `gpal` (Gemini) and `cpal` (Claude) — one "pal" per provider, each one a thin shim whose job is to expose that provider's distinctive features directly to an MCP client rather than flatten them into a lowest-common-denominator chat call.

For DeepSeek specifically, that means:

- **V4 thinking-mode `reasoning_content`** surfaced as a separate output field, not concatenated into `content`.
- **Function calling** wired to a sandboxed filesystem explorer so the explorer model can read the project on its own.
- **Cache hit/miss tokens** surfaced as OTel span attributes (`deepseek.usage.cache_hit_tokens` / `cache_miss_tokens`) alongside the standard `gen_ai.*` set.

If a feature is provider-specific, dpal's job is to pass it through, not hide it.

## Conventions worth knowing

- **Sessions: cap-based LRU, no TTL.** Amy keeps sessions open for days; eviction is capacity-driven only. Don't reintroduce time-based expiration.
- **`reasoning_content` IS replayed in V4 sessions.** When a thinking-mode model emits `reasoning_content`, it lands in `sess.messages` on the assistant turn and gets sent back in subsequent requests. V4 requires this in multi-turn-with-tools conversations. (This is the OPPOSITE of the old R1 rule, which forbade replay — if you're reading old issues or PRs that say "never replay reasoning_content", that was V3/R1-era guidance and no longer applies.) `sess.reasoning` still exists as a parallel array for `dpal://session/{id}` display; it duplicates data and should be refactored away eventually, but keep it for now.
- **Default models are explicit V4 IDs.** `defaultModel = ModelV4Pro` (`deepseek-v4-pro`), `defaultExplorerModel = ModelV4Flash` (`deepseek-v4-flash`). Legacy `deepseek-chat`/`deepseek-reasoner` aliases retire 2026-07-24 — do not reintroduce them as defaults.
- **Thinking mode: on for both phases.** The synthesizer call uses `EnableThinking: true` (V4-Pro is built for it). The explorer call now uses `EnableThinking: true` too: since `project_tree`, choosing which files to load is a reasoning task, not pattern-match-and-load, and a non-thinking explorer defaults to reading everything — a little thinking is far cheaper than the unnecessary whole-file reads it prevents. The explorer's `reasoning_content` is stripped before the synth call (it's flash's private file-selection deliberation, noise for V4-Pro), so it never enters the synthesizer's context. This **reverses** the earlier "explorer off" rule, which predated `project_tree` — if you read old commits/issues saying the explorer never thinks, that guidance is retired.
- **`consult_deepseek` is two-phase by default.** Explorer (`deepseek-v4-flash`, non-thinking) runs the tool loop, synthesizer (`deepseek-v4-pro`, thinking) writes the answer. Both upstream calls are billed; the explorer's own text answer is stripped before the synthesizer sees the history. `consult_deepseek_oneshot` is the non-agentic escape hatch (no tools, no explore, one call). Don't sneak an explorer onto the oneshot path — that's the whole reason it exists as a separate tool.
- **`docs/issues.md` is the live work tracker.** Skim it before proposing new work; delete entries when they ship rather than marking them done.
- **e2e tests are opt-in:** `DEEPSEEK_API_KEY=... go test -tags=e2e ./internal/server/`. Plain `go test ./...` stays offline.
