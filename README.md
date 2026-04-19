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

The script installs chart version `0.19.0` with automatic GPU node labeling enabled and prints the GPU capacity discovered on each node.

```bash

./scripts/install-nvidia-device-plugin.sh --clean

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

kubectl -n llm get pods -o wide
kubectl -n llm rollout status deploy/vllm
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
kubectl -n llm create secret generic huggingface-token \
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
kubectl -n llm rollout status deploy/vllm
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
kubectl -n llm get pods -o wide
```

This setup uses the Traefik ingress as the primary endpoint. The service itself stays internal to the cluster.

Create the basic-auth secret in the `llm` namespace:

```bash
VLLM_BASIC_AUTH_USER=admin
VLLM_BASIC_AUTH_PASSWORD='change-me'
HASH=$(openssl passwd -apr1 "$VLLM_BASIC_AUTH_PASSWORD")
printf '%s:%s\n' "$VLLM_BASIC_AUTH_USER" "$HASH" > users
kubectl -n llm create secret generic vllm-basic-auth --from-file=users=./users
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

These `http` examples are for interactive API checks. The helper script below uses `curl` and supports ingress basic auth with `VLLM_AUTH`.

Send a chat completion request with [ask.sh](/home/bartr/vllm/ask.sh):

`sudo ./scripts/base.sh` installs `glow`, so the script can render Markdown responses in the terminal.

```bash
./ask.sh
```

If `VLLM_AUTH` is not already set, `ask.sh` prompts for the ingress username and password interactively.

You can also pass the user context directly:

```bash
VLLM_AUTH='admin:change-me' ./ask.sh "Give me three uses for an edge-hosted LLM."
```

It will:

- set `VLLM_MODEL` automatically if it is not already set
- prompt for the user context
- send the request to the active model
- prompt for ingress credentials when `VLLM_AUTH` is not set and the script is run interactively
- authenticate to the ingress when `VLLM_AUTH` is set
- render the assistant content with `glow`
- print elapsed time and token usage on separate lines after the rendered response

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
3. Confirm `kubectl logs -n llm deploy/vllm` shows the model loaded successfully.
4. Test the authenticated ingress endpoint with `/v1/chat/completions`.

## Add Observability

The repo now includes a first-phase, in-cluster observability stack for K3s:

- `Prometheus` for time-series collection
- `Grafana` for dashboards
- `node-exporter` and `kube-state-metrics` for node and workload basics
- `NVIDIA DCGM exporter` for GPU utilization, memory, temperature, and power metrics
- `OpenCost` for infrastructure cost estimation using local custom pricing

This stack is tuned for the same local, single-node setup as the vLLM manifests. It keeps retention short, uses modest resource requests, and exposes the UIs with NodePorts so you can inspect the stack from the LAN without adding ingress auth yet.

The active `vllm` service is now included in Prometheus scraping through [manifests/vllm-servicemonitor.yaml](/home/bartr/vllm/manifests/vllm-servicemonitor.yaml). The pinned vLLM image exposes Prometheus metrics on the same API port at `/metrics`, so the stack can collect request, latency, token, cache, and HTTP server metrics from whichever model profile is deployed.

### Install the observability stack

The installer follows the same pattern as the existing GPU support scripts and will install the monitoring stack, the GPU exporter, OpenCost, and the repo-provided Grafana dashboards:

```bash
./scripts/install-observability.sh
```

It installs these charts into the `monitoring` namespace using the in-repo values files:

- [helm/kube-prometheus-stack-values.yaml](/home/bartr/vllm/helm/kube-prometheus-stack-values.yaml)
- [helm/dcgm-exporter-values.yaml](/home/bartr/vllm/helm/dcgm-exporter-values.yaml)
- [helm/opencost-values.yaml](/home/bartr/vllm/helm/opencost-values.yaml)

The script prints the node IP and these local endpoints when the rollout completes:

- Grafana: `http://<node-ip>:30080`
- Prometheus: `http://<node-ip>:30090`
- OpenCost: `http://<node-ip>:30081`

Grafana uses `admin` / `change-me` by default in this local-first setup. Change it in [helm/kube-prometheus-stack-values.yaml](/home/bartr/vllm/helm/kube-prometheus-stack-values.yaml) before installation if you do not want the default local password.

### What you get first

Out of the box, Grafana includes the kube-prometheus dashboards plus three repo-provided dashboards:

- [manifests/observability-grafana-dashboards.yaml](/home/bartr/vllm/manifests/observability-grafana-dashboards.yaml) provisions a `GPU Observability` dashboard
- [manifests/observability-grafana-dashboards.yaml](/home/bartr/vllm/manifests/observability-grafana-dashboards.yaml) provisions a `Cost Observability` dashboard
- [manifests/vllm-observability-dashboard.yaml](/home/bartr/vllm/manifests/vllm-observability-dashboard.yaml) provisions a `vLLM Observability` dashboard

Use the default dashboards for node CPU, memory, disk, pod health, and Kubernetes workload status. Use the custom dashboards for:

- GPU utilization by pod
- GPU framebuffer memory used by pod
- GPU power draw and temperature
- cluster hourly cost
- GPU, CPU, RAM, and persistent-volume hourly cost
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

- `monitoring-prometheus-node-exporter`
- `monitoring-kube-state-metrics`
- `dcgm-dcgm-exporter`
- `opencost`
- `vllm`

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

### Pricing assumptions

OpenCost is configured with a simple local custom pricing model in [helm/opencost-values.yaml](/home/bartr/vllm/helm/opencost-values.yaml). The default values are intentionally easy to edit for a laptop or edge node:

- CPU: `$0.03` per core-hour
- RAM: `$0.004` per GiB-hour
- GPU: `$0.35` per GPU-hour
- storage: `$0.0001` per GiB-hour

Adjust those values to match your own electricity, amortization, or internal chargeback model.

### vLLM application metrics

The pinned `vllm/vllm-openai:v0.19.1` image exposes Prometheus metrics at `/metrics` on the same API service port, so the current setup now scrapes request, token, cache, and HTTP server metrics from the active `vllm` workload. That adds application-level latency and throughput visibility on top of the node, GPU, and infrastructure-cost dashboards.

## Support

This project uses GitHub Issues to track bugs and feature requests. Please search the existing issues before filing new issues to avoid duplicates.  For new issues, file your bug or feature request as a new issue.

## Contributing

This project welcomes contributions and suggestions.  Most contributions require you to agree to a Contributor License Agreement (CLA) declaring that you have the right to, and actually do, grant us the rights to use your contribution. For details, visit <https://cla.opensource.microsoft.com>.

When you submit a pull request, a CLA bot will automatically determine whether you need to provide a CLA and decorate the PR appropriately (e.g., status check, comment). Simply follow the instructions provided by the bot. You will only need to do this once across all repos using our CLA.

This project has adopted the [Microsoft Open Source Code of Conduct](https://opensource.microsoft.com/codeofconduct/). For more information see the [Code of Conduct FAQ](https://opensource.microsoft.com/codeofconduct/faq/) or contact [opencode@microsoft.com](mailto:opencode@microsoft.com) with any additional questions or comments.

## Trademarks

This project may contain trademarks or logos for projects, products, or services. Authorized use of Microsoft trademarks or logos is subject to and must follow [Microsoft's Trademark & Brand Guidelines](https://www.microsoft.com/en-us/legal/intellectualproperty/trademarks/usage/general). Use of Microsoft trademarks or logos in modified versions of this project must not cause confusion or imply Microsoft sponsorship. Any use of third-party trademarks or logos are subject to those third-party's policies.
