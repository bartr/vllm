GO ?= go
BINARY ?= cllm
DOCKER ?= docker
IMAGE ?= cllm:0.1.0
K3S ?= sudo k3s

.PHONY: build run test fmt clean import

build:
	$(DOCKER) build -t $(IMAGE) .

import: build
	$(DOCKER) save $(IMAGE) | $(K3S) ctr images import -

run:
	$(GO) run ./cmd/cllm

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

clean:
	rm -rf bin
