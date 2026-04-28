# Phase-aware token allocation (workload-class policy)

Status: **draft for review**
Owners: bartr
Future Work item: §14, item 13 in [system-design.md](../../system-design.md)
Companion docs: [design-cost-admission.md](design-cost-admission.md), [design-memory-pressure.md](design-memory-pressure.md), [design-multi-node.md](design-multi-node.md)

---

## 1. Headline

Split per-request streaming into two pacing **phases** governed by the resolved
workload class (system-design §14.14, Phases 14A/14B):

* **Responsiveness phase** — the first `initial_tokens` after first byte stream
  out at `initial_tps` (typically *higher* than the baseline). This is where
  human perception of latency lives: TTFT, early-token velocity.
* **Sustained phase** — every token after the responsiveness phase streams at
  `sustained_tps` (typically *lower*), yielding the reclaimed capacity back to
  the fleet for other admitted requests.

The transition is per-request, observable, and composes with all existing DSL
overrides. Interactive traffic stays snappy *while the user is reading early
tokens*, then releases pressure on the cost budget once the user's reading
speed becomes the bottleneck — the canonical "interactive priority *until it
becomes readable*" claim from §14.13.

---

## 2. Why now

Phases 14A (class as a labeled dimension) and 14B (`max_queue_ms` enforcement)
have shipped. Today the workload class affects *who-can-wait-how-long*; it
does not yet affect *how each admitted request paces its tokens*. The only
per-request rate knob is the global `max_tokens_per_second` plus the DSL
`tps=N` override, which is a single rate for the whole stream.

Real serving systems give interactive workloads a **rate envelope**, not a
flat rate. A chat client only needs ~16 tps once the first paragraph is
visible — eyes can't read faster than that. A summarization batch job has no
human in the loop and benefits from sustained throughput, never from a
front-loaded burst.

Without phase-aware allocation, cLLM cannot reproduce the classic fairness
tension: *"a 1000-token batch reply should not block a 50-token interactive
ack."* Today both run at the same TPS and the only differentiator is admission
priority — a coarse all-or-nothing knob.

---

## 3. Goals

1. **Per-class pacing envelopes.** Each workload class can declare an
   `initial_tokens` / `initial_tps` / `sustained_tps` triple; the streamer
   honors both rates with a clean transition.
2. **Transition is a first-class observable event.** A `phase_transition`
   lifecycle event and a `cllm_phase_transitions_total{class,phase}` counter
   make the boundary inspectable at request and fleet scope.
3. **Backward compatible.** A class with no phase fields configured (and no
   per-request DSL override) behaves byte-for-byte like today's single-rate
   path.
4. **Composable with existing DSL.** `:dsl tps=N` continues to mean "single
   sustained rate"; new directives `initial-tps=` and `initial-tokens=` opt
   into the two-phase shape per request without naming a class.
5. **Honest about scope.** Phase pacing applies on the cache-replay path
   (where cLLM owns segment timing today) — same boundary as `tps`, `jitter`,
   `stall`, etc. Live downstream pass-through is unchanged.

## 3.1 Non-goals (this iteration)

- **No `max_ttft_ms` enforcement.** The system-design sketch lists
  `max_ttft_ms` as a class field. The current code already enforces a TTFT
  budget via `prefill_max_ms` and the simulated-prefill cap; reusing that
  budget per-class is a small follow-on, but mixing it into Phase 13 conflates
  prefill budgeting with stream pacing. Defer.
- **No global "reclaimed capacity" pool.** The reclaim is implicit:
  `sustained_tps < initial_tps` means the request consumes less effective TPS
  during phase B, leaving headroom under the global cost budget for other
  admitted requests. We measure the reclaim (§7) but do not redistribute it
  through a separate accounting channel.
- **No live-downstream phase pacing.** cLLM does not currently re-pace
  downstream SSE; introducing that here is out of scope and would require a
  separate buffer / shaper in front of the writer.
- **No cross-request priority ordering.** That is Phase 14C and is intentionally
  layered *on top* of phase-aware allocation: 13 changes per-request shape, 14C
  changes which request the FIFO dequeues next. They stack cleanly because 14C
  reorders admission and 13 reshapes streaming — orthogonal axes.
- **No per-token estimator.** The phase boundary is counted in cleaned-prompt
  output tokens, not bytes or characters. Reuses the same tokenizer the cache
  replay uses for `tokenCount`.

---

## 4. Configuration model

### 4.1 New optional fields on `ClassConfig`

```yaml
classes:
  default:
    priority: medium
    max_queue_ms: 0
    # No phase fields → single-rate behavior, byte-for-byte legacy.
  interactive:
    priority: high
    max_queue_ms: 500
    initial_tokens: 100         # phase boundary, in output tokens
    initial_tps: 32             # rate during the responsiveness phase
    sustained_tps: 16           # rate after the boundary
  batch:
    priority: low
    max_queue_ms: 10000
    initial_tokens: 0           # phase A skipped → pure sustained behavior
    sustained_tps: 32
```

