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
- **Thinking mode: on for both phases.** The synthesizer call uses `EnableThinking: true` (V4-Pro is built for it). The explorer call uses `EnableThinking: true` too: since `project_tree`, choosing which files to load (and how to curate them) is a reasoning task, not pattern-match-and-load, and a non-thinking explorer defaults to reading everything. This **reverses** the earlier "explorer off" rule, which predated `project_tree` — if you read old commits/issues saying the explorer never thinks, that guidance is retired.
- **`consult_deepseek` hands the synthesizer a REPORT, not the raw transcript.** Two phases, both billed. Explorer (`deepseek-v4-flash`, thinking) runs the tool loop, then writes a *curated report* of its findings — its final text turn (`extractExplorerReport`). That report, not the raw tool-call transcript, is what crosses to the synthesizer: it's folded into the synth's user prompt as reference context, while the tool dump and the explorer's `reasoning_content` are discarded (~7-15% of the bytes survive). This **reverses** the old "strip the explorer's answer, keep the transcript" rule — if you read commits/issues mentioning `stripExplorerSynthesis`, that's retired. **The synthesizer also gets the explorer's tools** (auto `tool_choice`) when the explore phase ran, so V4-Pro can `read_file`/`search_project` to fetch a precise span the report pointed to but didn't quote — the report is primary, the tools are the fallback. `read_file` takes optional `start_line`/`end_line` for precise windows that reach past the head byte cap. With `disable_explore` there's no explorer pass, so the synth runs direct with no tools and `tool_choice:"none"`. `consult_deepseek_oneshot` is the non-agentic escape hatch (no tools, no explore, one call) — don't sneak an explorer onto it.
- **Sessions persist lean `[user-prompt, synth-answer]` pairs.** The exploration report and any synth-phase tool fetches are ephemeral context for that turn's synth call only — they are never stored. Each turn re-explores fresh, so old reports would just be stale bloat. (This replaced the earlier "persist the tool exchanges" model, which only existed because the raw transcript *was* the hand-off.)
- **Working notes — delegate side quests, keep the narrative.** **`docs/issues.md`** is the live work tracker — skim it before proposing new work; record out-of-scope side quests here before moving on, and **delete entries when they ship** rather than marking them done. `docs/devlog.md` is a durable narrative from the agent's perspective. Write your story there.
- **e2e tests are opt-in:** `DEEPSEEK_API_KEY=... go test -tags=e2e ./internal/server/`. Plain `go test ./...` stays offline.

## Commit style

Commits explain **why, not what** — the diff already shows what changed. Write the body as a short summary of the decisions behind the change, **drawn from the working conversation with the user**: what we chose, what we rejected, and why. A few sentences of reasoning beat a list of files.

- **Subject:** imperative — the decision or outcome, not "update X".
- **Body:** the reasoning and tradeoffs; cite a decision's source when it matters.
- Set a `Co-Authored-By:` trailer crediting the model that did the work.
