#!/usr/bin/env bash

set -euo pipefail

VLLM_BENCH_URL=${VLLM_BENCH_URL:-http://localhost:8088}
VLLM_BENCH_TOKEN=${VLLM_BENCH_TOKEN:-}
VLLM_BENCH_MODEL=${VLLM_BENCH_MODEL:-}
VLLM_BENCH_CONCURRENCY=${VLLM_BENCH_CONCURRENCY:-20}
VLLM_BENCH_DURATION=${VLLM_BENCH_DURATION:-}
VLLM_BENCH_MAX_TOKENS=${VLLM_BENCH_MAX_TOKENS:-128}
VLLM_BENCH_TEMPERATURE=${VLLM_BENCH_TEMPERATURE:-0}
VLLM_BENCH_STREAM=${VLLM_BENCH_STREAM:-1}
VLLM_BENCH_WARMUP=${VLLM_BENCH_WARMUP:-0}
VLLM_BENCH_SYSTEM_PROMPT=${VLLM_BENCH_SYSTEM_PROMPT:-You are a helpful assistant.}
VLLM_BENCH_PROMPT=${VLLM_BENCH_PROMPT:-Explain Azure}

stop_requested=0
stop_file=
result_fifo=
aggregator_pid=
timer_pid=
worker_pids=()

parse_duration_seconds() {
  python3 - "$1" <<'PY'
import re
import sys

value = sys.argv[1].strip().lower()
match = re.fullmatch(r"(\d+)([smh]?)", value)
if not match:
    raise SystemExit(1)

amount = int(match.group(1))
suffix = match.group(2) or "s"
multiplier = {"s": 1, "m": 60, "h": 3600}[suffix]
print(amount * multiplier)
PY
}

estimated_request_seconds() {
  python3 - "$VLLM_BENCH_MAX_TOKENS" <<'PY'
import sys

max_tokens = int(sys.argv[1])
print(f"{max_tokens / 32.0:.6f}")
PY
}

initial_thread_delay_seconds() {
  local thread_number=$1
  python3 - "$thread_number" "$VLLM_BENCH_CONCURRENCY" "$VLLM_BENCH_MAX_TOKENS" <<'PY'
import sys

thread_number = int(sys.argv[1])
concurrency = int(sys.argv[2])
max_tokens = int(sys.argv[3])

if concurrency <= 1:
    print("0")
else:
    request_seconds = max_tokens / 32.0
    delay_seconds = request_seconds * (thread_number - 1) / concurrency
    print(f"{delay_seconds:.6f}")
PY
}

usage() {
  cat <<'EOF'
Usage: ./scripts/benchmark.sh [options]

Run a fixed number of concurrent benchmark workers against an OpenAI-compatible
endpoint until Ctrl-C is pressed. Each completed request prints its worker
thread number, TTFT, request duration, per-request tokens/sec, and sampled
aggregate tokens/sec across the same request window.

Options:
  -h, --help                 Show this help message and exit
      --url URL              Base URL for the target endpoint (default: http://localhost:8088)
      --token TOKEN          Bearer token for authenticated endpoints
      --model MODEL          Model ID to benchmark; auto-detected from /v1/models if omitted
      --concurrency N        Number of concurrent workers to run continuously (default: 1)
      --duration VALUE       Stop automatically after VALUE, e.g. 10s, 5m, or 1h
      --max-tokens N         Max completion tokens per request (default: 64)
      --temperature VALUE    Sampling temperature (default: 0)
      --stream               Use streaming chat completions and measure TTFT
      --warmup               Run one untimed warmup request before the live loop starts
      --system-prompt TEXT   System prompt sent with each request
      --prompt TEXT          User prompt sent with each request

Environment:
  VLLM_BENCH_URL
  VLLM_BENCH_TOKEN
  VLLM_BENCH_MODEL
  VLLM_BENCH_CONCURRENCY
  VLLM_BENCH_DURATION
  VLLM_BENCH_MAX_TOKENS
  VLLM_BENCH_TEMPERATURE
  VLLM_BENCH_STREAM
  VLLM_BENCH_WARMUP
  VLLM_BENCH_SYSTEM_PROMPT
  VLLM_BENCH_PROMPT
EOF
}

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Missing required command: $1" >&2
    exit 1
  fi
}

require_command curl
require_command jq
require_command python3
require_command mkfifo
require_command mktemp

while [[ $# -gt 0 ]]; do
  case "$1" in
    -h|--help)
      usage
      exit 0
      ;;
    --url)
      VLLM_BENCH_URL=$2
      shift 2
      ;;
    --token)
      VLLM_BENCH_TOKEN=$2
      shift 2
      ;;
    --model)
      VLLM_BENCH_MODEL=$2
      shift 2
      ;;
    --concurrency)
      VLLM_BENCH_CONCURRENCY=$2
      shift 2
      ;;
    --duration)
      VLLM_BENCH_DURATION=$2
      shift 2
      ;;
    --max-tokens)
      VLLM_BENCH_MAX_TOKENS=$2
      shift 2
      ;;
    --temperature)
      VLLM_BENCH_TEMPERATURE=$2
      shift 2
      ;;
    --stream)
      VLLM_BENCH_STREAM=1
      shift 1
      ;;
    --warmup)
      VLLM_BENCH_WARMUP=1
      shift 1
      ;;
    --system-prompt)
      VLLM_BENCH_SYSTEM_PROMPT=$2
      shift 2
      ;;
    --prompt)
      VLLM_BENCH_PROMPT=$2
      shift 2
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

