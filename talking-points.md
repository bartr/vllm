# cLLM — Talking Points

Speaker notes, technical framing, demo recommendations, and skill-building path
for the cLLM project. Companion to `system-design.md` (the design artifact).
This file is intentionally informal: bullets, quotes, and working notes — not a
specification.

---

## 1. The 30-Second Pitch

> I built cLLM, a **GPU-calibrated experimentation platform for LLM inference**.
> It exposes an OpenAI-compatible API, runs next to a real vLLM deployment, and
> uses one physical GPU to calibrate a synthetic cache-replay path. After that,
> I can test admission, fairness, backpressure, routing, and capacity-scaling
> policies without provisioning more GPUs.
>
> The key design choice is that **tokens, not requests, are the resource**. The
> same token-cost estimate drives admission, tenant fairness, capacity scaling,
> and the Replay DSL. cLLM models capacity as a stock of tokens in flight plus a
> flow of generated tokens/sec, so the synthetic path reproduces the soft
> saturation and queueing behavior that matters at the control-plane layer.
>
> Validation is built into the system: real vLLM, pass-through, and synthetic
> streams run side-by-side in the same Kubernetes cluster, with Prometheus,
> Grafana, DCGM telemetry, and request correlation IDs on one time axis. The
> result is a reproducible experiment surface that can replay on a laptop, in
> CI, or on the calibration host.

That is the version to say out loud. Everything else in this file is supporting
material.

---

## 2. The 10-Second Pitch

> cLLM is a GPU-calibrated experimentation platform for LLM inference
> control-plane decisions — one GPU calibrates the envelope, then cached
> workloads replay anywhere.

---

## 3. What Lands Well (lead with these)

1. **Token-cost is the common currency.** `prompt + min(max_tokens, p95)` shows
   why request-count limits fail for LLMs, and it ties admission, fairness,
   DSL behavior, and capacity scaling together.
2. **Stock plus flow capacity model.** `max_tokens_in_flight` controls the
   admission stock; `max_tokens_per_second` controls the replay flow; `f(load)`
   couples them into soft saturation instead of a hard cliff.
3. **Calibration against real vLLM.** "One GPU calibrates the synthetic
   envelope" is practical, cost-aware, and easy to understand.
4. **In-process heterogeneous fleet.** A cLLM node models a vLLM instance, not
   a GPU. One process hosts N node structs from `configs/nodes.yaml`, each
   with its own admission stock, FIFO, and per-class realism. Routing is
   per-request (`:dsl node=` / `:dsl node-class=` / least-loaded), and
   single-node deployments still behave exactly as before.
5. **Per-request experiment surface.** The Replay DSL plus `/config` means one
   deployment can run mixed workloads, A/B comparisons, and targeted fault
   injection without restarts.
6. **Three-layer observability.** `cllm-overview`, `vllm-overview`,
   `gpu-overview` answers system / serving stack / hardware on one time axis;
   per-node panels light up when `nodes.yaml` defines more than one node.
7. **Reproducible workload artifacts.** Cache snapshots capture ground truth
   once, then replay the same workload shape on laptops, CI runners, and
   rebuilt clusters.
8. **Honest scope boundaries.** Naming what is *not* modeled (kernel-level GPU
   behavior, KV-cache pressure spikes, batch-scheduler interleaving) makes the
   rest of the claims more trustworthy.

When time is short, lead with #1 and #2 and let the discussion pull on the
others.

---

## 4. Quotable One-Liners

Memorize a few of these — they're the lines that stick:

* "Tokens, not requests, are the resource."
* "Calibrate once, experiment forever."
* "One GPU is enough to model an N-GPU deployment."
* "A cLLM node models a vLLM instance, not a GPU. The synthetic node is a struct, not a service."
* "Capacity has a stock and a flow: tokens in flight, and tokens per second."
* "The cost of experimentation is decoupled from the cost of the system being
  experimented on."
* "Validation is a primitive, not a milestone."
* "The DSL separates what is said from how it is executed."
* "Cache snapshots are workload artifacts, not just prompt storage."
* "Interactive gets priority *until it becomes readable*, then yields excess
  throughput back to the fleet."
