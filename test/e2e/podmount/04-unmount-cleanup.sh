#!/usr/bin/env bash
# Delete the workload pod, verify mount pods are cleaned up.

set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

log "=== Deleting workload pod ==="
kubectl delete pod test-resilience --timeout=60s

log "=== Waiting for mount pod cleanup ==="
for i in $(seq 1 30); do
  MOUNT_PODS=$(list_mount_pods)
  if [[ -z "$MOUNT_PODS" ]]; then
    log "Mount pods cleaned up after ${i}s"
    break
  fi
  sleep 2
done

MOUNT_PODS=$(list_mount_pods)
if [[ -n "$MOUNT_PODS" ]]; then
  log "WARNING: mount pods still exist after unmount: $MOUNT_PODS"
else
  ok "All mount pods cleaned up"
fi
