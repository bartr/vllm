# Multi-Node Simulation Design

**Status:** Design — pre-implementation
**Future Work item:** §14.6 in [system-design.md](system-design.md)
**Companion docs:** [system-design.md](system-design.md), [talking-points.md](talking-points.md)

---

## 1. Headline

A cLLM node models a **vLLM instance**, not a GPU. One cLLM process internally
hosts N heterogeneous nodes, each with its own calibrated capacity envelope,
FIFO waiting queue, admission stock, and optional upstream. **The synthetic
node is a struct, not a service.**

This is a refactor of the existing handler, not a new subsystem. Today's
single-handler behavior becomes the `nodes.len() == 1` case.

---

## 2. Why a Single Process, Not Router + Cluster

Splitting cLLM into a "router" service and a "cluster" service would be
correct if we were building a real fleet. We're not — we're *simulating*
one. The split costs more than it buys:

| Concern | One process, N internal nodes | Split into router + cluster services |
|---|---|---|
| Ops surface | One pod, one config, one binary, one cache library | N+1 pods, network hop, separate or shared-cache problem |
| Calibration | One real-GPU run anchors all envelopes via per-class scaling | Each "cluster" pod needs its own deployment + calibration |
| Observability | Existing three-layer dashboard stack; add `node` label | Two synthetic layers; correlation IDs cross a network boundary |
| Tenant fairness | Tenant buckets stay global, gate before routing | Tenant logic duplicated or has to live above the router |
| Cache identity | One cache, one workload artifact | Shared-cache concurrency bugs, or N caches that drift |
| §13.3 portability | Laptop / CI / air-gapped still work | A laptop cannot host 16 cLLM pods sensibly |

---

## 3. Why Per-Node FIFO

A real fleet has N independent vLLM instances, each with its own scheduler,
KV cache, and waiting queue. A request routed to instance A does **not**
migrate to instance B if B frees capacity. Per-node FIFOs are the realistic
model, not a simplification.

```text
Real fleet (production):
  ingress / router  →  vLLM-A  (queue, KV cache, GPU)
                    →  vLLM-B  (queue, KV cache, GPU)
                    →  vLLM-C  (queue, KV cache, GPU)

cLLM model:
  router            →  Node A  (per-node FIFO, per-node f(load), upstream optional)
                    →  Node B  (per-node FIFO, per-node f(load), upstream optional)
                    →  Node C  (per-node FIFO, per-node f(load), upstream optional)
```

Same shape, same isolation, same failure modes. The "wrong-node-saturates-while-
neighbor-is-idle" failure is a feature, not a bug — it's exactly what makes
load-aware routing matter in production.

A future Option B (global waiting queue with cross-node hand-off) is rejected
for v1: it would only be correct if the real router could re-route already-
queued requests across instances, which is not how real fleets work.

---

## 4. Calibration Generalizes Per-Class

One real-GPU run anchors the envelope for an **entire class**; the same envelope
drives every synthetic node of that class. A fleet of 100 H100s + 50 A10s
reduces to **two** calibration hours, not 150 — the §13.1 GPU-multiplier story
scaled across SKUs.

The host's physical GPU is itself **node 0**: e.g., an NVIDIA RTX 2000 with
`class: rtx-2000`, `upstream: vllm-local`, calibrated at ~30 tps/req. Today's
`:dsl no-cache` ("skip cache, go to upstream") becomes "route to a node that
has an upstream configured." Concurrent multi-target benchmarking (§10.2)
becomes a routing decision rather than a separate flag.

---

## 5. Configuration

New file: `configs/nodes.yaml`. Loaded at startup with `CLLM_NODES_FILE`
override. If absent, the handler builds a single default node from existing
flat config — **zero behavior change for existing deployments.**

```yaml
nodes:
  rtx-2000-0:
    class: rtx-2000
    upstream: http://vllm:8000/v1     # node 0: real GPU, real vLLM
    max_tokens_per_second: 30
    max_tokens_in_flight: 8192
    max_waiting_requests: 100

  h100-0:
    class: H100
    max_tokens_per_second: 96
    max_tokens_in_flight: 65536
    max_waiting_requests: 200

  a10-0:
    class: A10
    max_tokens_per_second: 32
    max_tokens_in_flight: 16384
    max_waiting_requests: 100

  a10-1:
    class: A10
    max_tokens_per_second: 32
    max_tokens_in_flight: 16384
    max_waiting_requests: 100

classes:
  rtx-2000:
    f_load_shape: piecewise_linear
    max_degradation: 10
    prefill_rate_multiplier: 4
  H100:
    f_load_shape: piecewise_linear
    max_degradation: 15
    prefill_rate_multiplier: 12
  A10:
    f_load_shape: piecewise_linear
    max_degradation: 10
    prefill_rate_multiplier: 6

router:
  policy: least-loaded               # class-pinned | least-loaded
  fallback: any                      # any | none
```

