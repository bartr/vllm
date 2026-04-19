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

## Support

This project uses GitHub Issues to track bugs and feature requests. Please search the existing issues before filing new issues to avoid duplicates.  For new issues, file your bug or feature request as a new issue.

## Contributing

This project welcomes contributions and suggestions.  Most contributions require you to agree to a Contributor License Agreement (CLA) declaring that you have the right to, and actually do, grant us the rights to use your contribution. For details, visit <https://cla.opensource.microsoft.com>.

When you submit a pull request, a CLA bot will automatically determine whether you need to provide a CLA and decorate the PR appropriately (e.g., status check, comment). Simply follow the instructions provided by the bot. You will only need to do this once across all repos using our CLA.

This project has adopted the [Microsoft Open Source Code of Conduct](https://opensource.microsoft.com/codeofconduct/). For more information see the [Code of Conduct FAQ](https://opensource.microsoft.com/codeofconduct/faq/) or contact [opencode@microsoft.com](mailto:opencode@microsoft.com) with any additional questions or comments.

## Trademarks

This project may contain trademarks or logos for projects, products, or services. Authorized use of Microsoft trademarks or logos is subject to and must follow [Microsoft's Trademark & Brand Guidelines](https://www.microsoft.com/en-us/legal/intellectualproperty/trademarks/usage/general). Use of Microsoft trademarks or logos in modified versions of this project must not cause confusion or imply Microsoft sponsorship. Any use of third-party trademarks or logos are subject to those third-party's policies.
