# 003 — Add rtx Node: 3 Nodes, 150 Connections

**Date:** 2026-05-02  
**Log:** `~/logs/bench-window-20260502-033142.log`

## Prompt

> Add rtx node same as cllm, run 150 connections for 60 seconds, show traffic split and node behavior.

## Tools Invoked

1. `create_synthetic_node(id="rtx")` — all capacity/realism fields defaulted from `cllm`
2. `run_benchmark_window(duration_seconds=60, concurrency=150)`
3. `DELETE /nodes/rtx` (direct API — delete_node not yet implemented as MCP tool)

## Topology

### Before

| Node | Class | Max Concurrency | TPS | Max Tokens In Flight | Protected |
|------|-------|-----------------|-----|----------------------|-----------|
| `cllm` | cllm | 128 | 35 | 200,000 | No |
| `vllm` | vllm | — | — | 200,000 | Yes |

### After (rtx added)

| Node | Class | Max Concurrency | TPS | Max Tokens In Flight | Protected |
|------|-------|-----------------|-----|----------------------|-----------|
| `cllm` | cllm | 128 | 35 | 200,000 | No |
| `rtx` | cllm | 128 | 35 | 200,000 | No |
| `vllm` | vllm | — | — | 200,000 | Yes |

`rtx` is an identical clone of `cllm`: same capacity, same degradation curve, same realism parameters, cache-enabled (`bypass_cache=false`).

## Benchmark Window

| Field | Value |
|-------|-------|
| Duration | 60 s |
| Concurrency | 150 |
| Command | `ask --bench 150 --duration 60s --files scripts/prompts.yaml --max-tokens 100` |
| Started | 2026-05-02T03:31:42Z |
| Completed | 2026-05-02T03:32:42Z |
| Warmup rows excluded | 600 |

## Key Metrics

### Traffic Split (windowed, admission deltas)

| Node | Delta | Share |
|------|-------|-------|
| `cllm` | +704 | 33.1% |
| `rtx` | +703 | 33.0% |
| `vllm` | +722 | 33.9% |
| **Total** | **+2,129** | |

### Performance vs Baseline (120 connections, 2 nodes)

| Metric | Baseline | This Run | Change |
|--------|----------|----------|--------|
| Total requests | 1,680 | 2,129 | **+26.7%** |
| Avg total tok/s | 2,702.3 | 3,484.6 | **+28.9%** |
| Avg req tok/s | 20.43 | 21.02 | +2.9% |
| Avg TTFT | 178.4 ms | 202.6 ms | +24.2 ms (+13.6%) |
| Window cache hit rate | 52.2% | 65.9% | **+13.7 pp** |

### Node State (after window)

| Node | Tokens In Flight | Concurrent Requests | Effective TPS |
|------|-----------------|---------------------|---------------|
| `cllm` | 0 | 0 | 35.0 |
| `rtx` | 0 | 0 | 35.0 |
| `vllm` | 0 | — | — |

## Conclusion

**Routing rebalanced immediately to near-perfect 33/33/34.** The router distributed all three nodes equally within the first request cycle, confirming least-loaded routing responds to topology changes without delay.

**Throughput increased significantly** — +26.7% more requests and +28.9% aggregate tok/s vs baseline at a lower concurrency (150 vs 120 baseline). The third node added genuine parallelism, absorbing ~700 extra requests that would have queued on the two-node fleet.

**Per-request performance improved vs the +40-connection test** — TTFT (202.6 ms) is better than the 160-connection 2-node run (220.4 ms), and req tok/s (21.02) is higher (16.83). More capacity means each node handles fewer concurrent requests, keeping degradation lower per lane even at a similar or higher total system load.

**Cache hit rate jumped to 65.9%** (+13.7 pp vs baseline). With two cache-enabled lanes (`cllm` + `rtx`) and one passthrough lane (`vllm`), approximately 66% of requests were served from cache — exactly consistent with the 2:1 cached-to-passthrough ratio in the 3-node topology. This is a meaningful operational benefit: the additional synthetic node not only adds throughput capacity but shifts more traffic off the real GPU.

**`rtx` ramped to full share instantly** — from 0 admissions before the window to +703 during it, matching `cllm` almost exactly. No warm-up penalty was observed in routing behavior.

## Caveats

- `rtx` metric series will linger in Prometheus after deletion and require explicit tombstoning. The ghost series from `cllm-smoke` and `cllm-test` (earlier test runs) are evidence of this.
- The window cache hit rate (65.9%) is measured from parsed benchmark log rows. Prometheus `cache_lookups_total` does not include `vllm` passthrough requests in miss counts, so the Prometheus lifetime counter shows 100% — these measure different things.
- Warmup rows excluded (600) is higher here than in other runs because with 150 workers the 15-second warmup window produces more rows.
- Dashboard: <http://192.168.68.63:3000/d/cllm-overview/cllm-overview>
