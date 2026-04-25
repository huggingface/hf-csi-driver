#!/usr/bin/env bash
# Force-delete the mount pod, observe driver behavior.

set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

MOUNT_POD=$(kubectl get pods -l hf.csi.huggingface.co/app=hf-mount -o jsonpath='{.items[0].metadata.name}')
log "Killing mount pod: $MOUNT_POD"
kubectl delete pod "$MOUNT_POD" --grace-period=0 --force

log "Waiting for the driver to recreate the mount pod (up to 30s)..."
for i in $(seq 1 30); do
  current=$(list_mount_pods)
  if [[ -n "$current" && "$current" != "$MOUNT_POD" ]]; then
    log "Driver recreated mount pod after ${i}s: $current"
    break
  fi
  sleep 1
done

log "=== Mount pods ==="
kubectl get pods -l hf.csi.huggingface.co/app=hf-mount
log "=== CSI driver logs after mount pod kill ==="
kubectl logs -l app=hf-csi-node --tail=30 -c hf-csi-plugin

ok "podmount/03-kill-mount-pod"
