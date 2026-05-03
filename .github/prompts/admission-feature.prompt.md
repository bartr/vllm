---
mode: agent
description: Add a new per-class admission gate to cllm (e.g., max_X_ms or per-class budget). Encodes the proven pattern from class_queue_timeout, priority, and class_ttft_budget. Use when the user says "add a class-level gate", "new admission axis", "add max-* budget", or names a per-class limit.
---

# Add a class-level admission gate

Same shape as `max_queue_ms` (14B), `priority` (14C), `max_ttft_ms` (item 13 follow-on). Follow this exact sequence — deviating is how the regression traps in `docs/release-process.md` got created.

## Inputs (ask up-front)
1. **Field name** on `ClassSpec`/`ClassConfig` (snake_case YAML, CamelCase Go).
2. **DSL directive** name (kebab-case, e.g. `max-foo-ms`).
3. **Rejection reason** label for `cllm_tenant_rejections_total{reason=...}`.
4. **Where the gate fires** in `handler.go`:
   - Pre-routing (cost-shaped → next to admission cost)?
   - Post-cache-hit, pre-replay (TTFT-shaped)?
   - Post-routing, pre-acquire (queue-shaped)?
5. **Default value** semantics: 0 means disabled? -1 means disabled? (Pick 0 unless 0 is a meaningful override — then add a `Set` companion flag.)
6. **Predicted-vs-measured**: is this gate predicting a future cost, or enforcing a measured one?

## Implementation checklist (in order)

### 1. Class config
- `cllm/internal/config/classes.go`: add field on `ClassSpec` + `ClassConfig`. Validator rejects negative.
- `cllm/configs/classes.yaml`: add to one or more classes for testing.
- Test: `cllm/internal/config/classes_test.go` — round-trip + validation.

### 2. DSL directive
- `cllm/internal/httpapi/dsl.go`: parse `max-foo-ms=N`. If 0 is a meaningful override of a non-zero class cap, add `dslMaxFooMsSet bool` + `dslMaxFooMsOverride` companion. Negative → reject.
- `dslDirectiveFamily()`: add a case for the new directive name so `dsl_directives_total` doesn't bucket it as `"other"`.
- Test: `cllm/internal/httpapi/dsl_test.go` — covers set/unset/override/negative.

### 3. Resolver (effective value = DSL > class > default)
- New helper, e.g. `resolveMaxFooMs(class, dsl) int`. Mirror `resolvePhaseEnvelope` shape.

### 4. Gate in `handler.go`
- Place per the answer to input #4. Predicted-cost gates use a deterministic helper (no jitter) — see `computePrefillDelayDeterministic`.
- On reject: refund tenant bucket, emit lifecycle `"rejected"` with `reason: "class_foo"`, return 429 with body `"class foo budget\n"`.
- `markCacheHit(false)` if the gate fires inside a cache-hit branch.

### 5. Metrics
- Reuse `cllm_tenant_rejections_total{reason="class_foo"}` (already a label). Update its Help string to mention the new reason.
- If you need a new gauge/counter, mirror `cllm_class_initial_tps_effective{class}` style. Always include the `class` label.

### 6. Smoke fixtures (TWO new prompts in `scripts/smoke-test.yaml`)
- One that PASSES the gate (loose limit). Pin `node=cllm`. Use `no-cache`.
- One that REJECTS (tight limit + provoking conditions). Pin `node=cllm`. Use `no-cache`.
- Append at end of the file; renumber comments accordingly.

### 7. Tests
- Unit: `cllm/internal/httpapi/<feature>_test.go` covering admit, reject, refund, metric increment.
- Integration if it crosses class+tenant boundaries: extend `class_integration_test.go`.

### 8. Dashboard
- `clusters/z01/grafana/dashboards/cllm-overview.json`: extend the rejection-reason regex to include the new reason. Bump dashboard version.

### 9. Docs
- `docs/system-design.md`: add a §6.X subsection describing the gate, default, and bypass conditions.
- `docs/`: new `spec-<feature>.md` if the gate has non-trivial semantics.
- `cllm/configs/classes.yaml`: comment the new field with default + units.

### 10. Repo memory
- Update `/memories/repo/cllm.md` "Architecture invariants" with a one-line entry mirroring the existing class_queue_timeout / priority / max_ttft_ms entries: field name, where it fires, bypass list, metric reason label, default = byte-for-byte legacy.

## Validation gate (BLOCKING before commit)
```sh
cd /home/bartr/vllm/cllm && go test ./...
~/go/bin/ask --files /home/bartr/vllm/scripts/smoke-test.yaml --bench 1
curl -s http://localhost:8088/metrics | grep 'cllm_tenant_rejections_total{.*reason="class_foo"'
```
The smoke run must include both new fixtures (one accept, one reject), and the metric line must appear with a non-zero value after the reject fixture.

## Anti-patterns to refuse
- Don't add the gate before the cost gate. Cost gate is always first.
- Don't make 0 mean "enabled with limit 0" silently. Either 0 = disabled (preferred) or add a `Set` companion flag.
- Don't store the gauge unconditionally — gate it on `class != ""` and on the loader resolving the field.
- Don't skip the smoke fixture pair. The fixture pair is the regression test that catches future drift.
