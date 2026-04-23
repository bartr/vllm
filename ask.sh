#!/usr/bin/env bash

set -euo pipefail

ASK_URL=${ASK_URL:-http://localhost:8000}
ASK_TOKEN=${ASK_TOKEN:-}
ASK_MODEL=${ASK_MODEL:-}
ASK_SYSTEM_PROMPT=${ASK_SYSTEM_PROMPT:-You are a helpful assistant.}
ASK_MAX_TOKENS=${ASK_MAX_TOKENS:-4000}
ASK_TEMPERATURE=${ASK_TEMPERATURE:-0.2}
ASK_DEBUG=0

usage() {
  cat <<'EOF'
Usage: ./ask.sh [--debug] [prompt...]

Send a streaming chat request to the configured endpoint.

Options:
  -h, --help   Show this help message and exit
  --debug      Print raw SSE lines to stderr while streaming

Environment:
  ASK_URL            Base URL for the chat service (default: http://localhost:8080)
  ASK_TOKEN          Bearer token for OpenAI-compatible endpoints
  ASK_MODEL          Model name for OpenAI-compatible chat completions
  ASK_SYSTEM_PROMPT  System prompt sent with the request
  ASK_MAX_TOKENS     Max tokens for the request
  ASK_TEMPERATURE    Sampling temperature for the request
  ASK_DEBUG          Set to 1 to enable raw SSE debug output

Input:
  Pass the user prompt as arguments or pipe it on stdin.
EOF
}

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Missing required command: $1" >&2
    exit 1
  fi
}

require_command curl
require_command python3

build_curl_args() {
  curl_args=(-sS -N)
  if [[ -n "$ASK_TOKEN" ]]; then
    curl_args+=(-H "Authorization: Bearer $ASK_TOKEN")
  fi
}

build_curl_args

user_args=()
for arg in "$@"; do
  if [[ "$arg" == "-h" || "$arg" == "--help" ]]; then
    usage
    exit 0
  fi
  if [[ "$arg" == "--debug" ]]; then
    ASK_DEBUG=1
    continue
  fi
  user_args+=("$arg")
done

if [[ ${#user_args[@]} -gt 0 ]]; then
  user_context="${user_args[*]}"
elif [[ ! -t 0 ]]; then
  user_context=$(cat)
else
  read -r -p "User context: " user_context
fi

if [[ "$ASK_URL" == "https://api.openai.com" || "$ASK_URL" == "https://api.openai.com/" ]]; then
  ASK_MODEL=gpt-4.1
fi

export ASK_DEBUG

if [[ -z "$user_context" ]]; then
  echo "User context cannot be empty." >&2
  exit 1
fi

start_ms=$(date +%s%3N)
request_body=$(jq -n \
  --arg model "$ASK_MODEL" \
  --arg system_prompt "$ASK_SYSTEM_PROMPT" \
  --arg user_context "$user_context" \
  --argjson temperature "$ASK_TEMPERATURE" \
  --argjson max_tokens "$ASK_MAX_TOKENS" \
  '{
    model: $model,
    messages: [
      {role: "system", content: $system_prompt},
      {role: "user", content: $user_context}
    ],
    temperature: $temperature,
    max_tokens: $max_tokens,
    stream: true,
    stream_options: {
      include_usage: true
    }
  }')
response_body=$(mktemp)
http_code_file=$(mktemp)
stream_fifo=$(mktemp -u)
mkfifo "$stream_fifo"

tee "$response_body" < "$stream_fifo" | python3 -c '
import json
import os
import sys

debug = os.environ.get("ASK_DEBUG") == "1"

for raw_line in sys.stdin:
    if debug:
        sys.stderr.write(raw_line)
        sys.stderr.flush()
    line = raw_line.strip()
    if not line or not line.startswith("data: "):
        continue
    payload = line[6:]
    if payload == "[DONE]":
        break
    try:
        obj = json.loads(payload)
    except json.JSONDecodeError:
        continue
    for choice in obj.get("choices", []):
        delta = choice.get("delta", {})
        content = delta.get("content")
        if content:
            sys.stdout.write(content)
            sys.stdout.flush()
' &
consumer_pid=$!

cleanup() {
  rm -f "$response_body" "$http_code_file"
  if [[ -p "$stream_fifo" ]]; then
    rm -f "$stream_fifo"
  fi
}
trap cleanup EXIT

curl "${curl_args[@]}" \
  -o "$stream_fifo" \
  -w '%{http_code}' \
  -H 'Content-Type: application/json' \
  -d "$request_body" \
  "$ASK_URL/v1/chat/completions" > "$http_code_file"

if ! wait "$consumer_pid"; then
  echo "Failed to process streamed response." >&2
  exit 1
fi

end_ms=$(date +%s%3N)
elapsed_ms=$((end_ms - start_ms))
http_code=$(cat "$http_code_file")

if [[ "$http_code" == "401" || "$http_code" == "403" ]]; then
  echo "Request was rejected by the chat service. Set ASK_TOKEN to a valid bearer token and retry." >&2
  exit 1
fi

if [[ "$http_code" -lt 200 || "$http_code" -ge 300 ]]; then
  cat "$response_body" >&2
  echo "Chat completion request failed with HTTP $http_code." >&2
  exit 1
fi

printf '\n'
printf '%s\n' '------------------'
python3 - "$response_body" "$elapsed_ms" <<'PY'
import json
import sys

response_path = sys.argv[1]
elapsed_ms = sys.argv[2]
usage = None
cache = "unknown"

with open(response_path, encoding="utf-8") as handle:
    for raw_line in handle:
        line = raw_line.strip()
        if not line or not line.startswith("data: "):
            continue
        payload = line[6:]
        if payload == "[DONE]":
            continue
        try:
            obj = json.loads(payload)
        except json.JSONDecodeError:
            continue
        if "cache" in obj:
            cache = str(obj["cache"]).lower()
        if obj.get("usage"):
            usage = obj["usage"]

print(f"elapsed_ms: {elapsed_ms}")
print(f"cache: {cache}")
if usage is None:
    print("prompt_tokens: unknown")
    print("completion_tokens: unknown")
    print("total_tokens: unknown")
else:
    print(f"prompt_tokens: {usage.get('prompt_tokens', 'unknown')}")
    print(f"completion_tokens: {usage.get('completion_tokens', 'unknown')}")
    print(f"total_tokens: {usage.get('total_tokens', 'unknown')}")
PY
