GO ?= go
BINARY ?= cllm

.PHONY: build run test fmt clean

build:
	$(GO) build -o bin/$(BINARY) ./cmd/cllm

run:
	$(GO) run ./cmd/cllm

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

clean:
	rm -rf bin
