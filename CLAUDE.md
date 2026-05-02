# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repo is

cLLM is a GPU-Calibrated LLM Inference Experimentation Platform. A single real GPU node (`vllm`) runs a physical vLLM instance; cLLM fronts it as a Chat Completions API proxy, adds token-based admission control, and simulates an N-node fleet through in-process synthetic nodes — enabling cheap routing and fairness experiments without extra hardware.

## Repository layout

```
cllm/           Go server (cllm) and CLI (ask)
  cmd/cllm/     Server entry point
  cmd/ask/      Benchmark CLI entry point
  internal/
    httpapi/    Main handler, DSL parsing, admission, multi-node API, metrics
    node/       Per-node primitives: cost, budget, p95 estimator, loader
    router/     Routing policies (least-loaded, class-pinned, chained)
    config/     Flag/env config loading
    runtimeconfig/ Defaults and shared constants
  configs/      Runtime config files (nodes.yaml, profiles.yaml, tenants.yaml)
mcp/            Python FastMCP server exposing cLLM ops to MCP clients
benchmark/
  scenarios/    YAML experiment definitions
  reports/      Markdown reports (one per run)
  logs/         Raw ask --bench output
clusters/z01/   K3s/Kubernetes manifests (Flux-managed)
scripts/        Helper shell scripts
```

## Common commands

All Go work runs from `cllm/`:

```bash
cd cllm
make run          # go run ./cmd/cllm  (local dev server)
make test         # go test -race ./...
make fmt          # go fmt ./...
make build        # docker build -t cllm:0.17.0 .
make deploy       # build + import to k3s + rollout restart
make install      # install ask CLI to $GOBIN

# Run a single package's tests
go test -race ./internal/httpapi/
go test -race ./internal/node/

# Install ask for use via scripts/ask
go install ./cmd/ask
```

MCP server (from repo root):

```bash
python3 -m venv .venv && source .venv/bin/activate
pip install 'mcp[cli]' httpx pyyaml
python3 mcp/server.py   # smoke test — exits cleanly on Ctrl-C
```

## Architecture: how a request flows

1. `ask` sends a Chat Completions request to cLLM on port 8088.
2. The handler (`httpapi/handler.go`) parses any `:dsl` directives from the message content and strips them before they reach the cache key or downstream.
3. Admission control (`httpapi/admission.go`, `node/budget.go`) charges `cost = prompt_tokens + min(max_tokens, p95_completion_tokens)` against the routed node's token budget. Requests that exceed `max_tokens_in_flight` queue up to `max_waiting_requests`; overflow is rejected 429.
4. The router (`router/router.go`) picks a node using the configured policy (least-loaded / class-pinned / chained). The real `vllm` node has an upstream URL and forwards to vLLM; synthetic nodes replay cached responses with controlled pacing.
5. Responses are cached in `cllm/cache.json`; cache key = SHA-256 of (model + system-prompt + user messages, DSL-stripped).
6. Prometheus metrics are exposed at `/metrics`; Grafana dashboards in `clusters/z01/grafana/dashboards/` visualise GPU, vLLM, and cLLM layers.

## Key configuration files

| File | Purpose |
|------|---------|
| `cllm/configs/nodes.yaml` | Fleet topology: node IDs, class refs, capacity, optional upstream URL |
| `cllm/configs/classes.yaml` | Hardware class templates (load shape, degradation, prefill multiplier) |
| `cllm/configs/profiles.yaml` | Named DSL profiles (e.g. `fast`, `slow`) |
| `cllm/configs/tenants.yaml` | Multi-tenant rate/burst limits |

See `cllm/configs/nodes.yaml.example` for the annotated schema. Router policy (`least-loaded`, `class-pinned`, `chained`) and fallback behavior live at the bottom of that file.

## DSL directives

Requests can carry per-request tuning in message content after `:dsl` (case-insensitive). Directives are stripped before cache lookup and downstream forwarding. Examples:

```
:dsl tps=20 jitter+10 stall+5
:dsl no-cache profile=fast
:dsl max-ttft-ms=500 priority=high
```

Named profiles defined in `configs/profiles.yaml` can be referenced with `profile=<name>`. `no-delay` is a shorthand for `no-prefill no-jitter no-variability no-stall` (does not disable TPS pacing; use `no-tps` for that).

## MCP server

`mcp/server.py` exposes cLLM control-plane operations to any MCP-aware client (Claude Code, Copilot Chat in agent mode). The server reads from environment variables:

```bash
CLLM_BASE_URL=http://192.168.68.63:8088
CLLM_GRAFANA_URL=http://192.168.68.63:3000/d/cllm-overview/cllm-overview
CLLM_BENCH_LOG=$HOME/logs/bench.log
CLLM_READ_ONLY=false          # set true to disable all mutations
CLLM_PROTECT_REAL_NODE=true   # never mutate the vllm node
```

**The `vllm` node is protected** — `create_synthetic_node` and `update_node` must never target it.

Tools: `list_nodes`, `get_node`, `get_config`, `get_cache_status`, `get_metrics_snapshot`, `get_benchmark_status`, `create_synthetic_node`, `update_node`, `run_benchmark_window`, `run_scenario`, `summarize_experiment`.

## Benchmark scenarios

Scenarios live in `benchmark/scenarios/` as YAML. Run them via the MCP `run_scenario` tool. Reports are written to `benchmark/reports/`; the canonical (no-timestamp) report is the headline result for each scenario.

Scenario schema is documented in `benchmark/SPEC.md`. A scenario declares groups of concurrent `ask --bench` workers, optional temporary node overrides (restored after the run), and an optional `baseline:` reference for auto-diff.

## Cluster endpoints (current node: 192.168.68.63)

| Service | URL |
|---------|-----|
| vLLM | `http://192.168.68.63:8000` |
| cLLM | `http://192.168.68.63:8088` |
| Prometheus | `http://192.168.68.63:9090` |
| Grafana | `http://192.168.68.63:3000` |

Apply cluster manifests with `kubectl apply -k clusters/z01/<component>`. Grafana dashboards are imported via `clusters/z01/grafana/scripts/import-dashboards.sh`.

## ask CLI

`scripts/ask` wraps the `ask` binary and defaults to `http://localhost:8088`. Key env vars: `CLLM_URL`, `CLLM_TOKEN`, `CLLM_MODEL`.

```bash
./scripts/ask "What is Kubernetes?"                          # single-shot
./scripts/ask --bench 10 --duration 30s --prompt 'hi'        # 10 concurrent workers for 30s
./scripts/ask --bench 50 --ramp 1:50 --ramp-duration 30s --duration 2m --prompt 'hi'
./scripts/ask --bench 8 --file prompts.yaml --random --duration 1m
./scripts/ask --help
```
