# spec — askd (`ask --serve`) HTTP control plane

`askd` is `ask --serve`: a long-running HTTP control plane around the
existing `ask` benchmark code, designed to be run as a Kubernetes
Deployment in the `cllm` namespace. The MCP server (`mcp/`) talks to it
over HTTP instead of running `ask --bench` as a local subprocess.

`ask --bench` (without `--serve`) is unchanged — it remains a one-shot
CLI tool. The HTTP control plane is opt-in via the `--serve` flag.

## Why

Before askd, MCP shelled out to `ask --bench`, scraped `pgrep`, and
tailed `~/logs/bench.log`. That tied "is a benchmark running?" to a
local process and a single log file. With askd:

- Benchmarks run in-cluster against the live cllm service.
- Multiple operators / agents see the same job state via the API.
- Each run gets its own log file (no timestamp slicing).
- pause / stop / restart map to deterministic API calls.

## Endpoints

| method | path                | purpose |
| ------ | ------------------- | ------- |
| GET    | `/health`           | liveness probe — always 200 once the server is up |
| GET    | `/ready`            | readiness probe — 200 once listening; includes current job state |
| GET    | `/version`          | name + version (matches the `ask --version` string) |
| GET    | `/config`           | JSON snapshot of the runtime defaults (HTML form when `Accept: text/html`) |
| PUT    | `/config`           | merge a JSON `jobSpec` into the runtime defaults |
| POST   | `/config/reset`     | revert runtime defaults to the askd-startup values |
| GET    | `/config/html`      | HTML form view + edit |
| POST   | `/config/html`      | form-encoded merge (browser-friendly) |
| GET    | `/bench`            | current job status |
| POST   | `/bench`            | start a new job (single job at a time) |
| POST   | `/bench/pause`      | drain in-flight requests, stop accepting new prompts |
| POST   | `/bench/start`      | resume from pause; or, if idle, start using current defaults |
| POST   | `/bench/stop`       | drain in-flight, **close the per-run log file**, return to idle |
| POST   | `/bench/restart`    | stop + immediate fresh start (warmup forced on) |
| GET    | `/logs`             | list per-run log files, newest first |
| GET    | `/logs/<name>`      | raw log; `?tail=N` returns last N bytes |

### `jobSpec` (POST /bench, PUT /config, POST /bench/restart)

```jsonc
{
  "bench": 120,                // concurrent workers (required for /bench)
  "count": 1000,               // stop after N requests (optional)
  "duration_ms": 60000,        // stop after wall-clock duration (optional)
  "ramp_start": 1, "ramp_end": 120, "ramp_duration_ms": 30000,
  "loop": false, "random": false, "warmup": false,
  "files": ["/path/in/pod/prompts.yaml"],   // optional
  "prompt": "explain azure",                 // alternative to files
  "url":   "http://cllm.cllm.svc.cluster.local:8080",
  "model": "",                                // empty = autodetect
  "system": "You are a helpful assistant.",
  "max_tokens": 100, "temperature": 0.2,
  "stream": true, "dsl": "",
  "quiet": false, "json": false, "report": true
}
```

Zero / unset fields fall back to the **runtime config** (defaults from
the ConfigMap; see below). Any field can be overridden per request.

## Job state machine

```
       POST /bench (idle)            POST /bench/pause
  idle ─────────────────────► running ───────────────► paused
   ▲                            │  │                    │
   │                            │  │ POST /bench/stop   │ POST /bench/start
   │ POST /bench/stop           │  ▼                    │
   └──────── stopping ──────────┘  idle ◄───────────────┘
        (drain + close log)
```

- **pause:** sets a gate; in-flight requests finish, workers block
  before pulling the next prompt. The current per-run log file stays
  open; pause/resume markers are written to it.
- **stop:** drains in-flight, marks the controller stopped, the worker
  goroutines exit, the per-run log file is closed. State returns to
  `idle`.
- **restart:** stop + start with the same spec, `warmup=true`. The
  next run gets a fresh log file (clean baseline for the next
  benchmark run).

This means the operator workflow

> stop → reconfigure → start → run benchmark → stop

produces a single per-run log file rather than requiring timestamp
slicing of a shared log.

## Runtime config: ConfigMap → process defaults → API overrides

1. The `askd-defaults` ConfigMap is mounted as env vars (`CLLM_URL`,
   `CLLM_MAX_TOKENS`, `ASK_ADDR`, `ASK_LOG_DIR`, …).
2. `ask --serve` reads those into `defaultOptions()` at process start.
3. `runtimeConfig.defaults` is frozen at that moment.
4. `runtimeConfig.current` starts equal to `defaults`. PUT /config and
   POST /config/html merge into `current`.
5. POST /config/reset reverts `current` back to `defaults` (the
   ConfigMap values). It does NOT re-read the ConfigMap from the
   cluster; pod restart does that.

## Local CLI behavior

