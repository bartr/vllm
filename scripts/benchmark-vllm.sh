#!/usr/bin/env bash

set -euo pipefail

VLLM_BENCH_URL=${VLLM_BENCH_URL:-http://localhost:8080}
VLLM_BENCH_TOKEN=${VLLM_BENCH_TOKEN:-}
VLLM_BENCH_MODEL=${VLLM_BENCH_MODEL:-}
VLLM_BENCH_START=${VLLM_BENCH_START:-1}
VLLM_BENCH_MAX_CONCURRENCY=${VLLM_BENCH_MAX_CONCURRENCY:-5}
VLLM_BENCH_REQUESTS_PER_WORKER=${VLLM_BENCH_REQUESTS_PER_WORKER:-1}
VLLM_BENCH_MAX_TOKENS=${VLLM_BENCH_MAX_TOKENS:-64}
VLLM_BENCH_TEMPERATURE=${VLLM_BENCH_TEMPERATURE:-0}
VLLM_BENCH_STREAM=${VLLM_BENCH_STREAM:-1}
VLLM_BENCH_WARMUP=${VLLM_BENCH_WARMUP:-0}
VLLM_BENCH_SYSTEM_PROMPT=${VLLM_BENCH_SYSTEM_PROMPT:-You are a helpful assistant.}
VLLM_BENCH_PROMPT=${VLLM_BENCH_PROMPT:-Explain Azure}

usage() {
  cat <<'EOF'
Usage: ./scripts/benchmark-vllm.sh [options]

Benchmark the local vLLM OpenAI-compatible endpoint and report the best aggregate
completion tokens/sec found across a concurrency sweep.

Options:
  -h, --help                 Show this help message and exit
      --url URL              Base URL for vLLM (default: http://localhost:8000)
      --token TOKEN          Bearer token for authenticated endpoints
      --model MODEL          Model ID to benchmark; auto-detected from /v1/models if omitted
      --start N              Lowest concurrency level to test (default: 20)
      --max-concurrency N    Highest concurrency level to test (default: 50)
      --requests-per-worker N
                             Number of sequential requests per worker (default: 2)
      --max-tokens N         Max completion tokens per request (default: 1024)
      --temperature VALUE    Sampling temperature (default: 0)
      --stream               Use streaming chat completions and measure TTFT
      --warmup               Run one untimed warmup request
      --system-prompt TEXT   System prompt sent with each request
      --prompt TEXT          User prompt sent with each request

Environment:
  VLLM_BENCH_URL
  VLLM_BENCH_TOKEN
  VLLM_BENCH_MODEL
  VLLM_BENCH_START
  VLLM_BENCH_MAX_CONCURRENCY
  VLLM_BENCH_REQUESTS_PER_WORKER
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
    --start)
      VLLM_BENCH_START=$2
      shift 2
      ;;
    --max-concurrency)
      VLLM_BENCH_MAX_CONCURRENCY=$2
      shift 2
      ;;
    --requests-per-worker)
      VLLM_BENCH_REQUESTS_PER_WORKER=$2
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

if ! [[ "$VLLM_BENCH_START" =~ ^[0-9]+$ ]] || (( VLLM_BENCH_START < 1 )); then
  echo "VLLM_BENCH_START must be an integer >= 1." >&2
  exit 1
fi

if ! [[ "$VLLM_BENCH_MAX_CONCURRENCY" =~ ^[0-9]+$ ]] || (( VLLM_BENCH_MAX_CONCURRENCY < 1 )); then
  echo "VLLM_BENCH_MAX_CONCURRENCY must be an integer >= 1." >&2
  exit 1
fi

if (( VLLM_BENCH_START > VLLM_BENCH_MAX_CONCURRENCY )); then
  echo "VLLM_BENCH_START must be less than or equal to VLLM_BENCH_MAX_CONCURRENCY." >&2
  exit 1
fi

