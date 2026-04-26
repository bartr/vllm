# vLLM on K3s on Ubuntu


## Clone this Repo

```powershel

git clone https://github.com/bartr/vllm

```

## Install components

```bash

sudo ./scripts/base.sh

./scripts/config.sh

```

`./scripts/base.sh` also configures Ubuntu to stay awake on this laptop by ignoring lid-close events and disabling suspend, hibernate, and hybrid sleep.

## Install NVIDIA GPU Support For K3s

K3s needs the NVIDIA container toolkit on the host before the device plugin can start pods with `runtimeClassName: nvidia`.

```bash

sudo ./scripts/install-nvidia-container-toolkit.sh

```

Apply the GitOps-managed NVIDIA device plugin manifests:

```bash

kubectl apply -k ./clusters/z01/nvidia-plugin

```

For a reusable GPU smoke test inside K3s:

```bash

kubectl apply -f manifests/gpu-smoke-test.yaml
kubectl logs gpu-smoke-test
kubectl delete -f manifests/gpu-smoke-test.yaml

```

## Run vLLM In Kubernetes

For your setup, vLLM is the right default if you want an OpenAI-compatible API and better throughput than simpler wrappers. If your priority is the absolute simplest single-node experience, Ollama is easier to get running, but vLLM is the better fit for a GPU-backed Kubernetes service.

### Pick a starter model

Choose a model that fits your GPU before you deploy:

| GPU VRAM | Good starter model |
| --- | --- |
| 8 GB | `Qwen/Qwen2.5-1.5B-Instruct`, `Qwen/Qwen2.5-3B-Instruct`, or `Qwen/Qwen2.5-7B-Instruct-AWQ` |
| 12 GB | `Qwen/Qwen2.5-3B-Instruct` |
| 16 GB | `Qwen/Qwen2.5-7B-Instruct` |
| 24 GB+ | `meta-llama/Llama-3.1-8B-Instruct` |

The manifests are tuned for the node in this repo: an `NVIDIA GeForce RTX 3070 Laptop GPU` with `8192 MiB` of VRAM. They share the same `vllm` service and the same PVC-backed cache:

- [manifests/vllm-small.yaml](/home/bartr/vllm/manifests/vllm-small.yaml) for `Qwen/Qwen2.5-1.5B-Instruct`
- [manifests/vllm-medium.yaml](/home/bartr/vllm/manifests/vllm-medium.yaml) for `Qwen/Qwen2.5-3B-Instruct`
- [manifests/vllm-large.yaml](/home/bartr/vllm/manifests/vllm-large.yaml) for `Qwen/Qwen2.5-7B-Instruct-AWQ`

Because the node has only one GPU, deploy only one of these manifests at a time.

### Deploy the starter service

Apply exactly one manifest:

```bash
kubectl apply -f manifests/vllm-small.yaml
# or
kubectl apply -f manifests/vllm-medium.yaml
# or
kubectl apply -f manifests/vllm-large.yaml

kubectl -n vllm get pods -o wide
kubectl -n vllm rollout status deploy/vllm
```

The deployment uses:

- `runtimeClassName: nvidia`
- one GPU via `nvidia.com/gpu: 1`
- a `PersistentVolumeClaim` named `vllm-model-cache` for Hugging Face model files
- one shared internal `Service` named `vllm` on port `8000`
- `enableServiceLinks: false` to avoid the Kubernetes `VLLM_PORT` env var collision
- a `startupProbe` plus `Recreate` rollout strategy so first boot works reliably on a single GPU node
- exactly one model manifest should be applied at a time on a single 8 GB GPU

The cache claim requests `20Gi` from the `local-path` storage class so model weights survive pod restarts on this K3s node.

If your model download is rate-limited, add a Hugging Face token before starting the deployment:

```bash
kubectl -n vllm create secret generic huggingface-token \
	--from-literal=HF_TOKEN='<your-token>'
```

The starter manifest already reads `HF_TOKEN` from that secret when it exists.

### Choose which model to run

Use one of the three manifests depending on the tradeoff you want:

- `vllm-small.yaml`: fastest startup, lowest quality, more context headroom
- `vllm-medium.yaml`: balanced option using `Qwen/Qwen2.5-3B-Instruct`
- `vllm-large.yaml`: slowest startup, best quality on this 8 GB GPU, uses `Qwen/Qwen2.5-7B-Instruct-AWQ`

If you want to switch models later, apply a different manifest over the same `vllm` deployment and wait for rollout:

```bash
kubectl apply -f manifests/vllm-medium.yaml
kubectl -n vllm rollout status deploy/vllm
```

This keeps the same service name, same ingress route target, and same PVC cache.

