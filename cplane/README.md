# cplane

`cplane` is a small Go web server that listens on port `8080` by default.

## Endpoints

- `GET /healthz` returns `ok`
- `GET /readyz` returns `ready`
- `GET /ask` returns `success`

## Run locally

```bash
go run ./cmd/cplane
```

Or with an explicit port:

```bash
PORT=8081 go run ./cmd/cplane
```

## Build

```bash
go build -o bin/cplane ./cmd/cplane
```

## Test

```bash
go test ./...
```

## Docker

```bash
docker build -t cplane .
docker run --rm -p 8080:8080 cplane
```
