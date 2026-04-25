# shellcheck shell=bash
# Shared helpers for e2e tests. Source from each test script.

set -euo pipefail

CLUSTER_NAME=${CLUSTER_NAME:-hfcsi}
NAMESPACE=${NAMESPACE:-default}
DRIVER_IMAGE=${DRIVER_IMAGE:-hf-csi-driver:test}
HF_MOUNT_IMAGE=${HF_MOUNT_IMAGE:-ghcr.io/huggingface/hf-mount-fuse:v0.3.1}
BUSYBOX_IMAGE=${BUSYBOX_IMAGE:-public.ecr.aws/docker/library/busybox:latest}

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
