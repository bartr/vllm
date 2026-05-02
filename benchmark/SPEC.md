# cLLM Benchmark Scenario Specification

## Purpose

A scenario file is a self-contained, reproducible experiment definition. It declares what prompts to send, how many concurrent workers per group, which nodes to target, and any temporary node configuration changes needed for the experiment. The MCP `run_scenario` tool executes it, captures logs, takes before/after metrics snapshots, and writes a structured report.

## File Locations

```
benchmark/
  scenarios/          Scenario YAML files (the experiment definitions)
  reports/            Markdown reports — one per run
  logs/               Raw ask --bench output — one file per group per run
```

**Naming conventions:**

| Artifact | Path |
|----------|------|
| Scenario file | `benchmark/scenarios/{name}.yaml` |
| Group log | `benchmark/logs/{timestamp}-{scenario}-{group}.log` |
| Run report | `benchmark/reports/{timestamp}-{scenario}.md` |

where `{timestamp}` is `YYYYMMDD-HHMMSS` UTC.

## Schema

```yaml
# ── Identity ──────────────────────────────────────────────────────────────────
scenario: mixed-tenants                        # required; used in filenames
description: "Interactive vs batch load mix"   # required; appears in report header
tags: [throughput, multi-tenant]               # optional; for filtering/search

# ── Timing ────────────────────────────────────────────────────────────────────
duration: 120s      # shared wall-clock duration for all groups (30s–600s)
warmup: 15s         # rows with blank total_tok_s excluded from stats

# ── Baseline reference ────────────────────────────────────────────────────────
# When set, run_scenario fetches this scenario's report and auto-diffs key metrics.
baseline: baseline.yaml   # relative to benchmark/scenarios/, or omit

# ── Temporary node overrides ──────────────────────────────────────────────────
# Applied via update_node before the run; restored to captured values after,
# even on error. Original values are written to the audit log entry.
nodes:
  cllm:
    per_request_tokens_per_second: 48   # any update_node field is valid here

# ── Groups ────────────────────────────────────────────────────────────────────
# Each group runs as an independent ask --bench process.
# Groups start simultaneously and run for `duration`.
groups:

  interactive:                         # group name; used in log filename
    concurrency: 80                    # number of concurrent ask workers
    prompts: prompts/short.txt         # see Prompt Sources below
    max_tokens: 100
    tenant: interactive                # optional; injected as DSL directive
    dsl: "tps-80 jitter+20 stall+5"   # optional; appended after tenant directive
    node: cllm                         # optional; pins group to a specific node

  batch:
    concurrency: 40
    prompts: prompts/long.txt
    max_tokens: 500
    tenant: batch
    dsl: "prefill-scale-2.0 tps-20"
    node: vllm

  # Minimal group — only concurrency and prompts are required
  background:
    concurrency: 10
    prompts: "What is Kubernetes?"
```

## Field Reference

### Top-level

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `scenario` | string | yes | Short identifier, used in filenames. No spaces. |
| `description` | string | yes | Human-readable description for the report header. |
| `tags` | list[string] | no | Arbitrary labels for search and filtering. |
| `duration` | duration string | yes | Shared run duration for all groups. Format: `30s`, `5m`. Clamped to 30s–600s. |
| `warmup` | duration string | no | Warmup period to exclude from stats. Default: `15s`. |
| `baseline` | string | no | Filename of a reference scenario (relative to `benchmark/scenarios/`). When set, the report includes a diff table. |
| `nodes` | map | no | Temporary node overrides. See Node Overrides. |
| `groups` | map | yes | One or more named groups. At least one required. |

### Group fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `concurrency` | int | yes | Number of concurrent `ask` workers. Range: 1–256. |
| `prompts` | string | yes | Prompt source. See Prompt Sources. |
| `max_tokens` | int | no | Max completion tokens per request. Default: 100. |
| `tenant` | string | no | Tenant label. Injected as `:dsl tenant={value}` prepended to the request. |
| `dsl` | string | no | Raw DSL string appended after the tenant directive. |
| `node` | string | no | Pin this group to a specific node via DSL node-pin directive. Must exist in the fleet. Protected nodes (`vllm`) can be targeted but not mutated. |
| `ramp_to` | int | no | If set, ramp concurrency from `concurrency` to `ramp_to` over `ramp_duration`. |
| `ramp_duration` | duration string | no | Duration of the ramp. Default: `30s`. |

### Node overrides

Any field accepted by `update_node` is valid under `nodes.{id}`. The runner:

1. Fetches the current node state via `GET /nodes/{id}` before the run.
2. Records the original values in the audit log entry.
3. Applies the overrides with `update_node`.
4. Restores the original values after the run completes — or on error.

Only fields explicitly listed under `nodes.{id}` are changed; all other fields are left untouched.

## Prompt Sources

| Format | Example | Behaviour |
|--------|---------|-----------|
| Inline string | `prompts: "Explain Azure"` | Single prompt, repeated across all workers |
| Text file | `prompts: prompts/short.txt` | File content used as a single prompt |
| YAML list | `prompts: prompts/list.yaml` | Passed to `ask --files`; workers cycle through the list |

Paths are resolved relative to the repo root. The existing `scripts/prompts.yaml` is a valid YAML list source.

## DSL and Tenant Injection

When both `tenant` and `dsl` are set, the combined directive passed to `ask` is:

```
--dsl "tenant={tenant} {dsl}"
```

When only `tenant` is set:

```
--dsl "tenant={tenant}"
```

When only `dsl` is set:

```
--dsl "{dsl}"
```

## Command Construction

For each group, `run_scenario` constructs a command of the form:

```bash
ask --bench {concurrency} \
    --duration {duration} \
    --files {resolved_prompts_path} \     # or --prompt for inline
    --max-tokens {max_tokens} \
    [--dsl "{tenant_and_dsl}"] \
    [--ramp {concurrency}:{ramp_to} --ramp-duration {ramp_duration}]
```

No arbitrary arguments are passed. All fields are validated before any process is started.

## Run Lifecycle

```
1. Validate scenario YAML (required fields, bounds, node existence)
2. Capture before-metrics snapshot
3. Apply nodes: overrides (record originals in audit log)
4. Start all group processes simultaneously
5. Tee each group's stdout+stderr to benchmark/logs/{timestamp}-{scenario}-{group}.log
6. Wait for all groups to complete (bounded by duration)
7. Capture after-metrics snapshot
8. Restore node overrides to original values
9. Parse all group logs; compute per-group and aggregate stats
10. Diff against baseline scenario report if baseline: is set
11. Write benchmark/reports/{timestamp}-{scenario}.md
12. Write audit log entry (tool, inputs, scenario hash, before/after node state)
```

If any step after 3 fails, node restoration (step 8) is still attempted before returning the error.

## Example: Baseline Scenario

```yaml
scenario: baseline
description: "2-node fleet, 120 connections, prompts.yaml, max-tokens 100"
duration: 60s
warmup: 15s

groups:
  default:
    concurrency: 120
    prompts: scripts/prompts.yaml
    max_tokens: 100
```

## Report Structure

Each run report includes:

- Scenario name, description, timestamp, scenario file hash
- Topology (before and after, with any node overrides noted)
- Per-group benchmark window stats (requests, tok/s, TTFT, req tok/s, cache hit rate)
- Aggregate stats across all groups
- Traffic split (windowed admission deltas per node)
- Baseline diff table (if `baseline:` is set)
- Raw log paths
- Caveats
- Dashboard link
