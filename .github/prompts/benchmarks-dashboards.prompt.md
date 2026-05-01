---
mode: agent
description: Iterate on cllm benchmarks (`scripts/dashboard-load.sh`, prompt fixtures, ab scripts) and Grafana dashboards in `clusters/z01/grafana/dashboards/`. Use when the user wants to add panels, refactor queries, change load patterns, calibrate per-node behavior, or interpret what a panel is showing.
---

# Benchmarks + Dashboards

This prompt scopes a session to load-generation fixtures and Grafana dashboards. **Do not** touch admission-gate code, DSL, or release plumbing in this session — start a new conversation for those.

## Pin these facts first
- Repo: `/home/bartr/vllm`. Cluster: k3s on z01, namespace `cllm`. External entrypoint Traefik on `:8088`. cllm `/version` should be the latest `bartr` minor.
- Three nodes today (`clusters/z01/cllm/nodes-configmap.yaml`):
  - `cllm-rtx`: cacheable lane. `per_request_tokens_per_second=40`, `degradation_threshold=32`, `max_concurrency=64`, `max_waiting_requests=64`. Pacing only fires on cache HITS.
  - `vllm`: pure proxy / "real GPU" baseline. `per_request_tokens_per_second=0`, `max_concurrency=0`. Opts out of pacing AND admission gating.
  - `kv-node`: KV demo. NOT used by the dashboard fixture; only by `scripts/smoke-test.yaml`.
- Both lanes proxy to the SAME single vLLM pod (`vllm.vllm.svc.cluster.local:8000`, Qwen/Qwen2.5-3B-Instruct). Different cllm "nodes" = different admission lanes, ONE shared GPU. TTFT/latency claims that frame this as "cllm vs raw GPU" are invalid; admission/queue/cache claims are valid.
- Cache is in-memory. Every `kubectl rollout restart deploy/cllm` empties it; allow ~2 min on `dashboard-load.sh` for warm-up before declaring divergence.

## Canonical assets
- `scripts/dashboard-load.sh` — parallel per-lane workers (one `ask --bench --loop` per lane). REQUIRED structure: lanes need their own worker pool, otherwise fleet TPS = concurrency × per-req cancels and lanes look identical.
- `scripts/prompts.yaml` — 24 plain prompts, NO `:dsl` directives. Lane pinning is done by `dashboard-load.sh` workers.
- `scripts/smoke-test.yaml` — 22 prompts; targets `node=kv-node` for prompts 1-11. **NEVER** use this for dashboard verification — it leaks `kv-node` series into fleet panels. Use `prompts.yaml` or plain `ask --bench N "hello"` instead.
- `scripts/ab-vllm-vs-cllm.sh` — direct A/B harness.
- `clusters/z01/grafana/dashboards/cllm-overview.json` — the cllm dashboard. Per-node measurement model.
- `clusters/z01/grafana/scripts/run-dashboard-import-job.sh` — upserts all dashboards into Grafana.

## Dashboard layout invariants
Top section is per-node "Fleet · …" panels. Order (rows of `h: 8`):
- y=0: Fleet · Active Requests | Fleet · Waiting Requests | Fleet · Cache Hits | Cache Entries  (each w=6)
- y=8: Fleet · Tokens/s By Node | Fleet · Queue Wait By Node (p95)  (each w=12)
- y=16: Fleet · Time to First Token By Node (p95) | Fleet · Latency By Node (p95)
- y=24: Fleet · Tokens In Flight By Node | Fleet · Admission Rate By Node
- y=32: Fleet · Fill Ratio By Node | Fleet · KV Occupancy By Node
- y=40: Fleet · Combined Load By Node | Fleet · Cost vs KV Load
- y=48+: Request Status, HTTP Requests, Admission · Rejection Reasons, Phase · …, KV · Estimator p95

Rules when adding/changing panels:
- "By Node" → `sum by (node[, class]) (...)`, legendFormat `{{node}}` (or `{{node}} ({{class}})`), `(p95)` suffix on quantile titles.
- Cache-hit-style ratios that can have an empty numerator on one node need `(num) or (denom) * 0` to keep zero series alive (otherwise Prometheus drops the line and the panel only shows the hot lane).
- Histogram p95 with low traffic needs fine buckets in the `0.5..5s` band — current buckets include `1.5, 2, 2.5, 3, 4`. Don't widen this without evidence.
- Validate JSON after every edit: `python3 -m json.tool clusters/z01/grafana/dashboards/cllm-overview.json > /dev/null`.
- Reimport after every edit: `cd clusters/z01/grafana/scripts && ./run-dashboard-import-job.sh` (idempotent; safe to spam).

## Verification recipes
- Live metrics: `curl -s localhost:8088/metrics | grep '^cllm_<name>'`. Per-node: `| grep 'node="<n>"'`.
- Prom direct: `PROM=10.43.135.21:9090; curl -s "http://$PROM/api/v1/query?query=<urlencoded>" | jq`.
- Quick fleet snapshot:
  ```sh
  curl -s "http://10.43.135.21:9090/api/v1/query?query=sum%20by%20(node)%20(cllm_node_admissions_total%7Bresult%3D%22admitted%22%7D)" | python3 -c "import sys,json;[print(r['metric']['node'], r['value'][1]) for r in json.load(sys.stdin)['data']['result']]"
  ```

## Cleanup obligations
After ANY verification run that hits `smoke-test.yaml` (or otherwise emits `node=kv-node` / experimental labels):
```sh
PROM=10.43.135.21:9090
curl -s -X POST -g "http://$PROM/api/v1/admin/tsdb/delete_series?match[]={node=\"kv-node\"}"
curl -s -X POST    "http://$PROM/api/v1/admin/tsdb/clean_tombstones"
```
Add additional `match[]=` selectors for any other temporary labels introduced this session.

Do NOT restart Prometheus or Grafana — TSDB is persistent and dashboards live in Grafana's DB; restarts cost a data gap and fix nothing the targeted tombstone + dashboard reimport already fix.

## What's out of scope here (start a new chat)
- Admission-gate code, DSL parser, KV math, router (any change under `cllm/internal/{httpapi,node,router,config}`).
- Cutting a release / version bump (`.github/prompts/release.prompt.md`).
- Streaming behavior debugging (`.github/prompts/debug-streaming.prompt.md`).

## Communication style
Brief. Read first, edit second, validate third, reimport fourth. Never narrate "now I will…". Use `multi_replace_string_in_file` for batches of dashboard edits — gridPos moves and title prefixes are independent and parallel-safe.
