# Scenario: capacity-limit

**2-node fleet, 140 connections/node (280 total) vs max_concurrency=128 — forces queue wait**

**Date:** 2026-05-02 20:40:54 UTC  
**Duration:** 180s  
**Warmup:** 15s  
**Scenario hash:** `483263c95893`  
**Scenario file:** `benchmark/scenarios/capacity-limit.yaml`

**Tags:** 2-node, capacity, queue-wait

## Prompt

> Run scenario `capacity-limit` and summarize results.

## Tools Invoked

1. `run_scenario(name="capacity-limit")`

## Topology

| Node | TPS (effective) | Max Concurrency | Max Tokens In Flight | Protected |
|------|-----------------|-----------------|----------------------|-----------|
| `cllm` | 32.0 | 128 | 200,000 | No |
| `vllm` | — | 128 | 200,000 | Yes |

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

**Command:** `ask --bench 280 --duration 180s --files /home/bartr/vllm/scripts/prompts.yaml --max-tokens 100`  
**Log:** `benchmark/logs/20260502-204051-capacity-limit-default.log`

| Metric | Value |
|--------|-------|
| Total rows | 6127 |
| Warmup rows excluded | 668 |
| Cache hit rate | 46.0% |
| Avg TTFT | 1005.9 ms |
| Avg req tok/s | 11.55 |
| Avg total tok/s | 3291.4 |

## Traffic Split (windowed admission deltas)

| Node | Delta | Share |
|------|-------|-------|
| `cllm` | +2,825 | 46.1% |
| `vllm` | +3,298 | 53.9% |
| **Total** | **+6,123** | |

## Caveats

- Admission counts are lifetime counters; windowed deltas are used here.
- Ghost metric series for deleted nodes may appear with zero window deltas.
- For time-windowed conclusions, use Prometheus range queries or the Grafana dashboard.
- Dashboard: <http://192.168.68.63:3000/d/cllm-overview/cllm-overview>