Do not apply multiple model manifests and expect them to run side by side on this single GPU node. They manage the same `vllm` deployment and service on purpose.

### Model profiles

- `small`: `Qwen/Qwen2.5-1.5B-Instruct`, `--gpu-memory-utilization 0.85`, `--max-model-len 4096`
- `medium`: `Qwen/Qwen2.5-3B-Instruct`, `--gpu-memory-utilization 0.90`, `--max-model-len 4096`
- `large`: `Qwen/Qwen2.5-7B-Instruct-AWQ`, `--gpu-memory-utilization 0.95`, `--max-model-len 2048`

### Test the API

Get the node IP that is hosting the pod:

```bash
kubectl -n vllm get pods -o wide
```

This setup uses the Traefik ingress as the primary endpoint. The service itself stays internal to the cluster.

Create the basic-auth secret in the `vllm` namespace:

```bash
VLLM_BASIC_AUTH_USER=admin
VLLM_BASIC_AUTH_PASSWORD='change-me'
HASH=$(openssl passwd -apr1 "$VLLM_BASIC_AUTH_PASSWORD")
printf '%s:%s\n' "$VLLM_BASIC_AUTH_USER" "$HASH" > users
kubectl -n vllm create secret generic vllm-basic-auth --from-file=users=./users
rm -f users
```

Apply the Traefik middleware and ingress route:

```bash
kubectl apply -f manifests/vllm-ingress-basic-auth.yaml
```

The manifest uses the host `vllm.local`. Add a hosts-file entry on the client that will call the service:

```bash
NODE_IP=$(kubectl get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
echo "$NODE_IP vllm.local" | sudo tee -a /etc/hosts
```

Verify the server is up through ingress:

```bash
http -a admin:change-me http://vllm.local/health
http -a admin:change-me http://vllm.local/v1/models
```

These `http` examples are for interactive API checks. The helper script below uses `curl` and can call the local `cllm` cache service or another compatible endpoint.

Send a chat completion request with [scripts/ask](/home/bartr/vllm/scripts/ask):

`sudo ./scripts/base.sh` installs `glow`, so the script can render Markdown responses in the terminal.

```bash
./scripts/ask
```

`scripts/ask` is a thin wrapper around the Go binary at [cllm/cmd/ask](/home/bartr/vllm/cllm/cmd/ask). It runs both single-shot requests and concurrent benchmarks. By default it targets `http://localhost:8088` (the local `cllm` service); set `CLLM_URL` to point elsewhere.

For an in-cluster `cllm` deployment, use the manifests in [clusters/z01/cllm](/home/bartr/vllm/clusters/z01/cllm). They deploy the local image `cllm:0.1.0` with `imagePullPolicy: Never` and expose it through a dedicated Traefik entrypoint on port `8088`.

Local build and import flow:

```bash
cd /home/bartr/vllm/cllm
make import
```

Apply the cluster manifests:

```bash
kubectl apply -k /home/bartr/vllm/clusters/z01/cllm
kubectl -n kube-system rollout status deployment/traefik
kubectl -n cllm rollout status deployment/cllm
```

Then call `cllm` through the Traefik external IP on port `8088`:

```bash
curl -i http://192.168.68.63:8088/health
```

If `CLLM_TOKEN` is set, `ask` sends it as `Authorization: Bearer ...` for OpenAI-compatible endpoints. If you point `CLLM_URL` at `https://api.openai.com`, set `CLLM_MODEL` to a model you have access to, such as `gpt-4.1`.

You can also pass the user context directly:

```bash
CLLM_TOKEN='your-api-token' ./scripts/ask "Give me three uses for an edge-hosted LLM."
```

OpenAI example:

```bash
CLLM_URL='https://api.openai.com' \
CLLM_TOKEN='your-api-token' \
CLLM_MODEL='gpt-4.1' \
./scripts/ask "Give me three uses for an edge-hosted LLM."
```

Benchmark an OpenAI-compatible endpoint with the same tool:

```bash
./scripts/ask --bench 10 --duration 30s --prompt 'explain azure'
```

Bench mode runs a fixed number of concurrent workers, optionally ramped up over time, until any of `--duration`, `--count`, or `Ctrl-C` fires. Each completed request prints the worker thread number, returned completion tokens, TTFT, request duration, per-request tokens/sec, aggregate tokens/sec over the last 15 seconds, and cache hit/miss; a P50/P95/P99 summary report prints at the end.

Common bench shapes:

