# KV cache and memory pressure modeling

Status: **Phases 1+2+3+4 shipped (cllm 0.10.x); Phase 5 owned by §14 item 9**
Owners: bartr
Future Work item: §14, item 1 in [system-design.md](system-design.md)
Companion docs: [spec-cost-admission.md](spec-cost-admission.md), [spec-n-multi-node.md](spec-n-multi-node.md)

---

## 1. Headline

Add a **second admission axis** per node: in addition to the existing token-cost
budget (`max_tokens_in_flight`), each node tracks a **KV-cache occupancy
budget** (`max_kv_tokens`). KV occupancy is estimated per request as
`prompt_tokens + projected_completion_tokens`, charged on admission, released
on completion or cancel. When KV occupancy crosses configurable thresholds, the
node's `f(load)` curve is amplified and, past a hard ceiling, admission is
rejected with a new `429 kv_pressure` reason.

This closes the most prominent §6.6 honest-gap call-out: today the synthetic
path scales the throughput envelope but does not reproduce the **memory-bound
failure mode** that dominates real vLLM under long-context, high-concurrency
workloads.

The model is intentionally a coarse approximation, not a kernel-level
simulator. It reproduces the *shape* of KV-pressure-driven contention (TTFT
inflation, throughput plateau, evictions/preemptions) using one new
admission axis and one new degradation term.

---

## 2. Why now

The shipped multi-node fleet (§5.2, §6.7) reproduces queueing, routing, and
fairness under cost-based admission — but it has a single failure mode:
"too many tokens in flight, so degrade." Real vLLM has a second, often
*dominant* failure mode: "KV cache full, so preempt or reject." The
two failure modes interact differently:

| Pressure | Triggered by | Remedy in production |
|---|---|---|
| Compute (`max_tokens_in_flight`) | Many concurrent short requests | Throughput plateau, `f(load)` slowdown |
| Memory (KV cache) | Few concurrent **long-context** requests | Preemption, eviction, OOM, hard rejects |

Without a KV axis, cLLM cannot reproduce the canonical experiment of "two
4k-context requests degrade more than fifty 200-token requests at the same
admitted token cost." That experiment is what makes capacity scaling claims
defensible.

---

## 3. Goals

1. **A second admission axis per node.** KV occupancy is independent of
   `max_tokens_in_flight`: a workload can be compute-bound, memory-bound, or
   both.
2. **Realistic shape.** KV-pressure degradation amplifies `f(load)`; KV
   exhaustion produces hard rejection, not silent slowdown.
3. **Calibratable.** A real-vLLM run via `:dsl no-cache` produces measured KV
   utilization; the synthetic envelope reproduces the same ratio at the same
   workload.
4. **Backward compatible.** A node with `max_kv_tokens` unset (or `0`) has KV
   modeling disabled — single-node default deployments see zero behavior
   change.
5. **Observable.** Per-node KV occupancy gauges, KV-pressure rejection counters,
   and a KV-pressure component on the `/config` and dashboard surfaces.

## 3.1 Non-goals (this iteration)

- No per-token KV-bytes calibration. The unit is "KV tokens" — the model's KV
  cost per token is opaque to cLLM.
- No PagedAttention block accounting. cLLM models occupancy, not page
  fragmentation.
- No preemption / cancel-and-requeue. KV pressure produces *slowdown* and
  *rejection* in v1; preemption is §14 item 9 (streaming admission preemption)
  and stays separate.
- No content-aware KV cost. Two 1k-token prompts charge the same KV cost
  regardless of topic. Content-dependent decode variance is §14 item 10.
- No global KV pool. KV is per-node; a node models a vLLM instance, and KV
  cache does not migrate across instances. Same justification as per-node
  FIFO (§3 of [spec-n-multi-node.md](spec-n-multi-node.md)).

---

## 4. Cost model

### 4.1 KV cost estimate

KV cost is charged in **KV tokens** — a unit, not bytes — so calibration
maps it onto whatever the underlying model + GPU combination produces:

```text
kv_cost = prompt_tokens + projected_completion_tokens
```

where `projected_completion_tokens` is the same `min(max_tokens, p95)` term the
existing token-cost estimator already produces. This deliberately mirrors the
admission cost so the two axes share their warm-up behavior, fallback chain
(§6.5 of system-design.md), and refund semantics. The numeric value will
*usually* be the same as the admission cost; the two diverge only when a
future change introduces a separate KV-specific p95 (out of scope here).

