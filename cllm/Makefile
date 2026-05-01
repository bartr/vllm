GO ?= go
BINARY ?= cllm
DOCKER ?= docker
IMAGE ?= cllm:0.13.0
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

# Delete every container image of $(BINARY) from k3s and docker EXCEPT the
# current $(IMAGE). Run after a release bump to free disk on the node.
	@echo "==> Keeping $(IMAGE); pruning older $(BINARY) images"
	@keep="$(IMAGE)"; \
	echo "    k3s containerd:"; \
	$(K3S) ctr images ls -q | grep -E '/$(BINARY):' | grep -v ":$$(echo $$keep | sed 's/.*://')$$" | while read ref; do \
		echo "      rm $$ref"; \
		$(K3S) ctr images rm "$$ref" >/dev/null || true; \
	done; \
	echo "    docker:"; \
	if command -v $(DOCKER) >/dev/null 2>&1; then \
		$(DOCKER) images --format '{{.Repository}}:{{.Tag}}' | grep -E '^$(BINARY):' | grep -v "^$$keep$$" | while read ref; do \
			echo "      rm $$ref"; \
			$(DOCKER) image rm "$$ref" >/dev/null || true; \
		done; \
	fi
	@echo "==> Done. Remaining $(BINARY) images:"
	@$(K3S) ctr images ls | grep -E '/$(BINARY):' || true

# Install the `ask` CLI to $GOBIN (defaults to $(go env GOPATH)/bin).
# Make sure that directory is on your PATH.
install:
	$(GO) install ./cmd/ask
	@echo "installed: $(GOBIN)/ask"