if ! [[ "$VLLM_BENCH_REQUESTS_PER_WORKER" =~ ^[0-9]+$ ]] || (( VLLM_BENCH_REQUESTS_PER_WORKER < 1 )); then
  echo "VLLM_BENCH_REQUESTS_PER_WORKER must be an integer >= 1." >&2
  exit 1
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

    if output=$(curl "${curl_args[@]}" -o /dev/null -w '%{http_code}' "$VLLM_BENCH_URL/healthz" 2>&1); then
      if [[ "$output" == "200" ]]; then
        return 0
      fi
    fi

    if (( attempt == attempts )); then
      echo "vLLM endpoint did not become ready at $VLLM_BENCH_URL/health after $attempts attempts." >&2
      echo "$output" >&2
      return 1
    fi
    echo "waiting for vLLM endpoint at $VLLM_BENCH_URL/health (attempt $attempt/$attempts)..." >&2
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
  local result_file=$2
  local response_file error_file curl_metrics http_code latency_s ttft_s latency_ns ttft_ns curl_exit

  response_file=$(mktemp)
  error_file=$(mktemp)
  if ! curl_metrics=$(curl "${curl_args[@]}" \
    $( [[ "$VLLM_BENCH_STREAM" == "1" ]] && printf '%s' '-N' ) \
    -o "$response_file" \
    -w '%{http_code}\t%{time_total}\t%{time_starttransfer}' \
    -H 'Content-Type: application/json' \
    -d "$request_body" \
    "$VLLM_BENCH_URL/v1/chat/completions" 2>"$error_file"); then
    curl_exit=$?
    jq -nc \
      --arg error "$(tr '\n' ' ' < "$error_file")" \
      --argjson curl_exit "$curl_exit" \
      '{ok:false,error:(if $error == "" then "curl failed" else $error end),curl_exit:$curl_exit}' >> "$result_file"
    rm -f "$response_file"
    rm -f "$error_file"
    return 0
  fi
  IFS=$'\t' read -r http_code latency_s ttft_s <<< "$curl_metrics"
  latency_ns=$(python3 - "$latency_s" <<'PY'
import sys
print(int(float(sys.argv[1]) * 1_000_000_000))
PY
)
  ttft_ns=$(python3 - "$ttft_s" <<'PY'
import sys
print(int(float(sys.argv[1]) * 1_000_000_000))
PY
)

  if [[ "$http_code" -lt 200 || "$http_code" -ge 300 ]]; then
    if [[ "$http_code" -gt 399 ]]; then
      echo "request failed: status=$http_code endpoint=$VLLM_BENCH_URL/v1/chat/completions" >&2
    fi
    jq -nc \
      --arg status "$http_code" \
      --arg body "$(tr '\n' ' ' < "$response_file")" \
      '{ok:false,status:($status|tonumber),error:$body}' >> "$result_file"
    rm -f "$response_file"
    return 0
  fi

  if [[ "$VLLM_BENCH_STREAM" == "1" ]]; then
    if ! python3 - "$response_file" "$latency_ns" "$ttft_ns" >> "$result_file" <<'PY'
import json
import sys

response_path = sys.argv[1]
latency_ns = int(sys.argv[2])
ttft_ns = int(sys.argv[3])
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
    "completion_tokens": completion_tokens,
    "prompt_tokens": prompt_tokens,
    "total_tokens": total_tokens,
    "latency_ns": latency_ns,
    "ttft_ns": ttft_ns,
}))
PY
    then
      jq -nc --arg error "invalid streaming response" '{ok:false,error:$error}' >> "$result_file"
    fi
  else
    if ! jq -c --argjson latency_ns "$latency_ns" '{ok:true,completion_tokens:(.usage.completion_tokens // 0),prompt_tokens:(.usage.prompt_tokens // 0),total_tokens:(.usage.total_tokens // 0),latency_ns:$latency_ns,ttft_ns:null}' "$response_file" >> "$result_file"; then
      jq -nc --arg error "invalid JSON response" '{ok:false,error:$error}' >> "$result_file"
    fi
  fi
  rm -f "$response_file"
  rm -f "$error_file"
}

run_worker() {
  local request_body=$1
  local result_file=$2
  local count=$3
  : > "$result_file"
  for ((i = 0; i < count; i += 1)); do
    run_request "$request_body" "$result_file"
  done
}

summarize_results() {
  local result_dir=$1
  local elapsed_ns=$2
  python3 - "$result_dir" "$elapsed_ns" <<'PY'
import json
import pathlib
import sys

result_dir = pathlib.Path(sys.argv[1])
elapsed_ns = int(sys.argv[2])
completion_tokens = 0
prompt_tokens = 0
total_tokens = 0
latency_ns_total = 0
request_tps_total = 0.0
ttft_ns_total = 0
ttft_count = 0
ok = 0
failed = 0

for path in sorted(result_dir.glob("*.jsonl")):
    for line in path.read_text(encoding="utf-8").splitlines():
        if not line:
            continue
        item = json.loads(line)
        if item.get("ok"):
            ok += 1
            completion = int(item.get("completion_tokens", 0))
            prompt = int(item.get("prompt_tokens", 0))
            total = int(item.get("total_tokens", 0))
            latency_ns = int(item.get("latency_ns", 0))
            completion_tokens += completion
            prompt_tokens += prompt
            total_tokens += total
            latency_ns_total += latency_ns
            if latency_ns > 0:
                request_tps_total += completion / (latency_ns / 1_000_000_000)
            if item.get("ttft_ns") is not None:
                ttft_ns_total += int(item.get("ttft_ns", 0))
                ttft_count += 1
        else:
            failed += 1

elapsed_s = elapsed_ns / 1_000_000_000
completion_tps = completion_tokens / elapsed_s if elapsed_s else 0.0
avg_latency_ms = (latency_ns_total / ok) / 1_000_000 if ok else 0.0
avg_request_tps = request_tps_total / ok if ok else 0.0
avg_ttft_ms = (ttft_ns_total / ttft_count) / 1_000_000 if ttft_count else -1.0
print("\t".join([
    str(ok),
    str(failed),
    str(prompt_tokens),
    str(completion_tokens),
    str(total_tokens),
    f"{elapsed_s:.3f}",
    f"{completion_tps:.2f}",
    f"{avg_latency_ms:.2f}",
    f"{avg_request_tps:.2f}",
    (f"{avg_ttft_ms:.2f}" if avg_ttft_ms >= 0 else "n/a"),
]))
PY
}