Validation rules (all in `internal/config/classes.go`):

| Field | Type | Rule |
|---|---|---|
| `initial_tokens` | int | `>= 0`. `0` ⇒ phase A is skipped (request is sustained-only). |
| `initial_tps` | int | `>= 0`. `0` ⇒ inherit handler base TPS for phase A. |
| `sustained_tps` | int | `>= 0`. `0` ⇒ inherit handler base TPS for phase B. |

A class with `initial_tokens > 0` *and* `initial_tps == sustained_tps == 0`
is rejected as misconfigured (it would be indistinguishable from the legacy
single-rate path while still emitting phase-transition metrics — confusing).

### 4.2 New DSL directives

| Directive | Class | Effect |
|---|---|---|
| `initial-tokens=N` | `phase-boundary` | Override phase boundary for this request. `>= 0`. `0` skips phase A. |
| `initial-tps=N` (or `=A:B`) | `initial-tps` | Override the responsiveness-phase rate. `1..2048`. |
| `sustained-tps=N` (or `=A:B`) | `sustained-tps` | Override the sustained-phase rate. `1..2048`. |

All three are first-wins per their own directive class, mirroring the
existing `tps`, `kv-cost`, `max-queue-ms` patterns. **Precedence with `tps=N`:**
the existing `tps=N` continues to mean "single-rate request" — when present in
the same prompt as `initial-tps=` or `sustained-tps=`, the more-specific
directive wins because it claims a different class.

The bare keyword `no-phase` (new) resets to single-rate explicitly:

| Directive | Class | Effect |
|---|---|---|
| `no-phase` | `phase-boundary`, `initial-tps`, `sustained-tps` | Force single-rate; skip phase A regardless of class config. Useful for debugging or for a request that opts out of its class's envelope. |

`no-phase` claims all three classes (similar to how `no-delay` claims four),
so a later `initial-tps=N` is ignored.

### 4.3 Resolution order at admit

Identical pattern to `kv-cost`/`max-queue-ms`:

1. DSL overrides (per-request), if set.
2. Resolved class fields (`class.config.InitialTokens`, etc.), if non-zero.
3. Single-rate legacy path (no phase A, sustained = handler base TPS).

The handler computes a small `phaseEnvelope` struct at admit time (alongside
the existing `effectiveMaxQueueMs` resolution) and threads it through to
`replayCachedStream` via `replayOverrides`.

---

## 5. Mechanism: where the phase boundary lives

### 5.1 Pacing location

Today: [cllm/internal/httpapi/handler.go](../internal/httpapi/handler.go)
— `cachedReplayDelay(tokenCount, overrides)` returns a single
`time.Duration` per emitted segment, computed from a single rate
(`overrides.tpsOverride` if set, else handler's `maxTokensPerSecond`,
modulated by `effectiveTokensPerSecond` for degradation).

After: the same function takes an additional `tokensSoFar int` argument and
selects the rate based on whether `tokensSoFar < envelope.InitialTokens`:

```go
rate := envelope.SustainedTPS
if tokensSoFar < envelope.InitialTokens {
    rate = envelope.InitialTPS
}
return time.Duration(float64(tokenCount) * float64(time.Second) / scaledRate(rate))
```

`scaledRate` keeps applying the existing `effectiveTokensPerSecond`
degradation curve so phase pacing composes with cost-based slowdown rather
than overriding it. Importantly: degradation scales **both** rates uniformly,
so the *ratio* between `initial_tps` and `sustained_tps` is preserved under
load — interactive stays snappy *relative to* the global degradation.

### 5.2 Carrying the running token count

`replayCachedStream` already iterates segments in order. We add a local
`tokensEmitted` counter, incremented after each `flusher.Flush()`, and pass
`tokensEmitted - segment.tokenCount` (the count *before* this segment's
delay is applied) into `cachedReplayDelay`. This is the natural way to express
"the gap *between* token N and token N+1 is paced by the rate appropriate
for the position of token N+1."

If a single segment straddles the boundary (e.g. `initial_tokens=100`, segment
contains tokens 95..120), we charge the segment at the *higher* of the two
rates so the boundary is conservatively snappy. Fractional accounting would
add machinery for one-token-different timing — not worth it.

### 5.3 Transition observability

Inside the replay loop, when `tokensEmitted` first reaches or exceeds
`envelope.InitialTokens` *and* `envelope.InitialTokens > 0` *and* we have not
yet emitted a transition for this request:

* Emit a lifecycle event `phase_transition` with fields:
  `class`, `from=phase_a`, `to=phase_b`, `tokens_emitted`,
  `elapsed_ms_since_first_byte`, `initial_tps`, `sustained_tps`.