if ! [[ "$VLLM_BENCH_CONCURRENCY" =~ ^[0-9]+$ ]] || (( VLLM_BENCH_CONCURRENCY < 1 )); then
  echo "VLLM_BENCH_CONCURRENCY must be an integer >= 1." >&2
  exit 1
fi

duration_seconds=
if [[ -n "$VLLM_BENCH_DURATION" ]]; then
  if ! duration_seconds=$(parse_duration_seconds "$VLLM_BENCH_DURATION"); then
    echo "VLLM_BENCH_DURATION must look like 10s, 5m, 1h, or an integer number of seconds." >&2
    exit 1
  fi
  if (( duration_seconds < 1 )); then
    echo "VLLM_BENCH_DURATION must be at least 1 second." >&2
    exit 1
  fi
fi

if ! [[ "$VLLM_BENCH_MAX_TOKENS" =~ ^[0-9]+$ ]] || (( VLLM_BENCH_MAX_TOKENS < 1 )); then
  echo "VLLM_BENCH_MAX_TOKENS must be an integer >= 1." >&2
  exit 1
fi

if [[ "$VLLM_BENCH_WARMUP" != "0" && "$VLLM_BENCH_WARMUP" != "1" ]]; then
  echo "VLLM_BENCH_WARMUP must be 0 or 1." >&2
  exit 1
fi

if [[ "$VLLM_BENCH_STREAM" != "0" && "$VLLM_BENCH_STREAM" != "1" ]]; then
  echo "VLLM_BENCH_STREAM must be 0 or 1." >&2
  exit 1
fi

build_curl_args() {
  curl_args=(-sS)
  if [[ -n "$VLLM_BENCH_TOKEN" ]]; then
    curl_args+=(-H "Authorization: Bearer $VLLM_BENCH_TOKEN")
  fi
}

warn_if_bypassing_cllm() {
  case "$VLLM_BENCH_URL" in
    http://localhost:8000|http://127.0.0.1:8000)
      if [[ "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 1 http://127.0.0.1:8080/health 2>/dev/null || true)" == "200" ]]; then
        echo "note: cLLM is available at http://127.0.0.1:8080, but this benchmark is targeting downstream vLLM directly at $VLLM_BENCH_URL." >&2
        echo "      cLLM /metrics and /config queue counters only change when requests go through cLLM." >&2
        echo "      rerun with: VLLM_BENCH_URL=http://127.0.0.1:8080 ./scripts/benchmark.sh --concurrency $VLLM_BENCH_CONCURRENCY" >&2
      fi
      ;;
  esac
}

