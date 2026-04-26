# cllm

`cllm` is a small Go web server that listens on port `8080` by default.

## Endpoints

- `GET /cache` returns cache status, size, entries, and cache key summaries; it also supports cache actions through query params
- `GET /cache/{key}` returns details for one cache entry, including cached content, extracted tokens, and raw body
- `GET /health` returns `ok`
- `GET /ready` returns `ready`
- `GET /version` returns the current application version as plain text with no surrounding whitespace
- `GET /config` returns the live handler config and applies any supported query string updates before printing it
- `GET /metrics` returns Prometheus metrics for HTTP traffic, queue state, cache activity, downstream latency, TTFTB, and job processing

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
- `CACHE_CACHE_FILE_PATH` or `--cache-file-path`: cache persistence file path, default `/var/lib/cllm/cache.json`
- `CACHE_DOWNSTREAM_URL` or `--downstream-url`: downstream OpenAI-compatible base URL, default `http://localhost:8000`
- `CACHE_DOWNSTREAM_TOKEN` or `--downstream-token`: bearer token sent to the downstream API
- `CACHE_DOWNSTREAM_MODEL` or `--downstream-model`: default downstream model when incoming requests omit `model`
- `CACHE_SYSTEM_PROMPT` or `--system-prompt`: default system prompt for chat completions
- `CACHE_MAX_TOKENS` or `--max-tokens`: default max tokens for chat completions
- `CACHE_MAX_TOKENS_PER_SECOND` or `--max-tokens-per-second`: cached replay token rate per request, `0` to `1000`, default `32`; `0` disables cached replay delay
- `CACHE_MAX_CONCURRENT_REQUESTS` or `--max-concurrent-requests`: max concurrent request slots, `1` to `512`, default `512`
- `CACHE_MAX_WAITING_REQUESTS` or `--max-waiting-requests`: max queued waiting requests, `0` to `1024`, must be less than `2 * max-concurrent-requests`; default `1023`
- `CACHE_MAX_DEGRADATION` or `--max-degradation`: percent reduction applied to cached replay throughput once concurrency rises above `10%`, `0` to `95`, default `10`; `0` disables degradation
- `CACHE_TEMPERATURE` or `--temperature`: default temperature for chat completions
- `-h` or `--help`: show command usage and exit
- `--version`: show the application version and exit

Example:

```bash
CACHE_PORT=8081 CACHE_SHUTDOWN_TIMEOUT=15s CACHE_DOWNSTREAM_URL=https://api.openai.com CACHE_DOWNSTREAM_TOKEN=your-token CACHE_DOWNSTREAM_MODEL=gpt-4.1 CACHE_MAX_TOKENS_PER_SECOND=48 CACHE_MAX_CONCURRENT_REQUESTS=256 CACHE_MAX_WAITING_REQUESTS=512 CACHE_MAX_DEGRADATION=15 go run ./cmd/cllm --cache-size 200
```

For a local vLLM source, omit the downstream token and model settings and keep the default downstream URL of `http://localhost:8000`.

You can inspect or update the live handler config at runtime:

```bash
curl 'http://127.0.0.1:8080/config?cache-size=200&system-prompt=Be%20precise&max-tokens=700&max-tokens-per-second=48&max-concurrent-requests=256&max-waiting-requests=512&max-degradation=15&temperature=0.7'
```

`/config` now returns `concurrent_requests`, `waiting_requests`, and `version` first, then `cache_size` and `cache_entries`, followed by `downstream_url`, `downstream_model`, `max_tokens_per_second`, `effective_tokens_per_second`, `max_concurrent_requests`, `max_waiting_requests`, `max_degradation`, and `computed_degradation_percentage`. You can update the configurable values live with either hyphenated or snake_case query params where supported. Live updates currently support `system-prompt`, `max-tokens`, `max-tokens-per-second`, `max-concurrent-requests`, `max-waiting-requests`, `max-degradation`, `temperature`, `cache-size`, `downstream-url`, `downstream-token`, and `downstream-model`.

The upstream `/v1/models` response is cached for the lifetime of the process. If the downstream server starts serving a different model, restart `cllm` to pick it up.

Request admission is limited by concurrent slots and a waiting queue for `POST /v1/chat/completions`. When both are full, `cllm` returns `429` with `over capacity`.

If you lower `max-concurrent-requests` or `max-waiting-requests` below the current in-flight or queued counts, existing work is preserved. New admissions stay blocked until the live counts fall back within the new limits.

Cached responses are replayed at the configured token rate. Once more than `10%` of concurrent slots are in use, cached replay throughput degrades gradually up to the configured maximum. The live computed degradation percentage and effective token rate are exposed through `/config`, logged whenever they change, and included in the periodic queue-depth logs. Live downstream responses still stream through once admitted.