* "Tenant = who. Node class = what hardware. Workload class = what kind of work."

---

## 5. What to Tighten for Live Conversation

The design doc is intentionally complete. The spoken version should not be.

* **Drop the section numbers.** `§6.4` is a written-document tool; spoken,
  refer to "the admission model" or "the replay path."
* **Drop the formulas.** `effective_tps = max_tps × (1 − d(load))` is the right
  thing to point at on screen, the wrong thing to recite.
* **Lead with the artifact, not the architecture.** "Here are side-by-side
  Grafana dashboards showing real vLLM, synthetic replay, and GPU telemetry on
  one time axis" beats "I designed a three-layer observability surface."
* **One graph per claim.** If the claim doesn't have a graph, it's not ready
  for a technical review yet.

---

## 6. Demo Recommendations

### 6.1 The killer benchmark report (highest priority)

A short scripted report with three graphs does more for a review than five
more features:

1. **Concurrency vs TTFT** — soft-saturation, queue-dominated tail growth.
2. **Tenant isolation under noisy-neighbor load** — `customer-b` runs a batch
   flood; `customer-a` interactive TTFT stays protected.
3. **1× vs 4× vs 16× synthetic capacity scaling** — same physical GPU,
   `tps=N` walking the envelope.

Commit the report alongside the cache snapshots so it reproduces on any host.

### 6.2 Live demo flow (5 minutes)

1. Show three Grafana dashboards (cLLM, vLLM, GPU) — empty.
2. Run `ask --bench --files prompts.yaml --dsl no-cache` against vLLM. All
   three light up. **"This is the calibration."**
3. Drop `--dsl no-cache`, rerun. Only `cllm-overview` lights up; vLLM and GPU
   stay quiet. **"This is the synthetic envelope. Same workload shape. No GPU
   cost."**
4. Bump `tps` to 256, rerun. Synthetic envelope scales; physical GPU still
   idle. **"This is 8× capacity on a 1× GPU."**
5. Run a noisy-neighbor scenario (`customer-b` batch flood) while
   `customer-a` interactive traffic continues. Show TTFT panel separated by
   tenant. **"This is fairness."**
6. With a multi-node `nodes.yaml` mounted, send two requests with
   `:dsl node-class=H100` and `:dsl node-class=A10`. The fleet panels
   (`cllm_node_tokens_in_flight`, queue-wait p95, admission rate) split by
   node and class. **"This is heterogeneous routing on one process."**
7. Save or load the cache snapshot and rerun the synthetic side. **"This is the
   workload becoming portable."**

### 6.3 Backup demo (no GPU available)

Run cLLM on a laptop against a committed cache snapshot. Same envelope
reproduces. **"The calibration host is the only paid GPU hour. Experiments
replay anywhere."**

---

## 7. Anticipated Questions and Answers

**Q: Isn't this just a fancy load tester?**
A: A load tester drives traffic; cLLM models the *control plane* — admission,
fairness, backpressure, routing, and capacity scaling. The synthetic path is
calibrated against real vLLM on the same GPU, and the DSL lets each request
choose how it executes. The important artifact is not just traffic; it is a
validated, replayable workload.

**Q: Why not just rent more GPUs?**
A: Two answers. Operational: GPU quota, provisioning latency, and tear-down
churn make multi-GPU experiments slow even when budget exists. Economic:
calibration is the only paid GPU hour; everything else replays from the cache
library at zero marginal GPU cost. That makes a capacity study a routine
engineering loop instead of a fleet request.

**Q: What don't you model?**
A: Kernel-level effects — KV-cache pressure spikes, batch-scheduler
interleaving, content-dependent decode-time variance. Those are on the
roadmap. The synthetic path scales the *envelope*, not the underlying compute.
The claim is system-level fidelity, not micro-architectural simulation.

**Q: How do you know the synthetic path matches the real one?**
A: `:dsl no-cache` runs the same prompts through the real backend on the same
hardware. The Grafana dashboards line up side-by-side. When the synthetic path
drifts from real behavior, the dashboards show it immediately and the cache
gets refreshed with `:dsl re-cache`. Validation is continuous, not a milestone.

