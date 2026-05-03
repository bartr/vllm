# cLLM Presentation Track: 30-Minute Demo + 20-Minute Q&A

Audience: CVP, partner FDEs, principal FDEs
Goal: Show cLLM as a field-driven LLM inference experimentation platform, not as a toy benchmark or dashboard project.

## Core Message

cLLM grew out of real edge AI deployment work: deploying vLLM on Kubernetes with GitOps, instrumenting it, benchmarking it, and then asking the next field question: how do we reason about scaling, routing, fairness, and capacity without needing more physical GPUs for every experiment?

The answer is cLLM: one real vLLM lane calibrates the envelope, and cached synthetic lanes let us model control-plane behavior across many GPU-like backends. Claude/MCP now makes that experiment loop operator-friendly: inspect the fleet, adjust synthetic capacity, run bounded benchmarks, and generate evidence-backed reports.

Short version:

> Tokens, not requests, are the resource. cLLM lets us calibrate once against real vLLM, then experiment repeatedly with LLM serving behavior without repeatedly provisioning GPU fleets.

## Presentation Shape

| Time | Segment | Purpose |
|---:|---|---|
| 0:00-2:00 | Opening | State the problem and why this exists |
| 2:00-6:00 | Origin Story | Connect edge GitOps, vLLM, dashboards, benchmarks, and cLLM |
| 6:00-11:00 | Architecture | Explain nodes, cache replay, real vLLM lane, token admission, observability |
| 11:00-22:00 | Live Demo | Show topology, benchmark, add capacity, traffic split, dashboards |
| 22:00-27:00 | Evidence | Walk through reports 001-004 and what they prove |
| 27:00-30:00 | Why It Matters | Product/FDE value, next steps, crisp close |
| 30:00-50:00 | Q&A | Go deeper on fidelity, limitations, roadmap, productization |

## Opening: 0:00-2:00

Say:

> I want to show cLLM as a field-engineering artifact. It started from a practical edge AI problem: deploying and updating vLLM on Kubernetes at scale. Once that worked, the next questions were operational: how do we observe it, benchmark it, compare real and synthetic behavior, and reason about capacity without needing a fleet of GPUs for every experiment?

> cLLM is the platform that came out of that. It exposes a Chat Completions-compatible API, runs next to real vLLM, and models LLM serving as a token-throughput and admission-control problem. A real vLLM lane stays in the loop as the baseline; synthetic cached lanes let us test routing, capacity, fairness, and saturation behavior cheaply and repeatably.

Land the frame:

> This is not a kernel-level GPU simulator. It is a control-plane experimentation platform for the things field teams actually need to answer before customers trust AI workloads in production.

## Origin Story: 2:00-6:00

Talk track:

> The path was very natural. First, we needed to deploy vLLM on Kubernetes at the edge. GitOps was the only sane deployment model because edge systems need repeatability, drift control, and safe update mechanics.

> Once vLLM was running, I needed visibility, so I built dashboards and benchmark clients. Once I could see the system, the next question became: how do I scale vLLM experiments without physical GPUs for every imagined topology?

> That led to the first cache-backed LLM path. Then the work became: can the cached path behave closely enough to vLLM at the control-plane layer that we can use it for experiments? That evolved into cLLM: nodes, admission, routing, per-node capacity, observability, and now MCP-driven experiment control.

Emphasize:

- This came from customer/field reality, not theory.
- Each layer answered the next operational blocker.
- The output is a reusable pattern, not a one-off script.

## Architecture: 6:00-11:00

Show: `docs/system-design.md`, `/nodes`, and Grafana dashboard.

Talk track:

> The key design decision is that tokens are the resource, not requests. One 4k-token request can displace dozens of small requests from a serving system, so request-count limits misrepresent the actual pressure.

