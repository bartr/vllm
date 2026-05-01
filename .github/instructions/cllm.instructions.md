---
applyTo: "cllm/**"
description: cllm Go server architecture invariants and code conventions. Loaded automatically when editing files under cllm/.
---

# cllm code conventions (load before editing cllm/**)

## Hard invariants (do not break)
- **Admission ordering**: cost gate FIRST, then KV gate. Never reorder.
- **KV backward-compat**: `max_kv_tokens=0` → `Node.KV == nil` → byte-for-byte legacy cost-only path. KV directives (`kv-cost`, `no-kv`) must be no-ops on KV-disabled nodes.
- **KVCost sentinel**: `KVCost = -1` means "skip KV gate for this request" (set by `:dsl no-kv`). Don't conflate with 0.
- **Routing key**: unpinned requests use `combined_load = max(cost_load, kv_load × kv_weight)`.
- **First-wins DSL precedence**: bare macros (`no-kv`, `no-cache`, `no-delay`, `no-phase`) claim only classes that haven't been overridden. Mirror the `no-delay` structure exactly when adding new bare macros: `applied bool` flag, claim each class only if free.
- **Phase gate placement**: TTFT-budget gate runs AFTER `cache.Get` hit but BEFORE `markCacheHit(true)`. Cache miss / no-cache / re-cache all bypass it by construction.
- **Per-request DSL > class config > legacy**. Always.

## Code conventions
- DSL parser: presence flag (e.g. `dslInitialTokensSet`) is required when 0 is a meaningful override. Rate-shaped fields (`>0 == set`) don't need a flag because parser validation rejects <1.
- New `kv_estimator`-style features are gated on `MaxKVTokens > 0` in `loader.go`. Don't allocate the estimator unconditionally.
- New `cllm_*_total{class,...}` counters MUST add a case in `dslDirectiveFamily` if the directive is non-numeric, otherwise it buckets as `"other"`.
- Smoke-fixture rule: prompts that don't specifically exercise the KV gate must pin `node=default` (KV-disabled). Unpinned routing + estimator inflation on `kv-node` causes spurious `kv_oversize` rejections.
- Streaming: every `http.ResponseWriter` wrapper must implement its own `Flush()` forwarding to the underlying writer. Type assertions don't traverse embedded fields. See `loggingResponseWriter.Flush()` in `cllm/internal/httpapi/handler.go`.
- Tests: never re-pin via `Acquire` between releases in budget tests. Each `Release(1)` immediately promotes the next waiter inside `promoteLocked` while holding the lock; calling `Acquire` afterward queues behind remaining waiters and deadlocks.

## Version touchpoints (must move together on every release)
1. `cllm/internal/buildinfo/version.go` — DEFINITIVE.
2. `cllm/Makefile` — `IMAGE ?= cllm:X.Y.Z`.
3. `clusters/z01/cllm/deployment.yaml` — pod `image: cllm:X.Y.Z`.
4. `cllm/README.md` — 4 occurrences.

Verify: `grep -rn 'cllm:X\.Y\.Z\|var Version' cllm clusters` returns only the new version.

## Edit hygiene
- Use `multi_replace_string_in_file` for >1 edit in a single response.
- Don't add docstrings, comments, or type changes to lines you didn't otherwise modify.
- Don't refactor unrelated code under the cover of a feature change.
- Combine code edits with their corresponding doc edit (`system-design.md`, `cllm/docs/*.md`) in one turn.

## Prefer-grep rule
For symbol lookups in this codebase, prefer `grep_search`/`file_search` over `semantic_search`. Reserve `semantic_search` for unknown territory.
