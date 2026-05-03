  # cLLM Operations MCP Server Specification

## Purpose

Build a small, bounded MCP server that lets Claude inspect and operate the cLLM/vLLM experimentation environment through safe tools. The server should expose the existing cLLM control plane, benchmark evidence, and metrics-backed experiment summaries without giving Claude arbitrary shell or cluster access.

The target demo is:

1. Inspect the current cLLM fleet.
2. Observe the long-running `ask --bench` workload.
3. Add or resize synthetic cLLM capacity.
4. Confirm traffic distribution changes through metrics.
5. Generate a short experiment report with links to the dashboards.

This is intentionally an FDE-style artifact: a thin Claude-operable workflow over an already instrumented production-like system.

## Existing Environment

Default cLLM target:

- Base URL: `http://192.168.68.63:8088`
- Nodes API: `http://192.168.68.63:8088/nodes`
- Metrics: `http://192.168.68.63:8088/metrics`
- Cache: `http://192.168.68.63:8088/cache`
- Config: `http://192.168.68.63:8088/config`
- Grafana cLLM dashboard: `http://192.168.68.63:3000/d/cllm-overview/cllm-overview`

The `/nodes`, `/cache`, and `/config` endpoints can return HTML for browser use or JSON when requested with an appropriate `Accept` header. The MCP server must request JSON:

```text
Accept: application/json
```

The default live topology is two nodes:

- `vllm`: real, non-cached vLLM deployment. This lane should be protected.
- `cllm`: cached synthetic lane calibrated to behave similarly to the real vLLM lane.

Under steady unpinned benchmark traffic, two equal-capacity lanes should route approximately 50/50. Adding a third equal synthetic node should shift traffic toward 33/33/33. Resizing the synthetic node to be twice as large should shift routing toward roughly 2:1 synthetic-to-real, depending on active capacity and live admissions.

## Benchmark Context

The benchmark is usually run continuously when the system is not under active development:

```bash
ask --bench 120 --loop --files prompts.yaml --max-tokens 100
```

Recommended long-running log form:

```bash
mkdir -p logs

ask --bench 120 --loop --files prompts.yaml --max-tokens 100 \
  2>&1 | tee -a logs/ask-bench.log
```

The MCP server may use the log for liveness and recent request samples. Prometheus/cLLM metrics should remain the source of truth for experiment conclusions.

Important parser note: `total_tok/s` is intentionally blank for approximately the first 15 seconds of a run because the aggregate math is misleading during warmup. MCP tools must treat blank `total_tok/s` as `warming_up=true`, not as zero throughput.

## Design Principles

- Bounded tools only. Do not expose arbitrary shell execution.
- Prefer cLLM APIs and metrics over scraping dashboards.
- Treat Grafana as the human visual surface; MCP should query metrics directly and return dashboard links.
- Make destructive operations out of scope for v1.
- Protect the real `vllm` lane from mutation unless explicitly enabled in a later version.
- Return structured JSON-like results that Claude can summarize reliably.
- Include enough evidence in every experiment result to let a human verify the conclusion.

## Configuration

The server should be configurable by environment variables:

| Variable | Default | Purpose |
|---|---|---|
| `CLLM_BASE_URL` | `http://192.168.68.63:8088` | cLLM API base URL |
| `CLLM_GRAFANA_URL` | `http://192.168.68.63:3000/d/cllm-overview/cllm-overview` | Human dashboard link |
| `CLLM_BENCH_LOG` | `logs/ask-bench.log` | Optional benchmark log path |
| `CLLM_READ_ONLY` | `false` | When true, disables create/update benchmark-starting tools |
| `CLLM_PROTECT_REAL_NODE` | `true` | Prevents mutation of `vllm` node |
| `CLLM_REQUEST_TIMEOUT_SECONDS` | `10` | HTTP timeout for cLLM calls |
| `CLLM_BENCH_WARMUP_SECONDS` | `15` | Warmup period before aggregate throughput is trusted |