> cLLM models each node with a token stock and a token flow. Stock is `max_tokens_in_flight`: how much work can be admitted. Flow is per-request tokens per second, shaped by a degradation curve as concurrency rises. That is enough to reproduce the system-level dynamics we care about: admission, queueing, saturation, routing, and tail latency.

Explain current topology:

- `vllm`: protected real-GPU baseline lane, bypasses cache.
- `cllm`: cache-backed synthetic lane calibrated to vLLM-like behavior.
- Optional synthetic nodes, such as `rtx`, can be added live through `/nodes` or MCP.
- Least-loaded routing uses node-local capacity and live load.

Observability:

> I keep three views aligned: cLLM tells us what the control plane did, vLLM tells us what the real serving stack did, and GPU telemetry tells us whether physical hardware moved. That separation is important because it lets us distinguish synthetic capacity from real GPU consumption.

MCP layer:

> The recent addition is an MCP server. Claude can inspect nodes, read config/cache/metrics, run bounded benchmark windows, add synthetic nodes, and summarize results. The real vLLM lane is protected, deletion is intentionally not exposed yet, and benchmark windows are bounded.

## Live Demo: 11:00-22:00

Keep this paced. The goal is to prove the loop, not narrate every field.

### Demo Setup

Have open:

- Grafana cLLM dashboard: `http://192.168.68.63:3000/d/cllm-overview/cllm-overview`
- Nodes API/form: `http://192.168.68.63:8088/nodes`
- Metrics: `http://192.168.68.63:8088/metrics`
- Claude Code or Codex session with the MCP server available
- Reports directory

Say:

> I’ll use the MCP interface rather than hand-driving the API, because this shows the operator workflow. Claude gets bounded tools over the system, not arbitrary cluster access.

### Step 1: Inspect Fleet

Prompt:

```text
List the current cLLM nodes, identify which lane is real vLLM, and summarize the current benchmark status.
```

Expected tools:

- `list_nodes`
- `get_benchmark_status`
- `get_metrics_snapshot`

What to point out:

- `vllm` is protected and non-cached.
- `cllm` is synthetic and cache-enabled.
- Benchmark is active or ready.
- Dashboard is the visual confirmation surface.

Say:

> The important bit is that Claude is not guessing. It is calling the same APIs and metrics I use as an operator.

### Step 2: Baseline Benchmark

Prompt:

```text
Run a 60-second benchmark at 120 connections and summarize traffic split, throughput, TTFT, and cache behavior.
```

Expected result, from report 001:

- 2 nodes, 120 connections.
- `cllm` +871 admissions, 51.8%.
- `vllm` +809 admissions, 48.2%.
- 1,680 total requests.
- 2,702 tok/s average throughput.
- 178 ms average TTFT.
- 52.2% cache hit rate.

Say:

> This is the baseline: one synthetic lane, one real lane, near-even routing. Cache hit rate lands around half because half the traffic goes to the cache-enabled lane.

### Step 3: Increase Load Without Adding Capacity

Prompt:

```text
Run a 60-second benchmark at 160 connections and compare it to the 120-connection baseline.
```

Expected result, from report 002:

- Requests: 1,680 to 1,875, up 11.6%.
- Throughput: 2,702 to 3,011 tok/s, up 11.4%.
- Avg request tok/s: 20.43 to 16.83, down 17.6%.
- TTFT: 178 ms to 220 ms, up 42 ms.
- Split remains stable, 53.5% / 46.5%.

Say:

> This is the classic saturation trade-off. More concurrency finds some aggregate headroom, but per-request quality degrades. That is exactly why RPS-only reasoning is dangerous for LLM serving.

### Step 4: Add Synthetic Capacity

Prompt:

```text
Add a synthetic node named rtx with the same configuration as cllm, run a 60-second benchmark at 150 connections, and summarize traffic split and node behavior.
```

Expected result, from report 003:

- `rtx` cloned from `cllm`.
- Traffic split:
  - `cllm`: +704, 33.1%.
  - `rtx`: +703, 33.0%.
  - `vllm`: +722, 33.9%.
