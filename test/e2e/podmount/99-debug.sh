#!/usr/bin/env bash
# Debug dump for failed e2e runs. Always exits 0.

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"
# lib.sh sets -euo pipefail; relax those here so a single missing pod
# does not abort the dump.
set +eu +o pipefail

echo "=== CSI driver logs ==="
kubectl logs -l app=hf-csi-node --tail=100 -c hf-csi-plugin
for p in test-mount test-resilience test-bucket-rw test-fsgroup-rw test-remount test-multi test-ephemeral test-mount-fail; do
  echo "=== describe $p ==="
  kubectl describe pod "$p" 2>/dev/null
done
echo "=== Events ==="
kubectl get events --sort-by='.lastTimestamp'
echo "=== Mount pods ==="
kubectl get pods -l hf.csi.huggingface.co/app=hf-mount
echo "=== Mount pod logs (current and previous) ==="
for mp in $(kubectl get pods -l hf.csi.huggingface.co/app=hf-mount -o name 2>/dev/null); do
  echo "--- $mp ---"
  kubectl describe "$mp" 2>/dev/null
  kubectl logs "$mp" --tail=200 2>/dev/null
  kubectl logs "$mp" --previous --tail=200 2>/dev/null
done
echo "=== HFMount CRDs ==="
kubectl get hfmounts -o yaml 2>/dev/null
exit 0