If the server later supports a separate Prometheus base URL, add `PROMETHEUS_URL`. For v1, the `/metrics` endpoint is enough for a narrow set of current-value summaries; range queries can be added if a Prometheus API endpoint is available.

## Tool Set

### `list_nodes`

Return the current node fleet from `GET /nodes`.

Input:

```json
{}
```

Output:

```json
{
  "nodes": [
    {
      "id": "cllm",
      "class": "cllm",
      "bypass_cache": false,
      "max_tokens_in_flight": 200000,
      "max_waiting_requests": 32,
      "per_request_tokens_per_second": 30,
      "degradation_threshold": 10,
      "max_degradation": 50,
      "max_concurrency": 128
    }
  ],
  "protected_nodes": ["vllm"],
  "dashboard_url": "http://192.168.68.63:3000/d/cllm-overview/cllm-overview"
}
```

Notes:

- Always request JSON.
- Preserve unknown fields from cLLM responses where practical.
- Mark `vllm` as protected when `CLLM_PROTECT_REAL_NODE=true`.

### `get_node`

Return one node from `GET /nodes/{id}`.

Input:

```json
{
  "id": "cllm"
}
```

Output:

```json
{
  "node": {
    "id": "cllm",
    "class": "cllm"
  }
}
```

### `create_synthetic_node`

Create a new synthetic cLLM node through `POST /nodes/{id}` or the supported JSON body for the existing API.

Input:

```json
{
  "id": "cllm-2",
  "class": "cllm",
  "max_tokens_in_flight": 200000,
  "max_waiting_requests": 32,
  "per_request_tokens_per_second": 30,
  "degradation_threshold": 10,
  "max_degradation": 50,
  "max_concurrency": 128
}
```

Output:

```json
{
  "created": true,
  "node": {
    "id": "cllm-2",
    "class": "cllm"
  },
  "expected_effect": "With three similarly sized lanes, unpinned least-loaded traffic should move toward an even split across eligible nodes."
}
```

Validation:

- Refuse when `CLLM_READ_ONLY=true`.
- Refuse `id=vllm`.
- Refuse `bypass_cache=true` in v1.
- Enforce bounded numeric ranges matching cLLM's accepted ranges where known.
- Default omitted capacity fields from the existing `cllm` synthetic node when possible.

### `update_node`

Update non-destructive capacity and realism fields for a synthetic node.

Input:

```json
{
  "id": "cllm",
  "max_tokens_in_flight": 400000,
  "per_request_tokens_per_second": 60,
  "max_concurrency": 256
}
```

Output:

```json
{
  "updated": true,
  "node": {
    "id": "cllm",
    "max_tokens_in_flight": 400000,
    "per_request_tokens_per_second": 60,
    "max_concurrency": 256
  },
  "expected_effect": "Increasing the synthetic node capacity should bias least-loaded routing toward the synthetic lane."
}
```

Validation:

- Refuse when `CLLM_READ_ONLY=true`.
- Refuse mutation of `vllm` when `CLLM_PROTECT_REAL_NODE=true`.
- Do not support deleting, disabling, or bypass-cache changes in v1.
- Fetch the node before and after the update and return both if useful.

### `get_config`

Return cLLM runtime configuration from `GET /config`.

Input:

```json
{}
```

Output:

```json
{
  "config": {},
  "dashboard_url": "http://192.168.68.63:3000/d/cllm-overview/cllm-overview"
}
```

### `get_cache_status`

Return cLLM cache state from `GET /cache`.

Input:

```json
{}
```

Output:

```json
{
  "cache": {
    "size": 0,
    "entries": []
  }
}
```

### `get_benchmark_status`

Inspect whether the long-running `ask --bench` workload appears active.

Input:

```json
{
  "tail_lines": 40
}
```

Output:

```json
{
  "running": true,
  "command_hint": "ask --bench 120 --loop --files prompts.yaml --max-tokens 100",
  "log_path": "logs/ask-bench.log",
  "warming_up": false,
  "recent_rows": [
    {
      "thread": 5,
      "tokens": 100,
      "ttft_ms": 53.93,
      "duration_ms": 4236.04,
      "req_tok_s": 23.61,
      "total_tok_s": null,
      "cache": "miss"
    }
  ],
  "notes": [
    "total_tok_s may be blank during benchmark warmup and should not be treated as zero"
  ]
}
```

Implementation notes:

- A process-table check is acceptable for v1.
- The log is a liveness and sample source only.
- Parse rows defensively; headers and startup text should be ignored.
- Blank `total_tok/s` means unknown/warmup, not zero.

### `get_metrics_snapshot`

Fetch and parse `GET /metrics` for a current metrics snapshot.

Input:

```json
{
  "include_raw": false
}
```

Output:

```json
{
  "metrics_url": "http://192.168.68.63:8088/metrics",
  "dashboard_url": "http://192.168.68.63:3000/d/cllm-overview/cllm-overview",
  "node_metrics": [
    {
      "node": "cllm",
      "tokens_in_flight": 0,
      "max_tokens_in_flight": 200000,
      "waiting_requests": 0
    }
  ],
  "observed_metric_names": [
    "cllm_node_tokens_in_flight",
    "cllm_node_max_tokens_in_flight",
    "cllm_node_waiting_requests"
  ]
}
```

Implementation notes:

- Start with the metrics already emitted by cLLM, especially:
  - `cllm_node_tokens_in_flight`
  - `cllm_node_max_tokens_in_flight`
  - `cllm_node_waiting_requests`
  - `cllm_node_admissions_total`
  - `cllm_node_queue_wait_seconds`
  - `cllm_time_to_first_byte_seconds`
  - `cllm_job_duration_seconds`
  - `cllm_cache_lookups_total`
  - `cllm_downstream_request_duration_seconds`
- Counter deltas require either a Prometheus range API or before/after snapshots. For v1, use before/after snapshots inside `run_benchmark_window`.

### `run_benchmark_window`

Run a bounded benchmark for a fixed duration and return metrics before and after the window.

Input:

```json
{
  "duration_seconds": 120,
  "concurrency": 120,
  "files": "prompts.yaml",
  "max_tokens": 100
}
```

Output:

```json
{
  "duration_seconds": 120,
  "concurrency": 120,
  "warmup_seconds": 15,
  "started_at": "2026-05-01T00:00:00Z",
  "completed_at": "2026-05-01T00:02:00Z",
  "command": "ask --bench 120 --files prompts.yaml --max-tokens 100",
  "before": {
    "metrics_snapshot": {}
  },
  "after": {
    "metrics_snapshot": {}
  },
  "summary": {
    "benchmark_completed": true,
    "warming_up_excluded": true,
    "dashboard_url": "http://192.168.68.63:3000/d/cllm-overview/cllm-overview"
  }
}
```

Behavior:

- Refuse when `CLLM_READ_ONLY=true`.
- Enforce `duration_seconds` between 30 and 600 for v1.
- Enforce a bounded concurrency range, for example 1 to 512.
- Do not use `--loop` for a bounded window unless wrapping it with a reliable timeout.
- Capture metrics immediately before and after.
- When Prometheus range queries become available, prefer range queries over counter-delta snapshots.

Implementation detail:

```bash
timeout 120s ask --bench 120 --files prompts.yaml --max-tokens 100
```

The MCP implementation should not allow arbitrary command arguments. It should construct the command from validated fields.

### `summarize_experiment`

Generate a structured, evidence-backed summary from current nodes, benchmark status, and metrics.

Input:

```json
{
  "window_seconds": 120,
  "question": "Did adding cllm-2 rebalance traffic from 50/50 to 33/33/33?"
}
```

Output:

