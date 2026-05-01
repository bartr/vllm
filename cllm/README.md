# cllm

`cllm` is a small Go web server that listens on port `8080` by default.

## Endpoints

- `GET /cache` returns cache status, size, entries, and cache key summaries; it also supports cache actions through query params
- `GET /cache/{key}` returns details for one cache entry, including cached content, extracted tokens, and raw body
- `GET /health` returns `ok`
- `GET /ready` returns `ready`
- `GET /version` returns the current application version as plain text with no surrounding whitespace
- `GET /config` returns the live handler config and applies any supported query string updates before printing it. Browsers (`Accept: text/html`) get an HTML form: read-only by default, click **Edit** (or visit `/config?edit=1`) to make fields editable; submitting the form does a `POST /config` with form-encoded values that go through the same validation as the query-string API. Non-browser clients keep the legacy JSON contract.
- `GET /nodes` lists the live node fleet; `GET /nodes/{id}` returns one node; `POST`/`PUT /nodes/{id}` creates or updates a node from JSON, form values, or query params; `DELETE /nodes/{id}` removes a node, except the last remaining node cannot be deleted
- `GET /metrics` returns Prometheus metrics for HTTP traffic, queue state, cache activity, downstream latency, TTFTB, and job processing

## Run locally

```bash
go run ./cmd/cllm
```

Or with an explicit port:

```bash
CACHE_PORT=8081 go run ./cmd/cllm
```

Show help or version:

```bash
go run ./cmd/cllm --help
go run ./cmd/cllm --version
```

## Runtime Configuration

The server supports these runtime settings:

- `CACHE_CACHE_SIZE` or `--cache-size` / `-c`: maximum number of cached chat responses
- `CACHE_CACHE_FILE_PATH` or `--cache-file-path`: cache persistence file path, default `/var/lib/cllm/cache.json`
- `CACHE_DOWNSTREAM_URL` or `--downstream-url`: downstream Chat Completions API base URL (vLLM, OpenAI, Azure OpenAI, OpenRouter, etc.), default `http://localhost:8000`
- `CACHE_DOWNSTREAM_TOKEN` or `--downstream-token`: bearer token sent to the downstream API
- `CACHE_DOWNSTREAM_MODEL` or `--downstream-model`: default downstream model when incoming requests omit `model`
- `CACHE_SYSTEM_PROMPT` or `--system-prompt`: default system prompt for chat completions
- `CACHE_MAX_TOKENS` or `--max-tokens`: default max tokens for chat completions, `100` to `4000`, default `1024`
- `CACHE_MAX_TOKENS_IN_FLIGHT` or `--max-tokens-in-flight`: max admitted token cost in flight (cost-based admission), `1` to `2000000`, default `200000`. Each request is charged `prompt_tokens + min(max_tokens, p95_completion_tokens)` against this budget; oversized single requests are rejected immediately.
- `CACHE_MAX_WAITING_REQUESTS` or `--max-waiting-requests`: max queued waiting requests when the budget is full, `0` to `1024`, default `1024`
- `CACHE_TEMPERATURE` or `--temperature`: default temperature for chat completions
- `CACHE_PREFILL_RATE_MULTIPLIER` or `--prefill-rate-multiplier`: simulated prefill rate as a multiple of the routed node's per-request decode rate (`Capacity.PerRequestTPS` from `nodes.yaml`, default `32`), `0` to `20`, default `0` (prefill simulation disabled; set non-zero to enable)
- `CACHE_PREFILL_BASE_OVERHEAD_MS` or `--prefill-base-overhead-ms`: fixed simulated prefill startup overhead in ms, `0` to `60000`, default `0`
- `CACHE_PREFILL_JITTER_PERCENT` or `--prefill-jitter-percent`: ± jitter applied to simulated prefill latency, percent, `0` to `100`, default `0`
- `CACHE_PREFILL_MAX_MS` or `--prefill-max-ms`: safety cap on simulated prefill latency in ms, default `3000`
- `CACHE_STREAM_VARIABILITY_PERCENT` or `--stream-variability-percent`: ± token-rate oscillation during cached stream replay, `0` to `100`, default `0` (disabled)
- `CACHE_STREAM_JITTER_PERCENT` or `--stream-jitter-percent`: ± per-segment jitter during cached stream replay, `0` to `100`, default `0` (disabled)
- `CACHE_STREAM_STALL_PROBABILITY_PERCENT` or `--stream-stall-probability-percent`: per-segment chance of a partial stall, `0` to `100`, default `0` (stalls disabled)
- `CACHE_STREAM_STALL_MIN_MS` or `--stream-stall-min-ms`: minimum partial-stall duration in ms, default `100`
- `CACHE_STREAM_STALL_MAX_MS` or `--stream-stall-max-ms`: maximum partial-stall duration in ms, default `800` (must be ≥ `stream-stall-min-ms`)
- `CACHE_DSL_PROFILE` or `--dsl-profile`: server-wide default DSL profile applied when a request omits `:dsl` (must be a name defined in `configs/profiles.yaml`); also settable at runtime via `GET /config?dsl-profile=NAME`
- `-h` or `--help`: show command usage and exit
- `--version`: show the application version and exit

