#!/usr/bin/env bash
# Long-running workload, restart CSI driver, verify mount survives.

set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

kubectl delete pod test-mount --ignore-not-found

kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: test-resilience
spec:
  containers:
  - name: reader
    image: $BUSYBOX_IMAGE
    command: ["sh", "-c", "while true; do cat /data/config.json > /dev/null && echo OK; sleep 2; done"]
    volumeMounts:
    - name: vol
      mountPath: /data
      readOnly: true
  volumes:
  - name: vol
    persistentVolumeClaim:
      claimName: test-pvc
  restartPolicy: Always
EOF

wait_pod_ready test-resilience 120s
kubectl exec test-resilience -- cat /data/config.json | grep model_type

log "=== Restarting CSI driver DaemonSet ==="
kubectl rollout restart daemonset hf-csi-hf-csi-driver-node
kubectl rollout status daemonset hf-csi-hf-csi-driver-node --timeout=120s
sleep 10

log "=== Checking mount pod survived ==="
MOUNT_PODS=$(kubectl get pods -l hf.csi.huggingface.co/app=hf-mount -o jsonpath='{.items[*].metadata.name}')
[[ -n "$MOUNT_PODS" ]] || fail "mount pods disappeared after CSI driver restart"
log "Mount pods still alive: $MOUNT_PODS"

kubectl exec test-resilience -- cat /data/config.json | grep model_type
ok "podmount/02-resilience-driver-restart"
