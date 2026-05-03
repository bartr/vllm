# Cost-aware, multi-tenant request admission

Status: **draft for review**
Owners: bartr

## Goals

1. Stop treating one request as one slot. Admit based on **expected token cost**
   so a 10k-token request doesn't take the same slot as a 50-token request.
2. Provide **per-tenant isolation**: one tenant cannot saturate the cluster.
3. Provide **priority tiers** so `interactive` beats `batch` when capacity is tight.
4. Preserve cllm's existing "simulated GPU contention" story: the load-based
   degradation curve (`max_degradation`) keeps working, but it now operates on
   **cost in flight**, not request count.

## Non-goals (this iteration)

- Real authentication (JWT, OIDC). Tenant comes from a trusted header.
- Cross-node coordination. Single-process scheduler only.
- Preemption of in-flight requests. Admission-time decisions only.
- Persistent quotas. Buckets reset on process restart.

## Recommended model

A **two-tier admission gate**:

```
        ┌──────────────────────────────────────────┐
        │  global token-budget pool                │
        │  capacity = max_tokens_in_flight         │
        └──────────────────────────────────────────┘
                       ▲
                       │ admit iff
                       │   Σ in_flight_cost + req_cost ≤ capacity
                       │
        ┌──────────────────────────────────────────┐
        │  per-tenant token-bucket                 │
        │  rate = tenant.rate_tps                  │
        │  burst = tenant.burst_tokens             │
        └──────────────────────────────────────────┘
                       ▲
                       │ admit iff
                       │   bucket.tokens ≥ req_cost
                       │
                  request arrives
```

A request is admitted only when **both** gates pass. Otherwise it waits in the
tenant's FIFO until either a release frees global budget or the bucket refills.

### Why both gates

| Without global pool | Without per-tenant bucket |
| --- | --- |
| One tenant with infinite quota can OOM the GPU. | Tenants compete fairly, but a burst of small requests still spikes the GPU. |
| GPU degradation curve has nothing meaningful to react to. | One tenant can dominate; "fairness" claims are hollow. |

## Cost model

```
request_cost = prompt_tokens + min(max_tokens, p95_completion_tokens)
```

- **`prompt_tokens`** is computed exactly from the request payload using the
  same tokenizer cllm already uses for cache keys.
- **`p95_completion_tokens`** is a rolling estimate. Per-tenant once we have
  ≥ 50 observations for that tenant; otherwise we fall back to a **global**
  p95 across all tenants. If the global estimator also has < 50 samples
  (cold start of the whole process), we fall back to `max_tokens` (worst
  case) for that one request.
- Bounded by `max_tokens` so a tenant asking for 50 always pays ≤ 50.

When the request finishes, we **reconcile**: if actual completion was shorter
than estimated, return the unused budget to both pools immediately so we don't
under-admit the next caller. If it was longer (only possible if our p95
estimate was wrong), we just absorb the overrun — we never preempt.

This estimator is the trickiest piece. Two ways to fail safely:
- Cold-start tenant → use `max_tokens`. Conservative; never over-admits.
- Estimator drift → reconciliation on completion bounds the error.

## API surface

### Request headers (additive; backwards compatible)

| Header | Required | Default | Notes |
| --- | --- | --- | --- |
| `X-Tenant-Id` | no | `"default"` | Identifies the bucket. Untrusted; not authenticated. |
| `X-Priority` | no | `interactive` | One of `interactive`, `batch`. Phase 2. |

Missing headers route to the `default` tenant at `interactive` priority. This
keeps existing clients working unchanged.

### Configuration

Add to the existing config endpoint (`/config`):

```yaml
admission:
  max_tokens_in_flight: 50000     # global cost budget
  default_tenant:
    rate_tps: 200                 # bucket refill per second
    burst_tokens: 4000            # max bucket capacity
  tenants:                        # optional overrides
    - id: "team-alpha"
      rate_tps: 1000
      burst_tokens: 16000
    - id: "nightly-batch"
      rate_tps: 50
      burst_tokens: 2000
      priority_cap_pct: 30        # batch lane: max 30% of global budget
```

Phase 1 ships everything except `priority_cap_pct`.

### Mapping from existing config

| Old | New | Notes |
| --- | --- | --- |
| `max_concurrent_requests` | **removed** | Replaced by `max_tokens_in_flight`. Slot count was the wrong unit. |
| `max_waiting_requests` | per-tenant | Bounds queue depth per tenant (see below). |
| `max_degradation` | unchanged | Now driven by `in_flight_cost / max_tokens_in_flight`. |
| `max_tokens_per_second` | unchanged | Cache-replay pacing; unaffected. |

## Admission algorithm