**Class semantics.** Classes are templates. A node inherits its class's
defaults for `f_load`, `prefill_*`, and `stream_*` and may override per-node.
Capacity (`max_tokens_*`) is per-node, not per-class.

**Per-node upstream.** Optional; falls back to the global `downstream_url`
(§9). A node without an upstream is purely synthetic. This unifies today's
"pass-through" path with the multi-node model.

---

## 6. Code Structure

### 6.1 New package: `internal/node`

```go
// internal/node/node.go
package node

type Node struct {
    ID    string
    Class string

    // Capacity
    Budget    *TokenBudget          // lifted from httpapi
    Estimator *CompletionEstimator  // lifted from httpapi
    Capacity  Capacity

    // Realism (per-class defaults, optional per-node override)
    Degradation Degradation
    Realism     Realism

    // Pass-through
    Upstream *Upstream  // nil = pure synthetic
}

type Capacity struct {
    MaxTokensInFlight   int64
    MaxTokensPerSecond  int
    MaxWaitingRequests  int
}

type Degradation struct {
    Shape          string  // "piecewise_linear" for now
    MaxDegradation int     // percent
}

type Realism struct {
    PrefillRateMultiplier  float64
    PrefillBaseOverheadMs  int
    PrefillJitterPercent   int
    PrefillMaxMs           int
    StreamVariabilityPct   int
    StreamJitterPct        int
    StreamStallProbPct     int
    StreamStallMinMs       int
    StreamStallMaxMs       int
}

type Upstream struct {
    URL   string
    Token string
    Model string
}
```

`TokenBudget` and `CompletionEstimator` move from `internal/httpapi/admission.go`
to `internal/node/`. The existing logic doesn't change — only its address.

### 6.2 New package: `internal/router`

```go
// internal/router/router.go
package router

type Decision struct {
    Node     *node.Node
    Reason   string  // "class-pinned" | "least-loaded" | "fallback" | "no-match"
}

type Router interface {
    Pick(ctx context.Context, req *Request, nodes []*node.Node) (Decision, error)
}

type ClassPinned struct{}   // honors :dsl node= / :dsl node-class=; errors otherwise
type LeastLoaded struct{}   // pick node with min(in_flight / capacity)
type Chained struct{ Routers []Router }  // first non-error decision wins
```

Composed router for the typical case:

```go
Chained{[
    ClassPinned{},                  // honor explicit DSL pin
    LeastLoaded{},                  // otherwise pick by load
]}
```

Unmatched requests when `fallback: none` return `400 no_node_match`.

### 6.3 Handler integration

```go
// internal/httpapi/handler.go (sketch)
type Handler struct {
    tenants  *TenantManager      // unchanged, stays global
    cache    *Cache              // unchanged, stays global
    router   router.Router
    nodes    []*node.Node
    // ... unchanged: dsl, profiles, metrics, config
}

func (h *Handler) serveCompletions(w http.ResponseWriter, r *http.Request) {
    // 1. parse DSL                                          (unchanged)
    // 2. estimate cost (uses winning node's estimator after routing — see §6.4)
    // 3. tenant.acquire(cost)                               (unchanged — pre-routing)
    // 4. decision := h.router.Pick(ctx, req, h.nodes)
    // 5. waited, ok := decision.Node.Budget.acquire(ctx, cost)
    // 6. if !ok { tenant.refund(cost); 429 over_capacity{node=…} }
    // 7. defer decision.Node.Budget.release(cost)
    // 8. if decision.Node.Upstream != nil { forward(...) } else { replay(...) }
}
```

### 6.4 Cost estimation

The completion-token p95 estimator is per-node today (one estimator inside
the handler). After the refactor, each node has its own. **Open question:**
do we estimate cost using the *winning* node's p95, or a global p95?

Recommendation: **global p95 for tenant-gate cost; per-node p95 for in-node
admission gate.** Reason: the tenant gate is class-agnostic (a tenant doesn't
care which node serves them), but the per-node gate is the place where a
H100's "p95 = 200 tokens" should be different from an A10's "p95 = 50 tokens"
because the request mixes drift differently per class.

For Phase 1, keep estimator behavior identical to today (handler-global)
until per-node p95 has a measurable benefit.

---

## 7. DSL Extensions

Two new directive classes, orthogonal to existing classes (first-wins):

```text
:dsl node=h100-0           # pin a specific node
:dsl node-class=H100       # pin any node in a class
```

Parsing rules mirror existing classes (§12.3 of system-design.md):
- one of each class per request, first wins
- unknown ID / class is silently ignored (forward-compat) — request falls
  through to the default router, just like today's unknown directives
- node-routing directives are reflected in `cllm_dsl_directives_total{directive=…}`
  and `dsl_applied` lifecycle event

