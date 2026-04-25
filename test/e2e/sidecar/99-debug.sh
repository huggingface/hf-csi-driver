#!/usr/bin/env bash
# Debug dump for failed sidecar e2e runs. Always exits 0.

set +e
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

echo "=== CSI driver logs ==="
kubectl logs -l app=hf-csi-node --tail=100 -c hf-csi-plugin
echo "=== Webhook logs ==="
kubectl logs -l app=hf-csi-webhook --tail=100 2>/dev/null
for p in test-ephemeral test-pv-token test-bucket-rw test-fsgroup-rw test-token-perms test-multi-vol hf-stuck-sidecar; do
  echo "=== describe $p ==="
  kubectl describe pod "$p" 2>/dev/null
  echo "=== logs $p (hf-mount) ==="
  kubectl logs "$p" -c hf-mount --tail=50 2>/dev/null
  kubectl logs "$p" -c hf-mount --previous --tail=50 2>/dev/null
done
echo "=== Events ==="
kubectl get events --sort-by='.lastTimestamp'
echo "=== All pods ==="
kubectl get pods -A
exit 0