wait_for_endpoint() {
  local attempts=${1:-60}
  local delay_s=${2:-2}
  local attempt output

  for ((attempt = 1; attempt <= attempts; attempt += 1)); do
    if output=$(curl "${curl_args[@]}" -o /dev/null -w '%{http_code}' "$VLLM_BENCH_URL/health" 2>&1); then
      if [[ "$output" == "200" ]]; then
        return 0
      fi
    fi

    if output=$(curl "${curl_args[@]}" -o /dev/null -w '%{http_code}' "$VLLM_BENCH_URL/health" 2>&1); then
      if [[ "$output" == "200" ]]; then
        return 0
      fi
    fi

    if (( attempt == attempts )); then
      echo "endpoint did not become ready at $VLLM_BENCH_URL/health after $attempts attempts." >&2
      echo "$output" >&2
      return 1
    fi
    echo "waiting for endpoint at $VLLM_BENCH_URL/health (attempt $attempt/$attempts)..." >&2
    sleep "$delay_s"
  done
}

build_request_body() {
  local max_tokens_override=${1:-$VLLM_BENCH_MAX_TOKENS}
  jq -n \
    --arg model "$VLLM_BENCH_MODEL" \
    --arg system_prompt "$VLLM_BENCH_SYSTEM_PROMPT" \
    --arg prompt "$VLLM_BENCH_PROMPT" \
    --argjson max_tokens "$max_tokens_override" \
    --argjson temperature "$VLLM_BENCH_TEMPERATURE" \
    --argjson stream "$VLLM_BENCH_STREAM" \
    '{
      model: $model,
      messages: [
        {role: "system", content: $system_prompt},
        {role: "user", content: $prompt}
      ],
      max_tokens: $max_tokens,
      temperature: $temperature,
      stream: ($stream == 1),
      stream_options: (if $stream == 1 then {include_usage: true} else null end)
    }'
}

detect_model() {
  if [[ -n "$VLLM_BENCH_MODEL" ]]; then
    return
  fi

  local models_response
  models_response=$(curl "${curl_args[@]}" "$VLLM_BENCH_URL/v1/models")
  VLLM_BENCH_MODEL=$(printf '%s' "$models_response" | jq -r '.data[0].id // empty')
  if [[ -z "$VLLM_BENCH_MODEL" ]]; then
    echo "Failed to auto-detect a model from $VLLM_BENCH_URL/v1/models." >&2
    exit 1
  fi
}

