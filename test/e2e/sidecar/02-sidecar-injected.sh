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

# The webhook also raises terminationGracePeriodSeconds so the sidecar can
# flush bucket writes on shutdown. test-ephemeral was created without an
# explicit grace period, so the webhook should have set it to the minimum
# (60s) and recorded the original value as "unset" in an annotation.
GRACE=$(kubectl get pod test-ephemeral -o jsonpath='{.spec.terminationGracePeriodSeconds}')
[[ "$GRACE" == "60" ]] || fail "terminationGracePeriodSeconds: want 60, got '$GRACE'"
ORIG=$(kubectl get pod test-ephemeral \
  -o jsonpath='{.metadata.annotations.hf\.csi\.huggingface\.co/original-termination-grace-period-seconds}')
[[ "$ORIG" == "unset" ]] || fail "original grace annotation: want 'unset', got '$ORIG'"

ok "sidecar/02-sidecar-injected"
