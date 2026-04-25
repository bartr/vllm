#!/bin/sh

set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
DASHBOARDS_ROOT=${DASHBOARDS_ROOT:-"$SCRIPT_DIR/../dashboards"}
GRAFANA_URL=${GRAFANA_URL:-http://127.0.0.1:3000}
GRAFANA_NAMESPACE=${GRAFANA_NAMESPACE:-monitoring}
GRAFANA_SECRET_NAME=${GRAFANA_SECRET_NAME:-grafana-admin}
TARGET_FOLDER_UID=${TARGET_FOLDER_UID:-gpu}
TARGET_FOLDER_TITLE=${TARGET_FOLDER_TITLE:-GPU}
STALE_DASHBOARD_UIDS=${STALE_DASHBOARD_UIDS:-"gpu-power-thermals vllm-latency-http"}
STALE_FOLDER_TITLES=${STALE_FOLDER_TITLES:-"cLLM vLLM"}

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

require_command curl
require_command python3

if [ -z "${GRAFANA_USER:-}" ] || [ -z "${GRAFANA_PASSWORD:-}" ]; then
  require_command kubectl
  GRAFANA_USER=$(kubectl -n "$GRAFANA_NAMESPACE" get secret "$GRAFANA_SECRET_NAME" -o jsonpath='{.data.admin-user}' | base64 -d)
  GRAFANA_PASSWORD=$(kubectl -n "$GRAFANA_NAMESPACE" get secret "$GRAFANA_SECRET_NAME" -o jsonpath='{.data.admin-password}' | base64 -d)
fi

export GRAFANA_USER
export GRAFANA_PASSWORD

TMP_DIR=$(mktemp -d)
CLEANED_PROVISIONING=0
cleanup() {
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT INT TERM

clear_provisioning_metadata() {
  if [ "$CLEANED_PROVISIONING" = "1" ]; then
    return 0
  fi

  if ! command -v kubectl >/dev/null 2>&1; then
    echo "cannot clear Grafana provisioning metadata automatically because kubectl is not available" >&2
    return 1
  fi

  cat <<'YAML' | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: grafana-dashboard-provisioning-cleanup
  namespace: monitoring
spec:
  restartPolicy: Never
  containers:
    - name: cleanup
      image: python:3.12-alpine
      command:
        - /bin/sh
        - -ec
        - |
          python3 - <<'PY'
          import sqlite3
          conn = sqlite3.connect('/var/lib/grafana/grafana.db')
          cur = conn.cursor()
          cur.execute('DELETE FROM dashboard_provisioning')
          deleted = cur.rowcount
          conn.commit()
          conn.close()
          print(f'cleared {deleted} dashboard_provisioning rows')
          PY
      volumeMounts:
        - name: storage
          mountPath: /var/lib/grafana
  volumes:
    - name: storage
      persistentVolumeClaim:
        claimName: grafana-storage
YAML

  kubectl -n monitoring wait --for=jsonpath='{.status.phase}'=Succeeded --timeout=120s pod/grafana-dashboard-provisioning-cleanup >/dev/null
  kubectl -n monitoring logs grafana-dashboard-provisioning-cleanup >&2
  kubectl -n monitoring delete pod grafana-dashboard-provisioning-cleanup --ignore-not-found >/dev/null 2>&1 || true
  CLEANED_PROVISIONING=1
}

request() {
  method=$1
  url=$2
  body_file=$3
  response_file=$4

  if [ -n "$body_file" ]; then
    curl \
      -sS \
      -u "$GRAFANA_USER:$GRAFANA_PASSWORD" \
      -H 'Content-Type: application/json' \
      -X "$method" \
      --data-binary "@$body_file" \
      -o "$response_file" \
      -w '%{http_code}' \
      "$url"
  else
    curl \
      -sS \
      -u "$GRAFANA_USER:$GRAFANA_PASSWORD" \
      -H 'Content-Type: application/json' \
      -X "$method" \
      -o "$response_file" \
      -w '%{http_code}' \
      "$url"
  fi
}

folder_title() {
  case "$1" in
    cllm) printf '%s' 'cLLM' ;;
    gpu) printf '%s' 'GPU' ;;
    vllm) printf '%s' 'vLLM' ;;
    *) printf '%s' "$1" ;;
  esac
}

