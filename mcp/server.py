"""cLLM Operations MCP Server."""

import asyncio
import json
import os
import subprocess
import sys
from datetime import datetime, timezone

# Allow running as `python3 server.py` without installing the package.
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

_REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
_PROMPTS_FILE = os.path.join(_REPO_ROOT, "scripts", "prompts.yaml")

import httpx
from mcp.server.fastmcp import FastMCP

import settings
import client
import benchmark
import metrics
import audit
import scenario as scenario_mod

# Numeric field bounds for synthetic node creation and updates.
_BOUNDS: dict[str, tuple[int, int]] = {
    "max_tokens_in_flight":        (1,      10_000_000),
    "max_waiting_requests":        (1,      100_000),
    "per_request_tokens_per_second": (1,    10_000),
    "degradation_threshold":       (0,      1_000_000),
    "max_concurrency":             (1,      10_000),
    "max_degradation":             (0,      100),
    "prefill_rate_multiplier":     (0,      1_000),
    "prefill_base_overhead_ms":    (0,      10_000),
    "prefill_jitter_percent":      (0,      100),
    "prefill_max_ms":              (0,      60_000),
}


def _validate_bounds(fields: dict) -> str | None:
    """Return an error string if any provided field is out of range, else None."""
    for field, value in fields.items():
        if value is None or field not in _BOUNDS:
            continue
        lo, hi = _BOUNDS[field]
        if not (lo <= value <= hi):
            return f"Validation error: {field}={value} outside allowed range [{lo}, {hi}]"
    return None


def _flatten_node(node: dict) -> dict:
    """Extract a flat POST/PUT-compatible body from a nested node response."""
    cap = node.get("capacity", {})
    deg = node.get("degradation", {})
    rea = node.get("realism", {})
    return {
        "max_tokens_in_flight":          cap.get("max_tokens_in_flight"),
        "max_waiting_requests":          cap.get("max_waiting_requests"),
        "per_request_tokens_per_second": cap.get("per_request_tokens_per_second"),
        "degradation_threshold":         cap.get("degradation_threshold"),
        "max_concurrency":               cap.get("max_concurrency"),
        "bypass_cache":                  cap.get("bypass_cache"),
        "max_degradation":               deg.get("max_degradation"),
        "prefill_rate_multiplier":       rea.get("prefill_rate_multiplier"),
        "prefill_base_overhead_ms":      rea.get("prefill_base_overhead_ms"),
        "prefill_jitter_percent":        rea.get("prefill_jitter_percent"),
        "prefill_max_ms":                rea.get("prefill_max_ms"),
    }


mcp = FastMCP(
    "cllm-ops",
    instructions=(
        "Operate and inspect a cLLM/vLLM inference experimentation environment. "
        "Use list_nodes to inspect the fleet, get_benchmark_status to check active "
        "workloads, get_metrics_snapshot for live Prometheus data, and "
        "get_config/get_cache_status for runtime configuration. "
        "The 'vllm' node is a protected real-GPU lane—do not mutate it."
    ),
)


@mcp.tool()
async def list_nodes() -> str:
    """Return the current cLLM node fleet with protection annotations.

    Marks nodes in the protected list (by default just 'vllm') so callers
    know which lanes must not be mutated. Passes through the full nested
    capacity/degradation/realism/stats structure from the API.
    """
    try:
        data = await client.get("/nodes")
    except httpx.HTTPError as e:
        return json.dumps({"error": f"API error: {e}", "nodes": []})

    nodes = data.get("nodes", [])
    protected = [n["id"] for n in nodes if n.get("id") in settings.PROTECTED_NODES]

    result = {
        **data,
        "protected_nodes": protected,
        "dashboard_url": settings.CLLM_GRAFANA_URL,
    }
    return json.dumps(result, indent=2)


