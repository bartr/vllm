"""Scenario YAML parser and runner for run_scenario."""

import asyncio
import hashlib
import os
import re
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

import yaml

import settings
import benchmark
import metrics as metrics_mod


# ── Duration parsing ───────────────────────────────────────────────────────────

_DUR_RE = re.compile(r'^(\d+)(s|m|h)$')


def parse_duration(value: str | int, field_name: str) -> int:
    """Return duration in seconds. Accepts '120s', '5m', '1h', or bare int."""
    if isinstance(value, int):
        return value
    m = _DUR_RE.match(str(value).strip())
    if not m:
        raise ValueError(f"{field_name}: invalid duration {value!r}; use e.g. '120s', '5m'")
    n, unit = int(m.group(1)), m.group(2)
    return n * {"s": 1, "m": 60, "h": 3600}[unit]


# ── Data classes ───────────────────────────────────────────────────────────────

@dataclass
class GroupSpec:
    name: str
    concurrency: int
    prompts: str
    max_tokens: int = 100
    tenant: str = ""
    dsl: str = ""
    node: str = ""
    ramp_to: int = 0
    ramp_duration: int = 30


@dataclass
class ScenarioSpec:
    scenario: str
    description: str
    duration: int
    warmup: int
    groups: list[GroupSpec]
    tags: list[str] = field(default_factory=list)
    baseline: str = ""
    node_overrides: dict[str, dict] = field(default_factory=dict)
    create_nodes: dict[str, dict] = field(default_factory=dict)   # created before run, deleted after
    source_hash: str = ""


# ── Validation ─────────────────────────────────────────────────────────────────

def _validate_id(value: str, field_name: str) -> str:
    if not re.match(r'^[a-zA-Z0-9_-]+$', value):
        raise ValueError(f"{field_name}: invalid identifier {value!r}")
    return value


def _resolve_prompts(prompts: str) -> tuple[str, bool]:
    """Return (resolved_path_or_text, is_file). Paths resolved relative to repo root."""
    if "\n" in prompts or prompts.endswith(".txt") or prompts.endswith(".yaml"):
        path = prompts if os.path.isabs(prompts) else os.path.join(_REPO_ROOT, prompts)
        if not os.path.exists(path):
            raise ValueError(f"prompts file not found: {path}")
        return path, True
    return prompts, False


_REPO_ROOT = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))


def load(path: str) -> ScenarioSpec:
    """Parse and validate a scenario YAML file. Raises ValueError on any problem."""
    with open(path) as f:
        raw = f.read()

    data = yaml.safe_load(raw)
    source_hash = hashlib.sha256(raw.encode()).hexdigest()[:12]

    scenario = _validate_id(data.get("scenario", ""), "scenario")
    if not scenario:
        raise ValueError("scenario: field is required")
    description = data.get("description", "").strip()
    if not description:
        raise ValueError("description: field is required")

    duration = parse_duration(data.get("duration", ""), "duration")
    if not (30 <= duration <= 600):
        raise ValueError(f"duration: {duration}s outside allowed range [30, 600]")

    warmup_raw = data.get("warmup", "15s")
    warmup = parse_duration(warmup_raw, "warmup")

    tags = data.get("tags", [])
    baseline = data.get("baseline", "")

    node_overrides: dict[str, dict] = {}
    for node_id, fields in (data.get("nodes") or {}).items():
        if node_id in settings.PROTECTED_NODES:
            raise ValueError(f"nodes.{node_id}: cannot override a protected node")
        node_overrides[node_id] = dict(fields)

    create_nodes: dict[str, dict] = {}
    for node_id, fields in (data.get("create_nodes") or {}).items():
        _validate_id(node_id, f"create_nodes.{node_id}")
        if node_id in settings.PROTECTED_NODES:
            raise ValueError(f"create_nodes.{node_id}: cannot create a protected node id")
        if fields.get("bypass_cache"):
            raise ValueError(f"create_nodes.{node_id}: bypass_cache=true not allowed in v1")
        create_nodes[node_id] = dict(fields)

    raw_groups = data.get("groups") or {}
    if not raw_groups:
        raise ValueError("groups: at least one group is required")

    groups: list[GroupSpec] = []
    for name, g in raw_groups.items():
        _validate_id(name, f"groups.{name}")
        concurrency = int(g.get("concurrency", 0))
        if not (1 <= concurrency <= 512):
            raise ValueError(f"groups.{name}.concurrency: {concurrency} outside [1, 512]")

        prompts_raw = g.get("prompts", "")
        if not prompts_raw:
            raise ValueError(f"groups.{name}.prompts: required")
        _resolve_prompts(str(prompts_raw))  # validate path exists

        max_tokens = int(g.get("max_tokens", 100))
        ramp_to = int(g.get("ramp_to", 0))
        ramp_dur_raw = g.get("ramp_duration", "30s")
        ramp_duration = parse_duration(ramp_dur_raw, f"groups.{name}.ramp_duration")

        groups.append(GroupSpec(
            name=name,
            concurrency=concurrency,
            prompts=str(prompts_raw),
            max_tokens=max_tokens,
            tenant=g.get("tenant", ""),
            dsl=g.get("dsl", ""),
            node=g.get("node", ""),
            ramp_to=ramp_to,
            ramp_duration=ramp_duration,
        ))

    return ScenarioSpec(
        scenario=scenario,
        description=description,
        duration=duration,
        warmup=warmup,
        groups=groups,
        tags=tags,
        baseline=baseline,
        node_overrides=node_overrides,
        create_nodes=create_nodes,
        source_hash=source_hash,
    )


