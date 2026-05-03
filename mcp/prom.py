"""Thin Prometheus HTTP API client for windowed scenario queries.

Wraps `/api/v1/query` and `/api/v1/query_range` so scenario reports can ask
Prometheus directly for counter-reset-safe deltas and percentile-aware
windows over `[started_at, completed_at]` instead of subtracting two
in-process metric snapshots.

The recording rules in clusters/z01/prometheus/rules/cllm-windows.yaml
provide most of the metric names used here as `cllm:...` records, but
this client works against any PromQL expression.
"""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Iterable

import httpx

import settings


def _ts(t: datetime | float | int | str) -> float:
    """Coerce a datetime, ISO-8601 string, or epoch number into a UNIX timestamp (seconds)."""
    if isinstance(t, str):
        # Accept e.g. "2026-05-02T23:42:35.254528+00:00".
        # Python <3.11 doesn't parse trailing 'Z'; normalize it.
        s = t.replace("Z", "+00:00")
        t = datetime.fromisoformat(s)
    if isinstance(t, datetime):
        if t.tzinfo is None:
            t = t.replace(tzinfo=timezone.utc)
        return t.timestamp()
    return float(t)


async def query(expr: str, at: datetime | float | int | None = None) -> list[dict]:
    """Run an instant query. Returns the `data.result` list (possibly empty).

    Each element is `{"metric": {...labels}, "value": [ts, "string-value"]}`.
    """
    params: dict[str, str] = {"query": expr}
    if at is not None:
        params["time"] = f"{_ts(at):.3f}"
    url = f"{settings.CLLM_PROMETHEUS_URL}/api/v1/query"
    async with httpx.AsyncClient(timeout=settings.CLLM_REQUEST_TIMEOUT_SECONDS) as c:
        r = await c.get(url, params=params)
        r.raise_for_status()
        body = r.json()
    if body.get("status") != "success":
        raise RuntimeError(f"prometheus query failed: {body.get('error')}")
    return body["data"].get("result", [])


async def query_range(
    expr: str,
    start: datetime | float | int,
    end: datetime | float | int,
    step: str = "15s",
) -> list[dict]:
    """Run a range query. Returns the `data.result` list (possibly empty).

    Each element is `{"metric": {...labels}, "values": [[ts, "string-value"], ...]}`.
    """
    params = {
        "query": expr,
        "start": f"{_ts(start):.3f}",
        "end":   f"{_ts(end):.3f}",
        "step":  step,
    }
    url = f"{settings.CLLM_PROMETHEUS_URL}/api/v1/query_range"
    async with httpx.AsyncClient(timeout=settings.CLLM_REQUEST_TIMEOUT_SECONDS) as c:
        r = await c.get(url, params=params)
        r.raise_for_status()
        body = r.json()
    if body.get("status") != "success":
        raise RuntimeError(f"prometheus query_range failed: {body.get('error')}")
    return body["data"].get("result", [])


def _scalar(result: list[dict]) -> float | None:
    """Extract a single numeric value from an instant-query result."""
    if not result:
        return None
    val = result[0].get("value")
    if not val or len(val) < 2:
        return None
    try:
        return float(val[1])
    except (TypeError, ValueError):
        return None


def _by_label(result: list[dict], label: str) -> dict[str, float]:
    """Bucket scalar instant-query results by a single label."""
    out: dict[str, float] = {}
    for series in result:
        key = series.get("metric", {}).get(label, "")
        val = series.get("value")
        if not val or len(val) < 2:
            continue
        try:
            out[key] = float(val[1])
        except (TypeError, ValueError):
            continue
    return out


# ── Scenario-window helpers ────────────────────────────────────────────────

async def admissions_in_window(
    start: datetime | float | int,
    end: datetime | float | int,
) -> dict[str, dict[str, float]]:
    """Return {node: {"admitted": N, "rejected": M, "reject_ratio": x}} for the window.

    Counter-reset safe — uses `increase()` against the window length.
    """
    duration_s = max(1.0, _ts(end) - _ts(start))
    expr = f"increase(cllm_node_admissions_total[{int(duration_s)}s])"
    result = await query(expr, at=end)

    agg: dict[str, dict[str, float]] = {}
    for series in result:
        node = series["metric"].get("node", "")
        res  = series["metric"].get("result", "")
        try:
            value = float(series["value"][1])
        except (KeyError, IndexError, TypeError, ValueError):
            continue
        node_d = agg.setdefault(node, {"admitted": 0.0, "rejected": 0.0})
        if res in ("admitted", "rejected"):
            node_d[res] = value

    for node_d in agg.values():
        total = node_d["admitted"] + node_d["rejected"]
        node_d["reject_ratio"] = (node_d["rejected"] / total) if total > 0 else 0.0
    return agg


async def ttft_p95_in_window(
    start: datetime | float | int,
    end: datetime | float | int,
) -> dict[str, float]:
    """Return {node: p95_seconds} for TTFT over the window."""
    duration_s = max(1.0, _ts(end) - _ts(start))
    expr = (
        "histogram_quantile(0.95, "
        f"sum by (le, node) (rate(cllm_time_to_first_byte_seconds_bucket[{int(duration_s)}s])))"
    )
    return _by_label(await query(expr, at=end), "node")


async def cache_hit_ratio_in_window(
    start: datetime | float | int,
    end: datetime | float | int,
) -> float | None:
    """Return cache hit ratio (0–1) over the window, or None if no lookups."""
    duration_s = max(1.0, _ts(end) - _ts(start))
    expr = (
        f'sum(increase(cllm_cache_lookups_total{{result="hit"}}[{int(duration_s)}s]))'
        f"/clamp_min(sum(increase(cllm_cache_lookups_total[{int(duration_s)}s])),1)"
    )
    return _scalar(await query(expr, at=end))


async def tokens_per_second_in_window(
    start: datetime | float | int,
    end: datetime | float | int,
) -> dict[str, float]:
    """Return {node: avg_tokens_per_second} over the window."""
    duration_s = max(1.0, _ts(end) - _ts(start))
    expr = (
        "sum by (node) "
        f"(rate(cllm_completion_tokens_total[{int(duration_s)}s]))"
    )
    return _by_label(await query(expr, at=end), "node")


async def scenario_window_summary(
    start: datetime | float | int,
    end: datetime | float | int,
    extra_queries: Iterable[tuple[str, str]] = (),
) -> dict:
    """One-shot scenario-window evidence bundle.

    Returns admissions, TTFT p95 per node, cache hit ratio, and tokens/sec per
    node — all computed with `increase()`/`rate()` so counter resets within
    the window are handled correctly.

    `extra_queries` is an iterable of `(label, promql_expr)` pairs; results
    are attached under `extras[label]` as the raw Prometheus result list.
    """
    admissions = await admissions_in_window(start, end)
    ttft_p95   = await ttft_p95_in_window(start, end)
    hit_ratio  = await cache_hit_ratio_in_window(start, end)
    tps        = await tokens_per_second_in_window(start, end)

    extras: dict[str, list[dict]] = {}
    for label, expr in extra_queries:
        extras[label] = await query(expr, at=end)

    return {
        "window": {
            "start": _ts(start),
            "end":   _ts(end),
            "duration_seconds": _ts(end) - _ts(start),
        },
        "admissions_by_node": admissions,
        "ttft_p95_seconds_by_node": ttft_p95,
        "cache_hit_ratio": hit_ratio,
        "tokens_per_second_by_node": tps,
        "extras": extras,
    }