@mcp.tool()
async def get_node(id: str) -> str:
    """Return a single cLLM node by ID from GET /nodes/{id}.

    Annotates whether the node is protected. Returns an error if the node
    does not exist or the API is unreachable.

    Args:
        id: Node identifier, e.g. 'cllm' or 'vllm'.
    """
    try:
        data = await client.get(f"/nodes/{id}")
    except httpx.HTTPStatusError as e:
        if e.response.status_code == 404:
            return json.dumps({"error": f"Node '{id}' not found"})
        return json.dumps({"error": f"API error: {e}"})
    except httpx.HTTPError as e:
        return json.dumps({"error": f"API error: {e}"})

    return json.dumps(
        {
            "node": data,
            "protected": id in settings.PROTECTED_NODES,
            "dashboard_url": settings.CLLM_GRAFANA_URL,
        },
        indent=2,
    )


@mcp.tool()
async def get_metrics_snapshot(include_raw: bool = False) -> str:
    """Fetch and parse GET /metrics for a current node and cache metrics snapshot.

    Returns per-node gauge and counter values plus cache hit/miss counts.
    Set include_raw=true to include the full Prometheus text in the response.

    Args:
        include_raw: When true, attach the raw Prometheus text to the response.
    """
    try:
        text = await client.get_text("/metrics")
    except httpx.HTTPError as e:
        return json.dumps({"error": f"API error: {e}"})

    snapshot = metrics.build_snapshot(text, include_raw=include_raw)
    snapshot["metrics_url"] = settings.CLLM_BASE_URL + "/metrics"
    snapshot["dashboard_url"] = settings.CLLM_GRAFANA_URL
    return json.dumps(snapshot, indent=2)


@mcp.tool()
async def get_config() -> str:
    """Return cLLM runtime configuration from GET /config."""
    try:
        data = await client.get("/config")
    except httpx.HTTPError as e:
        return json.dumps({"error": f"API error: {e}"})

    return json.dumps({"config": data, "dashboard_url": settings.CLLM_GRAFANA_URL}, indent=2)


@mcp.tool()
async def get_cache_status() -> str:
    """Return cLLM cache state from GET /cache, including entry count and key list."""
    try:
        data = await client.get("/cache")
    except httpx.HTTPError as e:
        return json.dumps({"error": f"API error: {e}"})

    return json.dumps({"cache": data}, indent=2)


@mcp.tool()
async def get_benchmark_status(tail_lines: int = 40) -> str:
    """Inspect whether the long-running ask --bench workload appears active.

    Checks the process table for a running 'ask --bench' process and parses
    recent rows from the benchmark log. Blank total_tok_s on the most recent
    row means the 15-second sliding window hasn't filled yet (warming_up=true).

    Args:
        tail_lines: Number of recent log lines to read (default 40).
    """
    running = benchmark.is_running()
    lines = benchmark.tail_log(tail_lines)

    rows = [r for line in lines if (r := benchmark.parse_row(line)) is not None]

    # warming_up: based on the most recent data row's total_tok_s field
    warming_up: bool | None = None
    if rows:
        warming_up = rows[-1]["total_tok_s"] is None

    result = {
        "running": running,
        "command_hint": "ask --bench 120 --loop --files prompts.yaml --max-tokens 100",
        "log_path": settings.CLLM_BENCH_LOG,
        "warming_up": warming_up,
        "recent_rows": rows,
        "notes": [
            "total_tok_s may be blank during benchmark warmup and should not be treated as zero"
        ],
    }
    return json.dumps(result, indent=2)


