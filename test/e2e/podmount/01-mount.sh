#!/usr/bin/env bash
# Mount a public HF repo via PV+PVC. Verify reads, mount pod, HFMount CRD.

set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

kubectl get csidriver hf.csi.huggingface.co
kubectl get csidriver hf.csi.huggingface.co -o jsonpath='{.spec.attachRequired}' | grep false
kubectl get csidriver hf.csi.huggingface.co -o jsonpath='{.spec.requiresRepublish}' | grep true

kubectl create secret generic hf-token --from-literal=token='' \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolume
metadata:
  name: test-pv
spec:
  capacity:
    storage: 1Gi
  accessModes: [ReadOnlyMany]
  persistentVolumeReclaimPolicy: Retain
  csi:
    driver: hf.csi.huggingface.co
    volumeHandle: test-gpt2
    nodePublishSecretRef:
      name: hf-token
      namespace: $NAMESPACE
    volumeAttributes:
      sourceType: repo
      sourceId: openai-community/gpt2
      revision: main
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: test-pvc
spec:
  accessModes: [ReadOnlyMany]
  storageClassName: ""
  resources:
    requests:
      storage: 1Gi
  volumeName: test-pv
---
apiVersion: v1
kind: Pod
metadata:
  name: test-mount
spec:
  containers:
  - name: test
    image: $BUSYBOX_IMAGE
    command: ["sh", "-c", "ls /data && cat /data/config.json"]
    volumeMounts:
    - name: vol
      mountPath: /data
      readOnly: true
  volumes:
  - name: vol
    persistentVolumeClaim:
      claimName: test-pvc
  restartPolicy: Never
EOF

wait_pod_succeeded test-mount 120s
kubectl logs test-mount | grep config.json
kubectl logs test-mount | grep model_type

log "=== Mount pods ==="
kubectl get pods -l hf.csi.huggingface.co/app=hf-mount
MOUNT_PODS=$(kubectl get pods -l hf.csi.huggingface.co/app=hf-mount -o jsonpath='{.items[*].metadata.name}')
[[ -n "$MOUNT_PODS" ]] || fail "no mount pods found"
log "Found mount pods: $MOUNT_PODS"

log "=== HFMount CRDs ==="
HFMOUNTS=$(kubectl get hfmounts -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)
if [[ -z "$HFMOUNTS" ]]; then
  log "WARNING: No HFMount CRDs found (CRD tracking is best-effort)"
else
  log "Found HFMounts: $HFMOUNTS"
fi

ok "podmount/01-mount"