```json
{
  "answer": "The available metrics indicate traffic moved toward an even split across three eligible nodes.",
  "evidence": [
    {
      "metric": "cllm_node_admissions_total",
      "observation": "admission deltas were similar across cllm, cllm-2, and vllm over the window"
    }
  ],
  "caveats": [
    "Use dashboard inspection or Prometheus range queries for higher-confidence time-window analysis."
  ],
  "links": {
    "cllm_dashboard": "http://192.168.68.63:3000/d/cllm-overview/cllm-overview",
    "metrics": "http://192.168.68.63:8088/metrics",
    "nodes": "http://192.168.68.63:8088/nodes"
  }
}
```

This tool may be implemented either as server-side deterministic summarization over known metrics or as a convenience tool that gathers data and lets Claude produce the natural-language summary. The latter is acceptable for v1 if the returned evidence is structured.

## Out Of Scope For v1

- `delete_node`
- Mutation of `vllm`
- Arbitrary shell execution
- Arbitrary PromQL execution
- Visual scraping of Grafana dashboards
- Direct Kubernetes mutation
- Writing back to `nodes.yaml` or the Kubernetes ConfigMap
- Long-running benchmark process supervision beyond status/log inspection

Skipping `delete_node` is intentional. A later version can add it with confirmation, audit logging, protected-node policy, and a refusal to delete the final synthetic node.

## Safety And Guardrails

The MCP server should have these guardrails from the first version:

- Read-only mode via `CLLM_READ_ONLY=true`.
- Protected node list, initially containing `vllm`.
- No delete tool.
- No bypass-cache updates.
- Bounded numeric inputs.
- HTTP timeouts on every cLLM call.
- Clear error messages that distinguish API failure, validation failure, and read-only refusal.
- Audit log for every mutating tool:
  - timestamp
  - tool
  - requested input
  - result
  - before/after node state where applicable

## Recommended Demo Flow

### Baseline

Prompt:

```text
Inspect the current cLLM fleet and benchmark status.
```

Expected tool calls:

- `list_nodes`
- `get_benchmark_status`
- `get_metrics_snapshot`

Expected result:

- Claude identifies the real `vllm` lane and synthetic `cllm` lane.
- Claude reports that benchmark traffic is active.
- Claude links the cLLM dashboard.

### Add Synthetic Capacity

Prompt:

```text
Add a third synthetic node equivalent to cllm, then watch whether traffic rebalances.
```

Expected tool calls:

- `create_synthetic_node`
- `list_nodes`
- `get_metrics_snapshot`
- optionally `run_benchmark_window`
- `summarize_experiment`

Expected result:

- Claude reports that topology changed from two eligible lanes to three.
- Claude expects least-loaded unpinned routing to move toward 33/33/33.
- Metrics evidence is used for the conclusion.

### Resize Synthetic Capacity

Prompt:

```text
Resize the synthetic lane to twice its previous capacity and summarize the expected routing impact.
```

Expected tool calls:

- `update_node`
- `get_metrics_snapshot`
- `summarize_experiment`

Expected result:

- Claude reports the configured change.
- Claude expects traffic to bias toward the larger synthetic capacity, approximately 2:1 in the simple case.

### Bounded Benchmark

Prompt:

```text
Run a two-minute benchmark window at concurrency 120 and summarize traffic split, latency, cache behavior, and dashboard links.
```

Expected tool calls:

- `run_benchmark_window`
- `summarize_experiment`

Expected result:

- Claude excludes warmup aggregate throughput.
- Claude reports node split, TTFT/duration shape where available, and caveats.
- Claude links dashboards and raw metrics.

## Interview Framing

This MCP server is not meant to be a large product. Its value is the field-engineering pattern:

- cLLM already exposes a real operational control plane.
- Claude gets bounded tools over that control plane.
- Prometheus/cLLM metrics provide evidence.
- Grafana remains the human confirmation surface.
- The system can run small, repeatable experiments that change synthetic LLM capacity without requiring more physical GPUs.

The key interview line:

> This turns cLLM from a dashboard-driven inference lab into a Claude-operable workflow: inspect the fleet, safely adjust synthetic capacity, run a bounded benchmark, and produce an evidence-backed deployment-readiness report.