Example:

```bash
CACHE_PORT=8081 CACHE_SHUTDOWN_TIMEOUT=15s CACHE_DOWNSTREAM_URL=https://api.openai.com CACHE_DOWNSTREAM_TOKEN=your-token CACHE_DOWNSTREAM_MODEL=gpt-4.1 CACHE_MAX_TOKENS_IN_FLIGHT=128000 CACHE_MAX_WAITING_REQUESTS=512 go run ./cmd/cllm --cache-size 200
```

For a local vLLM source, omit the downstream token and model settings and keep the default downstream URL of `http://localhost:8000`.

You can inspect or update the live handler config at runtime:

```bash
curl 'http://127.0.0.1:8080/config?cache-size=200&system-prompt=Be%20precise&max-tokens=700&max-tokens-in-flight=128000&max-waiting-requests=512&temperature=0.7'
```

`/config` now returns `tokens_in_flight`, `waiting_requests`, and `version` first, then `cache_size` and `cache_entries`, followed by `downstream_url`, `downstream_model`, `max_tokens_in_flight`, `max_waiting_requests`, and request defaults. You can update the configurable values live with either hyphenated or snake_case query params where supported. Live updates currently support `system-prompt`, `max-tokens`, `temperature`, `cache-size`, `downstream-url`, `downstream-token`, `downstream-model`, and `dsl-profile`. Per-node admission, pacing, KV, bypass-cache, and realism knobs are live through `/nodes`; startup defaults still come from `configs/nodes.yaml` (or `CLLM_NODES_FILE`).

Examples:

```bash
curl 'http://127.0.0.1:8080/nodes'
curl 'http://127.0.0.1:8080/nodes/cllm'
curl -X POST 'http://127.0.0.1:8080/nodes/cllm?per-request-tokens-per-second=96&max-concurrency=128'
curl -X PUT -H 'Content-Type: application/json' \
  -d '{"class":"cllm","max_tokens_in_flight":200000,"max_waiting_requests":32,"per_request_tokens_per_second":32,"degradation_threshold":10,"max_concurrency":128,"max_degradation":60}' \
  'http://127.0.0.1:8080/nodes/cllm'
curl -X DELETE 'http://127.0.0.1:8080/nodes/cllm'
```

`POST` and `PUT` preserve omitted values when updating an existing node. For a new node, omitted numeric fields default to zero. These runtime edits are not written back to `nodes.yaml`; update the ConfigMap when you want the same fleet after a restart. `DELETE` refuses to remove the final node so the router always has a valid target for subsequent requests. A node may set `bypass_cache: true` to force `:dsl no-cache` semantics on every request routed to it (used by the real-GPU `vllm` baseline lane so cache replays from peer lanes never contaminate the upstream measurement).

