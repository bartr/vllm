# vLLM on K3s on Ubuntu


## Clone this Repo

```powershel

git clone https://github.com/bartr/vllm

```

## Install components

```bash

sudo ./base.sh

./config.sh

```

## Install NVIDIA GPU Support For K3s

K3s needs the NVIDIA container toolkit on the host before the device plugin can start pods with `runtimeClassName: nvidia`.

```bash

sudo ./install-nvidia-container-toolkit.sh

```

The script installs chart version `0.19.0` with automatic GPU node labeling enabled and prints the GPU capacity discovered on each node.

```bash

./install-nvidia-device-plugin.sh --clean

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
- one shared `NodePort` service on port `30080`
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

This keeps the same service name, same NodePort, and same PVC cache.

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

Then call the active model directly. If you are running the request from the K3s host, the service is reachable at `http://localhost:30080`.

Verify the server is up:

```bash
http http://localhost:30080/health
http http://localhost:30080/v1/models
```

These `http` examples are for interactive API checks. The helper script below uses `curl` and does not depend on `httpie`.

Send a chat completion request with [ask.sh](/home/bartr/vllm/ask.sh):

`sudo ./base.sh` installs `glow`, so the script can render Markdown responses in the terminal.

```bash
./ask.sh
```

You can also pass the user context directly:

```bash
./ask.sh "Give me three uses for an edge-hosted LLM."
```

It will:

- set `VLLM_MODEL` automatically if it is not already set
- prompt for the user context
- send the request to the active model
- render the assistant content with `glow`
- print elapsed time and token usage on separate lines after the rendered response

On the first startup, expect a delay while vLLM pulls the container image, downloads model weights, compiles kernels, and captures CUDA graphs. On this 8 GB GPU, the large 7B AWQ profile took about 150 seconds to download weights and about 56 seconds to finish engine initialization after that.

The 1.5B profile should start faster and leave more room for context length, while the 3B profile is a good middle ground. The 7B AWQ profile should produce the strongest answers of the three on this hardware.

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
4. Test the shared `NodePort` endpoint with `/v1/chat/completions`.

## Support

This project uses GitHub Issues to track bugs and feature requests. Please search the existing issues before filing new issues to avoid duplicates.  For new issues, file your bug or feature request as a new issue.

## Contributing

This project welcomes contributions and suggestions.  Most contributions require you to agree to a Contributor License Agreement (CLA) declaring that you have the right to, and actually do, grant us the rights to use your contribution. For details, visit <https://cla.opensource.microsoft.com>.

When you submit a pull request, a CLA bot will automatically determine whether you need to provide a CLA and decorate the PR appropriately (e.g., status check, comment). Simply follow the instructions provided by the bot. You will only need to do this once across all repos using our CLA.

This project has adopted the [Microsoft Open Source Code of Conduct](https://opensource.microsoft.com/codeofconduct/). For more information see the [Code of Conduct FAQ](https://opensource.microsoft.com/codeofconduct/faq/) or contact [opencode@microsoft.com](mailto:opencode@microsoft.com) with any additional questions or comments.

## Trademarks

This project may contain trademarks or logos for projects, products, or services. Authorized use of Microsoft trademarks or logos is subject to and must follow [Microsoft's Trademark & Brand Guidelines](https://www.microsoft.com/en-us/legal/intellectualproperty/trademarks/usage/general). Use of Microsoft trademarks or logos in modified versions of this project must not cause confusion or imply Microsoft sponsorship. Any use of third-party trademarks or logos are subject to those third-party's policies.