- Total requests: 2,129, up 26.7% vs baseline.
- Avg total tok/s: 3,485, up 28.9% vs baseline.
- Avg request tok/s: 21.02, up 2.9% vs baseline.
- TTFT: 203 ms, up only 24 ms vs baseline and better than the 160-connection 2-node run.
- Cache hit rate: 65.9%, matching the 2 cached lanes / 1 passthrough lane topology.

Say carefully:

> This does not mean I created a physical GPU. It means I added synthetic serving capacity to the control-plane model. The router immediately shifted from roughly 50/50 to roughly 33/33/34, and the cache ratio moved to about two-thirds because two of the three lanes are cache-enabled.

Then land the point:

> This is the capacity-planning loop I care about. I can ask “what if we had another backend class?” and immediately test routing, admission, queueing, and latency behavior without waiting for new hardware.

### Step 5: Dashboard Confirmation

Show Grafana:

- Per-node admissions/split.
- Tokens in flight.
- Queue wait.
- TTFT/job duration.
- Cache hit/miss where applicable.
- vLLM/GPU dashboard if open.

Say:

> The dashboard is not the source of magic; it is the visual audit trail. Claude produces the summary from tools and metrics, and humans can verify the same story here.

## Evidence Walkthrough: 22:00-27:00

Use the reports as proof artifacts.

### 001 Baseline

Message:

> Two lanes split close to evenly. This validates the router and establishes the reference behavior.

Key numbers:

- 51.8 / 48.2 split.
- 2,702 tok/s.
- 178 ms TTFT.

### 002 Add 40 Connections

Message:

> More load without more capacity increases aggregate throughput but worsens individual request experience.

Key numbers:

- +11.4% total tok/s.
- -17.6% per-request tok/s.
- +42 ms TTFT.

### 003 Add `rtx` Node

Message:

> Adding synthetic capacity shifts routing immediately and improves system behavior under comparable load.

Key numbers:

- 33.1 / 33.0 / 33.9 split.
- +28.9% aggregate tok/s vs baseline.
- 65.9% cache hit rate, consistent with two cache lanes out of three.

Avoid saying:

- "Added a GPU."
- "Perfectly simulates hardware."
- "Genuine physical parallelism."

Say instead:

> It added synthetic serving capacity in the control-plane model.

### 004 Add 60 Connections

Message:

> Longer windows give a more stable reading, and the system remains interpretable as load rises.

Key numbers:

- 180 connections for 2 minutes.
- 3,780 requests.
- 3,098 tok/s.
- 50.6 / 49.4 split.
- 206 ms TTFT.

Caveat:

> This run reset `cllm` TPS from 35 to 32, so it is directionally useful but not perfectly apples-to-apples with the first three reports.

## Why It Matters: 27:00-30:00

Talk track:

> For field work, the value is speed and evidence. We can test deployment patterns, capacity assumptions, routing policies, and workload behavior without making every question a GPU procurement exercise.

> For product work, the value is feedback. If a customer scenario breaks because of queueing, admission, cache behavior, or routing, we can reproduce that scenario and turn it into a reusable pattern.

> For AI operations, the value is bounded autonomy. Claude can operate the lab through MCP tools, but the real lane is protected, benchmark windows are bounded, destructive actions are withheld, and the output is metrics-backed.

Close:

> cLLM is not trying to replace real GPU validation. It makes real validation cheaper and more repeatable by separating the control-plane questions from the physical hardware questions. Calibrate once, experiment many times, and keep the real vLLM lane in the loop so the synthetic model stays honest.

## Q&A Prep: 30:00-50:00

### Q: Is this just a load tester?

Answer:

> A load tester drives traffic. cLLM models the serving control plane: node-local admission, token capacity, queueing, routing, degradation, cache replay, and per-node observability. The benchmark client creates pressure; cLLM is the system under experiment.

