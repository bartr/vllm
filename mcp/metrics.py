"""Prometheus text-format parser for cLLM metrics."""

import re
from collections import defaultdict

_LINE_RE = re.compile(r'^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+(\S+)')
_LABEL_RE = re.compile(r'(\w+)="([^"]*)"')

_NODE_SCALAR_METRICS = [
    "cllm_node_tokens_in_flight",
    "cllm_node_max_tokens_in_flight",
    "cllm_node_waiting_requests",
    "cllm_node_concurrent_requests",
    "cllm_node_max_concurrency",
    "cllm_node_per_request_tps_effective",
]

_NODE_COUNTER_METRICS = [
    "cllm_node_admissions_total",
]


def _parse_lines(text: str) -> list[dict]:
    rows = []
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        m = _LINE_RE.match(line)
        if not m:
            continue
        try:
            value = float(m.group(3))
        except ValueError:
            continue
        labels = dict(_LABEL_RE.findall(m.group(2) or ""))
        rows.append({"name": m.group(1), "labels": labels, "value": value})
    return rows


def build_snapshot(text: str, include_raw: bool = False) -> dict:
    rows = _parse_lines(text)
    observed: set[str] = set()

    # per-node metrics
    node_data: dict[str, dict] = defaultdict(dict)
    for row in rows:
        name, labels, value = row["name"], row["labels"], row["value"]
        node = labels.get("node")
        if not node:
            continue
        observed.add(name)
        short = name.removeprefix("cllm_node_")
        if name in _NODE_SCALAR_METRICS:
            node_data[node][short] = value
        elif name in _NODE_COUNTER_METRICS:
            result = labels.get("result", "")
            key = f"{short}_{result}" if result else short
            node_data[node][key] = value

    node_metrics = [{"node": k, **v} for k, v in sorted(node_data.items())]

    # cache metrics
    cache: dict = {}
    for row in rows:
        name, labels, value = row["name"], row["labels"], row["value"]
        if name == "cllm_cache_capacity":
            observed.add(name)
            cache["capacity"] = int(value)
        elif name == "cllm_cache_entries":
            observed.add(name)
            cache["entries"] = int(value)
        elif name == "cllm_cache_lookups_total":
            observed.add(name)
            result = labels.get("result", "")
            cache[f"lookups_{result}"] = int(value)

    result: dict = {
        "node_metrics": node_metrics,
        "cache_metrics": cache,
        "observed_metric_names": sorted(observed),
    }
    if include_raw:
        result["raw"] = text
    return result
