#!/usr/bin/env bash
# Create kind cluster, build/load images, install chart.
#   ./setup-cluster.sh podmount      # webhook disabled, mount-pod path
#   ./setup-cluster.sh sidecar       # webhook enabled, fd-passing path
#
# Env vars:
#   CLUSTER_NAME       (default: hfcsi-$MODE)
#   DRIVER_IMAGE       (default: hf-csi-driver:test)
#   HF_MOUNT_IMAGE     (default: ghcr.io/huggingface/hf-mount-fuse:v0.3.1)
#                      If pointing to a local tag, image must already be loaded.
#   HF_MOUNT_BUILD_DIR If set, docker build hf-mount from this directory
#                      instead of pulling HF_MOUNT_IMAGE.

set -euo pipefail

MODE=${1:?Usage: $0 podmount|sidecar}
case "$MODE" in
  podmount|sidecar) ;;
  *) echo "MODE must be podmount or sidecar" >&2; exit 2 ;;
esac

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/../.." && pwd)
CLUSTER_NAME=${CLUSTER_NAME:-hfcsi-$MODE}
export CLUSTER_NAME

# shellcheck source=lib.sh
source "$SCRIPT_DIR/lib.sh"

prepare_hfmount_image() {
  if [[ -n "${HF_MOUNT_BUILD_DIR:-}" ]]; then
    log "Building hf-mount from $HF_MOUNT_BUILD_DIR -> $HF_MOUNT_IMAGE"
    docker build -t "$HF_MOUNT_IMAGE" "$HF_MOUNT_BUILD_DIR"
  elif docker image inspect "$HF_MOUNT_IMAGE" >/dev/null 2>&1; then
    log "Using local hf-mount image $HF_MOUNT_IMAGE"
  else
    log "Pulling hf-mount image $HF_MOUNT_IMAGE"
    docker pull "$HF_MOUNT_IMAGE"
  fi
}

if cluster_exists; then
  log "kind cluster '$CLUSTER_NAME' already exists, reusing"
  CLUSTER_PID=""
else
  log "Creating kind cluster '$CLUSTER_NAME' (in background)"
  KIND_CONFIG=$(mktemp)
  cat > "$KIND_CONFIG" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraMounts:
      - hostPath: /dev/fuse
        containerPath: /dev/fuse
        propagation: None
EOF
  kind create cluster --name "$CLUSTER_NAME" --config "$KIND_CONFIG" --wait 60s &
  CLUSTER_PID=$!
fi

log "Building driver image $DRIVER_IMAGE (in background)"
docker build -t "$DRIVER_IMAGE" "$REPO_ROOT" &
DRIVER_PID=$!

prepare_hfmount_image

wait "$DRIVER_PID"
[[ -n "$CLUSTER_PID" ]] && wait "$CLUSTER_PID"
rm -f "${KIND_CONFIG:-}"

log "Loading images into kind"
kind load docker-image "$DRIVER_IMAGE" --name "$CLUSTER_NAME"
kind load docker-image "$HF_MOUNT_IMAGE" --name "$CLUSTER_NAME"

HFMOUNT_REPO=${HF_MOUNT_IMAGE%:*}
HFMOUNT_TAG=${HF_MOUNT_IMAGE##*:}
DRIVER_REPO=${DRIVER_IMAGE%:*}
DRIVER_TAG=${DRIVER_IMAGE##*:}

HELM_ARGS=(
  --namespace "$NAMESPACE"
  --set image.repository="$DRIVER_REPO"
  --set image.tag="$DRIVER_TAG"
  --set image.pullPolicy=Never
  --set hfMount.image.repository="$HFMOUNT_REPO"
  --set hfMount.image.tag="$HFMOUNT_TAG"
  --set hfMount.image.pullPolicy=Never
)

if [[ "$MODE" == "sidecar" ]]; then
  HELM_ARGS+=(
    --set webhook.enabled=true
    --set webhook.image.repository="$DRIVER_REPO"
    --set webhook.image.tag="$DRIVER_TAG"
    --set webhook.image.pullPolicy=Never
  )
fi

log "Helm upgrade --install hf-csi (mode=$MODE)"
helm upgrade --install hf-csi "$REPO_ROOT/deploy/helm/hf-csi-driver/" "${HELM_ARGS[@]}"

kubectl rollout status daemonset hf-csi-hf-csi-driver-node \
  --namespace "$NAMESPACE" --timeout=180s

if [[ "$MODE" == "sidecar" ]]; then
  kubectl rollout status deployment hf-csi-hf-csi-driver-webhook \
    --namespace "$NAMESPACE" --timeout=180s
fi

ok "Cluster '$CLUSTER_NAME' ready (mode=$MODE)"