find_folder_uid_by_title() {
  title=$1
  search_response="$TMP_DIR/folder-search-title-$title.json"
  search_code=$(request GET "$GRAFANA_URL/api/search?type=dash-folder&query=$title" "" "$search_response")
  if [ "$search_code" != "200" ]; then
    echo "failed to search for folder $title" >&2
    cat "$search_response" >&2
    exit 1
  fi

  python3 - <<'PY' "$search_response" "$title"
import json
import sys

response_path, title = sys.argv[1:3]
with open(response_path, encoding='utf-8') as handle:
    items = json.load(handle)

for item in items:
    if item.get('type') == 'dash-folder' and item.get('title') == title:
        print(item.get('uid', ''))
        break
PY
}

ensure_folder() {
  uid=$1
  title=$2
  response_file="$TMP_DIR/folder-$uid.json"
  code=$(request GET "$GRAFANA_URL/api/folders/$uid" "" "$response_file")

  case "$code" in
    200)
      printf '%s\n' "$uid"
      return 0
      ;;
    404)
      search_response="$TMP_DIR/folder-search-$uid.json"
      search_code=$(request GET "$GRAFANA_URL/api/search?type=dash-folder&query=$title" "" "$search_response")
      if [ "$search_code" != "200" ]; then
        echo "failed to search for folder $title" >&2
        cat "$search_response" >&2
        exit 1
      fi

      existing_uid=$(python3 - <<'PY' "$search_response" "$title"
import json
import sys

response_path, title = sys.argv[1:3]
with open(response_path, encoding='utf-8') as handle:
    items = json.load(handle)

for item in items:
    if item.get('type') == 'dash-folder' and item.get('title') == title:
        print(item.get('uid', ''))
        break
PY
)

      if [ -n "$existing_uid" ]; then
        printf '%s\n' "$existing_uid"
        return 0
      fi

      payload_file="$TMP_DIR/folder-$uid-payload.json"
      python3 - <<'PY' "$uid" "$title" "$payload_file"
import json
import sys

uid, title, output = sys.argv[1:4]
with open(output, 'w', encoding='utf-8') as handle:
    json.dump({'uid': uid, 'title': title}, handle)
PY
      create_response="$TMP_DIR/folder-$uid-create.json"
      create_code=$(request POST "$GRAFANA_URL/api/folders" "$payload_file" "$create_response")
      if [ "$create_code" != "200" ]; then
        echo "failed to create folder $uid ($title)" >&2
        cat "$create_response" >&2
        exit 1
      fi
      printf '%s\n' "$uid"
      ;;
    *)
      echo "unexpected response while reading folder $uid: $code" >&2
      cat "$response_file" >&2
      exit 1
      ;;
  esac
}

delete_dashboard_by_uid() {
  uid=$1
  response_file="$TMP_DIR/delete-dashboard-$uid.json"
  code=$(request DELETE "$GRAFANA_URL/api/dashboards/uid/$uid" "" "$response_file")

  case "$code" in
    200|404)
      ;;
    *)
      echo "failed to delete dashboard $uid" >&2
      cat "$response_file" >&2
      exit 1
      ;;
  esac
}

delete_folder_by_title() {
  title=$1
  uid=$(find_folder_uid_by_title "$title")
  if [ -z "$uid" ]; then
    return 0
  fi

  response_file="$TMP_DIR/delete-folder-$uid.json"
  code=$(request DELETE "$GRAFANA_URL/api/folders/$uid" "" "$response_file")

  case "$code" in
    200|404)
      ;;
    *)
      echo "failed to delete folder $title" >&2
      cat "$response_file" >&2
      exit 1
      ;;
  esac
}