@mcp.tool()
async def create_synthetic_node(
    id: str,
    max_tokens_in_flight: int | None = None,
    max_waiting_requests: int | None = None,
    per_request_tokens_per_second: int | None = None,
    degradation_threshold: int | None = None,
    max_concurrency: int | None = None,
    max_degradation: int | None = None,
    prefill_rate_multiplier: int | None = None,
    prefill_base_overhead_ms: int | None = None,
    prefill_jitter_percent: int | None = None,
    prefill_max_ms: int | None = None,
) -> str:
    """Create a new synthetic cLLM node via POST /nodes/{id}.

    Omitted capacity/realism fields default from the existing 'cllm' node.
    Refused when CLLM_READ_ONLY=true, when id is a protected node, or when
    bypass_cache=true would be implied.

    Args:
        id: Unique node identifier. Must not match a protected node (e.g. 'vllm').
        max_tokens_in_flight: Token capacity limit (1–10,000,000).
        max_waiting_requests: Queue depth limit (1–100,000).
        per_request_tokens_per_second: Simulated per-request throughput (1–10,000).
        degradation_threshold: Load level at which degradation begins (0–1,000,000).
        max_concurrency: Maximum concurrent requests (1–10,000).
        max_degradation: Maximum degradation percentage (0–100).
        prefill_rate_multiplier: Prefill speed multiplier (0–1,000).
        prefill_base_overhead_ms: Fixed prefill overhead in ms (0–10,000).
        prefill_jitter_percent: Prefill jitter percentage (0–100).
        prefill_max_ms: Prefill cap in ms (0–60,000).
    """
    if settings.CLLM_READ_ONLY:
        return json.dumps({"error": "Refused: CLLM_READ_ONLY=true"})
    if id in settings.PROTECTED_NODES:
        return json.dumps({"error": f"Refused: '{id}' is a protected node"})

    provided = {
        "max_tokens_in_flight":          max_tokens_in_flight,
        "max_waiting_requests":          max_waiting_requests,
        "per_request_tokens_per_second": per_request_tokens_per_second,
        "degradation_threshold":         degradation_threshold,
        "max_concurrency":               max_concurrency,
        "max_degradation":               max_degradation,
        "prefill_rate_multiplier":       prefill_rate_multiplier,
        "prefill_base_overhead_ms":      prefill_base_overhead_ms,
        "prefill_jitter_percent":        prefill_jitter_percent,
        "prefill_max_ms":                prefill_max_ms,
    }
    if err := _validate_bounds(provided):
        return json.dumps({"error": err})

    try:
        cllm_node = await client.get("/nodes/cllm")
        nodes_data = await client.get("/nodes")
    except httpx.HTTPError as e:
        return json.dumps({"error": f"API error fetching defaults: {e}"})

    defaults = _flatten_node(cllm_node)
    body: dict = {"class": "cllm"}
    for field, default_val in defaults.items():
        body[field] = False if field == "bypass_cache" else (provided.get(field) if provided.get(field) is not None else default_val)

    try:
        created = await client.post(f"/nodes/{id}", body)
    except httpx.HTTPStatusError as e:
        return json.dumps({"error": f"API error {e.response.status_code}: {e.response.text}"})
    except httpx.HTTPError as e:
        return json.dumps({"error": f"API error: {e}"})

    new_count = nodes_data.get("count", len(nodes_data.get("nodes", []))) + 1
    expected_effect = (
        f"With {new_count} nodes, unpinned least-loaded routing should move toward "
        f"~{round(100 / new_count)}% per node."
    )

    result = {"created": True, "node": created, "expected_effect": expected_effect}
    audit.log("create_synthetic_node", {"id": id, **{k: v for k, v in provided.items() if v is not None}}, result)
    return json.dumps(result, indent=2)


