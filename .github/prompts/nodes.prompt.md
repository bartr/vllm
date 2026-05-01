---
mode: agent
description: Iterate on cllm node configuration (`clusters/z01/cllm/nodes-configmap.yaml`), per-node calibration vs measured vLLM, dashboard panels that read per-node series, and the smoke-test fixture. Use when tuning `per_request_tokens_per_second`, `degradation_threshold`, `max_concurrency`, prefill knobs, KV gates, adding/removing/renaming a node, or proving a config change against the live cluster.
---

# Nodes — config, benchmarking, dashboards, smoke

Scope: everything that touches "what nodes does cllm expose, how are they shaped, and how do we prove they behave as configured." **Do not** change admission-gate code, DSL parser, KV math, scheduler, or release plumbing in this session — start a new chat for those (`admission-feature.prompt.md` / `release.prompt.md`).

## Pin these facts first
- Repo: `/home/bartr/vllm`. Cluster: k3s on z01, namespace `cllm`. Traefik on `:8088`. Service `cllm` port 8080. Latest tag: **0.14.0**; `bartr` dev = **0.15.0**; cluster pod runs 0.15.0.
- Node config starts from `clusters/z01/cllm/nodes-configmap.yaml` (mounted at `/etc/cllm/nodes.yaml` via `CLLM_NODES_FILE`) and can be edited live through `/nodes`. Per-node knobs (`per_request_tokens_per_second`, `degradation_threshold`, `max_concurrency`, KV) belong to the node fleet surface, not `/config`.
- Loader = [cllm/internal/node/loader.go](cllm/internal/node/loader.go). Yaml v3 default — unknown fields are silently dropped. Stale fields like `max_tokens_per_second:` (retired 0.14.0) survive in the ConfigMap as comments-by-accident; harmless but worth cleaning up when you're already editing the file.
- Three nodes today (one commented out):
  - `cllm-rtx`: cacheable / paced lane. `per_request_tokens_per_second=32`, `degradation_threshold=10`, `max_concurrency=128`, class `cllm-rtx` carries `max_degradation=60`, `prefill_rate_multiplier=4`. Calibrated against measured vLLM Qwen2.5-3B (see §"Calibration data" below).
  - `vllm`: pure proxy / "real GPU" baseline. `per_request_tokens_per_second=0`, `max_concurrency=0`. Opts out of pacing AND admission gating.
  - `kv-node` (currently commented in the configmap): KV-pressure demo. NOT used by dashboard load; ONLY `scripts/smoke-test.yaml` targets it.
- All three lanes proxy to the SAME single vLLM pod. Different cllm "nodes" = different admission lanes in front of one shared GPU. TTFT/latency claims that frame "cllm vs raw GPU" are invalid; admission/queue/cache claims are valid.

## What lives where
| Concern | File |
|---|---|
| Node + class + router config (the ConfigMap) | [clusters/z01/cllm/nodes-configmap.yaml](clusters/z01/cllm/nodes-configmap.yaml) |
| Loader + validation | [cllm/internal/node/loader.go](cllm/internal/node/loader.go) |
| Loader tests + yaml-doc comment | [cllm/internal/node/loader_test.go](cllm/internal/node/loader_test.go) |
| Node runtime (Capacity, PerRequestRate, ConcurrentRequests, TokenBudget) | [cllm/internal/node/node.go](cllm/internal/node/node.go) |
| Per-node Prom gauges (admissions, in-flight, fill, KV, per-request TPS, max-conc, combined-load) | [cllm/internal/httpapi/node_metrics.go](cllm/internal/httpapi/node_metrics.go) |
| Smoke fixture (22 prompts) | [scripts/smoke-test.yaml](scripts/smoke-test.yaml) |
| Dashboard-load harness (per-lane parallel workers) | [scripts/dashboard-load.sh](scripts/dashboard-load.sh) |
| Plain prompts (no DSL) | [scripts/prompts.yaml](scripts/prompts.yaml) |
| Grafana dashboard | [clusters/z01/grafana/dashboards/cllm-overview.json](clusters/z01/grafana/dashboards/cllm-overview.json) |
| Dashboard reimport | `clusters/z01/grafana/scripts/run-dashboard-import-job.sh` |

