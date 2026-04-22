GO ?= go
BINARY ?= cache

.PHONY: build run test fmt clean

build:
	$(GO) build -o bin/$(BINARY) ./cmd/cache

run:
	$(GO) run ./cmd/cache

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

clean:
	rm -rf bin
