# cache

`cache` is a small Go web server that listens on port `8080` by default.

## Endpoints

- `GET /healthz` returns `ok`
- `GET /readyz` returns `ready`
- `GET /ask` returns `success`

## Run locally

```bash
go run ./cmd/cache
```

Or with an explicit port:

```bash
CACHE_PORT=8081 go run ./cmd/cache
```

Show help or version:

```bash
go run ./cmd/cache --help
go run ./cmd/cache --version
```

## Runtime Configuration

The server supports these runtime settings:

- `CACHE_CACHE_SIZE` or `--cache-size` / `-c`: maximum number of cached chat responses
- `CACHE_MODELS_CACHE_TTL` or `--models-cache-ttl`: how long to keep the upstream `/v1/models` response cached, default `1h`
- `CACHE_REPLAY_DELAY` or `--replay-delay`: delay cached replay output to simulate load, default `0s`
- `CACHE_SYSTEM_PROMPT` or `--system-prompt`: default system prompt for `/ask`
- `CACHE_MAX_TOKENS` or `--max-tokens`: default max tokens for `/ask`
- `CACHE_TEMPERATURE` or `--temperature`: default temperature for `/ask`
- `-h` or `--help`: show command usage and exit
- `--version`: show the application version and exit

Example:

```bash
CACHE_PORT=8081 CACHE_SHUTDOWN_TIMEOUT=15s CACHE_MODELS_CACHE_TTL=30m CACHE_REPLAY_DELAY=20ms go run ./cmd/cache --cache-size 200 --models-cache-ttl 15m --replay-delay 10ms
```

## Build

```bash
go build -o bin/cache ./cmd/cache
```

## Test

```bash
go test ./...
```

## Docker

```bash
docker build -t cache .
docker run --rm -p 8080:8080 cache
```
