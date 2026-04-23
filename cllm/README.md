# cllm

`cllm` is a small Go web server that listens on port `8080` by default.

## Endpoints

- `GET /healthz` returns `ok`
- `GET /readyz` returns `ready`
- `GET /ask` returns `success`
- `GET /config` returns the live handler config and applies any supported query string updates before printing it

## Run locally

```bash
go run ./cmd/cllm
```

Or with an explicit port:

```bash
CACHE_PORT=8081 go run ./cmd/cllm
```

Show help or version:

```bash
go run ./cmd/cllm --help
go run ./cmd/cllm --version
```

## Runtime Configuration

The server supports these runtime settings:

- `CACHE_CACHE_SIZE` or `--cache-size` / `-c`: maximum number of cached chat responses
- `CACHE_DOWNSTREAM_URL` or `--downstream-url`: downstream OpenAI-compatible base URL, default `http://127.0.0.1:32080`
- `CACHE_DOWNSTREAM_TOKEN` or `--downstream-token`: bearer token sent to the downstream API
- `CACHE_DOWNSTREAM_MODEL` or `--downstream-model`: default downstream model when incoming requests omit `model`
- `CACHE_SYSTEM_PROMPT` or `--system-prompt`: default system prompt for `/ask`
- `CACHE_MAX_TOKENS` or `--max-tokens`: default max tokens for `/ask`
- `CACHE_MAX_TOKENS_PER_SECOND` or `--max-tokens-per-second`: cached replay token rate per request, `0` to `1000`, default `32`; `0` disables cached replay delay
- `CACHE_MAX_CONCURRENT_REQUESTS` or `--max-concurrent-requests`: max concurrent request slots, `1` to `512`, default `512`
- `CACHE_MAX_WAITING_REQUESTS` or `--max-waiting-requests`: max queued waiting requests, `0` to `1024`, must be less than `2 * max-concurrent-requests`; default `1023`
- `CACHE_MAX_DEGRADATION` or `--max-degradation`: percent reduction applied to cached replay throughput once concurrency rises above `10%`, `0` to `95`, default `10`; `0` disables degradation
- `CACHE_TEMPERATURE` or `--temperature`: default temperature for `/ask`
- `-h` or `--help`: show command usage and exit
- `--version`: show the application version and exit

Example:

```bash
CACHE_PORT=8081 CACHE_SHUTDOWN_TIMEOUT=15s CACHE_DOWNSTREAM_URL=https://api.openai.com CACHE_DOWNSTREAM_TOKEN=your-token CACHE_DOWNSTREAM_MODEL=gpt-4.1 CACHE_MAX_TOKENS_PER_SECOND=48 CACHE_MAX_CONCURRENT_REQUESTS=256 CACHE_MAX_WAITING_REQUESTS=512 CACHE_MAX_DEGRADATION=15 go run ./cmd/cllm --cache-size 200
```

For a local vLLM source, omit the downstream token and model settings and keep the default downstream URL of `http://127.0.0.1:32080`.

You can inspect or update the live handler config at runtime:

```bash
curl 'http://127.0.0.1:8080/config?cache-size=200&system-prompt=Be%20precise&max-tokens=700&max-tokens-per-second=48&max-concurrent-requests=256&max-waiting-requests=512&max-degradation=15&temperature=0.7&stream=true'
```

`/config` now also returns `downstream_url`, `downstream_model`, `max_tokens_per_second`, `max_concurrent_requests`, `concurrent_requests`, `max_waiting_requests`, `waiting_requests`, and `max_degradation`, and you can update the configurable values live with either hyphenated or snake_case query params.

The upstream `/v1/models` response is cached for the lifetime of the process. If the downstream server starts serving a different model, restart `cllm` to pick it up.

Request admission is limited by concurrent slots and a waiting queue for `GET /ask` and `POST /v1/chat/completions`. When both are full, `cllm` returns `429` with `over capacity`.

If you lower `max-concurrent-requests` or `max-waiting-requests` below the current in-flight or queued counts, existing work is preserved. New admissions stay blocked until the live counts fall back within the new limits.

Cached responses are replayed at the configured token rate. Once more than `10%` of concurrent slots are in use, cached replay throughput steps down by the configured degradation percentage. Live downstream responses still stream through once admitted.

It also returns `cache_size` and `cache_entries`. You can resize the cache live with `cache-size` or `cache_size`; if the new size is smaller than the current number of entries, the least recently used entries are evicted immediately.

Example switching the downstream source to OpenAI-compatible settings at runtime:

```bash
curl 'http://127.0.0.1:8080/config?downstream-url=https%3A%2F%2Fapi.openai.com&downstream-model=gpt-4.1'
```

Equivalent snake_case form:

```bash
curl 'http://127.0.0.1:8080/config?downstream_url=https%3A%2F%2Fapi.openai.com&downstream_model=gpt-4.1'
```

Example shrinking the cache to a single entry at runtime:

```bash
curl 'http://127.0.0.1:8080/config?cache-size=1'
```

The downstream token is intentionally not returned by `/config`.

## Build

```bash
go build -o bin/cllm ./cmd/cllm
```

## Test

```bash
go test ./...
```

## Docker

```bash
docker build -t cllm .
docker run --rm -p 8080:8080 cllm
```