```bash
# Linear ramp from 1 to 50 workers over 30s, then run for 2 minutes
./scripts/ask --bench 50 --ramp 1:50 --ramp-duration 30s --duration 2m --prompt 'hi'

# Walk a YAML prompt list once across 8 workers
./scripts/ask --bench 8 --file prompts.yaml

# Loop the list 5 times, or pick randomly per request
./scripts/ask --bench 8 --file prompts.yaml --loop 5
./scripts/ask --bench 8 --file prompts.yaml --random --duration 1m
```

YAML prompt file format:

```yaml
- prompt: "Explain Azure"
  dsl: "profile=fast"
- prompt: "What is Kubernetes?"
- prompt: "Compare AWS and GCP"
  dsl: "tps=20"
```

Use `./scripts/ask --help` for the full flag list.

On the first startup, expect a delay while vLLM pulls the container image, downloads model weights, compiles kernels, and captures CUDA graphs. On this 8 GB GPU, the large 7B AWQ profile took about 150 seconds to download weights and about 56 seconds to finish engine initialization after that.

The 1.5B profile should start faster and leave more room for context length, while the 3B profile is a good middle ground. The 7B AWQ profile should produce the strongest answers of the three on this hardware.

### Offline and edge operation

Once the vLLM pod is running and the model has already been cached in `vllm-model-cache`, the service can continue running without internet access.

For offline operation, make sure these artifacts are already local on the node:

- the pinned `vllm/vllm-openai:v0.19.1` container image has already been pulled
- the selected model has already been downloaded into `/cache/huggingface`
- the `vllm-model-cache` PVC is still attached and contains the cached model files

Internet access is still needed when:

- you start from a fresh node that does not have the vLLM image yet
- you switch to a model that is not already cached
- you clear or lose the PVC-backed Hugging Face cache
- you intentionally pull a newer container image or different model

### Tune after the first successful run

Once the starter deployment works, the next tuning knobs are:

- change `--model` to match your GPU capacity and use case
- if you move away from an 8 GB GPU, revisit `--model`, `--max-model-len`, and `--gpu-memory-utilization` together
- increase or decrease `--max-model-len` based on VRAM headroom
- set `--gpu-memory-utilization` higher or lower if startup fails or KV cache is too small
- increase the PVC size if you want to keep multiple larger models cached on disk
- expose the service through Ingress or a local gateway once the API is stable

### Recommended order of operations

1. Deploy the starter manifest unchanged.
2. Choose one of `vllm-small.yaml`, `vllm-medium.yaml`, or `vllm-large.yaml`.
3. Confirm `kubectl logs -n vllm deploy/vllm` shows the model loaded successfully.
4. Test the authenticated ingress endpoint with `/v1/chat/completions`.

## Add Observability

The repo now includes a Flux-managed, in-cluster observability stack for K3s:

- `Prometheus Operator` for `ServiceMonitor`-driven scrape management
- `Prometheus` for time-series collection
- `Grafana` for dashboards
- `vLLM` metrics scraped from the `vllm` service on `/metrics`
- `cLLM` is prepared for scraping through a `ServiceMonitor`, but its `/metrics` endpoint still needs to be implemented before that target will go healthy

This stack is tuned for the same local, single-node setup as the vLLM manifests. It keeps retention short, uses modest resource requests, and exposes the UIs through dedicated Traefik entrypoints so you can inspect the stack from the LAN without adding ingress auth yet.

The active `vllm` service is included in Prometheus scraping through the Flux-managed overlay in [clusters/z01/prometheus](/home/bartr/vllm/clusters/z01/prometheus). The pinned vLLM image exposes Prometheus metrics on the same API port at `/metrics`, so the stack can collect request, latency, token, cache, and HTTP server metrics from whichever model profile is deployed.

### Current access URLs

With the current Traefik service IP on this node, the exact URLs are:

- vLLM: `http://192.168.68.63:8000`
- cLLM: `http://192.168.68.63:8088`
- Prometheus: `http://192.168.68.63:9090`
- Grafana: `http://192.168.68.63:3000`

Useful direct checks:

```bash
curl http://192.168.68.63:8000/health
curl http://192.168.68.63:8088/health
curl http://192.168.68.63:9090/-/healthy
curl http://192.168.68.63:3000/api/health
```

### Install the observability stack

Apply the Flux-managed overlays directly:

```bash
kubectl apply -k /home/bartr/vllm/clusters/z01/prometheus/operator
kubectl apply -k /home/bartr/vllm/clusters/z01/prometheus
kubectl apply -k /home/bartr/vllm/clusters/z01/grafana
kubectl apply -k /home/bartr/vllm/clusters/z01/traefik
```

Then wait for the monitoring workloads:

```bash
kubectl -n monitoring rollout status deployment/prometheus-operator
kubectl -n monitoring rollout status deployment/grafana
kubectl -n monitoring rollout status statefulset/prometheus-prometheus
kubectl -n kube-system rollout status deployment/traefik
```

