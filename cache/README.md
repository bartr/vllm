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
PORT=8081 go run ./cmd/cache
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
