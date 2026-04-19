#!/usr/bin/env bash

set -euo pipefail

VLLM_URL=${VLLM_URL:-http://vllm.local}
VLLM_AUTH=${VLLM_AUTH:-}
VLLM_SYSTEM_PROMPT=${VLLM_SYSTEM_PROMPT:-You are a concise assistant.}
VLLM_MAX_TOKENS=${VLLM_MAX_TOKENS:-500}
VLLM_TEMPERATURE=${VLLM_TEMPERATURE:-0.2}

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Missing required command: $1" >&2
    exit 1
  fi
}

require_command curl
require_command jq
require_command python3
require_command glow

model_response=''

build_curl_args() {
  curl_args=(-sS)
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

if [[ $# -gt 0 ]]; then
  user_context="$*"
elif [[ ! -t 0 ]]; then
  user_context=$(cat)
else
  read -r -p "User context: " user_context
fi

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
    max_tokens: $max_tokens
  }')
response_body=$(mktemp)
http_code=$(curl "${curl_args[@]}" \
  -o "$response_body" \
  -w '%{http_code}' \
  -H 'Content-Type: application/json' \
  -d "$request_body" \
  "$VLLM_URL/v1/chat/completions")
end_ms=$(date +%s%3N)
elapsed_ms=$((end_ms - start_ms))

if [[ "$http_code" == "401" || "$http_code" == "403" ]]; then
  rm -f "$response_body"
  echo "Request was rejected by the ingress. Set VLLM_AUTH to 'user:password' or rerun interactively." >&2
  exit 1
fi

if [[ "$http_code" -lt 200 || "$http_code" -ge 300 ]]; then
  cat "$response_body" >&2
  rm -f "$response_body"
  echo "Chat completion request failed with HTTP $http_code." >&2
  exit 1
fi

response=$(cat "$response_body")
rm -f "$response_body"

assistant_content=$(printf '%s\n' "$response" | jq -r '.choices[0].message.content')

printf '%s\n' "$assistant_content" | glow -
printf '\n'
printf '%s\n' "$response" \
  | jq -r --arg elapsed_ms "$elapsed_ms" '"elapsed_ms: \($elapsed_ms)", "prompt_tokens: \(.usage.prompt_tokens)", "completion_tokens: \(.usage.completion_tokens)", "total_tokens: \(.usage.total_tokens)"'
