#!/usr/bin/env bash
# A/B comparison: cllm modeled node vs cllm passthrough node.
#
# Both sides go through cllm (http://localhost:8088), so the same
# Grafana dashboards (cllm-overview, cllm-kv, cllm-phase) cover both.
# The dashboards already break per-node series, so the two lanes
# appear as distinct lines.
#
#   A) node=vllm  - caps so loose that admission/queue/
#                          degradation never fire. Proxies to vLLM
#                          unmodified; latency reflects vLLM only.
#   B) node=cllm-rtx      - production-shape modeled node:
#                            max_tokens_per_second: 600
#                            max_tokens_in_flight:  6000
#                            max_degradation:       50
#                          Synthesizes graceful degradation as load
#                          exceeds 10% of in-flight capacity.
#
# Usage:
#   ./scripts/ab-vllm-vs-cllm.sh                 # default 60s, bench=20
#   BENCH=40 DURATION=2m ./scripts/ab-vllm-vs-cllm.sh
#   COUNT=400 ./scripts/ab-vllm-vs-cllm.sh       # bounded by request count
#
# Dashboard tip: filter cllm-overview by node=~"cllm-rtx|vllm"
# and put per-node panels side by side.

set -euo pipefail

BENCH="${BENCH:-20}"
DURATION="${DURATION:-60s}"
COUNT="${COUNT:-}"
MAX_TOKENS="${MAX_TOKENS:-100}"
FILES="${FILES:-$(dirname "$0")/prompts.yaml}"
URL="${CLLM_URL:-http://localhost:8088}"

OUT=/tmp/ab-$(date +%s)
mkdir -p "$OUT"
echo "writing results to $OUT"

run_side() {
  local label="$1" node="$2"
  echo
  echo "=========================================================="
  echo "  side: $label   node=$node"
  echo "=========================================================="

  curl -s "$URL/metrics" > "$OUT/$label.metrics.before" || true

  local stop_args=( --duration "$DURATION" )
  [[ -n "$COUNT" ]] && stop_args=( --count "$COUNT" )

  ~/go/bin/ask --url "$URL" --bench "$BENCH" --loop \
               --files "$FILES" --max-tokens "$MAX_TOKENS" \
               "${stop_args[@]}" \
               --dsl "node=$node no-cache" \
               --json --quiet > "$OUT/$label.ndjson" 2> "$OUT/$label.report"

  curl -s "$URL/metrics" > "$OUT/$label.metrics.after" || true

  echo "--- $label report ---"
  tail -25 "$OUT/$label.report"
}

run_side "A-passthrough" "vllm"
run_side "B-modeled"     "cllm-rtx"

echo
echo "=========================================================="
echo "  A/B summary"
echo "=========================================================="
for f in "$OUT"/A-passthrough.report "$OUT"/B-modeled.report; do
  echo
  echo "$(basename "$f")"
  grep -E '^(throughput|duration_ms|ttft_ms|cache_hits|requests):' "$f" | sed 's/^/    /'
done

echo
echo "Per-node metric deltas:"
for label in A-passthrough B-modeled; do
  echo
  echo "=== $label ==="
  diff \
    <(grep -E '^cllm_(tenant_admissions_total|tenant_rejections_total)' "$OUT/$label.metrics.before" | sort) \
    <(grep -E '^cllm_(tenant_admissions_total|tenant_rejections_total)' "$OUT/$label.metrics.after"  | sort) \
    | head -30
done

echo
echo "Raw NDJSON: $OUT/{A,B}-*.ndjson"
echo "Dashboards: http://localhost:8088/grafana — filter node=~\"cllm-rtx|vllm\""
