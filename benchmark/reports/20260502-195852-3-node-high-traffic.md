# Scenario: 3-node-high-traffic

**3-node fleet (cllm + rtx + vllm), 1.5x traffic (270 connections = 90/node), prompts.yaml, max-tokens 100**

**Date:** 2026-05-02 19:58:55 UTC  
**Duration:** 60s  
**Warmup:** 15s  
**Scenario hash:** `5fcde2a69e54`  
**Scenario file:** `benchmark/scenarios/3-node-high-traffic.yaml`

**Tags:** 3-node, high-traffic, 1.5x, rtx

## Prompt

> Run scenario `3-node-high-traffic` and summarize results.

## Tools Invoked

1. `run_scenario(name="3-node-high-traffic")`

## Topology

| Node | TPS (effective) | Max Concurrency | Max Tokens In Flight | Protected |
|------|-----------------|-----------------|----------------------|-----------|
| `cllm` | 32.0 | 128 | 200,000 | No |
| `vllm` | — | 0 | 200,000 | Yes |

### Node Overrides Applied

- `cllm.max_tokens_in_flight`: set to `200000` for this run, restored after
- `cllm.max_waiting_requests`: set to `32` for this run, restored after
- `cllm.per_request_tokens_per_second`: set to `32` for this run, restored after
- `cllm.degradation_threshold`: set to `10` for this run, restored after
- `cllm.max_concurrency`: set to `128` for this run, restored after
- `cllm.max_degradation`: set to `60` for this run, restored after
- `cllm.prefill_rate_multiplier`: set to `10` for this run, restored after
- `cllm.prefill_base_overhead_ms`: set to `30` for this run, restored after
- `cllm.prefill_jitter_percent`: set to `10` for this run, restored after
- `cllm.prefill_max_ms`: set to `800` for this run, restored after

Nodes restored: `cllm`

## Groups

### default

**Command:** `ask --bench 270 --duration 60s --files /home/bartr/vllm/scripts/prompts.yaml --max-tokens 100`  
**Log:** `benchmark/logs/20260502-195852-3-node-high-traffic-default.log`

| Metric | Value |
|--------|-------|
| Total rows | 2761 |
| Warmup rows excluded | 810 |
| Cache hit rate | 63.0% |
| Avg TTFT | 258.8 ms |
| Avg req tok/s | 13.83 |
| Avg total tok/s | 4316.5 |

## Traffic Split (windowed admission deltas)

| Node | Delta | Share |
|------|-------|-------|
| `cllm` | +887 | 47.5% |
| `vllm` | +982 | 52.5% |
| **Total** | **+1,869** | |

## Caveats

- Admission counts are lifetime counters; windowed deltas are used here.
- Ghost metric series for deleted nodes may appear with zero window deltas.
- For time-windowed conclusions, use Prometheus range queries or the Grafana dashboard.
- Dashboard: <http://192.168.68.63:3000/d/cllm-overview/cllm-overview>
