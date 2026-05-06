GO ?= go
BINARY ?= cllm
DOCKER ?= docker
IMAGE ?= cllm:0.19.0
K3S ?= sudo k3s
GOBIN ?= $(shell $(GO) env GOBIN)
ifeq ($(GOBIN),)
GOBIN := $(shell $(GO) env GOPATH)/bin
endif

.PHONY: build run test fmt clean cleanup deploy deploy-ask deploy-all install import

build:
	$(DOCKER) build -t $(IMAGE) .

# Import the (shared) image into k3s containerd. Both `cllm` and `askd`
# Deployments reference $(IMAGE), so we only need to do this once per
# build before either rollout.
import: build
	$(DOCKER) save $(IMAGE) | $(K3S) ctr images import -
	$(K3S) ctr images ls | grep $(BINARY)

deploy: import
	kubectl apply -k ../clusters/z01/cllm/
	kubectl -n cllm rollout restart deployment/cllm
	kubectl -n cllm rollout status deployment/cllm

# askd shares the cllm image; the Deployment overrides command/args to
# run /ask --serve. Apply the kustomization to pick up image-tag bumps
# in deployment.yaml (release-process.md step-6 gap), then rollout.
deploy-ask: import
	kubectl apply -k ../clusters/z01/ask/
	kubectl -n cllm rollout restart deployment/askd
	kubectl -n cllm rollout status deployment/askd

deploy-all: deploy deploy-ask
run:
	$(GO) run ./cmd/cllm

test:
	$(GO) test -race ./...

fmt:
	$(GO) fmt ./...

clean:
	rm -rf bin

# Delete every cllm/ask container image from k3s and docker EXCEPT the
# current $(IMAGE). Run after a release bump to free disk on the node.
# askd shares the cllm:VERSION image today (the Deployment overrides
# command/args), so pruning $(BINARY) covers both. The ask:* arm is
# defensive in case the image is ever split out.
cleanup:
	@echo "==> Keeping $(IMAGE); pruning older $(BINARY) and ask images"
	@keep_tag="$$(echo $(IMAGE) | sed 's/.*://')"; \
	for repo in $(BINARY) ask; do \
		echo "    k3s containerd ($$repo):"; \
		$(K3S) ctr images ls -q | grep -E "/$$repo:" | grep -v ":$$keep_tag$$" | while read ref; do \
			echo "      rm $$ref"; \
			$(K3S) ctr images rm "$$ref" >/dev/null || true; \
		done; \
		if command -v $(DOCKER) >/dev/null 2>&1; then \
			echo "    docker ($$repo):"; \
			$(DOCKER) images --format '{{.Repository}}:{{.Tag}}' | grep -E "^$$repo:" | grep -v ":$$keep_tag$$" | while read ref; do \
				echo "      rm $$ref"; \
				$(DOCKER) image rm "$$ref" >/dev/null || true; \
			done; \
		fi; \
	done
	@echo "==> Done. Remaining cllm/ask images:"
	@$(K3S) ctr images ls | grep -E "/(($(BINARY))|ask):" || true

# Install the `ask` CLI to $GOBIN (defaults to $(go env GOPATH)/bin).
# Make sure that directory is on your PATH.
install:
	$(GO) install ./cmd/ask
	@echo "installed: $(GOBIN)/ask"
