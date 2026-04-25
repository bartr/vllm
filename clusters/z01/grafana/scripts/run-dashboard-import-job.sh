#!/bin/sh

set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
MANIFEST_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)

kubectl -n monitoring delete job grafana-dashboard-import --ignore-not-found
kubectl apply -f "$MANIFEST_DIR/dashboard-import-job.yaml"
kubectl -n monitoring wait --for=condition=complete --timeout=5m job/grafana-dashboard-import
kubectl -n monitoring logs job/grafana-dashboard-import