Import the repo-managed Grafana dashboards through the Grafana API after Grafana is ready. The dashboard JSON source of truth now lives under [clusters/z01/grafana/dashboards](/home/bartr/vllm/clusters/z01/grafana/dashboards), so dashboards remain editable and saveable in the Grafana UI after import.

Run either the local importer:

```bash
./clusters/z01/grafana/scripts/import-dashboards.sh
```

Or the in-cluster Job:

```bash
./clusters/z01/grafana/scripts/run-dashboard-import-job.sh
```

Once the rollout completes, the current local endpoints are:

- Grafana: `http://192.168.68.63:3000`
- Prometheus: `http://192.168.68.63:9090`

Grafana uses the admin credentials stored in [clusters/z01/grafana/secret.yaml](/home/bartr/vllm/clusters/z01/grafana/secret.yaml).

### What you get first

After importing the repo-provided dashboards from [clusters/z01/grafana/dashboards](/home/bartr/vllm/clusters/z01/grafana/dashboards), Grafana includes:

- `GPU / GPU Overview`
- `GPU / GPU Power And Thermals`
- `cLLM / cLLM Overview`
- `vLLM / vLLM Overview`
- `vLLM / vLLM Latency And HTTP`

Use the custom dashboards for:

- GPU utilization by pod
- GPU framebuffer memory used by pod
- GPU power draw and temperature
- running and queued vLLM requests
- vLLM KV cache usage
- request success rate, token throughput, and p95 TTFT / end-to-end latency
- HTTP request rate for chat and completion endpoints

### Verify the stack

Once the installer finishes:

```bash
kubectl -n monitoring get pods
kubectl -n monitoring get servicemonitors
```

Check Prometheus target health at `http://<node-ip>:30090/targets` and confirm you can see healthy scrape targets for:

For the current node IP, that is:

`http://192.168.68.63:9090/targets`

- `vllm`

The `cllm` `ServiceMonitor` object exists, but the target will remain down until `cllm` serves a real `/metrics` endpoint.

You can also confirm the key metric families directly in Prometheus:

```bash
# GPU metrics
DCGM_FI_DEV_GPU_UTIL

# OpenCost metrics
node_total_hourly_cost
node_gpu_hourly_cost

# vLLM metrics
vllm:num_requests_running
vllm:request_success_total
vllm:time_to_first_token_seconds_bucket
```

### vLLM application metrics

The pinned `vllm/vllm-openai:v0.19.1` image exposes Prometheus metrics at `/metrics` on the same API service port, so the current setup now scrapes request, token, cache, and HTTP server metrics from the active `vllm` workload. That adds application-level latency and throughput visibility on top of the node, GPU, and infrastructure-cost dashboards.

## Reset the cluster

```bash

sudo /usr/local/bin/k3s-uninstall.sh
sudo rm -f ~/.kube/config
sudo rm -rf /etc/rancher/k3s

## agressive wipe
#sudo rm -rf /var/lib/rancher /var/lib/kubelet /etc/cni /var/lib/cni

```

### Create a new cluster

```bash

curl -sfL https://get.k3s.io | sh -
./scripts/config.sh
kubectl get nodes

```

## Support

This project uses GitHub Issues to track bugs and feature requests. Please search the existing issues before filing new issues to avoid duplicates.  For new issues, file your bug or feature request as a new issue.

## Contributing

This project welcomes contributions and suggestions.  Most contributions require you to agree to a Contributor License Agreement (CLA) declaring that you have the right to, and actually do, grant us the rights to use your contribution. For details, visit <https://cla.opensource.microsoft.com>.

When you submit a pull request, a CLA bot will automatically determine whether you need to provide a CLA and decorate the PR appropriately (e.g., status check, comment). Simply follow the instructions provided by the bot. You will only need to do this once across all repos using our CLA.

This project has adopted the [Microsoft Open Source Code of Conduct](https://opensource.microsoft.com/codeofconduct/). For more information see the [Code of Conduct FAQ](https://opensource.microsoft.com/codeofconduct/faq/) or contact [opencode@microsoft.com](mailto:opencode@microsoft.com) with any additional questions or comments.

## Trademarks

This project may contain trademarks or logos for projects, products, or services. Authorized use of Microsoft trademarks or logos is subject to and must follow [Microsoft's Trademark & Brand Guidelines](https://www.microsoft.com/en-us/legal/intellectualproperty/trademarks/usage/general). Use of Microsoft trademarks or logos in modified versions of this project must not cause confusion or imply Microsoft sponsorship. Any use of third-party trademarks or logos are subject to those third-party's policies.
