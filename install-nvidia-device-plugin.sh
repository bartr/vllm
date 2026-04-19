#!/bin/bash

set -euo pipefail

cd "$(dirname "$BASH_SOURCE")"
dir=$(pwd)

release_name="nvdp"
namespace="nvidia-device-plugin"
chart="nvdp/nvidia-device-plugin"
chart_version="0.19.0"
values_file="$dir/helm/nvidia-device-plugin-values.yaml"
clean_install="false"

if [[ "${1:-}" == "--clean" ]]; then
    clean_install="true"
fi

if ! command -v helm >/dev/null 2>&1; then
    echo "helm is required"
    exit 1
fi

if ! command -v kubectl >/dev/null 2>&1; then
    echo "kubectl is required"
    exit 1
fi

if [[ ! -f "$values_file" ]]; then
    echo "missing values file: $values_file"
    exit 1
fi

helm repo add nvdp https://nvidia.github.io/k8s-device-plugin >/dev/null 2>&1 || true
helm repo update nvdp

if [[ "$clean_install" == "true" ]]; then
    helm uninstall -n "$namespace" "$release_name" >/dev/null 2>&1 || true
    kubectl delete namespace "$namespace" --ignore-not-found=true >/dev/null
fi

helm upgrade -i "$release_name" "$chart" \
    --namespace "$namespace" \
    --create-namespace \
    --version "$chart_version" \
    -f "$values_file"

kubectl -n "$namespace" rollout status daemonset/${release_name}-node-feature-discovery-worker --timeout=180s
kubectl -n "$namespace" rollout status daemonset/${release_name}-nvidia-device-plugin --timeout=180s
kubectl -n "$namespace" rollout status daemonset/${release_name}-nvidia-device-plugin-gpu-feature-discovery --timeout=180s

echo
echo "GPU node summary:"
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.allocatable.nvidia\.com/gpu}{"\t"}{.metadata.labels.nvidia\.com/gpu\.product}{"\n"}{end}'