## Editing the ConfigMap — apply path
Flux is **not** used. After every edit:
```sh
kubectl apply -k clusters/z01/cllm/
kubectl -n cllm rollout restart deployment/cllm
kubectl -n cllm rollout status deployment/cllm --timeout=90s
```
Cache is in-memory; restart empties it. Allow ~2 min on `dashboard-load.sh` before declaring lane divergence.

Verify the new config loaded:
```sh
kubectl -n cllm port-forward svc/cllm 8088:8080 &
curl -s localhost:8088/config | python3 -m json.tool | head -40
curl -s localhost:8088/nodes | python3 -m json.tool | head -80
curl -s localhost:8088/metrics | grep -E '^cllm_node_(per_request_tps_effective|max_concurrency|combined_load)' | sort
```
The metric set should match the configmap one-to-one. Passthrough nodes (`per_request_tps=0` AND `max_concurrency=0`) deliberately emit NO `cllm_node_max_concurrency` / `cllm_node_per_request_tps_effective` rows.

## Calibration data — measured vLLM (Qwen2.5-3B, max-num-seqs=128, max-num-batched-tokens=2048, GPU mem 0.90)
| concurrency | tok/s/req | TTFT ms | duration ms |
|---:|---:|---:|---:|
| 1   | 32.4 | 40  | 3080 |
| 10  | 31.2 | 101 | 3209 |
| 20  | 29.1 | 181 | 3468 |
| 40  | 25.5 | 248 | 3921 |
| 80  | 18.5 | 333 | 5562 |
| 120 | 14.0 | 500 | 7369 |
| 130 | 12.0 | 641 | 8461 |
| 256 | ~5   | --  | ~16000 (saturation) |

Linear-fit recipe for any new modeled lane that fronts this same GPU: `threshold` = where degradation begins (≈10 here), `max_concurrency` = vLLM `max-num-seqs` ceiling (128), `max_degradation` = `(1 - floor/base) × 100` so that at `c=max_concurrency` the per-request rate matches the measured floor. Predicted vs measured fits within 11% mid-curve, <7% at endpoints.

`prefill_rate_multiplier` is per-class (`classes:` block, not `nodes:`). It scales prefill against the effective decode rate so TTFT tracks per-node concurrency. Current cllm-rtx uses `4` (prefill 4× faster than decode), giving ~40ms@c=1 → ~500ms@c=128.

## Smoke test — canonical fixture
`scripts/smoke-test.yaml` covers every admission path in 22 prompts:
- 1-11: KV ladder + `kv-cost=`/`no-kv` directives, all pinned `node=kv-node`.
- 12-22: pinned `node=cllm` (KV-disabled) to exercise workload-class, max-queue-ms, phase-aware DSL, priority, max-ttft-ms without inflating the kv-node estimator.

Run:
```sh
~/go/bin/ask --bench 1 --files scripts/smoke-test.yaml
```
Expect 21/22 ok; the single 429 with reason `class_ttft_budget` on prompt 22 is intentional. Throughput at `defaultPerRequestTPS=32` should sit at 32–33 tok/s with TTFT p50 ≈ 60–95 ms.

If `node=kv-node` is commented out in the configmap and the fixture still pins to it, prompts 1-11 will route to the implicit fallback. Either uncomment the kv-node entry before smoke OR repin the KV-bearing prompts. Repinning belongs in this session; toggling the gate logic does not.

**MANDATORY** post-smoke cleanup (kv-node leakage):
```sh
PROM=$(kubectl -n monitoring get svc prometheus -o jsonpath='{.spec.clusterIP}'):9090
curl -s -X POST -g "http://$PROM/api/v1/admin/tsdb/delete_series?match[]={node=\"kv-node\"}"
curl -s -X POST    "http://$PROM/api/v1/admin/tsdb/clean_tombstones"
```