```go
func (s *scheduler) admit(ctx, tenant, cost) (release func(), err error) {
    // Per-tenant gate first: cheaper to fail fast and protects the global
    // pool from one noisy tenant exhausting our queue depth.
    if !tenant.bucket.reserve(cost, ctx) {
        return nil, errOverCapacity
    }

    // Global gate.
    if !s.globalPool.acquire(cost, ctx) {
        tenant.bucket.refund(cost)
        return nil, errOverCapacity
    }

    return func(actualCost int) {
        delta := cost - actualCost
        if delta > 0 {
            s.globalPool.release(delta)
            tenant.bucket.refund(delta)
        }
        s.globalPool.release(actualCost)
    }, nil
}
```

Both `bucket.reserve` and `globalPool.acquire` block up to a deadline (existing
`maxWaitingRequests` semantics, but per-tenant) and return false if the queue
is full.

## Metrics

Add Prometheus gauges/counters labeled by `tenant` and `priority`:

- `cllm_admission_inflight_cost{tenant=}` — gauge
- `cllm_admission_bucket_tokens{tenant=}` — gauge (current bucket fill)
- `cllm_admission_admitted_total{tenant=,priority=}` — counter
- `cllm_admission_rejected_total{tenant=,priority=,reason=}` — counter (`reason`: `tenant_exhausted`, `global_exhausted`, `queue_full`)
- `cllm_admission_wait_seconds{tenant=,priority=}` — histogram
- `cllm_admission_cost_estimate{tenant=}` — histogram (estimated vs actual, for tuning)

Update the existing `effective_tokens_per_second` log line to also emit
`inflight_cost` and `bucket_tokens` for the admitted tenant.

## Build plan (single PR)

Phases below describe logical chunks of work in commit order; they ship as
one PR.

### Phase 1 — cost-based admission

1. Add `requestCost(payload)` helper using existing tokenizer + global p95
   estimator (the per-tenant estimator follows in Phase 2; until then all
   samples feed one global rolling window).
2. Add `tokenBudget` type (semaphore over an int64 cost; blocks on `acquire`
   until `release` frees enough).
3. Replace `acquireRequestSlot` count semantics with cost. Remove
   `max_concurrent_requests`; keep a per-tenant queue-depth bound.
4. Update `processingStats` and `degradationMetricsLocked` to use
   `inflight_cost / max_tokens_in_flight`.
5. Tests: cost is computed correctly; long request blocks short one only when
   global pool is full; reconciliation refunds correctly on early stop.

### Phase 2 — tenants

1. Parse `X-Tenant-Id` header; fall back to `"default"`.
2. Add per-tenant `tokenBucket` (rate + burst). Lazy-create on first request.
3. Wire two-stage admission: tenant gate, then global gate.
4. Per-tenant config in `/config` endpoint + YAML profile file.
5. Per-tenant p95 estimator (with global p95 fallback for cold-start tenants).
6. Per-tenant metrics labels.
7. Tests: one tenant cannot starve another; bucket refunds work; cold-start
   tenant uses global p95 (or `max_tokens` if global also cold).

### Phase 3 — priority lanes

1. Parse `X-Priority` header.
2. Per-tenant FIFO becomes two FIFOs (interactive, batch); drain interactive
   first.
3. Optional `priority_cap_pct` per tenant: batch admission caps at N% of
   global budget so it can't starve interactive.
4. Tests: batch yields to interactive under contention; cap honored.

### Phase 4 — fairness polish (optional)

1. Per-tenant p95 estimator (replace process-wide).
2. Adaptive bucket sizing based on observed demand.
3. WFQ-style virtual time across tenants if simple priority lanes prove
   insufficient.

## Open questions

1. **Stream vs non-stream cost**: should `stream=false` requests get a
   discount (no decode-pacing back-pressure) or pay full cost? Recommend
   full cost for now — they still consume GPU.
2. **Cache hits**: a cache replay has near-zero GPU cost. Should it bypass the
   global pool entirely, or pay a flat 0-cost for accounting? Recommend
   bypassing the global pool but still debiting the tenant bucket at 1/10
   cost so abusive cache-hit floods are still bounded.
3. **DSL `no-cache`**: today this skips the cache. Should it imply
   `priority=batch` automatically since it always hits vLLM? Possibly.
4. **`max_waiting_requests`**: per-tenant or global? Recommend per-tenant
   so one noisy tenant can't fill the queue and lock out others.

## Rollout

Phase 1+2 ship together as one PR. This is a behavior change
(slots → cost; new headers; `max_concurrent_requests` removed):
- New `--admission-mode={count,cost}` flag, default `cost`.
- `count` mode preserves the legacy code path for one release as an escape
  hatch, then is removed.
- `X-Tenant-Id` and `X-Priority` headers are additive; missing headers map to
  `default`/`interactive`.