It also returns `cache_size` and `cache_entries`. You can resize the cache live with `cache-size` or `cache_size`; if the new size is smaller than the current number of entries, the least recently used entries are evicted immediately.

On startup, `cllm` attempts to load the configured cache file if it exists. A missing file is ignored; a malformed cache file fails startup. The cache file stores persisted entries only; runtime `cache_size` still comes from `CACHE_CACHE_SIZE` or `--cache-size`.

You can also inspect and manage the cache through `/cache`:

```bash
curl 'http://127.0.0.1:8080/cache'
curl 'http://127.0.0.1:8080/cache/<cache-key>'
curl 'http://127.0.0.1:8080/cache?action=clear'
curl 'http://127.0.0.1:8080/cache?action=save'
curl 'http://127.0.0.1:8080/cache?action=load'
curl 'http://127.0.0.1:8080/cache?size=200'
```

`/cache` supports these query parameters:

- `action=clear` clears the current cache contents
- `action=save` writes the current cache entries to `cache_file_path`
- `action=load` replaces the current in-memory cache with the contents of `cache_file_path`
- `size=n` resizes the cache to `n`, where `n` must be between `0` and `10000`; `0` disables the cache

`/cache` responses include `cache_file_path`, and save/load actions also report the number of entries written or loaded. `/cache/{key}` returns metadata for the matching cache entry plus the cached content, a whitespace-split `text_tokens` view, and the raw cached body.

Example switching the downstream source to OpenAI-compatible settings at runtime:

```bash
curl 'http://127.0.0.1:8080/config?downstream-url=https%3A%2F%2Fapi.openai.com&downstream-token=your-token&downstream-model=gpt-4.1'
```

Equivalent snake_case form:

```bash
curl 'http://127.0.0.1:8080/config?downstream_url=https%3A%2F%2Fapi.openai.com&downstream_token=your-token&downstream_model=gpt-4.1'
```

Example shrinking the cache to a single entry at runtime:

```bash
curl 'http://127.0.0.1:8080/config?cache-size=1'
```

The downstream token is intentionally not returned by `/config`, but it can be updated through `/config?downstream-token=...` or `/config?downstream_token=...`.

Prometheus scraping is exposed at `/metrics`. In addition to the standard Go and process collectors, `cllm` exports HTTP request metrics and service-specific metrics covering queue wait time, in-flight and waiting request counts, effective token throughput, cache hit and miss counts, downstream request latency, time to first byte, and overall job duration for `/v1/chat/completions`.

## Build

```bash
make build
```

This builds the local container image `cllm:0.5.0`.

To build and import that image into the local k3s container runtime:

```bash
make deploy
```

That runs the equivalent of:

```bash
docker build -t cllm:0.5.0 .
docker save cllm:0.5.0 | sudo k3s ctr images import -
```

## Test

```bash
go test ./...
```

## Docker

```bash
docker build -t cllm:0.5.0 .
docker run --rm -p 8080:8080 cllm:0.5.0
```

The Docker image copies the committed [cache.json](/home/bartr/vllm/cllm/cache.json) artifact into `/var/lib/cllm/cache.json`, which `cllm` then auto-loads on startup if it contains entries.

That image-bundled seed only applies when nothing else is mounted at `/var/lib/cllm`. In the local Kubernetes deployment below, the PVC is mounted at that same path, so the live pod reads and writes the PVC-backed `cache.json` instead of the file baked into the image.

## Kubernetes

The local k3s manifests live under [clusters/z01/cllm](/home/bartr/vllm/clusters/z01/cllm).

They:

- deploy `cllm:0.5.0`
- set `imagePullPolicy: Never` so the local image is never pulled from a registry
- run `cllm` in the `cllm` namespace
- mount a `local-path` PVC at `/var/lib/cllm` so `cache.json` persists across pod replacement and overrides the image-bundled cache seed at that path
- expose it internally on service port `8080`
- expose it through a dedicated Traefik `cllm` entrypoint on external port `8080`

If you want Kubernetes to start with a preloaded cache, seed the PVC-backed file rather than relying on the image copy. The repository helper [scripts/export-cache.sh](/home/bartr/vllm/scripts/export-cache.sh) exports the current PVC-backed cache back into [cllm/cache.json](/home/bartr/vllm/cllm/cache.json); the inverse flow is to write the desired `cache.json` into the mounted volume before or after deployment.

Apply the manifests with:

```bash
kubectl apply -k /home/bartr/vllm/clusters/z01/cllm
kubectl -n kube-system rollout status deployment/traefik
kubectl -n cllm rollout status deployment/cllm
```

Then call it through the Traefik external IP on port `8080`:

```bash
curl -i http://192.168.68.63:8080/health
```