**Why not separate the two estimators?** Token-cost models compute
parallelism; KV-cost models memory residency. For prompt-then-decode
workloads they track each other almost perfectly. Splitting them adds
configuration surface without a measurable benefit until KV-aware decoding
(speculative, prefix-cache hits) is modeled — both are explicit non-goals
above.

### 4.1bis KV-aware estimator (Phase 4, cllm 0.10.x)

Phase 4 separates the two estimator streams without changing the v1
contract. Each KV-modeled node gains a `KVEstimator` that observes
completion-token counts on the same successful-downstream signal as the
compute estimator but feeds an independent p95. The handler refines
`cost.KVCost` *after* routing — the router still uses the legacy compute
cost — using

```text
KVCost = PromptTokens + clamp(int(kv_p95 * kv_completion_factor), 0, max_tokens)
```

`kv_completion_factor` is a per-class / per-node knob (default 1.0; clamped
to (0, 4.0]) and the operator's calibration lever for hardware where peak
KV residency runs below prompt+completion. A factor < 1.0 amortizes the
KV charge for prefix-cache or short-residency decode without changing
the cost gate. Cold or absent estimators fall back to `KVCost = TotalCost`,
so the backward-compat contract from §10 is preserved byte-for-byte.

### 4.2 KV pressure curve

The existing `f(load)` from §6.4 is driven by the cost-based fill ratio
`inFlight / capacity`. KV pressure introduces a second fill ratio:

```text
kv_load = kv_in_flight / max_kv_tokens
```

`f(load)` is replaced by **`f(combined_load)`** where:

```text
combined_load = max(cost_load, kv_load × kv_weight)
```

`kv_weight` defaults to `1.0` (KV pressure dominates equally with compute
pressure once both pass the 10% threshold). Operators may set
`kv_weight > 1.0` to model GPU classes where KV is the binding constraint
sooner than compute, e.g. small-VRAM A10s.

The existing degradation curve shape is unchanged — same 10% deadband, same
linear ramp to `max_degradation`. Only the fill-ratio input changes, so the
math, defaults, and live-config knobs (`max_degradation`,
`computed_degradation_percentage`) remain identical. **The output of `/config`
keeps its current shape; only the source of the fill ratio gains a second
contributor.**

### 4.3 KV admission

Admission becomes a three-step gate per request, layered on top of the existing
two-step gate (§6.3):

1. Per-tenant token-bucket — unchanged.
2. **Per-node token-cost budget** — unchanged (`max_tokens_in_flight`).
3. **Per-node KV budget** — *new*: `kv_in_flight + kv_cost ≤ max_kv_tokens`.

Steps 2 and 3 share the same per-node FIFO and are charged atomically: a
request that fits step 2 but not step 3 waits in the same FIFO it would have
waited in for compute pressure. Refund semantics are also identical — a
node-capacity rejection refunds the tenant bucket, an in-flight cancel
returns both `cost` and `kv_cost` to the node.

A request whose `kv_cost` alone exceeds `max_kv_tokens` is rejected
immediately with `429 kv_oversize` (matches the existing
`429 over_capacity` "oversize" path).

### 4.4 Configuration

New per-node and per-class fields in `configs/nodes.yaml`:

```yaml
nodes:
  h100-0:
    class: H100
    max_tokens_in_flight: 65536
    max_tokens_per_second: 96
    max_waiting_requests: 200
    max_kv_tokens: 131072        # NEW: KV occupancy ceiling, in tokens
    kv_weight: 1.0               # NEW: weight in combined_load (default 1.0)

classes:
  H100:
    max_degradation: 15
    kv_weight: 1.0               # class default; node may override
```

Single-node default deployments gain a corresponding flag /
environment-variable / `/config` knob:

| Flag | Env | `/config` key | Default |
|---|---|---|---|
| `--max-kv-tokens` | `CACHE_MAX_KV_TOKENS` | `max_kv_tokens` | `0` (disabled) |
| `--kv-weight` | `CACHE_KV_WEIGHT` | `kv_weight` | `1.0` |

`max_kv_tokens = 0` disables the entire KV axis: no charge, no gate, no
metrics — identical to today's behavior. This is the backward-compat
contract.

---

## 5. Code structure

### 5.1 New type in `internal/node`

`Node.Capacity` gains two fields, mirroring the existing capacity story:

```go
type Capacity struct {
    MaxTokensInFlight  int64
    MaxTokensPerSecond int
    MaxWaitingRequests int

    MaxKVTokens int64   // NEW: 0 = KV modeling disabled
    KVWeight    float64 // NEW: combined-load weight, default 1.0
}
```

The existing `*TokenBudget` (which gates `max_tokens_in_flight`) is joined by
a sibling `*KVBudget`:

```go
type KVBudget struct {
    capacity int64
    inFlight atomic.Int64
    // No FIFO — the existing TokenBudget FIFO orders waiters.
}

func (b *KVBudget) TryCharge(n int64) bool
func (b *KVBudget) Release(n int64)
func (b *KVBudget) Stats() (capacity, inFlight int64)
```

`KVBudget` is intentionally **not a semaphore**. The existing per-node FIFO
on `TokenBudget` orders waiters; `KVBudget` is just a checked-counter that
admission consults. This avoids two-mutex deadlock (compute frees, but KV is
still tight, so the next waiter would still block — a deadlock if KV had its
own waiter list pointing back at compute slots).

### 5.2 Admission flow

`requestScheduler.AcquireOnNode` becomes:

```go
1. acquire on n.Budget (compute)            // existing
2. if n.KV != nil:
     if !n.KV.TryCharge(kvCost):
         n.Budget.Release(cost)             // unwind compute
         return 429 kv_pressure
3. defer n.Budget.Release(cost)
   defer n.KV.Release(kvCost)
```

Step 2 *can* fail under the same FIFO that admitted step 1 because compute
freed first. The unwind is bounded — at most one level deep — and the
released compute slot wakes the next FIFO waiter so the system stays
work-conserving.

### 5.3 Combined load

`degradationMetricsFor` (handler.go) gains a `kvLoad` argument:

```go
func degradationMetricsFor(
    capacity, inFlight int64,
    kvCapacity, kvInFlight int64,
    kvWeight float64,
    baseTokensPerSecond, maxDegradation int,
) (computedDegradationPercentage, effectiveTokensPerSecond float64)
```

The function takes `max(cost_load, kv_load × kv_weight)` and feeds it into the
existing piecewise-linear curve. When `kvCapacity == 0` the function is
mathematically equivalent to today's call site, so the single-node
default-fleet behavior is unchanged.

### 5.4 Cancel and refund

The existing cancel path (§6.3, "Releasing capacity is cancel-aware") releases
`cost` to `Node.Budget`. The new path also releases `kvCost` to `Node.KV`.
Both are deferred and idempotent.

---

## 6. DSL extensions

One new directive class:

```text
:dsl kv-cost=N           # override the KV charge for this request
:dsl kv-cost=A:B         # range, drawn once
```

`kv-cost` claims a new directive class (first-wins, like every other class).
It does **not** affect cache identity (the cache key is still derived from the
cleaned prompt — see §12.6 of system-design.md). The directive is a tool for
fault injection: pin a 200-token prompt to a 16k-KV cost to simulate a
long-context request without paying the prompt-tokenization cost.

A DSL macro `no-kv` zeroes the request's KV charge for testing the
no-KV-pressure baseline against the same prompts.

### 6.1 Profiles

Three new profiles in `configs/profiles.yaml`:

| Name | Bundle | Effect |
|---|---|---|
| `kv-light` | `kv-cost=128` | Short-context probe |
| `kv-heavy` | `kv-cost=4096:8192` | Long-context probe |
| `kv-stress` | `kv-cost=12288:16384` | Pathological-context probe |

These compose with the existing `tps-*` family so a single benchmark can sweep
KV occupancy at fixed pacing.

---

## 7. Metrics

New per-node series, gated on the same `len(nodes) > 1` rule the existing
fleet collector uses (single-node deployments stay quiet to keep cardinality
predictable):

```text
cllm_node_kv_tokens_in_flight{node, class}
cllm_node_max_kv_tokens{node, class}
cllm_node_kv_admissions_total{node, class, result}
        # result ∈ admitted | rejected_pressure | rejected_oversize
```

A new fill-ratio derived series surfaces the combined load that drives `f(load)`
without adding a separate gauge:

```text
cllm_node_combined_load{node, class}
        # max(cost_load, kv_load × kv_weight)
```

The existing `cllm_tenant_rejections_total{reason}` family gains two new
`reason` values:

```text
reason ∈ tenant_rate | over_capacity | kv_pressure | kv_oversize
```

**Cardinality budget.** Two new gauges + one new counter, each at `node ×
class`. Nodes are configured (not user-supplied), so cardinality is bounded.
Adding `kv_pressure` / `kv_oversize` to `cllm_tenant_rejections_total{reason}`
expands its enum from 2 to 4 values. Comfortable.

---

## 8. Dashboards

`cllm-overview` gains one row at the bottom of the multi-node fleet block:

- **KV occupancy by node** — `cllm_node_kv_tokens_in_flight /
  cllm_node_max_kv_tokens`, stacked by node.
- **KV admission rate** — `cllm_node_kv_admissions_total` partitioned by
  result.
- **KV pressure rejection rate** — slice of
  `cllm_tenant_rejections_total{reason="kv_pressure"|"kv_oversize"}`.
- **Combined load** — `cllm_node_combined_load`, one line per node, with
  the existing fill-ratio panel above for visual diff.

`vllm-overview` already shows real KV occupancy from the upstream `/metrics`;
the new cLLM panels sit alongside for direct ratio comparison during
calibration.

`gpu-overview` is unchanged (KV pressure is a memory-residency story, not a
DCGM metric).

---

## 9. Calibration

The same `:dsl no-cache` calibration loop that anchors the throughput envelope
(§6.6, §11.2) anchors `max_kv_tokens`:

1. Run a long-context benchmark through `:dsl no-cache` against vLLM. The
   `vllm-overview` `gpu_cache_usage_perc` panel shows the actual KV
   utilization on the calibration GPU.
2. Pick `max_kv_tokens` so the synthetic node hits the same KV-pressure
   *fill ratio* as the calibration run at the same workload — typically
   `vllm.max_num_batched_tokens × vllm.gpu_memory_utilization × scaling_factor`,
   but the calibration step measures it directly rather than computing it.
3. Optionally tune `kv_weight` per class so a small-VRAM A10 hits KV pressure
   before compute pressure, and an H100 with abundant VRAM stays
   compute-bound until very high KV occupancy.

Because the calibration is a single benchmark run, every claim about KV
behavior is reproducible at the cost of one GPU hour, the same as the
throughput-envelope calibration.

---

## 10. Backward compatibility

The change is structured so existing deployments see **zero diff**:

1. `max_kv_tokens` defaults to `0`. Nodes built from the synthesized default
   fleet (no `nodes.yaml`) have it `0`. No `KVBudget` is constructed.
2. `degradationMetricsFor` with `kvCapacity == 0` short-circuits to today's
   `cost_load` math, byte-for-byte.
3. Admission with `n.KV == nil` skips step 2 of §5.2 entirely.
4. The new metric series are gated on the same `len(nodes) > 1` rule used by
   the existing `cllm_node_*` family. Single-node default deployments emit
   nothing new.
5. The `kv_pressure` / `kv_oversize` reasons are *additive* to
   `cllm_tenant_rejections_total{reason}`; existing alerts and dashboards
   that filter on `tenant_rate` / `over_capacity` are unaffected.
6. The DSL `kv-cost=` directive is opt-in. A request that omits it, in a
   deployment with `max_kv_tokens = 0`, behaves identically to today.

---

## 11. Phasing

Three phases, each shippable on its own.

### Phase 1 — KV budget and admission
- Add `MaxKVTokens` / `KVWeight` to `Capacity`.
- Add `KVBudget` type with `TryCharge` / `Release` / `Stats`.
- Wire `AcquireOnNode` to consult `KVBudget` after the existing compute gate.
- Add `kv_pressure` / `kv_oversize` rejection reasons to
  `cllm_tenant_rejections_total`.
- Add unit tests: KV exhaustion, oversize, refund, cancel.
- **Deliverable:** KV admission works; degradation curve unchanged. A
  benchmark with `kv_cost > max_kv_tokens` produces 429s; everything else
  behaves as today.

### Phase 2 — Combined load and metrics
- Replace `cost_load` input to `degradationMetricsFor` with `combined_load`.
- Add `cllm_node_kv_tokens_in_flight`, `cllm_node_max_kv_tokens`,
  `cllm_node_kv_admissions_total`, `cllm_node_combined_load`.
- Update `cllm-overview` dashboard with the four new panels.
- Add integration test: long-context concurrency degrades faster than
  short-context concurrency at the same admitted token cost.
- **Deliverable:** the canonical "two 4k requests > fifty 200-token
  requests" experiment reproduces on the synthetic path.

### Phase 3 — DSL and profiles
- Add `kv-cost=` directive class and `no-kv` macro.
- Add `kv-light` / `kv-heavy` / `kv-stress` profiles.
- Document calibration loop in §11.2 of `system-design.md` and update
  `talking-points.md` honest-limits.
- **Deliverable:** mixed-context benchmarks land in one prompt set.

### Phase 4 — KV-aware completion estimator (cllm 0.10.x, shipped)
Closes the §4.1 / §12 Q2 honest-gap that `KVCost = TotalCost` ignores
prefix-cache amortization, mid-stream KV release, and any future per-node
KV calibration.