@mcp.tool()
async def update_node(
    id: str,
    max_tokens_in_flight: int | None = None,
    max_waiting_requests: int | None = None,
    per_request_tokens_per_second: int | None = None,
    degradation_threshold: int | None = None,
    max_concurrency: int | None = None,
    max_degradation: int | None = None,
    prefill_rate_multiplier: int | None = None,
    prefill_base_overhead_ms: int | None = None,
    prefill_jitter_percent: int | None = None,
    prefill_max_ms: int | None = None,
) -> str:
    """Update capacity or realism fields on an existing synthetic cLLM node via PUT /nodes/{id}.

    Only provided fields are changed. Returns before and after node state.
    Refused when CLLM_READ_ONLY=true or when id is a protected node.

    Args:
        id: Node identifier to update. Must not be a protected node (e.g. 'vllm').
        max_tokens_in_flight: New token capacity limit (1–10,000,000).
        max_waiting_requests: New queue depth limit (1–100,000).
        per_request_tokens_per_second: New simulated per-request throughput (1–10,000).
        degradation_threshold: New degradation onset threshold (0–1,000,000).
        max_concurrency: New maximum concurrent requests (1–10,000).
        max_degradation: New maximum degradation percentage (0–100).
        prefill_rate_multiplier: New prefill speed multiplier (0–1,000).
        prefill_base_overhead_ms: New fixed prefill overhead in ms (0–10,000).
        prefill_jitter_percent: New prefill jitter percentage (0–100).
        prefill_max_ms: New prefill cap in ms (0–60,000).
    """
    if settings.CLLM_READ_ONLY:
        return json.dumps({"error": "Refused: CLLM_READ_ONLY=true"})
    if id in settings.PROTECTED_NODES:
        return json.dumps({"error": f"Refused: '{id}' is a protected node"})

    updates = {k: v for k, v in {
        "max_tokens_in_flight":          max_tokens_in_flight,
        "max_waiting_requests":          max_waiting_requests,
        "per_request_tokens_per_second": per_request_tokens_per_second,
        "degradation_threshold":         degradation_threshold,
        "max_concurrency":               max_concurrency,
        "max_degradation":               max_degradation,
        "prefill_rate_multiplier":       prefill_rate_multiplier,
        "prefill_base_overhead_ms":      prefill_base_overhead_ms,
        "prefill_jitter_percent":        prefill_jitter_percent,
        "prefill_max_ms":                prefill_max_ms,
    }.items() if v is not None}

    if not updates:
        return json.dumps({"error": "No fields provided to update"})
    if err := _validate_bounds(updates):
        return json.dumps({"error": err})

    try:
        before = await client.get(f"/nodes/{id}")
    except httpx.HTTPStatusError as e:
        if e.response.status_code == 404:
            return json.dumps({"error": f"Node '{id}' not found"})
        return json.dumps({"error": f"API error: {e}"})
    except httpx.HTTPError as e:
        return json.dumps({"error": f"API error: {e}"})

    try:
        after = await client.put(f"/nodes/{id}", updates)
    except httpx.HTTPStatusError as e:
        return json.dumps({"error": f"API error {e.response.status_code}: {e.response.text}"})
    except httpx.HTTPError as e:
        return json.dumps({"error": f"API error: {e}"})

    before_tif = _flatten_node(before).get("max_tokens_in_flight", 0) or 0
    after_tif = _flatten_node(after).get("max_tokens_in_flight", 0) or 0
    if after_tif > before_tif:
        expected_effect = f"Increasing capacity on '{id}' should bias least-loaded routing toward this lane."
    elif after_tif < before_tif:
        expected_effect = f"Decreasing capacity on '{id}' may reduce its share of least-loaded routing."
    else:
        expected_effect = f"Realism parameters updated on '{id}'. Routing weight is unchanged."

    result = {"updated": True, "before": before, "after": after, "expected_effect": expected_effect}
    audit.log("update_node", {"id": id, **updates}, result, before=before, after=after)
    return json.dumps(result, indent=2)


