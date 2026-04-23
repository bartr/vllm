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
- `CACHE_TEMPERATURE` or `--temperature`: default temperature for `/ask`
- `-h` or `--help`: show command usage and exit
- `--version`: show the application version and exit

Example:

```bash
CACHE_PORT=8081 CACHE_SHUTDOWN_TIMEOUT=15s CACHE_DOWNSTREAM_URL=https://api.openai.com CACHE_DOWNSTREAM_TOKEN=your-token CACHE_DOWNSTREAM_MODEL=gpt-4.1 go run ./cmd/cllm --cache-size 200
```

For a local vLLM source, omit the downstream token and model settings and keep the default downstream URL of `http://127.0.0.1:32080`.

You can inspect or update the live handler config at runtime:

```bash
curl 'http://127.0.0.1:8080/config?cache-size=200&system-prompt=Be%20precise&max-tokens=700&temperature=0.7&stream=true'
```

`/config` now also returns `downstream_url` and `downstream_model`, and you can update them live with either hyphenated or snake_case query params.

The upstream `/v1/models` response is cached for the lifetime of the process. If the downstream server starts serving a different model, restart `cllm` to pick it up.

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