- `ask hello` → unchanged (single shot).
- `ask --bench 8 --duration 30s` → unchanged (CLI bench, no HTTP).
- `ask --serve` → starts the HTTP server. CLI flags (`--bench`,
  `--prompt`, …) become **defaults** the API can override per-job, but
  no work runs until something hits `POST /bench`.

This is intentional: running `--bench` locally must never spin up the
HTTP service (the user explicitly requested this, so that local CLI
runs stay one-shot and don't collide with a long-running askd).

## Kubernetes deployment

Manifests live in [clusters/z01/ask/](../clusters/z01/ask/):

- `configmap.yaml` — `askd-defaults`, env vars consumed by askd at
  startup.
- `deployment.yaml` — uses the same `cllm:VERSION` image, overrides
  `command: ["/ask"]`, `args: ["--serve"]`. Mounts an `emptyDir` at
  `/var/log/askd` for per-run log files.
- `service.yaml` — ClusterIP on port 8008.
- `ingress.yaml` — Traefik IngressRoute on the `ask` entrypoint.

The Traefik `ask` entrypoint (added in
[clusters/z01/traefik/entrypoint.yaml](../clusters/z01/traefik/entrypoint.yaml))
exposes container port 8008 on host port **8008**.

Flux picks the kustomization up via
[clusters/z01/flux-system/listeners/ask.yaml](../clusters/z01/flux-system/listeners/ask.yaml).
A manual deploy looks like:

```sh
kubectl apply -k clusters/z01/ask/
```

After that, askd is reachable from the dev host at:

- `http://192.168.68.63:8008/health`
- `http://192.168.68.63:8008/config/html`

## MCP integration

`mcp/ask_client.py` wraps the askd HTTP API. `mcp/benchmark.py`
delegates `is_running()` and `tail_log()` there. The MCP tools that
used to shell out to `ask --bench` (`run_benchmark_window`,
`get_benchmark_status`, the `_experiment_report` helper) now talk to
askd.

If askd is not deployed, every MCP tool that depends on it returns

```json
{"error": "ask service unreachable at http://...:8008. Deploy with: kubectl apply -k clusters/z01/ask/ ..."}
```

instead of silently failing or hanging.

New MCP tools added:

- `get_askd_status()` — one-shot view: deployed?, version, current job
  state, current config, recent log file. Use this to answer "is askd
  running and under what config?".
- `update_ask_config(...)` — typed args for every common field plus an
  `extra` dict passthrough. PUT /config under the hood.
- `query_askd_logs(last_seconds | last_minutes | last_hours | since +
  until | run_name, include_rows, max_rows_per_run, max_runs)` — find
  matching per-run log files, return parsed summary stats and the
  askd marker lines (start/pause/resume/stop/end) for each. The
  caller (LLM) translates natural-language windows like "around 1:00
  today" or "yesterday between 12:30 and 1:30" into ISO-8601 UTC
  `since` / `until`.
- `start_bench(bench, duration_seconds, count, files, prompt, max_tokens, warmup)`
- `pause_bench()`, `resume_bench()`, `stop_bench()`, `restart_bench()`
- `list_bench_logs()`
- `get_ask_config()`, `reset_ask_config()`

## Settings (MCP env vars)

| var                         | default                       |
| --------------------------- | ----------------------------- |
| `CLLM_ASK_BASE_URL`         | `http://192.168.68.63:8008`   |

(Existing `CLLM_BASE_URL`, `CLLM_PROMETHEUS_URL`, etc. are unchanged.)

## askd-mode env vars (configmap)

These are read by `ask --serve` at process start and seed the runtime
defaults. Any of them can be overridden per-job via `PUT /config` or
`POST /bench`.

| var             | default                  | purpose |
| --------------- | ------------------------ | ------- |
| `ASK_ADDR`      | `:8008`                  | listen address |
| `ASK_LOG_DIR`   | `/var/log/askd`          | per-run log dir |
| `ASK_AUTOSTART` | `true`                   | start a benchmark when the pod becomes ready |
| `ASK_BENCH`     | `120`                    | concurrent workers for the autostart run |
| `ASK_FILES`     | `/configs/prompts.yaml`  | prompt source baked into the image |
| `ASK_LOOP`      | `true`                   | cycle the prompt list forever |

The autostart run is just `POST /bench` with an empty spec — the same
pause/stop/restart endpoints control it. Set `ASK_AUTOSTART=false` to
require a manual `POST /bench` instead.

## What's intentionally not done (yet)

- **No PVC for logs.** Logs live in an `emptyDir`; pod restart loses
  history. If that becomes painful, switch the deployment to a small
  PVC.
- **Form UX caveat.** The HTML form's `loop`, `random`, `warmup`,
  `quiet`, `json` checkboxes can be turned **on**, but unchecking
  them in the form does not flip the runtime default back to false —
  use `PUT /config` with explicit booleans, or `POST /config/reset`.
  The JSON API has no such limitation.
- **No auth.** Same posture as the cllm service today (cluster-local +
  trusted dev host). Add Traefik basic-auth middleware before exposing
  externally.