- Each KV-modeled node carries an independent `KVEstimator
  *CompletionEstimator` (constructed by the loader iff `MaxKVTokens > 0`).
  Same window/warm-up shape as the compute estimator so the two converge
  in lock-step when no factor is supplied.
- New `EstimateCostWithKV(prompt, max, costEst, kvEst, kvFactor)` derives
  `KVCost = PromptTokens + clamp(int(kvP95 × factor), 0, maxTokens)` once
  the KV estimator is warm; cold or nil estimator falls back to
  `TotalCost` byte-for-byte.
- New per-class / per-node config field `kv_completion_factor` (default
  1.0; clamped to (0, 4.0]) is the operator's calibration knob.
  Setting `< 1.0` models hardware where peak KV residency runs below
  prompt+completion (prefix cache, short-residency decode).
- The handler refines `cost.KVCost` *after* routing using the routed
  node's `KVEstimator`, but only when neither `:dsl kv-cost=` nor
  `:dsl no-kv` already pinned it. Pre-routing cost estimation is
  unchanged so the router still uses the legacy compute cost as input.
- The KV estimator is fed on the same successful-downstream signal as
  the compute estimator (`emitCompleted` in `handler.go`).
- New gauge `cllm_node_kv_estimator_p95{node, class}` is emitted only
  when the estimator is warm; cold nodes stay quiet so dashboards do
  not plot 0 as a real reading.
- **Deliverable:** the calibration loop becomes "aim a long-prompt
  benchmark at vLLM, watch `gpu_cache_usage_perc` and
  `cllm_node_kv_estimator_p95` converge, then tune `kv_completion_factor`
  until the synthetic node tracks the real one."

### Phase 5 — Preemption-on-pressure (out of scope for item 1)
Cooperatively cancel-and-requeue an in-flight long-context request when
KV pressure exceeds a hard ceiling. **Owned by §14 item 9 (streaming
admission preemption), not item 1.** Merging the two stories blurs the
design boundary; item 9 is the canonical home and Phase 4 of this doc
is the close-out for item 1.

---

## 12. Open questions for review

1. **Combined-load function.** `max(cost_load, kv_load × kv_weight)` is
   simple and matches the "first axis to bind, binds" intuition. An additive
   form (`min(1, cost_load + kv_load × kv_weight)`) couples the two axes more
   tightly — closer to real vLLM where compute and memory pressure compound.
   Default proposal: `max`. Configurable: not yet.
2. **Single estimator vs separate KV estimator.** Proposal in §4.1: one
   estimator. Risk: a deployment with KV-aware decoding (prefix cache hits,
   speculative decode) where KV cost decouples from token cost will be
   inaccurate. Mitigation: `kv-cost=` DSL override lets benchmarks pin the
   value; a future per-node KV estimator can be added without an API break.
3. **`429 kv_pressure` vs `429 over_capacity`.** Splitting them aids
   debugging but adds an enum value to a metric label. Worth the cardinality
   cost? Default proposal: yes, because operators need to distinguish "more
   GPUs would help" from "smaller-context workloads would help."
4. **Per-class default `max_kv_tokens`.** Should classes carry a default
   that nodes inherit, mirroring `max_degradation`? Default proposal: yes
   (consistent with §5 of `spec-n-multi-node.md`).
5. **Cancel-on-KV-pressure for streaming requests.** Today admitted requests
   run to completion. Should KV pressure trigger a cooperative cancel of an
   in-flight long-context request to save the fleet? Default proposal: no —
   that's §14 item 9 (streaming admission preemption), and merging the two
   stories blurs the design boundary.

---

## 13. Out of scope (revisit later)

- **PagedAttention block accounting.** cLLM models occupancy, not page
  fragmentation. A node that "OOMs at 95% occupancy due to fragmentation"
  is approximated by lowering `max_kv_tokens` to 95% of the calibrated
  ceiling. Real fragmentation modeling is a distinct future-work item.
- **Prefix-cache hit modeling.** vLLM amortizes KV cost when prompts share
  prefixes. cLLM does not, so a benchmark with shared prefixes will
  *overcharge* KV in the synthetic path. Workaround: `kv-cost=` DSL
  override per request.
- **Speculative-decode KV residency.** Same story; a future KV estimator
  fork can model it without breaking the v1 admission contract.
- **Cross-tenant KV fairness.** Today KV is a per-node admission gate; it
  does not enforce per-tenant KV quotas. Tenant-level KV fairness is a
  natural extension of tenant rate/burst (§6.5) and is left for a follow-up.
