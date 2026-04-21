GO ?= go
BINARY ?= cplane

.PHONY: build run test fmt clean

build:
	$(GO) build -o bin/$(BINARY) ./cmd/cplane

run:
	$(GO) run ./cmd/cplane

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

clean:
	rm -rf bin