## Benchmarking — proving a node config change
Three valid harnesses, in order of "what's the question":
1. **Single-lane sanity** — `~/go/bin/ask --bench N --files scripts/prompts.yaml --dsl "node=<lane> no-cache" --max-tokens 100`. One lane, fixed concurrency, no DSL pinning beyond the `--dsl` flag. Measures real per-request tok/s, TTFT, duration. Use for vLLM-baseline numbers and for verifying a paced-lane configured rate matches measurement.
2. **Multi-lane dashboard view** — `scripts/dashboard-load.sh`. Parallel per-lane workers; required because a single shared worker pool collapses lane differences (slower lane accumulates concurrency, fleet TPS = c × per-req cancels).
3. **A/B harness** — `scripts/ab-vllm-vs-cllm.sh`. Direct head-to-head; framing limited to admission/queue/cache claims (see A/B caveat above).

Choose harness based on the claim being made. NEVER use `smoke-test.yaml` for verification — it pollutes fleet panels.

## Dashboard expectations after node changes
Per-node panels read these series; the legends / `sum by (node, ...)` selectors will pick up new node labels automatically but old node labels persist as zombie series until tombstoned:
- `cllm_node_admissions_total{node, class, result}`
- `cllm_node_tokens_in_flight{node, class}`
- `cllm_node_max_tokens_in_flight{node, class}`
- `cllm_node_concurrent_requests{node, class}`
- `cllm_node_max_concurrency{node, class}` (gated on `Concurrency != nil`)
- `cllm_node_per_request_tps_effective{node, class}` (gated on `PerRequestTPS > 0`)
- `cllm_node_combined_load{node, class}`
- `cllm_node_kv_occupancy{node, class}` and `cllm_node_kv_max_tokens{node, class}` (KV-only)
- `cllm_node_kv_estimator_p95{node, class}` (warm-only)

After renaming or removing a node, tombstone its old label:
```sh
PROM=$(kubectl -n monitoring get svc prometheus -o jsonpath='{.spec.clusterIP}'):9090
curl -s -X POST -g "http://$PROM/api/v1/admin/tsdb/delete_series?match[]={node=\"<old-name>\"}"
curl -s -X POST    "http://$PROM/api/v1/admin/tsdb/clean_tombstones"
```
Then `cd clusters/z01/grafana/scripts && ./run-dashboard-import-job.sh` if dashboard JSON changed. Never restart Prometheus or Grafana — TSDB persists; restart costs a data gap and fixes nothing the targeted tombstone already fixes.

## Common gotchas (already learned, don't relearn)
- **Pacing only applies to cache hits.** `cachedReplayDelay` is inside the cache-hit branch. `no-cache` streams from vLLM unpaced regardless of `per_request_tokens_per_second`. So a "real GPU baseline" lane needs `per_request_tps=0 AND max_concurrency=0`, not just one of them.
- **Stale `max_tokens_per_second:` in the ConfigMap.** The 0.14.0 retirement removed this field from the loader. Yaml v3 silently drops unknown keys, so it's harmless, but the value is misleading to readers. Strip it next time you're already editing.
- **Implicit fallback Node** (no `nodes.yaml`) seeds `Capacity.PerRequestTPS = 32` from `runtimeconfig.DefaultMaxTokensPerSecond`. Don't hand-test a TTFT/pacing assertion expecting the legacy 100 tok/s baseline.
- **Loader does not enforce `degradation_threshold ≤ max_concurrency`.** A misconfigured node with threshold > max_concurrency still loads but produces a degenerate `f(c)` curve. Validate by hand or add a loader test before the next configmap edit.
- **Startup class names must match between `nodes:` and `classes:`** blocks. Runtime `/nodes` edits use effective node values directly. Verify after edits with `curl /nodes`.

## Out of scope here (start a new chat)
- Admission-gate code, DSL parser, KV math, router, scheduler internals (anything under `cllm/internal/{httpapi,node/cost.go,node/budget.go,router}` that's not just metric labels).
- Cutting a release / version bump (`.github/prompts/release.prompt.md`).
- Streaming behavior debugging (`.github/prompts/debug-streaming.prompt.md`).
- Adding panels or refactoring queries that aren't per-node specific (`.github/prompts/benchmarks-dashboards.prompt.md`).

## Communication style
Brief. Read the ConfigMap, the loader, and `node.go` before editing. Apply with `kubectl apply -k`. Verify with `/config` + `/metrics`. Tombstone any zombie labels. Validate JSON if dashboard changes. Never narrate "now I will…".
