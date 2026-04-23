#!/bin/bash

helm repo add dcgm-exporter https://nvidia.github.io/dcgm-exporter/helm-charts
helm repo update

helm upgrade -i dcgm-exporter dcgm-exporter/dcgm-exporter \
  --namespace monitoring \
  --create-namespace \
  --version 4.8.1 \
  --set runtimeClassName=nvidia \
  --set kubernetes.enablePodLabels=true \
  --set serviceMonitor.enabled=true \
  --set serviceMonitor.interval=15s \
  --set serviceMonitor.additionalLabels.monitoring\\.coreos\\.com/instance=prometheus \
  --set resources.requests.cpu=100m \
  --set resources.requests.memory=256Mi \
  --set resources.limits.cpu=200m \
  --set resources.limits.memory=512Mi