Future "node DSL" (Phase 4): per-request node-parameter overrides
(`node-tps=128`, `node-degradation=50`), mirroring `:dsl tps=` / `:dsl
prefill=` for nodes. Out of scope for v1.

---

## 8. Metrics

Add `node` and `class` as labels on metric families that already carry
`tenant`:

```text
cllm_tenant_admissions_total{tenant, node, class}
cllm_tenant_rejections_total{tenant, reason, node, class}
        # reason now includes "node_capacity" alongside "tenant_rate" / "over_capacity"
cllm_request_lifecycle_events_total{event, outcome, node, class}
cllm_completion_tokens_total{node, class}
cllm_time_to_first_byte_seconds{node, class}
cllm_queue_wait_duration_seconds{node, class}
cllm_job_duration_seconds{node, class}
cllm_prefill_duration_seconds{node, class}
cllm_stream_stall_duration_seconds{node, class}
```

New metric:

```text
cllm_router_decisions_total{policy, reason, class}
        # how often each routing policy fired and which class it picked
```

**Cardinality budget.** `tenant × node × class × outcome`
≈ 5 × 16 × 5 × 4 = 1600 series per metric. Comfortable. Nodes are
configured (not user-supplied), so cardinality is bounded.

---

## 9. Dashboards

No new dashboards. `cllm-overview` (§8.4) gains:

- per-node admission saturation panel (`in_flight / capacity` stacked by node)
- per-class TTFT P95 panel (one line per class)
- router-decision rate panel (class-pinned vs least-loaded vs fallback)
- per-node 429 rate broken down by reason

`vllm-overview` and `gpu-overview` are unchanged.

---

## 10. Backward Compatibility

The refactor is structured so existing deployments see **zero diff**:

1. If `nodes.yaml` is absent, the handler synthesizes one node from existing
   flat config (`max_tokens_in_flight`, `max_tokens_per_second`,
   `downstream_url`, etc.). That node is named `default`, class `default`.
2. Single-node deployments continue to use the same metric series; the new
   `node` and `class` labels carry value `default`.
3. DSL directives without `node=`/`node-class=` route to the default node
   via `LeastLoaded{}` over a single-element list, which always picks node 0.
4. `:dsl no-cache` continues to mean "route to a node with an upstream";
   in single-node mode that is the default node iff `downstream_url` is set,
   identical to today.

---

## 11. Phasing

Four phases, each shippable on its own.

### Phase 1 — Node abstraction refactor
- Create `internal/node` package; move `tokenBudget`, `completionEstimator`,
  and the realism knobs into `Node`.
- `Handler` holds `[]*Node` of length 1, built from existing flat config.
- No config file, no router, no DSL changes, no metric label changes.
- Existing tests pass unchanged. Add unit tests for `Node` in isolation.
- **Deliverable:** pure refactor, observable diff is zero.

### Phase 2 — Static multi-node + class-pinned routing
- Load `configs/nodes.yaml`; build `[]*Node` from it.
- Add `Router` interface with `ClassPinned` + `LeastLoaded` + `Chained`.
- Add `node=` / `node-class=` DSL directives.
- Add `node` and `class` labels to metrics.
- Add `cllm_router_decisions_total`.
- Update `cllm-overview` dashboard with per-node panels.
- **Deliverable:** multi-node deployments work; RTX 2000 + synthetic H100
  + synthetic A10 demo.

### Phase 3 — Load-aware routing polish
- `LeastLoaded` becomes load-aware bin packing (in-flight + queue depth
  signal).
- Class-fallback policy (`fallback: any|none`).
- Document trade-offs in §6.3 of system-design.md.

### Phase 4 — Live editing + node DSL
- Add/remove/resize nodes via `/config` (mirrors §14, item 4 — live tenant
  editing).
- `:dsl node-tps=`, `:dsl node-degradation=` per-request overrides.

---

## 12. Open Questions Resolved

| Question | Decision |
|---|---|
| Is a node strictly homogeneous? | Classes are templates; nodes inherit and may override |
| Per-node upstream? | Yes; falls back to global `downstream_url` |
| Does the global FIFO survive? | No — per-node FIFO. A node models a vLLM instance |
| Routing-failure semantics | `429 over_capacity` with `node` label; tenant refund unchanged |
| Class-affinity routing in v1? | No — wait for workload class (§14, item 14) |
| Cost estimator scope | Global p95 in v1; per-node p95 deferred until measured benefit |
| Global vs per-node waiting queue | Per-node (matches real vLLM fleet shape) |

---

## 13. Non-Goals (v1)

- No real distributed routing; nodes are in-process structs.
- No KV-cache modeling per node; that is §14, item 1.
- No class-affinity routing; that is §14, item 14.
- No autoscaling of nodes; nodes are statically configured.
- No node-DSL for live parameter overrides; that is Phase 4.
- No global waiting queue with cross-node hand-off; rejected by §3.