async def _experiment_report(question: str = "", snapshot: dict | None = None) -> dict:
    """Gather nodes + metrics + benchmark state into a structured evidence report.

    If a pre-fetched metrics snapshot is provided it is used directly; otherwise
    a fresh one is fetched from /metrics.
    """
    errors: list[str] = []

    try:
        nodes_data = await client.get("/nodes")
    except httpx.HTTPError as e:
        nodes_data = {"nodes": []}
        errors.append(f"nodes: {e}")

    if snapshot is None:
        try:
            snapshot = metrics.build_snapshot(await client.get_text("/metrics"))
        except httpx.HTTPError as e:
            snapshot = {"node_metrics": [], "cache_metrics": {}}
            errors.append(f"metrics: {e}")

    bench_running = benchmark.is_running()
    bench_rows = [r for line in benchmark.tail_log(40) if (r := benchmark.parse_row(line)) is not None]
    bench_warming = bench_rows[-1]["total_tok_s"] is None if bench_rows else None

    nodes = nodes_data.get("nodes", [])
    node_metrics = snapshot.get("node_metrics", [])
    cache = snapshot.get("cache_metrics", {})
    evidence: list[dict] = []

    admissions = {m["node"]: m["admissions_total_admitted"] for m in node_metrics if "admissions_total_admitted" in m}
    total_admitted = sum(admissions.values())
    if total_admitted > 0:
        split = {n: round(v / total_admitted * 100, 1) for n, v in admissions.items()}
        evidence.append({
            "metric": "cllm_node_admissions_total",
            "observation": (
                "Cumulative admission split: "
                + ", ".join(f"{n}={p}%" for n, p in split.items())
                + ". Lifetime counter — not a windowed rate."
            ),
        })

    tif = {m["node"]: int(m["tokens_in_flight"]) for m in node_metrics if "tokens_in_flight" in m}
    if tif:
        evidence.append({
            "metric": "cllm_node_tokens_in_flight",
            "observation": "Current tokens in flight: " + ", ".join(f"{n}={v}" for n, v in tif.items()),
        })

    hits = cache.get("lookups_hit", 0)
    misses = cache.get("lookups_miss", 0)
    total_lookups = hits + misses
    if total_lookups > 0:
        hit_rate = round(hits / total_lookups * 100, 1)
        evidence.append({
            "metric": "cllm_cache_lookups_total",
            "observation": f"Lifetime cache hit rate: {hit_rate}% ({hits:,} hits / {total_lookups:,} lookups)",
        })

    if bench_running:
        bench_obs = "Benchmark is active"
        if bench_warming:
            bench_obs += " (warming up — aggregate throughput not yet stable)"
        elif bench_rows:
            tps_values = [r["total_tok_s"] for r in bench_rows if r.get("total_tok_s")]
            if tps_values:
                bench_obs += f" — recent avg total throughput: {round(sum(tps_values)/len(tps_values), 1)} tok/s"
    else:
        bench_obs = "No active benchmark process detected"
    evidence.append({"metric": "benchmark_status", "observation": bench_obs})

    result: dict = {
        "question": question or None,
        "topology": {
            "node_count": len(nodes),
            "nodes": [{"id": n.get("id"), "class": n.get("class")} for n in nodes],
            "protected_nodes": [n["id"] for n in nodes if n.get("id") in settings.PROTECTED_NODES],
        },
        "evidence": evidence,
        "caveats": [
            "Admission and cache counts are lifetime counters, not windowed rates. "
            "Use run_benchmark_window for time-bounded analysis.",
            "For higher-confidence time-window conclusions, use Prometheus range queries "
            "or inspect the Grafana dashboard directly.",
        ],
        "links": {
            "cllm_dashboard": settings.CLLM_GRAFANA_URL,
            "metrics": settings.CLLM_BASE_URL + "/metrics",
            "nodes": settings.CLLM_BASE_URL + "/nodes",
        },
    }
    if errors:
        result["errors"] = errors
    return result


@mcp.tool()
async def summarize_experiment(question: str = "") -> str:
    """Generate a structured, evidence-backed summary from nodes, benchmark, and metrics.

    Gathers current node topology, a live metrics snapshot, and benchmark status,
    then returns structured evidence suitable for answering questions about traffic
    distribution, load balance, cache behavior, and routing changes.

    Args:
        question: Optional question to frame the summary around, e.g.
                  'Did adding cllm-2 rebalance traffic from 50/50 to 33/33/33?'
    """
    return json.dumps(await _experiment_report(question), indent=2)


