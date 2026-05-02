"""Log-tail parser and process-table check for get_benchmark_status."""

import os
import subprocess
import settings

# Column byte ranges from the ask --bench fixed-width output format:
#   "%-7d %-7d %-9s %-12s %-11.2f %-12s %-6s"
#    0-6   8-14  16-24  26-37   39-49    51-62   64-69
_HEADER_TOKEN = "thread"


def is_running() -> bool:
    result = subprocess.run(
        ["pgrep", "-f", "ask --bench"],
        capture_output=True,
    )
    return result.returncode == 0


def tail_log(n: int) -> list[str]:
    path = os.path.expanduser(settings.CLLM_BENCH_LOG)
    if not os.path.exists(path):
        return []
    with open(path, "r", errors="replace") as f:
        lines = f.readlines()
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