run_request() {
  local request_body=$1
  local thread_number=$2
  local request_started_ns request_ended_ns response_file error_file curl_metrics http_code latency_s ttft_s latency_ns ttft_ns curl_exit
  local stream_args=()

  request_started_ns=$(date +%s%N)
  response_file=$(mktemp)
  error_file=$(mktemp)

  if [[ "$VLLM_BENCH_STREAM" == "1" ]]; then
    stream_args=(-N)
  fi

  if ! curl_metrics=$(curl "${curl_args[@]}" \
    "${stream_args[@]}" \
    -o "$response_file" \
    -w '%{http_code}\t%{time_total}\t%{time_starttransfer}' \
    -H 'Content-Type: application/json' \
    -d "$request_body" \
    "$VLLM_BENCH_URL/v1/chat/completions" 2>"$error_file"); then
    curl_exit=$?
    request_ended_ns=$(date +%s%N)
    jq -nc \
      --argjson thread "$thread_number" \
      --arg error "$(tr '\n' ' ' < "$error_file")" \
      --argjson curl_exit "$curl_exit" \
      --argjson started_at_ns "$request_started_ns" \
      --argjson ended_at_ns "$request_ended_ns" \
      '{
        ok: false,
        thread: $thread,
        error: (if $error == "" then "curl failed" else $error end),
        curl_exit: $curl_exit,
        started_at_ns: $started_at_ns,
        ended_at_ns: $ended_at_ns,
        duration_ns: ($ended_at_ns - $started_at_ns),
        completion_tokens: 0,
        prompt_tokens: 0,
        total_tokens: 0,
        ttft_ns: null
      }'
    rm -f "$response_file" "$error_file"
    return 0
  fi

  request_ended_ns=$(date +%s%N)
  IFS=$'\t' read -r http_code latency_s ttft_s <<< "$curl_metrics"
  latency_ns=$(python3 - "$latency_s" <<'PY'
import sys
print(int(float(sys.argv[1]) * 1_000_000_000))
PY
)

  if [[ "$VLLM_BENCH_STREAM" == "1" ]]; then
    ttft_ns=$(python3 - "$ttft_s" <<'PY'
import sys
print(int(float(sys.argv[1]) * 1_000_000_000))
PY
)
  else
    ttft_ns=null
  fi

  if [[ "$http_code" -lt 200 || "$http_code" -ge 300 ]]; then
    if [[ "$http_code" -gt 399 ]]; then
      echo "request failed: thread=$thread_number status=$http_code endpoint=$VLLM_BENCH_URL/v1/chat/completions" >&2
    fi
    jq -nc \
      --argjson thread "$thread_number" \
      --argjson status "$http_code" \
      --arg body "$(tr '\n' ' ' < "$response_file")" \
      --argjson started_at_ns "$request_started_ns" \
      --argjson ended_at_ns "$request_ended_ns" \
      --argjson duration_ns "$latency_ns" \
      '{
        ok: false,
        thread: $thread,
        status: $status,
        error: $body,
        started_at_ns: $started_at_ns,
        ended_at_ns: $ended_at_ns,
        duration_ns: $duration_ns,
        completion_tokens: 0,
        prompt_tokens: 0,
        total_tokens: 0,
        ttft_ns: null
      }'
    rm -f "$response_file" "$error_file"
    return 0
  fi

  if [[ "$VLLM_BENCH_STREAM" == "1" ]]; then
    if ! python3 - "$response_file" "$thread_number" "$request_started_ns" "$request_ended_ns" "$latency_ns" "$ttft_ns" <<'PY'
import json
import sys

response_path = sys.argv[1]
thread_number = int(sys.argv[2])
started_at_ns = int(sys.argv[3])
ended_at_ns = int(sys.argv[4])
duration_ns = int(sys.argv[5])
ttft_ns = int(sys.argv[6])
completion_tokens = 0
prompt_tokens = 0
total_tokens = 0

with open(response_path, encoding="utf-8") as handle:
    for raw_line in handle:
        line = raw_line.strip()
        if not line or not line.startswith("data: "):
            continue
        payload = line[6:]
        if payload == "[DONE]":
            continue
        obj = json.loads(payload)
        usage = obj.get("usage")
        if usage:
            completion_tokens = int(usage.get("completion_tokens", 0))
            prompt_tokens = int(usage.get("prompt_tokens", 0))
            total_tokens = int(usage.get("total_tokens", 0))

