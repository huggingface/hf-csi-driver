#!/usr/bin/env bash
# Sidecar mode replaces mount pods.

set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

MOUNT_PODS=$(list_mount_pods)
if [[ -n "$MOUNT_PODS" ]]; then
  log "INFO: mount pods found (sidecar fd-passing may have fallen back): $MOUNT_PODS"
else
  ok "no mount pods, sidecar handled the mount"
fi