**Q: What's the actual capacity model?**
A: Two knobs, two regimes. `max_tokens_in_flight` is the stock of work admitted
at once; `max_tokens_per_second` is the flow rate for replay. `f(load)` reduces
per-request flow as the stock fills, which gives you throughput plateau plus
queue-driven TTFT growth instead of a simplistic fixed-RPS limit.

**Q: What about tenant fairness?**
A: Per-tenant token buckets gate eligibility into a global FIFO. Token cost is
estimated as `prompt + min(max_tokens, p95)` — the same currency admission
uses. Refunds on global rejection preserve work conservation. Weighted
decode-time fairness is on the roadmap.

**Q: Why an in-prompt DSL?**
A: Because per-request configuration is the experiment surface. `/config` sets
the default behavior; `:dsl` overrides one request without redeploying or
forking the workload. The cache key is based on the cleaned prompt, so the same
content can be replayed with different pacing, faults, profiles, or cache
behavior.

**Q: Why Kubernetes?**
A: Because that's where the production serving stack lives. cLLM, vLLM, DCGM,
Prometheus, and Grafana run in one cluster with one observability stack. The
experimentation platform looks identical to the production environment it's
designed to inform.

**Q: What's the next feature?**
A: Phase-aware token allocation: interactive traffic gets a high TPS for the
first ~100 tokens, then yields excess capacity back to the fleet because user
reading speed becomes the bottleneck. That generalizes the fairness story from
"interactive always gets priority" to "interactive gets priority *until it
becomes readable*."

---

## 8. Positioning for Platform Engineering

The right framing for platform-engineering discussions:

> Controllable, instrumented, validated GPU experimentation platform for LLM
> inference control-plane decisions.

Not:

> Cached LLM simulator. *(too small)*
> Load testing tool. *(misses the point)*
> Inference benchmark. *(misses the platform layer)*
> GPU simulator. *(claims the wrong fidelity boundary)*

### High-signal next features, in priority order

1. **KV-cache pressure model — shipped, with extensions remaining.** Each
   node now carries an optional `max_kv_tokens` budget, admission gates on
   both compute and memory axes (`kv_pressure` / `kv_oversize` join
   `over_capacity`), and `f(load)` consumes
   `combined_load = max(cost_load, kv_load × kv_weight)` so memory-bound
   regimes degrade independently. `:dsl kv-cost=N` / `:dsl no-kv` plus the
   `kv-light` / `kv-heavy` / `kv-stress` profiles drive the second axis;
   the `cllm-overview` dashboard surfaces it with four panels. Remaining
   work is a KV-aware completion estimator (currently `kv_cost = total_cost`)
   and per-class KV inheritance shorthand. Closes the biggest honest-gap
   call-out. (Future Work §14, item 1.)
2. **Multi-node operational polish.** Multi-node simulation already ships —
   `internal/node`, `internal/router`, `configs/nodes.yaml`, per-node
   Prometheus metrics, and `:dsl node=` / `:dsl node-class=` are all in place.
   The remaining gaps are: dispatch real-backend calls through the routed
   node's `Upstream` block (today they still use the global `downstream_url`),
   consume the parsed-but-unused `router.fallback: any|none`, add/remove/resize
   nodes via `/config`, and per-request `:dsl node-tps=` / `:dsl
   node-degradation=` overrides. (Future Work §14, items 6 and 8.)
3. **Cache library tooling and CI gates.** Diff, prune, export, and assert
   synthetic envelope behavior against committed cache snapshots. This turns
   reproducibility from a demo claim into a regression test. (Future Work §14,
   item 12.)
4. **Scenario runner.** YAML scenario files that compose tenants, classes,
   traffic mixes, and DSL profiles into a single repeatable experiment.
   (Future Work §14, item 15.)
5. **Reference benchmark report.** The three-graph artifact above. (Future
   Work §14, item 16.)

---

## 9. Skill-Building Path

To deepen the platform-engineering story, build expertise in:

* **Kubernetes GPU stack:** NVIDIA device plugin, DCGM exporter, GPU Feature
  Discovery, MIG basics. cLLM already touches the first two; add hands-on with
  the rest.