print(json.dumps({
    "ok": True,
    "thread": thread_number,
    "completion_tokens": completion_tokens,
    "prompt_tokens": prompt_tokens,
    "total_tokens": total_tokens,
    "started_at_ns": started_at_ns,
    "ended_at_ns": ended_at_ns,
    "duration_ns": duration_ns,
    "ttft_ns": ttft_ns,
}))
PY
    then
      jq -nc \
        --argjson thread "$thread_number" \
        --argjson started_at_ns "$request_started_ns" \
        --argjson ended_at_ns "$request_ended_ns" \
        --argjson duration_ns "$latency_ns" \
        '{
          ok: false,
          thread: $thread,
          error: "invalid streaming response",
          started_at_ns: $started_at_ns,
          ended_at_ns: $ended_at_ns,
          duration_ns: $duration_ns,
          completion_tokens: 0,
          prompt_tokens: 0,
          total_tokens: 0,
          ttft_ns: null
        }'
    fi
  else
    if ! jq -cn \
      --argjson thread "$thread_number" \
      --argjson started_at_ns "$request_started_ns" \
      --argjson ended_at_ns "$request_ended_ns" \
      --argjson duration_ns "$latency_ns" \
      --slurpfile payload "$response_file" \
      '{
        ok: true,
        thread: $thread,
        completion_tokens: ($payload[0].usage.completion_tokens // 0),
        prompt_tokens: ($payload[0].usage.prompt_tokens // 0),
        total_tokens: ($payload[0].usage.total_tokens // 0),
        started_at_ns: $started_at_ns,
        ended_at_ns: $ended_at_ns,
        duration_ns: $duration_ns,
        ttft_ns: null
      }'; then
      jq -nc \
        --argjson thread "$thread_number" \
        --argjson started_at_ns "$request_started_ns" \
        --argjson ended_at_ns "$request_ended_ns" \
        --argjson duration_ns "$latency_ns" \
        '{
          ok: false,
          thread: $thread,
          error: "invalid JSON response",
          started_at_ns: $started_at_ns,
          ended_at_ns: $ended_at_ns,
          duration_ns: $duration_ns,
          completion_tokens: 0,
          prompt_tokens: 0,
          total_tokens: 0,
          ttft_ns: null
        }'
    fi
  fi

  rm -f "$response_file" "$error_file"
}

run_worker_loop() {
  local thread_number=$1
  local request_body=$2
  local fifo_path=$3
  local initial_delay_seconds=$4

  trap '' INT TERM
  if [[ ! -f "$stop_file" ]] && [[ "$initial_delay_seconds" != "0" && "$initial_delay_seconds" != "0.000000" ]]; then
    python3 - "$initial_delay_seconds" <<'PY'
import sys
import time

time.sleep(float(sys.argv[1]))
PY
  fi
  while [[ ! -f "$stop_file" ]]; do
    run_request "$request_body" "$thread_number" > "$fifo_path"
  done
}

run_aggregator() {
  local fifo_path=$1

  python3 - "$fifo_path" <<'PY'
from collections import deque
import json
import signal
import sys

fifo_path = sys.argv[1]
window_ns = 15 * 1_000_000_000
completion_window = deque()

signal.signal(signal.SIGPIPE, signal.SIG_DFL)

def total_tps_last_minute(end_ns: int) -> float:
  window_start_ns = end_ns - window_ns
  while completion_window and completion_window[0][0] < window_start_ns:
    completion_window.popleft()
  if not completion_window:
        return 0.0
  total_tokens = sum(tokens for _, tokens in completion_window)
  covered_start_ns = max(window_start_ns, completion_window[0][0])
  elapsed_ns = end_ns - covered_start_ns
  if elapsed_ns <= 0:
    return 0.0
  return total_tokens / (elapsed_ns / 1_000_000_000)

print("")
print(f"{'thread':<8} {'tokens':<8} {'ttft_ms':<10} {'duration_ms':<12} {'req_tok/s':<12} {'total_tok/s':<12}")
sys.stdout.flush()

with open(fifo_path, encoding="utf-8") as handle:
    for raw_line in handle:
        line = raw_line.strip()
        if not line:
            continue
        item = json.loads(line)
        ended_at_ns = int(item.get("ended_at_ns", 0))
        duration_ns = int(item.get("duration_ns", 0))
        completion_tokens = int(item.get("completion_tokens", 0))

        if item.get("ok"):
          completion_window.append((ended_at_ns, completion_tokens))

        req_tps = 0.0
        if duration_ns > 0:
            req_tps = completion_tokens / (duration_ns / 1_000_000_000)

        total_tps = total_tps_last_minute(ended_at_ns)
        ttft_ns = item.get("ttft_ns")
        ttft_display = "n/a" if ttft_ns is None else f"{int(ttft_ns) / 1_000_000:.2f}"
        duration_display = f"{duration_ns / 1_000_000:.2f}"

        print(f"{int(item.get('thread', 0)):<8} {completion_tokens:<8} {ttft_display:<10} {duration_display:<12} {req_tps:<12.2f} {total_tps:<12.2f}")
        sys.stdout.flush()
PY
}

