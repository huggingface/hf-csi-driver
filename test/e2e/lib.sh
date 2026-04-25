# shellcheck shell=bash
# Shared helpers for e2e tests. Source from each test script.

set -euo pipefail

CLUSTER_NAME=${CLUSTER_NAME:-hfcsi}
NAMESPACE=${NAMESPACE:-default}
DRIVER_IMAGE=${DRIVER_IMAGE:-hf-csi-driver:test}
HF_MOUNT_IMAGE=${HF_MOUNT_IMAGE:-ghcr.io/huggingface/hf-mount-fuse:v0.3.1}
BUSYBOX_IMAGE=${BUSYBOX_IMAGE:-public.ecr.aws/docker/library/busybox:latest}
HUB_API=${HUB_API:-https://huggingface.co/api}
HUB_BUCKET=${HUB_BUCKET:-XciD/csi-e2e-bucket}

log() { printf '[%(%H:%M:%S)T] %s\n' -1 "$*" >&2; }
fail() { log "FAIL: $*"; exit 1; }
ok() { log "OK: $*"; }

wait_pod_succeeded() {
  local name=$1 timeout=${2:-180s}
  kubectl wait "pod/$name" --for=jsonpath='{.status.phase}'=Succeeded --timeout="$timeout"
}

wait_pod_ready() {
  local name=$1 timeout=${2:-180s}
  kubectl wait "pod/$name" --for=condition=Ready --timeout="$timeout"
}

cluster_exists() {
  kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"
}

list_mount_pods() {
  kubectl get pods -l hf.csi.huggingface.co/app=hf-mount \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null
}

# Validate HF_TOKEN, ensure HUB_BUCKET exists, create the hf-ci-token Secret.
# Required by bucket-rw and fsgroup tests.
setup_bucket_token() {
  : "${HF_TOKEN:?HF_TOKEN is required for bucket tests}"
  curl -sf "$HUB_API/whoami-v2" -H "Authorization: Bearer $HF_TOKEN" | head -1
  curl -sf -X POST "$HUB_API/buckets/$HUB_BUCKET" \
    -H "Authorization: Bearer $HF_TOKEN" || true
  kubectl create secret generic hf-ci-token \
    --from-literal=token="$HF_TOKEN" \
    --dry-run=client -o yaml | kubectl apply -f -
}
