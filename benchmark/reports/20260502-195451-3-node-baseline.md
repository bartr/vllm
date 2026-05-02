# Scenario: 3-node-baseline

**3-node fleet (cllm + rtx + vllm), baseline traffic (180 connections = 60/node), prompts.yaml, max-tokens 100**

**Date:** 2026-05-02 19:54:51 UTC  
**Duration:** 60s  
**Warmup:** 15s  
**Scenario hash:** `6cf449754176`  
**Scenario file:** `benchmark/scenarios/3-node-baseline.yaml`

**Tags:** 3-node, baseline-traffic, rtx

## Prompt

> Run scenario `3-node-baseline` and summarize results.

## Tools Invoked

1. `run_scenario(name="3-node-baseline")`

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

**Command:** `ask --bench 180 --duration 60s --files /home/bartr/vllm/scripts/prompts.yaml --max-tokens 100`  
**Log:** `benchmark/logs/20260502-195451-3-node-baseline-default.log`

| Metric | Value |
|--------|-------|
| Total rows | 1979 |
| Warmup rows excluded | 540 |
| Cache hit rate | 49.8% |
| Avg TTFT | 231.0 ms |
| Avg req tok/s | 15.06 |
| Avg total tok/s | 3079.2 |

## Traffic Split (windowed admission deltas)

| Node | Delta | Share |
|------|-------|-------|
| `cllm` | +991 | 50.1% |
| `vllm` | +988 | 49.9% |
| **Total** | **+1,979** | |

## Caveats

- Admission counts are lifetime counters; windowed deltas are used here.
- Ghost metric series for deleted nodes may appear with zero window deltas.
- For time-windowed conclusions, use Prometheus range queries or the Grafana dashboard.
- Dashboard: <http://192.168.68.63:3000/d/cllm-overview/cllm-overview>