# ── Command builder ────────────────────────────────────────────────────────────

def build_cmd(group: GroupSpec, duration: int) -> list[str]:
    prompts_path, is_file = _resolve_prompts(group.prompts)

    bench_n = group.ramp_to if group.ramp_to else group.concurrency
    cmd = ["ask", "--bench", str(bench_n), "--duration", f"{duration}s"]

    if is_file and prompts_path.endswith(".yaml"):
        cmd += ["--files", prompts_path]
    elif is_file:
        cmd += ["--prompt-file", prompts_path]
    else:
        cmd += ["--prompt", group.prompts]

    cmd += ["--max-tokens", str(group.max_tokens)]

    parts = []
    if group.tenant:
        parts.append(f"tenant={group.tenant}")
    if group.dsl:
        parts.append(group.dsl)
    if group.node:
        parts.append(f"node={group.node}")
    if parts:
        cmd += ["--dsl", " ".join(parts)]

    if group.ramp_to:
        cmd += ["--ramp", f"{group.concurrency}:{group.ramp_to}",
                "--ramp-duration", f"{group.ramp_duration}s"]

    return cmd


# ── Group runner ───────────────────────────────────────────────────────────────

@dataclass
class GroupResult:
    name: str
    cmd: list[str]
    log_path: str
    rows: list[dict]
    stats: dict


async def _run_group(group: GroupSpec, duration: int, log_path: str) -> GroupResult:
    cmd = build_cmd(group, duration)
    os.makedirs(os.path.dirname(log_path), exist_ok=True)

    proc = await asyncio.create_subprocess_exec(
        *cmd,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.STDOUT,
    )

    lines: list[str] = []
    with open(log_path, "w") as log_f:
        while True:
            line = await proc.stdout.readline()
            if not line:
                break
            decoded = line.decode("utf-8", errors="replace")
            log_f.write(decoded)
            log_f.flush()
            lines.append(decoded.rstrip())

    await proc.wait()

    rows = [r for ln in lines if (r := benchmark.parse_row(ln)) is not None]
    warmed = [r for r in rows if r.get("total_tok_s") is not None]

    stats: dict[str, Any] = {
        "total_rows": len(rows),
        "warmup_rows_excluded": len(rows) - len(warmed),
    }
    if warmed:
        hits = sum(1 for r in warmed if r.get("cache") == "hit")
        total_cache = sum(1 for r in warmed if r.get("cache") in ("hit", "miss"))
        stats["cache_hit_rate_pct"] = round(hits / total_cache * 100, 1) if total_cache else None
        ttft = [r["ttft_ms"] for r in warmed if r.get("ttft_ms") is not None]
        if ttft:
            stats["avg_ttft_ms"] = round(sum(ttft) / len(ttft), 1)
        req = [r["req_tok_s"] for r in warmed if r.get("req_tok_s") is not None]
        if req:
            stats["avg_req_tok_s"] = round(sum(req) / len(req), 2)
        tps = [r["total_tok_s"] for r in warmed if r.get("total_tok_s") is not None]
        if tps:
            stats["avg_total_tok_s"] = round(sum(tps) / len(tps), 1)

    return GroupResult(name=group.name, cmd=cmd, log_path=log_path, rows=rows, stats=stats)


# ── Scenario runner ────────────────────────────────────────────────────────────

