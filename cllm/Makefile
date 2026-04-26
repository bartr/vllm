GO ?= go
BINARY ?= cllm
DOCKER ?= docker
IMAGE ?= cllm:0.6.0
K3S ?= sudo k3s
GOBIN ?= $(shell $(GO) env GOBIN)
ifeq ($(GOBIN),)
GOBIN := $(shell $(GO) env GOPATH)/bin
endif

.PHONY: build run test fmt clean deploy install

build:
	$(DOCKER) build -t $(IMAGE) .

deploy: build
	$(DOCKER) save $(IMAGE) | $(K3S) ctr images import -
	$(K3S) ctr images ls | grep $(BINARY)
	kubectl -n cllm rollout restart deployment/cllm
	kubectl -n cllm rollout status deployment/cllm
run:
	$(GO) run ./cmd/cllm

test:
	$(GO) test -race ./...

fmt:
	$(GO) fmt ./...

clean:
	rm -rf bin

# Install the `ask` CLI to $GOBIN (defaults to $(go env GOPATH)/bin).
# Make sure that directory is on your PATH.
install:
	$(GO) install ./cmd/ask
	@echo "installed: $(GOBIN)/ask"
