#!/usr/bin/env bash
# Long-running, balanced load generator for the cllm dashboards.
#
# Launches two independent `ask` workers in parallel — one pinned to
# node=cllm, one pinned to node=vllm — so each lane has its own
# concurrency pool. Without separate workers a single shared pool
# drifts toward the slower lane and per-node fleet TPS lines equalize.
#
# Each worker runs forever; whichever exits is auto-restarted so the
# dashboards stay populated.
#
# Usage:
#   ./scripts/dashboard-load.sh                 # bench=10 per lane (20 total)
#   BENCH=20 ./scripts/dashboard-load.sh        # 20 per lane
#   MAX_TOKENS=200 ./scripts/dashboard-load.sh  # longer completions
#
# Stop with Ctrl-C.

set -uo pipefail

BENCH="${BENCH:-32}"
MAX_TOKENS="${MAX_TOKENS:-100}"
URL="${CLLM_URL:-http://localhost:8088}"
FILES="${FILES:-$(dirname "$0")/prompts.yaml}"
ASK="${ASK:-$HOME/go/bin/ask}"

trap 'echo; echo "dashboard-load: stopping"; kill 0 2>/dev/null; exit 0' INT TERM

run_lane() {
  local lane="$1" extra_dsl="$2"
  while true; do
    "$ASK" --url "$URL" \
           --bench "$BENCH" --loop \
           --files "$FILES" \
           --max-tokens "$MAX_TOKENS" \
           --dsl "node=$lane $extra_dsl" \
           --quiet || true
    echo "dashboard-load[$lane]: ask exited, restarting in 2s..."
    sleep 2
  done
}

echo "dashboard-load: url=$URL bench=$BENCH/lane max-tokens=$MAX_TOKENS files=$FILES"
echo "dashboard-load: lanes=cllm (cacheable), vllm (no-cache)"
echo "dashboard-load: Ctrl-C to stop"

run_lane "cllm"     ""         &
run_lane "vllm"     "no-cache" &
wait