handle_stop() {
  if (( stop_requested == 0 )); then
    stop_requested=1
    if [[ -n "$stop_file" ]]; then
      touch "$stop_file"
    fi
    printf '\nStopping after in-flight requests finish...\n' >&2
  fi
}

wait_for_pid() {
  local pid=$1
  local status

  while kill -0 "$pid" >/dev/null 2>&1; do
    if wait "$pid"; then
      return 0
    fi
    status=$?
    if (( stop_requested == 1 )) && (( status > 128 )); then
      continue
    fi
    return "$status"
  done

  return 0
}

cleanup() {
  trap - EXIT INT TERM
  handle_stop

  if [[ -n "$timer_pid" ]]; then
    kill "$timer_pid" >/dev/null 2>&1 || true
    wait "$timer_pid" >/dev/null 2>&1 || true
  fi

  if [[ -n "$aggregator_pid" ]]; then
    wait "$aggregator_pid" >/dev/null 2>&1 || true
  fi

  if [[ -n "$result_fifo" ]]; then
    rm -f "$result_fifo"
  fi

  if [[ -n "$stop_file" ]]; then
    rm -f "$stop_file"
  fi
}

build_curl_args
warn_if_bypassing_cllm
wait_for_endpoint
detect_model
request_body=$(build_request_body)

echo "benchmark target: $VLLM_BENCH_URL"
echo "model: $VLLM_BENCH_MODEL"
echo "concurrency: $VLLM_BENCH_CONCURRENCY"
if [[ -n "$VLLM_BENCH_DURATION" ]]; then
  echo "duration: $VLLM_BENCH_DURATION"
fi
echo "max tokens per request: $VLLM_BENCH_MAX_TOKENS"
echo "stream mode: $VLLM_BENCH_STREAM"
echo "press Ctrl-C to stop"

if [[ "$VLLM_BENCH_WARMUP" == "1" ]]; then
  warmup_max_tokens=$VLLM_BENCH_MAX_TOKENS
  if (( warmup_max_tokens > 32 )); then
    warmup_max_tokens=32
  fi
  echo "warming up with max tokens: $warmup_max_tokens"
  warmup_request_body=$(build_request_body "$warmup_max_tokens")
  run_request "$warmup_request_body" 0 >/dev/null
fi

stop_file=$(mktemp)
rm -f "$stop_file"
result_fifo=$(mktemp -u)
mkfifo "$result_fifo"

trap handle_stop INT TERM
trap cleanup EXIT

if [[ -n "$duration_seconds" ]]; then
  (
    trap '' INT TERM
    python3 - "$duration_seconds" <<'PY'
import sys
import time

time.sleep(int(sys.argv[1]))
PY
    if [[ ! -f "$stop_file" ]]; then
      touch "$stop_file"
    fi
  ) &
  timer_pid=$!
fi

(trap '' INT TERM; run_aggregator "$result_fifo") &
aggregator_pid=$!

for ((thread_number = 1; thread_number <= VLLM_BENCH_CONCURRENCY; thread_number += 1)); do
  initial_delay_seconds=$(initial_thread_delay_seconds "$thread_number")
  run_worker_loop "$thread_number" "$request_body" "$result_fifo" "$initial_delay_seconds" &
  worker_pids+=("$!")
done

for pid in "${worker_pids[@]}"; do
  wait_for_pid "$pid"
done

wait_for_pid "$aggregator_pid"