async def run(spec: ScenarioSpec, timestamp: str) -> dict:
    """Execute the scenario and return a structured result dict."""
    import subprocess
    import client

    # kill any lingering ask --bench processes so they don't inflate per-node counts
    subprocess.run(["pkill", "-f", "ask --bench"], capture_output=True)
    await asyncio.sleep(3)  # let metrics settle after kill

    # before snapshot (clean — no external benchmark running)
    before_snapshot = metrics_mod.build_snapshot(await client.get_text("/metrics"))
    before_cr: dict[str, float] = {
        nm["node"]: nm.get("concurrent_requests", 0.0)
        for nm in before_snapshot["node_metrics"]
    }

    # create ephemeral nodes (fail fast if any already exist)
    created_node_ids: list[str] = []
    for node_id, fields in spec.create_nodes.items():
        await client.post(f"/nodes/{node_id}", fields)
        created_node_ids.append(node_id)

    # apply node overrides, capture originals for restore
    originals: dict[str, dict] = {}
    for node_id, overrides in spec.node_overrides.items():
        originals[node_id] = await client.get(f"/nodes/{node_id}")
        await client.put(f"/nodes/{node_id}", overrides)

    total_concurrency = sum(g.concurrency for g in spec.groups)
    started_at = datetime.now(timezone.utc)
    group_results: list[GroupResult] = []

    try:
        # start all groups as live tasks so they run during the warmup sleep
        running_tasks: list[asyncio.Task] = []
        for group in spec.groups:
            log_name = f"{timestamp}-{spec.scenario}-{group.name}.log"
            log_path = os.path.join(settings.BENCHMARK_LOGS_DIR, log_name)
            running_tasks.append(asyncio.create_task(_run_group(group, spec.duration, log_path)))

        # post-warmup concurrency check: abort if the delta above clean baseline exceeds
        # scenario concurrency by >5% (catches background inflation)
        await asyncio.sleep(spec.warmup + 10)  # extra 10s for connections to reach steady state
        warmup_snapshot = metrics_mod.build_snapshot(await client.get_text("/metrics"))
        violations = [
            f"node '{nm['node']}': delta={nm.get('concurrent_requests', 0) - before_cr.get(nm['node'], 0):.0f} "
            f"(expected ≤{total_concurrency * 1.05:.0f})"
            for nm in warmup_snapshot["node_metrics"]
            if (nm.get("concurrent_requests", 0) - before_cr.get(nm["node"], 0)) > total_concurrency * 1.05
        ]
        if violations:
            for t in running_tasks:
                t.cancel()
            await asyncio.gather(*running_tasks, return_exceptions=True)
            raise ValueError(
                f"warmup check failed — per-node load exceeds scenario concurrency. "
                f"Violations: {'; '.join(violations)}"
            )

        group_results = list(await asyncio.gather(*running_tasks))
    finally:
        # always restore node overrides
        for node_id, original in originals.items():
            flat = {k: v for k, v in {
                "max_tokens_in_flight":          original.get("capacity", {}).get("max_tokens_in_flight"),
                "max_waiting_requests":          original.get("capacity", {}).get("max_waiting_requests"),
                "per_request_tokens_per_second": original.get("capacity", {}).get("per_request_tokens_per_second"),
                "degradation_threshold":         original.get("capacity", {}).get("degradation_threshold"),
                "max_concurrency":               original.get("capacity", {}).get("max_concurrency"),
                "max_degradation":               original.get("degradation", {}).get("max_degradation"),
                "prefill_rate_multiplier":       original.get("realism", {}).get("prefill_rate_multiplier"),
                "prefill_base_overhead_ms":      original.get("realism", {}).get("prefill_base_overhead_ms"),
                "prefill_jitter_percent":        original.get("realism", {}).get("prefill_jitter_percent"),
                "prefill_max_ms":                original.get("realism", {}).get("prefill_max_ms"),
            }.items() if v is not None}
            await client.put(f"/nodes/{node_id}", flat)

        # always delete ephemeral nodes
        for node_id in created_node_ids:
            try:
                await client.delete(f"/nodes/{node_id}")
            except Exception:
                pass

    completed_at = datetime.now(timezone.utc)
    after_snapshot = metrics_mod.build_snapshot(await client.get_text("/metrics"))

    return {
        "scenario": spec.scenario,
        "description": spec.description,
        "source_hash": spec.source_hash,
        "tags": spec.tags,
        "duration_seconds": spec.duration,
        "warmup_seconds": spec.warmup,
        "started_at": started_at.isoformat(),
        "completed_at": completed_at.isoformat(),
        "node_overrides_applied": spec.node_overrides,
        "node_overrides_restored": list(originals.keys()),
        "nodes_created": created_node_ids,
        "nodes_deleted": created_node_ids,
        "before": before_snapshot,
        "after": after_snapshot,
        "groups": [
            {
                "name": gr.name,
                "cmd": " ".join(gr.cmd),
                "log_path": f"benchmark/logs/{os.path.basename(gr.log_path)}",
                "stats": gr.stats,
            }
            for gr in group_results
        ],
    }
