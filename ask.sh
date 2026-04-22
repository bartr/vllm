#!/usr/bin/env bash

set -euo pipefail

CACHE_URL=${CACHE_URL:-${VLLM_URL:-http://localhost:8080}}
CACHE_AUTH=${CACHE_AUTH:-${VLLM_AUTH:-}}
CACHE_SYSTEM_PROMPT=${CACHE_SYSTEM_PROMPT:-${VLLM_SYSTEM_PROMPT:-You are a detailed assistant.}}
CACHE_MAX_TOKENS=${CACHE_MAX_TOKENS:-${VLLM_MAX_TOKENS:-4000}}
CACHE_TEMPERATURE=${CACHE_TEMPERATURE:-${VLLM_TEMPERATURE:-0.2}}
ASK_DEBUG=0

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
  if [[ -n "$CACHE_AUTH" ]]; then
    curl_args+=(-u "$CACHE_AUTH")
  fi
}

if [[ -z "$CACHE_AUTH" && -t 0 ]]; then
  read -r -p "Cache username: " cache_auth_user
  read -r -s -p "Cache password: " cache_auth_password
  printf '\n'

  if [[ -n "$cache_auth_user" && -n "$cache_auth_password" ]]; then
    export CACHE_AUTH="$cache_auth_user:$cache_auth_password"
  fi
fi

build_curl_args

user_args=()
for arg in "$@"; do
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

export ASK_DEBUG

if [[ -z "$user_context" ]]; then
  echo "User context cannot be empty." >&2
  exit 1
fi

start_ms=$(date +%s%3N)
request_body=$(jq -n \
  --arg system_prompt "$CACHE_SYSTEM_PROMPT" \
  --arg user_context "$user_context" \
  --argjson temperature "$CACHE_TEMPERATURE" \
  --argjson max_tokens "$CACHE_MAX_TOKENS" \
  '{
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
cleanup() {
  rm -f "$response_body" "$http_code_file"
}
trap cleanup EXIT

curl "${curl_args[@]}" \
  -o >(tee "$response_body" | python3 -c '
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
' ) \
  -w '%{http_code}' \
  -H 'Content-Type: application/json' \
  -d "$request_body" \
  "$CACHE_URL/v1/chat/completions" > "$http_code_file"
end_ms=$(date +%s%3N)
elapsed_ms=$((end_ms - start_ms))
http_code=$(cat "$http_code_file")

if [[ "$http_code" == "401" || "$http_code" == "403" ]]; then
  echo "Request was rejected by the cache service. Set CACHE_AUTH to 'user:password' or rerun interactively." >&2
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
        if obj.get("usage"):
            usage = obj["usage"]

print(f"elapsed_ms: {elapsed_ms}")
if usage is None:
    print("prompt_tokens: unknown")
    print("completion_tokens: unknown")
    print("total_tokens: unknown")
else:
    print(f"prompt_tokens: {usage.get('prompt_tokens', 'unknown')}")
    print(f"completion_tokens: {usage.get('completion_tokens', 'unknown')}")
    print(f"total_tokens: {usage.get('total_tokens', 'unknown')}")
PY