* Increment `cllm_phase_transitions_total{class, from, to}`.

For requests that finish before reaching `initial_tokens` (typical interactive
acks), no transition fires — the absence of a transition is itself diagnostic
(see §7.4).

---

## 6. Cost-budget interaction (the reclaim story)

The headline claim in §14.13 is *"interactive yields excess capacity back to
the fleet."* Today the cost budget is charged on admission and released on
completion: a single in-flight slot value, not a function of streaming rate.
Phase pacing **does not** change this contract — admission still charges the
full estimated cost up front.

The reclaim is a property of the **degradation curve**, not the budget. With
the curve held constant, lowering `sustained_tps` reduces the *total time*
the request occupies its slot only marginally (replay duration ≈
`tokens / rate`). The honest reclaim mechanism is different:

* During phase A, `effective_tokens_per_second` reported in the
  `cllm_effective_tokens_per_second` gauge climbs toward `initial_tps`.
* During phase B, it falls toward `sustained_tps`.
* The fleet-wide degradation curve `f(combined_load)` therefore relaxes more
  quickly *between requests*, because each interactive request finishes
  sooner relative to its sustained-phase tail.

We measure the effect via a new gauge (§7) rather than asserting it via a
new accounting channel. If a future iteration wants explicit reclaim
accounting — e.g. release a fraction of the request's cost slot at phase
transition — it composes cleanly: the transition event is already the hook.
For this iteration, `cllm_class_reclaim_token_seconds_total{class}` is the
honest measurement, and the design doc does not promise more than that.

---

## 7. Observability surface

All metrics carry the `class` label introduced by Phase 14A.

| Metric | Type | Labels | Help |
|---|---|---|---|
| `cllm_phase_transitions_total` | Counter | `class`, `from`, `to` | Number of streams that crossed a phase boundary. Today only `from="phase_a", to="phase_b"`. |
| `cllm_phase_a_tokens_total` | Counter | `class` | Total tokens emitted under phase-A pacing. |
| `cllm_phase_b_tokens_total` | Counter | `class` | Total tokens emitted under phase-B pacing. |
| `cllm_class_initial_tps_effective` | Gauge | `class` | Most recently observed effective phase-A TPS for the class (after degradation). |
| `cllm_class_sustained_tps_effective` | Gauge | `class` | Most recently observed effective phase-B TPS for the class. |
| `cllm_class_reclaim_token_seconds_total` | Counter | `class` | `(initial_tps - sustained_tps) × tokens_in_phase_b / 1.0` summed across requests; measures the throughput reclaimed by yielding to phase B. Always `>= 0` because a class with `sustained_tps >= initial_tps` is the legacy degenerate case. |

Lifecycle event additions:

* `phase_transition` (new, info-level): emitted exactly once per stream that
  crosses the boundary. Carries `class`, `from`, `to`, `tokens_emitted`,
  `elapsed_ms_since_first_byte`, `initial_tps`, `sustained_tps`.
* `completed` (existing): gains optional fields `phase_a_tokens`,
  `phase_b_tokens`, `phase_b_started_ms` (relative to first byte). Absent
  when the request was single-rate.

### 7.4 Dashboard panel set

Add a *Phase-aware allocation* row to `cllm-overview.json` with four panels:

1. **Phase transition rate by class** — `sum by (class) (rate(cllm_phase_transitions_total[5m]))`.
2. **Phase-A vs phase-B token mix by class** — stacked rate of
   `cllm_phase_a_tokens_total` vs `cllm_phase_b_tokens_total`.
3. **Effective TPS by class & phase** — overlay of
   `cllm_class_initial_tps_effective` and `cllm_class_sustained_tps_effective`,
   one series per class.
4. **Reclaimed token-seconds by class** — rate of
   `cllm_class_reclaim_token_seconds_total` per class. The headline efficiency
   metric: positive area means interactive traffic gave throughput back.

---

## 8. DSL examples

```text
# Snappy chat: 100 tokens at 32 tps, then 16 tps for the tail.
:dsl initial-tokens=100 initial-tps=32 sustained-tps=16

# Pin to class envelope (no per-request override).
:dsl workload-class=interactive

# Override the class's sustained tail without touching the head.
:dsl workload-class=interactive sustained-tps=8

# Force single-rate for one request (e.g. comparing against legacy).
:dsl workload-class=interactive no-phase

# Range form: jitter the phase boundary across the fleet.
:dsl initial-tokens=80:120 initial-tps=32 sustained-tps=16
```

`initial-tokens=A:B` is drawn once per request, like `prefill=A:B`.
`initial-tps=A:B` and `sustained-tps=A:B` follow the same once-per-request
draw rule as `tps=A:B`.

---

## 9. Phasing & estimated impact