The upstream `/v1/models` response is cached for the lifetime of the process. If the downstream server starts serving a different model, restart `cllm` to pick it up.

Request admission is cost-based: each request charges `prompt_tokens + min(max_tokens, p95_completion_tokens)` against `max-tokens-in-flight`. Requests that don't fit join a FIFO queue bounded by `max-waiting-requests`. When both the budget and queue are full, `cllm` returns `429` with `over capacity`. Single requests larger than the entire budget are rejected immediately rather than blocking forever.

If you lower `max-tokens-in-flight` or `max-waiting-requests` below the current in-flight or queued counts, existing work is preserved. New admissions stay blocked until the live counts fall back within the new limits.

### Multi-tenant admission

Requests can be tagged with an `X-Tenant-Id` header (e.g. `X-Tenant-Id: acme`). Tenant names are matched case-insensitively, validated against `[a-z0-9_-]{1,64}`, and any unknown, missing, or invalid value is routed to the `default` tenant.

Each tenant has its own token-bucket rate limiter that gates admission *before* the global cost budget. The bucket is refilled at the tenant's `rate` (token cost per second) up to `burst` (max single burst). On admission:

1. Cost is estimated using the tenant's own p95 completion-token history when warm, falling back to the global p95, then to the request's `max_tokens`.
2. If the tenant bucket can't cover the cost, `cllm` returns `429` with `tenant rate exceeded` (no global queue used).
3. Otherwise the global cost budget is checked. If the global gate later rejects, the tenant bucket is refunded so the quota isn't permanently drained by globally-rejected requests.

Configure tenants via `configs/tenants.yaml` (override path with `CLLM_TENANTS_FILE`):

```yaml
tenants:
  default:
    rate: 0       # 0 disables the tenant limit; only the global budget gates
    burst: 0
  interactive:
    rate: 5000
    burst: 50000
  batch:
    rate: 50000
    burst: 500000
```

Two Prometheus counters surface tenant decisions: `cllm_tenant_admissions_total{tenant, class}` and `cllm_tenant_rejections_total{tenant, class, reason}` where `reason` is `tenant_rate` or `over_capacity`. Lifecycle log events include `tenant`, `class`, and `cost` fields.

### Workload classes

Workload class is a third dimension orthogonal to tenant (who) and node class (what hardware), introduced in cllm 0.9.x as **Phase 14A** (labeled dimension only — no behavior change yet). Classes are loaded from `configs/classes.yaml` (override path with `CLLM_CLASSES_FILE`):

```yaml
classes:
  default:     { priority: medium, max_queue_ms: 0 }
  interactive: { priority: high,   max_queue_ms: 500 }
  batch:       { priority: low,    max_queue_ms: 10000 }
```

Class is selected per-request, first-wins:

1. `:dsl workload-class=NAME` directive in the prompt (highest precedence).
2. `X-Workload-Class` HTTP header.
3. `default`.

Names are matched case-insensitively, validated against `[a-z0-9_-]{1,32}`; unknown / missing / malformed values resolve to `default` so Prometheus label cardinality stays bounded by `configs/classes.yaml`. Phase 14A surfaces `class` on the admission counters and on `started`/`completed`/`rejected` lifecycle events.

**Phase 14B (cllm 0.9.x): `max_queue_ms` enforcement.** When a class sets `max_queue_ms > 0`, requests that would wait longer than the cap in the admission FIFO are rejected with HTTP 429 `class queue timeout` and `cllm_tenant_rejections_total{tenant, class, reason="class_queue_timeout"}` increments. The tenant bucket is refunded, so a deadline-driven rejection does not drain rate quota. Per-request override: `:dsl max-queue-ms=N` (positive integer; wins over the class default). Immediate over-capacity rejections (request larger than the entire budget, or queue full at arrival) keep `reason=over_capacity` — the deadline path only fires after waiting.