build_curl_args
wait_for_endpoint
detect_model
request_body=$(build_request_body)

echo "vLLM benchmark target: $VLLM_BENCH_URL"
echo "model: $VLLM_BENCH_MODEL"
echo "start concurrency: $VLLM_BENCH_START"
echo "max concurrency: $VLLM_BENCH_MAX_CONCURRENCY"
echo "requests per worker: $VLLM_BENCH_REQUESTS_PER_WORKER"
echo "max tokens per request: $VLLM_BENCH_MAX_TOKENS"
echo "stream mode: $VLLM_BENCH_STREAM"

if [[ "$VLLM_BENCH_WARMUP" == "1" ]]; then
  warmup_max_tokens=$VLLM_BENCH_MAX_TOKENS
  if (( warmup_max_tokens > 32 )); then
    warmup_max_tokens=32
  fi
  echo "warming up with max tokens: $warmup_max_tokens"
  warmup_result=$(mktemp)
  warmup_request_body=$(build_request_body "$warmup_max_tokens")
  run_request "$warmup_request_body" "$warmup_result"
  rm -f "$warmup_result"
fi

printf '\n'
printf '%-12s %-10s %-10s %-14s %-10s %-10s %-12s %-12s %-10s\n' 'concurrency' 'requests' 'failures' 'completion_tok' 'elapsed_s' 'tok/s' 'avg_lat_ms' 'req_tok/s' 'ttft_ms'

best_concurrency=0
best_tps=0
best_balanced_concurrency=0
best_balanced_tps=0
best_balanced_latency=0
min_latency_ms=0

for ((concurrency = VLLM_BENCH_START; concurrency <= VLLM_BENCH_MAX_CONCURRENCY; concurrency += 1)); do
  result_dir=$(mktemp -d)
  start_ns=$(date +%s%N)
  pids=()
  for ((worker = 1; worker <= concurrency; worker += 1)); do
    run_worker "$request_body" "$result_dir/$worker.jsonl" "$VLLM_BENCH_REQUESTS_PER_WORKER" &
    pids+=($!)
  done
  for pid in "${pids[@]}"; do
    wait "$pid"
  done
  end_ns=$(date +%s%N)
  elapsed_ns=$((end_ns - start_ns))

  IFS=$'\t' read -r ok failed prompt_tokens completion_tokens total_tokens elapsed_s completion_tps avg_latency_ms avg_request_tps avg_ttft_ms < <(summarize_results "$result_dir" "$elapsed_ns")
  rm -rf "$result_dir"

  printf '%-12s %-10s %-10s %-14s %-10s %-10s %-12s %-12s %-10s\n' "$concurrency" "$ok" "$failed" "$completion_tokens" "$elapsed_s" "$completion_tps" "$avg_latency_ms" "$avg_request_tps" "$avg_ttft_ms"

  if python3 - "$completion_tps" "$best_tps" <<'PY'
import sys
current = float(sys.argv[1])
best = float(sys.argv[2])
raise SystemExit(0 if current > best else 1)
PY
  then
    best_tps=$completion_tps
    best_concurrency=$concurrency
  fi

  if python3 - "$avg_latency_ms" "$min_latency_ms" <<'PY'
import sys
current = float(sys.argv[1])
best = float(sys.argv[2])
raise SystemExit(0 if best == 0 or current < best else 1)
PY
  then
    min_latency_ms=$avg_latency_ms
  fi

  if python3 - "$failed" "$avg_latency_ms" "$min_latency_ms" "$completion_tps" "$best_balanced_tps" <<'PY'
import sys
failed = int(sys.argv[1])
latency = float(sys.argv[2])
min_latency = float(sys.argv[3])
current_tps = float(sys.argv[4])
best_tps = float(sys.argv[5])
if failed != 0:
    raise SystemExit(1)
if min_latency == 0:
    raise SystemExit(0)
threshold = min_latency * 2.0
raise SystemExit(0 if latency <= threshold and current_tps > best_tps else 1)
PY
  then
    best_balanced_tps=$completion_tps
    best_balanced_concurrency=$concurrency
    best_balanced_latency=$avg_latency_ms
  fi
done

printf '\n'
echo "best_concurrency: $best_concurrency"
echo "best_completion_tokens_per_second: $best_tps"
if [[ "$best_balanced_concurrency" != "0" ]]; then
  echo "best_latency_balanced_concurrency: $best_balanced_concurrency"
  echo "best_latency_balanced_tokens_per_second: $best_balanced_tps"
  echo "best_latency_balanced_avg_latency_ms: $best_balanced_latency"
fi
