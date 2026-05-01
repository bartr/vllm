---
mode: agent
description: Diagnose SSE / streaming buffering or TTFT bugs in cllm. Encodes the flush-wrapper diagnostic recipe and known-correctness rules. Use when the user reports "first token is slow", "TTFB high", "chunks arrive together", "32KB buffering", "SSE buffered", or "streaming feels batched".
---

# Diagnose cllm streaming / SSE buffering

You are debugging an end-to-end first-byte / chunk-buffering issue. The bug class has bitten this repo before (see `cllm/internal/httpapi/handler.go` `loggingResponseWriter.Flush`). Follow the recipe in order — do NOT skip layers.

## Symptom triage (ask user up-front)
1. What's the **observed TTFT** at the client? Server `ttfb_ms` from logs?
2. Does the issue affect **live** responses, **cache-replay**, or both?
3. Are chunks arriving in **fixed-size blocks (32768 bytes)** or just delayed uniformly?
4. Through Traefik (`:8088`) or direct to the Service (`:8080`)?

If `ttfb_ms < 5ms` server-side but client sees seconds → almost certainly a flush-wrapper bug.

## Diagnostic ladder (run in this order)

### Layer 1: client-side
```sh
~/go/bin/ask --debug --dsl 'no-cache' "tell me a long story about kubernetes" 2>&1 | head -40
```
The `--debug` flag prints SSE lines to stderr as they arrive. If lines arrive smoothly here, the bug is in the user's downstream code, not cllm.

### Layer 2: bypass Traefik — curl from inside the cluster
```sh
kubectl -n cllm run --rm -i curl-test --image=curlimages/curl --restart=Never -- \
  curl -N -s -w '\n--- ttfb=%{time_starttransfer}s total=%{time_total}s\n' \
  -H 'Accept: text/event-stream' -H 'Accept-Encoding: identity' \
  http://cllm.cllm.svc.cluster.local:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"Qwen/Qwen2.5-3B-Instruct","stream":true,"messages":[{"role":"user","content":":dsl no-cache\nlong story"}]}'
```
- If TTFB is fast here but slow via `:8088` → Traefik buffering (check ingress annotations).
- If TTFB is slow here too → server-side. Continue.

### Layer 3: raw TCP, count chunk sizes
A small Go program with `net.Dial` + `bufio.Reader.ReadBytes('\n')` reveals whether bytes arrive in 32KiB blocks (= server `Flush()` is a no-op) or per-chunk.
- 32768-byte reads = the default `http.ResponseWriter` write buffer flushed only on close.
- This means a wrapper around `ResponseWriter` is missing `Flush()`.

### Layer 4: audit every ResponseWriter wrapper in `cllm/internal/httpapi/`
```sh
grep -nE 'type \w+ struct' cllm/internal/httpapi/*.go | xargs -I{} echo {}
grep -nE 'http\.ResponseWriter' cllm/internal/httpapi/*.go
grep -nE 'func \(\w+ \*?\w+\) Flush\(\)' cllm/internal/httpapi/*.go
```
Every type embedding or wrapping `http.ResponseWriter` MUST implement `Flush()` that forwards to the inner writer. Type assertions (`w.(http.Flusher)`) do NOT traverse embedded fields — a missing `Flush()` is silently a no-op.

Known wrappers that must have `Flush()`:
- `loggingResponseWriter` (handler.go) — already fixed.
- Any new `firstByteMetricsWriter`, `correlationResponseWriter`, etc.

### Layer 5: SSE correctness rules (verify all set)
- Response headers (live AND cache replay): `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `X-Accel-Buffering: no`.
- Upstream request to vLLM: `Accept-Encoding: identity` (Go's default transport gzip-decodes transparently, which buffers).
- Client (`ask`) request: `Accept-Encoding: identity` for the same reason.
- `replayCachedStream`: throttle AFTER `w.Write` + `flusher.Flush`, never before. Pre-write throttle inflates TTFT proportional to the first cached chunk's token count.

### Layer 6: phase / TPS confounders
If the gate is "TTFT predicted too high but actual is fine" — check:
- `simulatePrefillDelay` is sleeping the goroutine before any byte is written.
- `phase.active() && tokensSoFar < InitialTokens` may be applying initial-tps throttling to the first chunk.
- `:dsl no-tps` should short-circuit. Test with it.
- `:dsl no-delay` claims all delay-shaped classes (prefill + jitter + tps).

## Reporting format
- Layer at which the issue isolated.
- Specific wrapper / header / directive missing.
- Diff of the fix (smallest possible).
- Verification: re-run Layer 1 + Layer 2 with the fix in place. TTFB before/after.

## Anti-patterns to refuse
- Don't "fix" by increasing buffer flush frequency at the OS level. The bug is always in a missing `Flush()` or a buffering middleware, not in the kernel.
- Don't add `time.Sleep` workarounds.
- Don't disable streaming as a workaround unless the user explicitly asks for non-streaming.