**Phase 14C (cllm 0.9.x): priority-weighted dequeue.** When admission frees a slot, the highest-priority waiter that fits is promoted instead of the strict FIFO head. The class `priority` (`low`, `medium`, `high`) maps to numeric scores (`-1`, `0`, `+1`); a per-request `:dsl priority=NAME` directive overrides the class default for that request only. Within a tier, ordering remains arrival-order FIFO. Aging is on by default (1 s tick): for every full second a waiter spends queued, its effective priority gains `+1`, so a low-priority request waiting more than two seconds eventually out-ranks a fresh high-priority arrival — starvation is bounded without an explicit ordering scheduler. The new counter `cllm_admission_priority_skips_total{node, class}` increments whenever the queue promotes a non-head waiter, so dashboards can confirm priority is doing work in production. Strict over-capacity / KV-pressure rejections are unchanged.

Cached responses are replayed at the configured token rate. Once the in-flight token budget rises above `10%` of capacity, cached replay throughput degrades gradually up to the configured maximum. The live computed degradation percentage and effective token rate are exposed through `/config`, logged whenever they change, and included in the periodic queue-depth logs. Live downstream responses still stream through once admitted.

On a cache hit, `cllm` simulates **prefill latency** before emitting the first byte to mimic a CPU-based LLM. The delay is `prefill_base_overhead_ms + (prompt_tokens / prefill_rate) * 1000`, with `±prefill_jitter_percent` random jitter and a safety cap of `prefill_max_ms`. The prefill rate is `prefill_rate_multiplier * routed_node.PerRequestRate(c)`, so adjusting per-node `Capacity.PerRequestTPS` (or its concurrency-degradation curve in `nodes.yaml`) automatically scales prefill too. Setting `prefill-rate-multiplier=0` (or routing to a passthrough node with `per_request_tokens_per_second: 0`) disables prefill simulation. The simulated delay is reported as a `prefill` lifecycle event (`prompt_tokens`, `prefill_ms`) and as the `cllm_prefill_duration_seconds{source}` histogram. Live downstream requests are unaffected.

During cached stream replay, `cllm` also adds **streaming realism** to the per-segment pacing delay so token emission is not perfectly uniform. For each content segment the base delay (`tokens / routed_node.PerRequestRate(c)`) is multiplied by `(1 + v · stream_variability_percent/100)` to oscillate the rate, then by `(1 + j · stream_jitter_percent/100)` for per-segment jitter, where `v, j ∈ [-1, 1)` are random draws. With probability `stream_stall_probability_percent/100`, a uniform random partial stall in `[stream_stall_min_ms, stream_stall_max_ms]` is added on top to mimic GC, attention bottlenecks, or KV-cache pressure. Stalls are reported via `cllm_stream_stalls_total{source}` and `cllm_stream_stall_duration_seconds{source}`. Setting all four stream knobs (`stream-variability-percent`, `stream-jitter-percent`, `stream-stall-probability-percent`, plus a passthrough node with `per_request_tokens_per_second: 0`) tunes or disables this behavior. The non-stream cache path and live downstream requests are unaffected.

### Replay DSL

Prompts can carry a small per-request DSL that adjusts cache-replay pacing without restarts or `/config` changes. The literal token `:dsl` (case-insensitive) switches the prompt parser into DSL mode; every whitespace-separated token after it is interpreted as a directive and **stripped from the message** before the prompt reaches the downstream model or contributes to the cache key. This means `:dsl segment=50` and `:dsl segment=-50` resolve to the same cached entry while replaying it differently. DSL parsing is enabled by default and applies only to cache-replay; live downstream requests see only the cleaned prompt.

Tokens are processed left to right across all messages; the **first directive of each class wins** and duplicates are silently ignored (so `segment=50 segment=-50` keeps `segment=50`, `jitter=10 jitter=-30` keeps `jitter=10`). The directive `no-cache` has the highest precedence: it is honored regardless of position. `no-delay` is a macro that expands to `no-prefill no-jitter no-variability no-stall` (each subject to first-wins); it does **not** disable TPS pacing. Use `no-tps` to skip TPS pacing entirely.

