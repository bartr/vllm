# 001 — Baseline: 2 Nodes, 120 Connections

**Date:** 2026-05-02  
**Log:** `~/logs/bench-window-20260502-032927.log`

## Prompt

> Baseline 120 connections, 2 nodes — run a 60-second benchmark window and summarize results.

## Tools Invoked

1. `run_benchmark_window(duration_seconds=60, concurrency=120)`

## Topology (Before → After)

| Node | Class | Max Concurrency | TPS (configured) | Max Tokens In Flight | Protected |
|------|-------|-----------------|------------------|----------------------|-----------|
| `cllm` | cllm | 128 | 35 | 200,000 | No |
| `vllm` | vllm | — | — | 200,000 | Yes |

No topology changes during this window.

## Benchmark Window

| Field | Value |
|-------|-------|
| Duration | 60 s |
| Concurrency | 120 |
| Command | `ask --bench 120 --duration 60s --files scripts/prompts.yaml --max-tokens 100` |
| Started | 2026-05-02T03:29:27Z |
| Completed | 2026-05-02T03:30:27Z |
| Warmup rows excluded | 480 |

## Key Metrics

### Traffic Split (windowed, admission deltas)

| Node | Delta | Share |
|------|-------|-------|
| `cllm` | +871 | 51.8% |
| `vllm` | +809 | 48.2% |
| **Total** | **+1,680** | |

### Performance

| Metric | Value |
|--------|-------|
| Total requests completed | 1,680 |
| Avg total throughput | 2,702.3 tok/s |
| Avg req throughput | 20.43 tok/s |
| Avg TTFT | 178.4 ms |
| Window cache hit rate | 52.2% |

### Node State (after)

| Node | Tokens In Flight | Concurrent Requests | Effective TPS |
|------|-----------------|---------------------|---------------|
| `cllm` | 0 | 0 | 35.0 |
| `vllm` | 0 | — | — |

## Conclusion

With two equal-capacity nodes under least-loaded routing, traffic split near-evenly at **51.8% / 48.2%** — the slight lean toward `cllm` reflects its faster cache-served completions freeing the lane sooner. System throughput was **2,702 tok/s** with a **178 ms** average TTFT at this load level. Cache hit rate of 52.2% reflects that roughly half of requests were routed to `cllm` (cache-enabled) and half to `vllm` (passthrough).

This run establishes the reference baseline for experiments 002 and 003.

## Caveats

- Admission counts are Prometheus lifetime counters; windowed deltas are used here for split calculation.
- `per_request_tps_effective` of 35 on `cllm` reflects the configured value after an earlier `update_node` (+10%) in this session.
- Ghost metric series for `cllm-smoke`, `cllm-test`, and `rtx` (deleted nodes) appear in Prometheus but carry zero window deltas.
- Dashboard: <http://192.168.68.63:3000/d/cllm-overview/cllm-overview>