@mcp.tool()
async def run_benchmark_window(
    duration_seconds: int = 120,
    concurrency: int = 120,
) -> str:
    """Run a bounded benchmark and return before/after metrics with a summary report.

    Kills any existing ask --bench process, runs ask --bench for the requested
    duration, tees output to ~/logs/, then captures an after-metrics snapshot
    and generates an experiment report automatically.

    Args:
        duration_seconds: How long to run the benchmark (30–600, default 120).
        concurrency: Number of concurrent workers (1–256, default 120).
    """
    if settings.CLLM_READ_ONLY:
        return json.dumps({"error": "Refused: CLLM_READ_ONLY=true"})
    if not (30 <= duration_seconds <= 600):
        return json.dumps({"error": f"Validation error: duration_seconds={duration_seconds} outside [30, 600]"})
    if not (1 <= concurrency <= 256):
        return json.dumps({"error": f"Validation error: concurrency={concurrency} outside [1, 256]"})

    kill_result = subprocess.run(["pkill", "-f", "ask --bench"], capture_output=True)
    killed_existing = kill_result.returncode == 0

    try:
        before_snapshot = metrics.build_snapshot(await client.get_text("/metrics"))
    except httpx.HTTPError as e:
        return json.dumps({"error": f"Failed to capture before-snapshot: {e}"})

    timestamp = datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")
    os.makedirs(settings.BENCHMARK_LOGS_DIR, exist_ok=True)
    log_filename = f"bench-window-{timestamp}.log"
    log_path = os.path.join(settings.BENCHMARK_LOGS_DIR, log_filename)
    log_path_display = f"benchmark/logs/{log_filename}"

    cmd = [
        "ask", "--bench", str(concurrency),
        "--duration", f"{duration_seconds}s",
        "--files", _PROMPTS_FILE,
        "--max-tokens", "100",
    ]

    started_at = datetime.now(timezone.utc)
    proc = await asyncio.create_subprocess_exec(
        *cmd,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.STDOUT,
    )

    output_lines: list[str] = []
    with open(log_path, "w") as log_f:
        while True:
            line = await proc.stdout.readline()
            if not line:
                break
            decoded = line.decode("utf-8", errors="replace")
            log_f.write(decoded)
            log_f.flush()
            output_lines.append(decoded.rstrip())

    await proc.wait()
    completed_at = datetime.now(timezone.utc)

    try:
        after_text = await client.get_text("/metrics")
        after_snapshot = metrics.build_snapshot(after_text)
    except httpx.HTTPError as e:
        after_snapshot = {"error": str(e)}
        after_text = None

    # Parse rows from captured output; exclude warmup rows for stats
    rows = [r for line in output_lines if (r := benchmark.parse_row(line)) is not None]
    warmed = [r for r in rows if r.get("total_tok_s") is not None]

    window_stats: dict = {"total_rows": len(rows), "warmup_rows_excluded": len(rows) - len(warmed)}
    if warmed:
        cache_hits = sum(1 for r in warmed if r.get("cache") == "hit")
        cache_total = sum(1 for r in warmed if r.get("cache") in ("hit", "miss"))
        window_stats["cache_hit_rate_pct"] = round(cache_hits / cache_total * 100, 1) if cache_total else None
        ttft_vals = [r["ttft_ms"] for r in warmed if r.get("ttft_ms") is not None]
        if ttft_vals:
            window_stats["avg_ttft_ms"] = round(sum(ttft_vals) / len(ttft_vals), 1)
        req_vals = [r["req_tok_s"] for r in warmed if r.get("req_tok_s") is not None]
        if req_vals:
            window_stats["avg_req_tok_s"] = round(sum(req_vals) / len(req_vals), 2)
        tps_vals = [r["total_tok_s"] for r in warmed if r.get("total_tok_s") is not None]
        if tps_vals:
            window_stats["avg_total_tok_s"] = round(sum(tps_vals) / len(tps_vals), 1)

    report = await _experiment_report(
        question=f"How did a {duration_seconds}s benchmark at concurrency {concurrency} perform?",
        snapshot=after_snapshot if "error" not in after_snapshot else None,
    )

    result = {
        "duration_seconds": duration_seconds,
        "concurrency": concurrency,
        "started_at": started_at.isoformat(),
        "completed_at": completed_at.isoformat(),
        "command": " ".join(cmd),
        "log_path": log_path_display,
        "killed_existing": killed_existing,
        "before": before_snapshot,
        "after": after_snapshot,
        "window_stats": window_stats,
        "report": report,
    }
    audit.log("run_benchmark_window", {"duration_seconds": duration_seconds, "concurrency": concurrency}, result)
    return json.dumps(result, indent=2)


