"""Helpers to filter askd run logs by time window.

askd writes one log file per run named `run-YYYYMMDDTHHMMSSZ.log` (or
`run-YYYYMMDDTHHMMSS.NNNNNNNNNZ.log` on sub-second collisions). The file
spans roughly [start_timestamp, file_modified_time]. A query window
matches a run iff the two intervals overlap.

The functions here only do parsing + filtering. HTTP is in ask_client.
"""

from datetime import datetime, timedelta, timezone

import benchmark


def parse_run_name(name: str) -> datetime | None:
    """Return the UTC start time encoded in a `run-...` file name."""
    if not name.startswith("run-") or not name.endswith(".log"):
        return None
    stamp = name[len("run-"):-len(".log")]
    # Strip optional fractional seconds (askd's collision suffix uses
    # `.NNNNNNNNN` between the seconds and the trailing Z).
    head, sep, _frac = stamp.partition(".")
    candidate = (head + "Z") if sep else stamp
    try:
        return datetime.strptime(candidate, "%Y%m%dT%H%M%SZ").replace(tzinfo=timezone.utc)
    except ValueError:
        return None


def parse_iso(s: str | None) -> datetime | None:
    """Parse an ISO-8601 timestamp; assume UTC if no offset given."""
    if not s:
        return None
    # datetime.fromisoformat in 3.11+ accepts trailing 'Z'.
    try:
        dt = datetime.fromisoformat(s.replace("Z", "+00:00"))
    except ValueError:
        return None
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt


def resolve_window(
    last_seconds: int | None,
    last_minutes: int | None,
    last_hours: int | None,
    since: str | None,
    until: str | None,
) -> tuple[datetime | None, datetime | None, str | None]:
    """Resolve the (since, until, error) tuple from the supported inputs.

    `last_*` shortcuts win and pin until=now. Otherwise raw since/until
    ISO strings are parsed (UTC-default). Returns (None, None, None)
    when no window was requested.
    """
    relative = next((v for v in (last_seconds, last_minutes, last_hours) if v is not None), None)
    if relative is not None:
        delta = (
            timedelta(seconds=last_seconds or 0)
            + timedelta(minutes=last_minutes or 0)
            + timedelta(hours=last_hours or 0)
        )
        if delta.total_seconds() <= 0:
            return None, None, "last_* must be > 0"
        now = datetime.now(timezone.utc)
        return now - delta, now, None

    s = parse_iso(since)
    u = parse_iso(until)
    if since and not s:
        return None, None, f"could not parse since={since!r} (expected ISO-8601 like 2026-05-06T13:00:00Z)"
    if until and not u:
        return None, None, f"could not parse until={until!r} (expected ISO-8601 like 2026-05-06T13:30:00Z)"
    if s and u and u < s:
        return None, None, "until must be >= since"
    return s, u, None


def runs_in_window(
    listing: list[dict],
    since: datetime | None,
    until: datetime | None,
) -> list[dict]:
    """Filter the askd /logs listing to runs that overlap the window.

    Each entry is enriched with a `started_at` field parsed from its
    name. Entries with unparseable names are dropped.
    """
    out = []
    for entry in listing:
        started = parse_run_name(entry.get("name", ""))
        if started is None:
            continue
        modified = parse_iso(entry.get("modified")) or started
        # Overlap test: max(start, since) <= min(end, until).
        run_start, run_end = started, modified
        if since and run_end < since:
            continue
        if until and run_start > until:
            continue
        enriched = dict(entry)
        enriched["started_at"] = started.isoformat()
        out.append(enriched)
    return out


def summarize_log_text(text: str) -> dict:
    """Parse the fixed-width rows in a log and produce summary stats."""
    rows = [r for line in text.splitlines() if (r := benchmark.parse_row(line)) is not None]
    warmed = [r for r in rows if r.get("total_tok_s") is not None]
    summary: dict = {
        "rows": len(rows),
        "warmup_rows": len(rows) - len(warmed),
    }
    if warmed:
        cache_hits = sum(1 for r in warmed if r.get("cache") == "hit")
        cache_total = sum(1 for r in warmed if r.get("cache") in ("hit", "miss"))
        summary["cache_hit_rate_pct"] = round(cache_hits / cache_total * 100, 1) if cache_total else None

        def avg(field: str) -> float | None:
            vals = [r[field] for r in warmed if r.get(field) is not None]
            return round(sum(vals) / len(vals), 2) if vals else None

        summary["avg_ttft_ms"] = avg("ttft_ms")
        summary["avg_req_tok_s"] = avg("req_tok_s")
        summary["avg_total_tok_s"] = avg("total_tok_s")
    return summary


def extract_markers(text: str, max_markers: int = 20) -> list[str]:
    """Pull out the `=== askd ... ===` marker lines (start/pause/stop/end)."""
    out = []
    for line in text.splitlines():
        if line.startswith("=== askd ") and line.endswith(" ==="):
            out.append(line)
            if len(out) >= max_markers:
                break
    return out