* **Inference serving internals:** vLLM scheduling, PagedAttention, prefill /
  decode separation, KV-cache behavior. Read the vLLM scheduler code; map it
  to the abstractions in `system-design.md` §6–§7.
* **Fleet scheduling:** heterogeneous node routing, bin packing, admission
  control, backpressure. The phase-aware allocation and workload-class items
  in Future Work are the entry points.
* **Experiment design:** workload identity, replay semantics, cache snapshot
  hygiene, and CI regression gates. The strongest version of cLLM is a library
  of reusable experiments, not just a service that can run them.
* **Performance storytelling:** every graph should answer a decision. *"This
  graph tells me when to reject, when to queue, when to scale."* If a graph
  doesn't drive a decision, cut it.

---

## 10. Tenant vs Workload Class vs Node Class — the Sharper Model

A common modeling trap: framing "interactive" and "batch" as tenants. They're
not. They're **workload classes**, orthogonal to tenant. And both are
orthogonal to the now-shipped *node* class (`H100`, `A10`, `rtx-2000`), which
is a hardware dimension, not a behavior dimension.

```text
tenant         = customer / team / workload owner   (who)
workload class = interactive / batch / eval         (what kind of work)
node class     = H100 / A10 / rtx-2000              (what hardware)
```

| Dimension | Mechanism | Purpose |
|---|---|---|
| Tenant | Per-tenant token buckets (§6.5) | Quota, isolation, noisy-neighbor protection |
| Workload class | Priority, queue policy, phase-aware allocation | Latency budget, scheduling priority |
| Node class | `configs/nodes.yaml` `class:` + router (§6.7) | Routing target, calibrated capacity envelope |

The canonical demonstration:

> Customer-B runs a batch flood on the A10 pool. Customer-A's interactive
> TTFT on the H100 pool stays protected.

That's a stronger story than "interactive always gets priority" because it
separates *who is over budget* from *what their work needs* from *what hardware
they ran on*. Node class ships today; workload class is Future Work §14 items
13 and 14.

---

## 11. Honest Limits — Always Volunteer These

Volunteering the limits is more credible than being asked about them. Lead
with these when scope comes up:

* No kernel-level GPU simulation. The synthetic path scales the envelope, not
  the compute.
* No KV-aware completion estimator yet — `kv_cost` currently mirrors
  `total_cost`, so a long-context, short-output request charges the same KV
  as a short-context, long-output one. The two-axis admission gate and
  `combined_load` curve already ship; only the per-axis estimator is open.
* No content-dependent decode variance yet. (On the roadmap.)
* `f(load)` is a fixed-shape, single-knob curve today. Pluggable curves are on
  the roadmap.
* Tenant rate / burst is startup-only, not yet live-editable. (On the
  roadmap.)
* The active-set scheduler is FIFO. Weighted decode-time fairness is on the
  roadmap.
* Multi-node simulation ships today (in-process node fleet, per-node FIFO,
  router with class-pinned + least-loaded policies, per-node Prometheus
  metrics). What's *not* yet wired: per-node upstream dispatch in the request
  path, the parsed-but-unused `router.fallback`, live add/remove/resize via
  `/config`, and per-request `:dsl node-tps=` / `node-degradation=`. (On the
  roadmap.)

Each of these maps to a numbered Future Work item — say so. It demonstrates
the design doc is current and the limits are known, not hidden.

---

## 12. Things to Avoid Saying

* **"Simulator."** It's a *GPU experimentation platform*. The reframe matters.
* **"GPU simulator."** That overclaims. cLLM models system dynamics, not GPU
  kernels.
* **"It's basically vLLM."** It's not. It's the control plane around vLLM.
* **"You just point it at vLLM."** That undersells the synthetic path. Lead
  with calibration.
* **"The cache is just memoization."** The cache is a workload library:
  ground-truth content plus replayable execution behavior.
* **Promising features that aren't built.** Future Work is on the roadmap;
  point at it explicitly. Don't blur the line.
* **Numbers without dashboards.** Every claim should land on a graph the
  audience can see.