### Q: Why not just use more GPUs?

Answer:

> We still need real GPUs for calibration and validation. The point is that every scheduling or routing question should not require new hardware. cLLM lets us run the cheap part of the loop quickly, then spend GPU time where it actually validates a decision.

### Q: How accurate is it?

Answer:

> It is accurate at the control-plane envelope level, not the kernel level. It models token admission, per-request pacing, concurrency degradation, cache behavior, queueing, and routing. It does not claim to model CUDA kernels, detailed KV-cache allocator behavior, or vLLM batch-scheduler internals. The real vLLM lane is kept in the same benchmark window so drift is visible.

### Q: What does the real vLLM lane prove?

Answer:

> It gives us a ground-truth lane in the same environment, under the same benchmark pressure. The synthetic lane has to stay close enough to the real lane for the class of questions we are asking. If it diverges, the dashboard shows it and we recalibrate.

### Q: Why cache replay?

Answer:

> Cache replay decouples workload identity from execution behavior. Once a real response is captured, the same prompt/response shape can be replayed with controlled pacing, jitter, prefill, routing, and fault behavior. That makes experiments reproducible.

### Q: Why tokens instead of requests?

Answer:

> Because LLM serving cost scales with prompt and completion tokens. One large context request can consume what would otherwise be many small requests. If we admit by request count, we misallocate capacity and hide noisy-neighbor behavior.

### Q: What is the MCP layer actually adding?

Answer:

> It turns the platform into an operator workflow. Claude can inspect the fleet, run bounded benchmarks, add synthetic capacity, and produce a report. It is not arbitrary automation; the tool surface is deliberately narrow and evidence-oriented.

### Q: Why is delete not an MCP tool?

Answer:

> On purpose. For v1, Claude can create/update synthetic nodes and run bounded experiments. Deletion is more destructive, so I kept it out until there is confirmation, protected-node policy, audit logging, and tombstone handling for metrics.

### Q: What are the product implications?

Answer:

> This could become a field validation harness for edge AI and private inference: repeatable benchmark scenarios, reusable workload snapshots, deployment-readiness reports, and topology experiments before investing in hardware or customer rollout changes.

### Q: What would you do next?

Answer:

> Three things. First, clean up metrics lifecycle and tombstoning for deleted nodes. Second, add Prometheus range-query support so reports are less dependent on before/after snapshots. Third, build a small scenario library: baseline, noisy neighbor, add capacity, degrade backend, queue deadline, and cache replay validation.

### Q: What are the current limitations?

Answer:

> Synthetic nodes model the envelope, not the hardware internals. Deleted-node metric series need cleanup. Some comparisons are directionally useful but need controlled config consistency. And the MCP layer should remain conservative until audit and confirmation flows are stronger.

## Backup Plan

If the live demo misbehaves:

1. Open `reports/001-baseline.md`.
2. Open `reports/002-add-40-connections.md`.
3. Open `reports/003-add-rtx-node.md`.
4. Show the Grafana dashboard with current metrics.
5. Use the reports as recorded experiment evidence.

Say:

> The live system is useful, but the real artifact is the repeatable experiment report. Here are the exact prompts, tools, topology, metrics, conclusions, and caveats from the last clean runs.

## Rehearsal Checklist

- Confirm MCP server is connected from Claude Code or Codex.
- Confirm `/nodes` shows only expected active nodes before starting.
- Confirm no leftover `rtx` node unless the demo wants it present.
- Confirm benchmark log path is readable.
- Confirm dashboard loads.
- Confirm `vllm` is protected in the MCP output.
- Run one short smoke benchmark before the meeting.
- Keep report files open as backup.

## Final One-Liner

> cLLM lets us use one real GPU lane to keep the experiment honest, then use synthetic lanes to explore the operational questions that usually make LLM deployments slow, expensive, and hard to reason about.
