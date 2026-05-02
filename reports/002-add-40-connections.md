# 002 — Add 40 Connections: 2 Nodes, 160 Connections

**Date:** 2026-05-02  
**Log:** `~/logs/bench-window-20260502-033033.log`

## Prompt

> Add 40 connections to the existing benchmark for 1 minute. Summarize latency, throughput, and cache changes vs baseline.

## Tools Invoked

1. `run_benchmark_window(duration_seconds=60, concurrency=160)`

## Topology (Before → After)

Unchanged from baseline — 2 nodes, same configuration.

| Node | Class | Max Concurrency | TPS (configured) | Max Tokens In Flight | Protected |
|------|-------|-----------------|------------------|----------------------|-----------|
| `cllm` | cllm | 128 | 35 | 200,000 | No |
| `vllm` | vllm | — | — | 200,000 | Yes |

## Benchmark Window

| Field | Value |
|-------|-------|
| Duration | 60 s |
| Concurrency | 160 (+40 vs baseline) |
| Command | `ask --bench 160 --duration 60s --files scripts/prompts.yaml --max-tokens 100` |
| Started | 2026-05-02T03:30:33Z |
| Completed | 2026-05-02T03:31:33Z |
| Warmup rows excluded | 480 |

## Key Metrics

### Traffic Split (windowed, admission deltas)

| Node | Delta | Share |
|------|-------|-------|
| `cllm` | +1,004 | 53.5% |
| `vllm` | +871 | 46.5% |
| **Total** | **+1,875** | |

### Performance vs Baseline

| Metric | Baseline (120) | This Run (160) | Change |
|--------|---------------|----------------|--------|
| Total requests | 1,680 | 1,875 | +11.6% |
| Avg total tok/s | 2,702.3 | 3,011.3 | **+11.4%** |
| Avg req tok/s | 20.43 | 16.83 | −17.6% |
| Avg TTFT | 178.4 ms | 220.4 ms | **+42.0 ms (+23.5%)** |
| Window cache hit rate | 52.2% | 53.8% | +1.6 pp |

### Node State (after)

| Node | Tokens In Flight | Concurrent Requests | Effective TPS |
|------|-----------------|---------------------|---------------|
| `cllm` | 0 | 0 | 35.0 |
| `vllm` | 0 | — | — |

## Conclusion

Adding 40 connections (+33% load) produced clear and expected trade-offs:

**Throughput increased** — system completed 11.6% more requests and sustained 11.4% higher aggregate token throughput. The additional connections found capacity headroom on both lanes.

**Per-request latency degraded** — TTFT rose by 42 ms (+23.5%) and per-request token rate fell 17.6%. At 160 concurrent connections, `cllm`'s degradation model is active (threshold=10, max_degradation=60%, max_concurrency=128). With ~86 of the 160 connections routed to `cllm` (~53%), the lane is well above its degradation onset point, reducing individual request speed even as aggregate output rises.

**Traffic split held steady** — 53.5% / 46.5% is consistent with baseline (51.8% / 48.2%). The extra connections distributed proportionally with no routing imbalance.

**Cache hit rate was unchanged** — 53.8% vs 52.2% is within noise. The ratio of cache-enabled (`cllm`) to passthrough (`vllm`) traffic didn't materially change, so cache behavior is stable.

## Caveats

- At 160 concurrency the `cllm` node is absorbing ~86 concurrent requests against a `max_concurrency` of 128 — there is still headroom, but the degradation curve is active.
- For higher-load tests, consider whether `max_concurrency` or `max_tokens_in_flight` become the binding constraint before the degradation curve.
- Dashboard: <http://192.168.68.63:3000/d/cllm-overview/cllm-overview>