@mcp.tool()
async def run_scenario(name: str) -> str:
    """Run a named benchmark scenario from benchmark/scenarios/ and write a report.

    Loads benchmark/scenarios/{name}.yaml, validates it, applies any node overrides
    (restoring them on exit), runs all groups in parallel, captures before/after
    metrics snapshots, writes a markdown report to benchmark/reports/, and logs an
    audit entry. Node overrides are always restored even if the run fails.

    Args:
        name: Scenario filename without extension, e.g. 'baseline' or 'mixed-tenants'.
    """
    if settings.CLLM_READ_ONLY:
        return json.dumps({"error": "Refused: CLLM_READ_ONLY=true"})

    scenario_path = os.path.join(settings.BENCHMARK_SCENARIOS_DIR, f"{name}.yaml")
    if not os.path.exists(scenario_path):
        available = [f[:-5] for f in os.listdir(settings.BENCHMARK_SCENARIOS_DIR) if f.endswith(".yaml")]
        return json.dumps({"error": f"Scenario '{name}' not found", "available": available})

    try:
        spec = scenario_mod.load(scenario_path)
    except (ValueError, KeyError) as e:
        return json.dumps({"error": f"Scenario parse error: {e}"})

    timestamp = datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")

    try:
        result = await scenario_mod.run(spec, timestamp)
    except Exception as e:
        return json.dumps({"error": f"Scenario run failed: {e}"})

    # Write markdown report
    report_filename = f"{timestamp}-{spec.scenario}.md"
    report_path = os.path.join(settings.BENCHMARK_REPORTS_DIR, report_filename)
    os.makedirs(settings.BENCHMARK_REPORTS_DIR, exist_ok=True)
    _write_scenario_report(spec, result, report_path, scenario_path)

    result["report_path"] = f"benchmark/reports/{report_filename}"
    audit.log("run_scenario", {"name": name, "scenario_hash": spec.source_hash}, result)
    return json.dumps(result, indent=2)


