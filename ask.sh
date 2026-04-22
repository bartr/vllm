#!/usr/bin/env bash

set -euo pipefail

VLLM_URL=${VLLM_URL:-http://localhost:32080}
VLLM_AUTH=${VLLM_AUTH:-}
VLLM_SYSTEM_PROMPT=${VLLM_SYSTEM_PROMPT:-You are a helpful assistant.}
VLLM_MAX_TOKENS=${VLLM_MAX_TOKENS:-4000}
VLLM_TEMPERATURE=${VLLM_TEMPERATURE:-0.2}
ASK_DEBUG=0

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Missing required command: $1" >&2
    exit 1
  fi
}

require_command curl
require_command jq
require_command python3

model_response=''

build_curl_args() {
  curl_args=(-sS -N)
  if [[ -n "$VLLM_AUTH" ]]; then
    curl_args+=(-u "$VLLM_AUTH")
  fi
}

if [[ -z "$VLLM_AUTH" && -t 0 ]]; then
  read -r -p "Ingress username: " vllm_auth_user
  read -r -s -p "Ingress password: " vllm_auth_password
  printf '\n'

  if [[ -n "$vllm_auth_user" && -n "$vllm_auth_password" ]]; then
    export VLLM_AUTH="$vllm_auth_user:$vllm_auth_password"
  fi
fi

build_curl_args

curl_json() {
  local endpoint=$1
  local response_body
  local http_code

  response_body=$(mktemp)
  http_code=$(curl "${curl_args[@]}" -o "$response_body" -w '%{http_code}' "$VLLM_URL$endpoint")

  if [[ "$http_code" == "401" || "$http_code" == "403" ]]; then
    rm -f "$response_body"
    echo "Request was rejected by the ingress. Set VLLM_AUTH to 'user:password' or rerun interactively." >&2
    exit 1
  fi

  if [[ "$http_code" -lt 200 || "$http_code" -ge 300 ]]; then
    cat "$response_body" >&2
    rm -f "$response_body"
    echo "Request to $endpoint failed with HTTP $http_code." >&2
    exit 1
  fi

  cat "$response_body"
  rm -f "$response_body"
}

if [[ -z "${VLLM_MODEL:-}" ]]; then
  model_response=$(curl_json /v1/models)
  VLLM_MODEL=$(printf '%s\n' "$model_response" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"][0]["id"])')
fi

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
  --arg model "$VLLM_MODEL" \
  --arg system_prompt "$VLLM_SYSTEM_PROMPT" \
  --arg user_context "$user_context" \
  --argjson temperature "$VLLM_TEMPERATURE" \
  --argjson max_tokens "$VLLM_MAX_TOKENS" \
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
  "$VLLM_URL/v1/chat/completions" > "$http_code_file"
end_ms=$(date +%s%3N)
elapsed_ms=$((end_ms - start_ms))
http_code=$(cat "$http_code_file")

if [[ "$http_code" == "401" || "$http_code" == "403" ]]; then
  echo "Request was rejected by the ingress. Set VLLM_AUTH to 'user:password' or rerun interactively." >&2
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