| Phase | Slice | Est. LOC | Notes |
|---|---|---|---|
| **13.1** | Class fields + loader validation + `no-phase` resolution; threading `phaseEnvelope` through `replayOverrides`; legacy path unchanged | ~200 | No metric or pacing change; pure plumbing. Tested by parser + config-loader unit tests. |
| **13.2** | Two-rate `cachedReplayDelay`; `tokensEmitted` counter in `replayCachedStream`; `phase_transition` lifecycle event + `cllm_phase_transitions_total` | ~250 | Behavior change. Integration test asserts measured TTFT for first 100 tokens vs tail-end TPS, with a saturated and unsaturated baseline. |
| **13.3** | Phase-A/B token counters, effective-TPS gauges, reclaim counter; new dashboard panels (4) | ~150 | Pure observability; can ship same-day as 13.2 once the measurement points exist. |
| **13.4** | DSL directives `initial-tokens=`, `initial-tps=`, `sustained-tps=`, `no-phase`; smoke fixture prompts | ~120 | Mirrors the 14A/14B DSL precedent exactly. |

Total: ~720 LOC across 4 commits on `bartr`. Each commit is independently
mergeable and shippable as a 0.9.x increment; the natural cut points are
(13.1+13.2) for behavior, (13.3) for observability, (13.4) for DSL surface.

### 9.1 Test plan

* **Unit:**
  * Class loader rejects misconfigured envelopes (`initial_tokens > 0` with
    both rates `0`).
  * `cachedReplayDelay` returns the right rate at boundaries
    (`tokensSoFar = initial_tokens - 1` → phase A; `= initial_tokens` →
    phase B; straddling segment → phase A).
  * DSL parsing: each new directive (positive, range, malformed, override
    over class, `no-phase`).
  * `phaseEnvelope` resolution precedence (DSL > class > legacy).
* **Integration:**
  * Stream-rate measurement: replay a fixture with `initial_tokens=10
    initial_tps=100 sustained_tps=10`; assert (a) first 10 tokens emit in
    `<200 ms`, (b) tokens 11–110 emit in `~10 s`, (c) one
    `phase_transition` event recorded, (d)
    `cllm_phase_transitions_total{class=test, from=phase_a, to=phase_b} == 1`.
  * No-transition case: replay a 5-token fixture with `initial_tokens=100`;
    assert no transition event, no transition counter increment.
  * Single-rate compatibility: a request with no class envelope and no DSL
    overrides paces identically to today (compare segment timestamps within
    a tolerance band).
  * Composability: `:dsl workload-class=interactive sustained-tps=4` measures
    the override rate, not the class default.

### 9.2 Smoke fixture additions (`scripts/smoke-test.yaml`)

* Prompt 15: `:dsl initial-tokens=20 initial-tps=200 sustained-tps=20 no-cache`
  — visibly two-phase; phase transition should appear in lifecycle log.
* Prompt 16: `:dsl workload-class=interactive no-cache` — exercises the
  class-resolved envelope (assuming `interactive` is configured in
  `classes.yaml` example).
* Prompt 17: `:dsl no-phase no-cache` — explicit single-rate request for
  baseline comparison against prompt 15.

### 9.3 Risks

1. **Measurement noise on slow CI.** The integration test's timing assertions
   need generous tolerance (`±20%` on phase-A duration is reasonable on a
   loaded test runner). Alternative: use a deterministic mock clock if/when
   we standardize one.
2. **`cachedReplayDelay` signature change.** Changes a hot path. Mitigated
   by keeping the legacy path explicit (`if envelope.InitialTokens == 0
   { return legacyDelay(...) }`) so a single caller can be audited.
3. **Class-loader validation tightening.** Existing `classes.yaml` files in
   the wild (today: just the example) become invalid only if they set
   `initial_tokens > 0` without rates — currently nobody does, but a release
   note should mention the rule.

---

## 10. Future work this enables

* **Phase 14C** (priority-weighted dequeue) becomes meaningful once
  per-request rate envelopes exist: priority can decide *which* class drains
  the FIFO first, and phase-aware allocation makes the priority-ordered
  draining cheap (interactive finishes phase A in tens of ms, then the queue
  head moves to the next request even though phase B continues in the
  background).
* **`max_ttft_ms` enforcement** (deferred non-goal) lays naturally on top:
  if first-byte simulated prefill plus `1 / initial_tps` would blow past the
  class budget, reject at admission with new reason `class_ttft_budget`.
  Reuses the prefill-cap machinery already in `simulatePrefillDelay`.
* **Class-aware capacity scaling** (extension of §6.6 capacity-scaling
  story): the same calibrated `tps=N` knob can declare per-class envelopes,
  giving multi-target benchmarks a way to model "this hardware delivers
  these envelopes" rather than "this hardware delivers this single rate."
