#!/bin/bash

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

NAMESPACE="${NAMESPACE:-cllm}"
CLAIM_NAME="${CLAIM_NAME:-cllm-storage}"
EXPORT_POD_NAME="${EXPORT_POD_NAME:-cllm-cache-export}"
CACHE_FILE_PATH="${CACHE_FILE_PATH:-/var/lib/cllm/cache.json}"
OUTPUT_FILE="${OUTPUT_FILE:-$REPO_DIR/cllm/cache.json}"
APP_LABEL="${APP_LABEL:-app=cllm}"

cleanup() {
    kubectl -n "$NAMESPACE" delete pod "$EXPORT_POD_NAME" --ignore-not-found >/dev/null 2>&1 || true
}

trap cleanup EXIT

if ! kubectl -n "$NAMESPACE" get pvc "$CLAIM_NAME" >/dev/null 2>&1; then
    echo "PVC $CLAIM_NAME was not found in namespace $NAMESPACE"
    echo "Apply the cllm manifests first: kubectl apply -k $REPO_DIR/clusters/z01/cllm"
    exit 1
fi

NODE_NAME="$(kubectl -n "$NAMESPACE" get pod -l "$APP_LABEL" -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null || true)"
if [ -z "$NODE_NAME" ]; then
    echo "Could not determine the cllm pod node from label $APP_LABEL in namespace $NAMESPACE"
    exit 1
fi

echo "Creating helper pod $EXPORT_POD_NAME in namespace $NAMESPACE"

cleanup

kubectl -n "$NAMESPACE" apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $EXPORT_POD_NAME
spec:
  nodeName: $NODE_NAME
  restartPolicy: Never
  containers:
    - name: export
      image: busybox:1.36
      command: ["sh", "-c", "sleep 3600"]
      volumeMounts:
        - name: storage
          mountPath: /var/lib/cllm
  volumes:
    - name: storage
      persistentVolumeClaim:
        claimName: $CLAIM_NAME
EOF

kubectl -n "$NAMESPACE" wait --for=condition=Ready "pod/$EXPORT_POD_NAME" --timeout=120s >/dev/null

mkdir -p "$(dirname -- "$OUTPUT_FILE")"
kubectl -n "$NAMESPACE" exec "$EXPORT_POD_NAME" -- cat "$CACHE_FILE_PATH" > "$OUTPUT_FILE"

echo "Exported $CACHE_FILE_PATH to $OUTPUT_FILE"
