"""Append-only audit log for mutating MCP tool calls."""

import json
import os
from datetime import datetime, timezone

import settings


def log(tool: str, inputs: dict, result: dict, before: dict | None = None, after: dict | None = None) -> None:
    entry: dict = {
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "tool": tool,
        "inputs": inputs,
        "result": result,
    }
    if before is not None:
        entry["before"] = before
    if after is not None:
        entry["after"] = after

    path = os.path.expanduser(settings.CLLM_AUDIT_LOG)
    try:
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "a") as f:
            f.write(json.dumps(entry) + "\n")
    except OSError:
        pass
