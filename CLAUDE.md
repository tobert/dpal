# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What dpal is

A stdio MCP server fronting DeepSeek. Sibling of `gpal` (Gemini) and `cpal` (Claude) — one "pal" per provider, each one a thin shim whose job is to expose that provider's distinctive features directly to an MCP client rather than flatten them into a lowest-common-denominator chat call.

For DeepSeek specifically, that means:

- **R1's `reasoning_content` channel** surfaced as a separate output field, not concatenated into `content`.
- **Function calling** wired to a sandboxed filesystem explorer so R1 can read the project on its own mid-reasoning.
- **Cache hit/miss tokens** surfaced as OTel span attributes (`deepseek.usage.cache_hit_tokens` / `cache_miss_tokens`) alongside the standard `gen_ai.*` set.

If a feature is provider-specific, dpal's job is to pass it through, not hide it.

## Conventions worth knowing

- **Sessions: cap-based LRU, no TTL.** Amy keeps sessions open for days; eviction is capacity-driven only. Don't reintroduce time-based expiration.
- **`reasoning_content` is never replayed.** Surface it on the tool output, but `sess.messages` stores `Content` only — that matches DeepSeek's own guidance and is intentional. Consequence: `dpal://session/{id}` is lossy for R1 sessions (tracked in `docs/issues.md`).
- **`docs/issues.md` is the live work tracker.** Skim it before proposing new work; delete entries when they ship rather than marking them done.
- **e2e tests are opt-in:** `DEEPSEEK_API_KEY=... go test -tags=e2e ./internal/server/`. Plain `go test ./...` stays offline.