import_dashboard() {
  folder_uid=$1
  dashboard_file=$2
  payload_file="$TMP_DIR/$(basename "$dashboard_file").payload.json"
  response_file="$TMP_DIR/$(basename "$dashboard_file").response.json"
  dashboard_uid=$(python3 - <<'PY' "$dashboard_file"
import json
import sys

with open(sys.argv[1], encoding='utf-8') as handle:
    dashboard = json.load(handle)
print(dashboard.get('uid', ''))
PY
)

  python3 - <<'PY' "$dashboard_file" "$folder_uid" "$payload_file"
import json
import sys

dashboard_path, folder_uid, output = sys.argv[1:4]
with open(dashboard_path, encoding='utf-8') as handle:
    dashboard = json.load(handle)

dashboard.pop('id', None)
dashboard.pop('meta', None)

payload = {
    'dashboard': dashboard,
    'folderUid': folder_uid,
    'overwrite': True,
}

with open(output, 'w', encoding='utf-8') as handle:
    json.dump(payload, handle)
PY

  code=$(request POST "$GRAFANA_URL/api/dashboards/db" "$payload_file" "$response_file")
  if [ "$code" != "200" ]; then
    if grep -q 'Cannot save provisioned dashboard' "$response_file" && [ -n "$dashboard_uid" ]; then
      if clear_provisioning_metadata; then
        code=$(request POST "$GRAFANA_URL/api/dashboards/db" "$payload_file" "$response_file")
      else
        echo "failed to clear stale provisioning metadata for $(basename "$dashboard_file")" >&2
        cat "$response_file" >&2
        exit 1
      fi
    fi
  fi

  if [ "$code" != "200" ]; then
    echo "failed to import $(basename "$dashboard_file") into folder $folder_uid" >&2
    cat "$response_file" >&2
    exit 1
  fi

  dashboard_id=$(python3 - <<'PY' "$response_file"
import json
import sys

with open(sys.argv[1], encoding='utf-8') as handle:
    data = json.load(handle)
print(data.get('id', ''))
PY
)

  if [ -n "$dashboard_id" ]; then
    star_response="$TMP_DIR/star-$dashboard_id.json"
    star_code=$(request POST "$GRAFANA_URL/api/user/stars/dashboard/$dashboard_id" "" "$star_response")
    case "$star_code" in
      200|409)
        ;;
      *)
        echo "failed to favorite dashboard $(basename "$dashboard_file")" >&2
        cat "$star_response" >&2
        exit 1
        ;;
    esac
  fi

  echo "imported $(basename "$dashboard_file") into $(folder_title "$folder_uid")"
}

health_response="$TMP_DIR/health.json"
health_code=$(request GET "$GRAFANA_URL/api/health" "" "$health_response")
if [ "$health_code" != "200" ]; then
  echo "grafana is not reachable at $GRAFANA_URL" >&2
  cat "$health_response" >&2
  exit 1
fi

if [ ! -d "$DASHBOARDS_ROOT" ]; then
  echo "dashboard source directory not found: $DASHBOARDS_ROOT" >&2
  exit 1
fi

target_folder_api_uid=$(ensure_folder "$TARGET_FOLDER_UID" "$TARGET_FOLDER_TITLE")

for dashboard_file in "$DASHBOARDS_ROOT"/*.json; do
  [ -f "$dashboard_file" ] || continue
  import_dashboard "$target_folder_api_uid" "$dashboard_file"
done

for dashboard_uid in $STALE_DASHBOARD_UIDS; do
  delete_dashboard_by_uid "$dashboard_uid"
done

for folder_title_name in $STALE_FOLDER_TITLES; do
  delete_folder_by_title "$folder_title_name"
done
