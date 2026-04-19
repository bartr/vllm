#!/bin/bash

set -euo pipefail

cd "$(dirname "$BASH_SOURCE")/.."
dir=$(pwd)

namespace="monitoring"
monitoring_release="monitoring"
dcgm_release="dcgm"
opencost_release="opencost"

monitoring_values="$dir/helm/kube-prometheus-stack-values.yaml"
dcgm_values="$dir/helm/dcgm-exporter-values.yaml"
opencost_values="$dir/helm/opencost-values.yaml"
dashboards_manifest="$dir/manifests/observability-grafana-dashboards.yaml"
vllm_dashboards_manifest="$dir/manifests/vllm-observability-dashboard.yaml"
vllm_servicemonitor_manifest="$dir/manifests/vllm-servicemonitor.yaml"

for dependency in helm kubectl; do
    if ! command -v "$dependency" >/dev/null 2>&1; then
        echo "$dependency is required"
        exit 1
    fi
done

for file in "$monitoring_values" "$dcgm_values" "$opencost_values" "$dashboards_manifest" "$vllm_dashboards_manifest" "$vllm_servicemonitor_manifest"; do
    if [[ ! -f "$file" ]]; then
        echo "missing required file: $file"
        exit 1
    fi
done

helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
helm repo add gpu-helm-charts https://nvidia.github.io/dcgm-exporter/helm-charts >/dev/null 2>&1 || true
helm repo add opencost https://opencost.github.io/opencost-helm-chart >/dev/null 2>&1 || true
helm repo update >/dev/null

helm upgrade -i "$monitoring_release" prometheus-community/kube-prometheus-stack \
    --namespace "$namespace" \
    --create-namespace \
    -f "$monitoring_values"

kubectl -n "$namespace" rollout status deployment/monitoring-grafana --timeout=300s
kubectl -n "$namespace" rollout status deployment/monitoring-kube-state-metrics --timeout=300s
kubectl -n "$namespace" rollout status deployment/monitoring-operator --timeout=300s
kubectl -n "$namespace" rollout status daemonset/monitoring-prometheus-node-exporter --timeout=300s
kubectl -n "$namespace" rollout status statefulset/prometheus-monitoring-prometheus --timeout=300s

helm upgrade -i "$dcgm_release" gpu-helm-charts/dcgm-exporter \
    --namespace "$namespace" \
    -f "$dcgm_values"

kubectl -n "$namespace" rollout status daemonset/dcgm-dcgm-exporter --timeout=300s

helm upgrade -i "$opencost_release" opencost/opencost \
    --namespace "$namespace" \
    -f "$opencost_values"

kubectl -n "$namespace" rollout status deployment/opencost --timeout=300s
kubectl apply -f "$vllm_servicemonitor_manifest"
kubectl apply -f "$dashboards_manifest"
kubectl apply -f "$vllm_dashboards_manifest"

node_ip=$(kubectl get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')

echo
echo "Observability endpoints:"
echo "Grafana:    http://$node_ip:30080  (admin / change-me)"
echo "Prometheus: http://$node_ip:30090"
echo "OpenCost:   http://$node_ip:30081"
echo
echo "Prometheus targets:"
echo "- http://$node_ip:30090/targets"
echo "- http://$node_ip:30090/graph?g0.expr=DCGM_FI_DEV_GPU_UTIL"
echo "- http://$node_ip:30090/graph?g0.expr=sum(vllm:num_requests_running)"
