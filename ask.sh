#!/usr/bin/env bash

set -euo pipefail

VLLM_URL=${VLLM_URL:-http://localhost:30080}
VLLM_SYSTEM_PROMPT=${VLLM_SYSTEM_PROMPT:-You are a concise assistant.}
VLLM_MAX_TOKENS=${VLLM_MAX_TOKENS:-200}
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

if [[ -z "${VLLM_MODEL:-}" ]]; then
  VLLM_MODEL=$(curl -s "$VLLM_URL/v1/models" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"][0]["id"])')
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
response=$(curl -sS \
  -H 'Content-Type: application/json' \
  -d "$request_body" \
  "$VLLM_URL/v1/chat/completions")
end_ms=$(date +%s%3N)
elapsed_ms=$((end_ms - start_ms))

assistant_content=$(printf '%s\n' "$response" | jq -r '.choices[0].message.content')

printf '%s\n' "$assistant_content" | glow -
printf '\n'
printf '%s\n' "$response" \
  | jq -r --arg elapsed_ms "$elapsed_ms" '"elapsed_ms: \($elapsed_ms)", "prompt_tokens: \(.usage.prompt_tokens)", "completion_tokens: \(.usage.completion_tokens)", "total_tokens: \(.usage.total_tokens)"'
