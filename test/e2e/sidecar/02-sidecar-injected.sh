#!/usr/bin/env bash
# The webhook should have injected an hf-mount init container and shared emptyDir.

set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

INIT=$(kubectl get pod test-ephemeral -o jsonpath='{.spec.initContainers[*].name}')
log "Init containers: $INIT"
echo "$INIT" | grep -q hf-mount || { kubectl get pod test-ephemeral -o yaml; fail "sidecar not injected"; }

VOLS=$(kubectl get pod test-ephemeral -o jsonpath='{.spec.volumes[*].name}')
echo "$VOLS" | grep -q hf-csi-tmp || fail "hf-csi-tmp volume not injected"

ok "sidecar/02-sidecar-injected"
