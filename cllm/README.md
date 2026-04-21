# cllm

`cllm` is a small Go web server that listens on port `8080` by default.

## Endpoints

- `GET /healthz` returns `ok`
- `GET /readyz` returns `ready`
- `GET /ask` returns `success`

## Run locally

```bash
go run ./cmd/cllm
```

Or with an explicit port:

```bash
PORT=8081 go run ./cmd/cllm
```

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
