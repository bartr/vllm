"""Benchmark log parser + askd HTTP shims for MCP tools.

The local `pgrep` / `~/logs/bench.log` path is gone. `is_running()` and
`tail_log()` are now thin async wrappers around the askd HTTP control
plane in the cllm namespace. `parse_row()` still parses the same
fixed-width row format that `ask` (and now askd) emit through
`runAggregator`, so downstream parsing logic in scenario.py / server.py
is unchanged.
"""

import ask_client

# Column byte ranges from the ask --bench fixed-width output format:
#   "%-7d %-7d %-9s %-12s %-11.2f %-12s %-6s"
#    0-6   8-14  16-24  26-37   39-49    51-62   64-69
_HEADER_TOKEN = "thread"


async def is_running() -> bool:
    """True iff askd reports a job in 'running' or 'paused' state.

    Raises ask_client.AskNotDeployedError if askd is unreachable.
    """
    status = await ask_client.get_status()
    return status.get("state") in ("running", "paused")


async def tail_log(n: int) -> list[str]:
    """Return up to the last n lines of the most recent askd run log.

    Returns [] if no logs exist yet. Raises AskNotDeployedError if askd
    is unreachable.
    """
    listing = await ask_client.list_logs()
    logs = listing.get("logs", [])
    if not logs:
        return []
    # listing is sorted newest-first by askd.
    name = logs[0]["name"]
    # Pull a generous tail in bytes (~200 chars per fixed-width row).
    text = await ask_client.tail_log_text(name, tail_bytes=max(n, 1) * 256)
    lines = text.splitlines()
    return lines[-n:] if len(lines) > n else lines


def parse_row(line: str) -> dict | None:
    """Parse one fixed-width data row. Returns None for header/blank/non-data lines."""
    line = line.rstrip("\n\r")
    if len(line) < 64:
        return None

    def col(start: int, end: int) -> str:
        return line[start : min(end, len(line))].strip()

    thread_s = col(0, 7)
    if not thread_s or thread_s == _HEADER_TOKEN:
        return None
    try:
        thread = int(thread_s)
    except ValueError:
        return None

    def to_float(s: str) -> float | None:
        try:
            return float(s) if s else None
        except ValueError:
            return None

    def to_int(s: str) -> int | None:
        try:
            return int(s) if s else None
        except ValueError:
            return None

    ttft_s = col(16, 25)
    return {
        "thread":      thread,
        "tokens":      to_int(col(8, 15)),
        "ttft_ms":     to_float(ttft_s) if ttft_s != "n/a" else None,
        "duration_ms": to_float(col(26, 38)),
        "req_tok_s":   to_float(col(39, 50)),
        # blank here means the 15-second sliding window hasn't filled yet (warmup)
        "total_tok_s": to_float(col(51, 63)),
        "cache":       col(64, 70) or None,
    }