Numeric directives use a uniform `key=value` shape. The `=` is optional — `segment 50` and `segment=50` are equivalent. `value` is either a signed integer (`50`, `-30`) or a signed range `lo:hi` (`30:50`, `-50:-30`, `-20:20`). When `lo > hi` the bounds are normalized by swapping. For ranges, the value is drawn uniformly from `[lo, hi]` per request (per segment for `segment=…`).

| Token | Effect |
|---|---|
| `no-cache` | bypass cache lookup; always go to the downstream model. The response is still written to the cache (refresh semantics). Highest precedence. |
| `no-delay` | macro for `no-prefill no-jitter no-variability no-stall`. TPS pacing still applies — combine with `no-tps` (or `tps=N`) for full speed. |
| `no-tps` | skip TPS pacing for cached replay (per-segment delay = 0). Claims the `tps` class, so a later `tps=N` is ignored under first-wins. |
| `no-prefill` | skip prefill simulation only |
| `no-jitter` / `no-variability` / `no-stall` | set jitter / variability / stall-probability to 0 |
| `tps=N` (or `tps=A:B`) | use `N` tokens/sec as the base decode rate (1–2048); scheduler degradation still applies |
| `max-tokens=N` (or `max-tokens=A:B`) | override `max_tokens` for this request (applies to both cached replay and live downstream); the cache key is unaffected |
| `profile=NAME` | expand a named directive bundle (see [Profiles](#replay-dsl-profiles)). Profile tokens are applied **after** explicit directives, so explicit prompt tokens always win on conflicts. |
| `segment=N` (or `segment=A:B`) | per-segment delay scale of `(1 + N/100)`. Use negative values to shrink: `segment=-30` runs 30% faster, `segment=-20:20` jitters between 0.8× and 1.2×. |
| `jitter=N` (or `jitter=A:B`) | add `N` (or random in `[A, B]`) percentage points to jitter percent, clamped 0–100. Negative values reduce jitter. |
| `variability=N` (or `variability=A:B`) | same shape, on rate variability percent |
| `stall=N` (or `stall=A:B`) | same shape, on stall probability percent |
| `prefill=N` (or `prefill=A:B`) | scale the prefill duration once by `(1 + N/100)`; use negatives to shrink |
| `workload-class=NAME` | tag the request with a workload class (Phase 14A: labels admission + lifecycle metrics; see [Workload classes](#workload-classes)). Wins over the `X-Workload-Class` header. |
| `max-queue-ms=N` (or `max-queue-ms=A:B`) | per-request admission queue wait cap, in milliseconds (Phase 14B). Wins over the resolved class's `max_queue_ms`. Must be `>= 0`; `0` is a no-op. Exceeding the cap returns `429 class queue timeout` and refunds the tenant bucket. |
| `initial-tokens=N` (or `initial-tokens=A:B`) | phase-aware token allocation (Phase 13.4): size of the prefill-fast band before the stream slows to `sustained-tps`. `0` explicitly disables phase A for this request (single-rate). Wins over the resolved class's `initial_tokens`. |
| `initial-tps=N` (or `initial-tps=A:B`) | per-request override for the phase A token rate (range `1..2048`). Wins over the resolved class's `initial_tps`. |
| `sustained-tps=N` (or `sustained-tps=A:B`) | per-request override for the phase B (sustained) token rate (range `1..2048`). Wins over the resolved class's `sustained_tps` and over `tps=N`'s effect on the tail when both are set; if `tps=N` is also present it forces single-rate and the phase fields are ignored. |
| `no-phase` | force single-rate replay for this request even when the resolved class declares a phase envelope. Claims `initial-tokens`, `initial-tps`, and `sustained-tps` simultaneously, so later per-field directives are first-wins-dropped. Pairs with `cllm_phase_transitions_total` staying flat. |
| `priority=low\|medium\|high` | per-request admission-queue priority override (Phase 14C). Wins over the resolved class's `priority`. `high` lifts the request above same-cost queued waiters at lower tiers; `low` defers to higher tiers but is protected from starvation by the 1 s aging tick. Invalid values are dropped silently. |

Random ranges are drawn fresh per segment for `segment=A:B`; once per request for `jitter`, `variability`, `stall`, `prefill`, `tps`, and `max-tokens`. Unknown or malformed tokens are silently ignored. A bare keyword (e.g. `jitter`) followed by a non-numeric next token is treated as a no-op and the next token is processed independently. Each parsed directive emits a `dsl_applied` lifecycle event and increments `cllm_dsl_directives_total{directive}`.

Examples:

```text
:dsl no-cache                  # force a downstream call, refresh the cache entry
:dsl no-delay no-tps           # replay as fast as possible (no prefill, no pacing)
:dsl no-delay tps=512          # smooth stream pinned at 512 tps
:dsl tps=16                    # simulate a slow CPU box
:dsl max-tokens=64             # cap reply at 64 tokens (cached and live)
:dsl segment=30:50 jitter=25   # slower, more jagged stream
:dsl segment=-20:20            # jitter the stream symmetrically around real-time
:dsl no-prefill no-stall       # cache hits with TTFB ~0 and steady streaming
:dsl tps=200 prefill=-50       # fast decode, prefill cut in half
:dsl segment 30:50             # `=` is optional
:dsl profile=interactive       # snappy chat: no stalls/jitter
:dsl profile=batch tps=50      # batch baseline, but force tps=50 for this run
```

### Replay DSL profiles

Profiles are named bundles of DSL tokens. They're useful for benchmark scenarios: instead of repeating low-level knobs in every prompt, the prompt names a profile and the server expands it.

Profiles are loaded from [`configs/profiles.yaml`](configs/profiles.yaml) at startup. Resolution order:

1. `CLLM_DSL_PROFILES_FILE` if set (explicit override; YAML or JSON; missing file is an error).
2. `./configs/profiles.yaml` relative to the current working directory.
3. `configs/profiles.yaml` next to the running binary.

Each value may be either a space-separated string of directive tokens or a YAML list of token strings:

```yaml
interactive: "no-stall no-jitter"
slow-cpu:    "tps=8 stall=30 jitter=20"
smooth:
  - "no-jitter"
  - "no-variability"
  - "no-stall"
  - "tps=300"
```

The shipped [`configs/profiles.yaml`](configs/profiles.yaml) defines:

| Group | Names | Effect |
|---|---|---|
| Style | `interactive`, `batch`, `stall-heavy`, `prefill-heavy` | Snappy / throughput / pathological / slow-TTFB |
| Speed (faster) | `fast`, `faster`, `fastest` | Per-segment delay **and** prefill duration shrunk by 0–10%, 10–25%, 25–50% |
| Speed (slower) | `slow`, `slower`, `slowest` | Per-segment delay **and** prefill duration grown by 0–10%, 10–25%, 25–50% |
| TPS sweep | `tps-16`, `tps-32`, `tps-64`, `tps-128`, `tps-256`, `tps-512`, `tps-1024`, `tps-1536`, `tps-2048` | Pin tokens-per-second; `no-delay` macro disables prefill/jitter/variability/stall |

Profile tokens are applied **after** the explicit directives in the prompt. Combined with first-wins semantics, this means an explicit token in the prompt always overrides the same class supplied by the profile (e.g. `:dsl tps=50 profile=interactive` keeps `tps=50` and inherits `no-stall`/`no-jitter` from `interactive`). Only the first `profile=` token is honored; unknown profile names are silently ignored.

#### Server-wide default profile

A default profile can be configured so that every request which omits `:dsl` entirely is replayed as if the prompt had said `:dsl profile=NAME`. Three precedence rules apply:

1. **Lowest priority.** Any prompt that includes a `:dsl` marker — even one with no directives, or only `no-cache` — completely suppresses the default profile.
2. **Override.** An explicit `profile=` token in the prompt replaces (does not stack with) the default profile.
3. **First-wins still applies.** Individual class tokens in the prompt (e.g. `tps=50`) win over the same class in the default profile bundle.

Configure it three ways:

- Flag: `--dsl-profile fast`
- Env: `CACHE_DSL_PROFILE=fast`
- Runtime: `GET /config?dsl-profile=fast` (or `dsl_profile=fast`); send `?dsl-profile=` with an empty value to clear it.

Unknown profile names are rejected at startup and on `/config` updates. The current value is reported in `GET /config` as `dsl_default_profile`.

The cache key is a SHA-256 of the non-system messages only (system messages, `model`, `temperature`, `max_tokens`, and `stream` are excluded). Each remaining message's content is normalized before hashing: lowercased, punctuation/symbols stripped, split on whitespace, English stop words removed, and the surviving tokens sorted alphabetically. As a result, prompts like `"Explain Azure"`, `"please explain azure!"`, and `"Could you explain what Azure is?"` all collapse to the same cache entry. The same cache entry is reused across streaming and non-streaming requests; the response is converted between SSE and JSON on the fly when the request format does not match the cached format.

On a cache hit, `max_tokens` from the request controls how much of the cached response is delivered:

- For streaming responses, the replay stops emitting content chunks once the cumulative completion-token count reaches `max_tokens`. A synthetic usage chunk and `data: [DONE]` terminator are appended when needed. `finish_reason` is always preserved as cached (`stop`).
- For non-streaming responses, the entire cached body is returned, but the pacing delay is capped at `max_tokens` worth of tokens at the current effective rate.
- If the cached response is shorter than the requested `max_tokens`, the full cached response is returned with no padding.

It also returns `cache_size` and `cache_entries`. You can resize the cache live with `cache-size` or `cache_size`; if the new size is smaller than the current number of entries, the least recently used entries are evicted immediately.

On startup, `cllm` attempts to load the configured cache file if it exists. A missing file is ignored; a malformed cache file fails startup. The cache file stores persisted entries only; runtime `cache_size` still comes from `CACHE_CACHE_SIZE` or `--cache-size`.

You can also inspect and manage the cache through `/cache`:

```bash
curl 'http://127.0.0.1:8080/cache'
curl 'http://127.0.0.1:8080/cache/<cache-key>'
curl 'http://127.0.0.1:8080/cache?action=clear'
curl 'http://127.0.0.1:8080/cache?action=save'
curl 'http://127.0.0.1:8080/cache?action=load'
curl 'http://127.0.0.1:8080/cache?size=200'
```

`/cache` supports these query parameters:

- `action=clear` clears the current cache contents
- `action=save` writes the current cache entries to `cache_file_path`
- `action=load` replaces the current in-memory cache with the contents of `cache_file_path`
- `size=n` resizes the cache to `n`, where `n` must be between `0` and `10000`; `0` disables the cache

`/cache` responses include `cache_file_path`, and save/load actions also report the number of entries written or loaded. `/cache/{key}` returns metadata for the matching cache entry plus the cached content, a whitespace-split `text_tokens` view, and the raw cached body.

Example switching the downstream source to another Chat Completions API at runtime:

```bash
curl 'http://127.0.0.1:8080/config?downstream-url=https%3A%2F%2Fapi.openai.com&downstream-token=your-token&downstream-model=gpt-4.1'
```

Equivalent snake_case form:

```bash
curl 'http://127.0.0.1:8080/config?downstream_url=https%3A%2F%2Fapi.openai.com&downstream_token=your-token&downstream_model=gpt-4.1'
```

Example shrinking the cache to a single entry at runtime:

```bash
curl 'http://127.0.0.1:8080/config?cache-size=1'
```

The downstream token is intentionally not returned by `/config`, but it can be updated through `/config?downstream-token=...` or `/config?downstream_token=...`.

Prometheus scraping is exposed at `/metrics`. In addition to the standard Go and process collectors, `cllm` exports HTTP request metrics and service-specific metrics covering queue wait time, in-flight and waiting request counts, effective token throughput, cache hit and miss counts, downstream request latency, time to first byte, overall job duration for `/v1/chat/completions`, and per-request lifecycle events (`cllm_request_lifecycle_events_total`).

## Request correlation IDs and lifecycle events

Correlation IDs are scoped to `POST /v1/chat/completions` only. Health, readiness, metrics, version, config, and cache endpoints do not assign or log a `request_id`, and do not set the `X-Request-ID` response header.

For chat completion requests:

- If the inbound request includes a valid `X-Request-ID` header (matching `^[A-Za-z0-9_-]{1,128}$`), it is preserved.
- Otherwise a new 26-character ULID is generated.
- The resolved id is echoed on the response in the `X-Request-ID` header and attached to every related log line.

For `POST /v1/chat/completions`, structured lifecycle events are emitted as logs (with the `request_id` attribute) and counted on the Prometheus counter `cllm_request_lifecycle_events_total{endpoint,event,outcome}`:

| Event | Level | Outcome values | Notes |
|---|---|---|---|
| `admitted` | INFO | `""` | Logged once per request when the scheduler grants a slot (direct or after waiting). Includes `source` and `queue_wait_ms`. |
| `queued` | INFO | `""` | Only emitted when the request actually waits in the queue. |
| `started` | INFO | `""` | Logged after admission, before defaults/cache lookup. Includes `mode` and `max_tokens`. |
| `first_token` | INFO | `""` | Logged once per request when the first response byte is written to the client. Includes `source` (`cache` or `downstream`), `mode`, and `ttfb_ms`. |
| `completed` | INFO | `completed`, `failed` | Terminal event for any request that was admitted. Includes `source`, `mode`, `status`, `queue_wait_ms`, `duration_ms`, `prompt_tokens`, `completion_tokens`, `max_tokens`. |
| `rejected` | WARN | `over_capacity`, `bad_request`, `missing_messages` | Terminal event for any request that did not reach the processing phase. |

## Build

```bash
make build
```

This builds the local container image `cllm:0.17.0`.

To build and import that image into the local k3s container runtime:

```bash
make deploy
```

That runs the equivalent of:

```bash
docker build -t cllm:0.17.0 .
docker save cllm:0.17.0 | sudo k3s ctr images import -
```

## Test

```bash
go test ./...
```

## Docker

```bash
docker build -t cllm:0.17.0 .
docker run --rm -p 8080:8080 cllm:0.17.0
```

The Docker image copies the committed [cache.json](/home/bartr/vllm/cllm/cache.json) artifact into `/var/lib/cllm/cache.json`, which `cllm` then auto-loads on startup if it contains entries.

That image-bundled seed only applies when nothing else is mounted at `/var/lib/cllm`. In the local Kubernetes deployment below, the PVC is mounted at that same path, so the live pod reads and writes the PVC-backed `cache.json` instead of the file baked into the image.

## Kubernetes

The local k3s manifests live under [clusters/z01/cllm](/home/bartr/vllm/clusters/z01/cllm).

They:

- deploy `cllm:0.17.0`
- set `imagePullPolicy: Never` so the local image is never pulled from a registry
- run `cllm` in the `cllm` namespace
- mount a `local-path` PVC at `/var/lib/cllm` so `cache.json` persists across pod replacement and overrides the image-bundled cache seed at that path
- expose it internally on service port `8080`
- expose it through a dedicated Traefik `cllm` entrypoint on external port `8080`

If you want Kubernetes to start with a preloaded cache, seed the PVC-backed file rather than relying on the image copy. The repository helper [scripts/export-cache.sh](/home/bartr/vllm/scripts/export-cache.sh) exports the current PVC-backed cache back into [cllm/cache.json](/home/bartr/vllm/cllm/cache.json); the inverse flow is to write the desired `cache.json` into the mounted volume before or after deployment.

Apply the manifests with:

```bash
kubectl apply -k /home/bartr/vllm/clusters/z01/cllm
kubectl -n kube-system rollout status deployment/traefik
kubectl -n cllm rollout status deployment/cllm
```

Then call it through the Traefik external IP on port `8080`:

```bash
curl -i http://192.168.68.63:8080/health
```
