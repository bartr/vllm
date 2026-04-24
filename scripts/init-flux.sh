#!/bin/bash

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$script_dir" || exit 1

cd ../clusters/z01/flux-system
kubectl apply -f components.yaml
kubectl apply -f source.yaml

echo ''
echo 'Waiting for Flux to start - this can take up to 5 minutes'
echo ''
kubectl wait --namespace flux-system --for=condition=Ready pod --all --timeout=5m

kubectl apply -f listeners/nvidia-plugin.yaml
flux reconcile kustomization nvidia-plugin

kubectl apply -f listeners/vllm.yaml
flux reconcile kustomization vllm

kubectl apply -f listeners/traefik.yaml
flux reconcile kustomization traefik

kubectl apply -f listeners/prometheus-operator.yaml
flux reconcile kustomization prometheus-operator
echo ''
echo 'Waiting for Prometheus operator to start - this can take up to 10 minutes'
echo ''
kubectl wait --namespace monitoring --for=condition=Ready pod --all --timeout=10m

kubectl apply -f listeners/prometheus.yaml
flux reconcile kustomization prometheus
echo ''
echo 'Waiting for Prometheus to start - this can take up to 5 minutes'
echo ''
kubectl wait --namespace monitoring --for=condition=Ready pod --all --timeout=5m

kubectl apply -f listeners/dcgm-exporter.yaml
flux reconcile kustomization dcgm-exporter

kubectl apply -f listeners/grafana.yaml
flux reconcile kustomization grafana
echo ''
echo 'Waiting for Grafana to start - this can take up to 5 minutes'
echo ''
kubectl wait --namespace monitoring --for=condition=Ready pod --all --timeout=5m

echo ''
echo 'Waiting for vLLM pod to start - this can take up to 20 minutes'
echo ''
kubectl wait --namespace vllm --for=condition=Ready pod --all --timeout=20m

kubectl apply -f flux-listeners.yaml
flux reconcile kustomization flux-listeners

kubectl wait --namespace monitoring --for=condition=Ready pod --all --timeout=5m
kubectl wait --namespace cllm --for=condition=Ready pod --all --timeout=5m