def _write_scenario_report(spec: scenario_mod.ScenarioSpec, result: dict, path: str, scenario_path: str) -> None:
    groups = result["groups"]
    started = result["started_at"][:19].replace("T", " ") + " UTC"
    completed = result["completed_at"][:19].replace("T", " ") + " UTC"

    # windowed admission deltas
    b_adm = {n["node"]: int(n.get("admissions_total_admitted", 0)) for n in result["before"].get("node_metrics", [])}
    a_adm = {n["node"]: int(n.get("admissions_total_admitted", 0)) for n in result["after"].get("node_metrics", [])}
    active_nodes = {n for n in a_adm if a_adm[n] - b_adm.get(n, 0) > 0}
    total_delta = sum(a_adm[n] - b_adm.get(n, 0) for n in active_nodes) or 1

    lines = [
        f"# Scenario: {spec.scenario}",
        "",
        f"**{spec.description}**",
        "",
        f"**Date:** {started}  ",
        f"**Duration:** {spec.duration}s  ",
        f"**Warmup:** {spec.warmup}s  ",
        f"**Scenario hash:** `{spec.source_hash}`  ",
        f"**Scenario file:** `benchmark/scenarios/{os.path.basename(scenario_path)}`",
        "",
    ]

    if spec.tags:
        lines += [f"**Tags:** {', '.join(spec.tags)}", ""]

    lines += ["## Prompt", "", "> Run scenario `" + spec.scenario + "` and summarize results.", ""]

    lines += ["## Tools Invoked", "", "1. `run_scenario(name=\"" + spec.scenario + "\")`", ""]

    # topology
    before_nodes = result["before"].get("node_metrics", [])
    lines += ["## Topology", ""]
    lines += ["| Node | TPS (effective) | Max Concurrency | Max Tokens In Flight | Protected |"]
    lines += ["|------|-----------------|-----------------|----------------------|-----------|"]
    for n in before_nodes:
        if "max_tokens_in_flight" not in n:
            continue
        node_id = n["node"]
        protected = "Yes" if node_id in settings.PROTECTED_NODES else "No"
        tps = n.get("per_request_tps_effective", "—")
        conc = int(n.get("max_concurrency", 0))
        tif = int(n.get("max_tokens_in_flight", 0))
        lines.append(f"| `{node_id}` | {tps} | {conc} | {tif:,} | {protected} |")
    lines.append("")

    if spec.node_overrides:
        lines += ["### Node Overrides Applied", ""]
        for node_id, fields in spec.node_overrides.items():
            for k, v in fields.items():
                lines.append(f"- `{node_id}.{k}`: set to `{v}` for this run, restored after")
        lines += ["", f"Nodes restored: {', '.join(f'`{n}`' for n in result['node_overrides_restored'])}", ""]

    # groups
    lines += ["## Groups", ""]
    for g in groups:
        lines += [f"### {g['name']}", ""]
        lines += [f"**Command:** `{g['cmd']}`  "]
        lines += [f"**Log:** `{g['log_path']}`", ""]
        s = g["stats"]
        lines += ["| Metric | Value |", "|--------|-------|"]
        lines += [f"| Total rows | {s.get('total_rows', '—')} |"]
        lines += [f"| Warmup rows excluded | {s.get('warmup_rows_excluded', '—')} |"]
        if s.get("cache_hit_rate_pct") is not None:
            lines += [f"| Cache hit rate | {s['cache_hit_rate_pct']}% |"]
        if s.get("avg_ttft_ms") is not None:
            lines += [f"| Avg TTFT | {s['avg_ttft_ms']} ms |"]
        if s.get("avg_req_tok_s") is not None:
            lines += [f"| Avg req tok/s | {s['avg_req_tok_s']} |"]
        if s.get("avg_total_tok_s") is not None:
            lines += [f"| Avg total tok/s | {s['avg_total_tok_s']} |"]
        lines.append("")

    # traffic split
    lines += ["## Traffic Split (windowed admission deltas)", ""]
    lines += ["| Node | Delta | Share |", "|------|-------|-------|"]
    for node in sorted(active_nodes):
        d = a_adm[node] - b_adm.get(node, 0)
        lines.append(f"| `{node}` | +{d:,} | {100*d/total_delta:.1f}% |")
    lines += [f"| **Total** | **+{total_delta:,}** | |", ""]

    lines += [
        "## Caveats",
        "",
        "- Admission counts are lifetime counters; windowed deltas are used here.",
        "- Ghost metric series for deleted nodes may appear with zero window deltas.",
        "- For time-windowed conclusions, use Prometheus range queries or the Grafana dashboard.",
        f"- Dashboard: <{settings.CLLM_GRAFANA_URL}>",
    ]

    with open(path, "w") as f:
        f.write("\n".join(lines) + "\n")


if __name__ == "__main__":
    mcp.run()
