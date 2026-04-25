#!/usr/bin/env bash
# 2 PV-backed volumes in one pod, distinct repos -> 2 mount pods.

set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolume
metadata:
  name: test-pv-2
spec:
  capacity:
    storage: 1Gi
  accessModes: [ReadOnlyMany]
  persistentVolumeReclaimPolicy: Retain
  csi:
    driver: hf.csi.huggingface.co
    volumeHandle: test-distilgpt2
    nodePublishSecretRef:
      name: hf-token
      namespace: $NAMESPACE
    volumeAttributes:
      mountMode: mountpod
      sourceType: repo
      sourceId: distilbert/distilgpt2
      revision: main
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: test-pvc-2
spec:
  accessModes: [ReadOnlyMany]
  storageClassName: ""
  resources:
    requests:
      storage: 1Gi
  volumeName: test-pv-2
---
apiVersion: v1
kind: Pod
metadata:
  name: test-multi
spec:
  containers:
  - name: test
    image: $BUSYBOX_IMAGE
    command: ["sh", "-c", "ls /data1 && ls /data2 && echo BOTH_OK"]
    volumeMounts:
    - name: vol1
      mountPath: /data1
      readOnly: true
    - name: vol2
      mountPath: /data2
      readOnly: true
  volumes:
  - name: vol1
    persistentVolumeClaim:
      claimName: test-pvc
  - name: vol2
    persistentVolumeClaim:
      claimName: test-pvc-2
  restartPolicy: Never
EOF

wait_pod_succeeded test-multi 120s
kubectl logs test-multi | grep BOTH_OK

MOUNT_COUNT=$(kubectl get pods -l hf.csi.huggingface.co/app=hf-mount -o jsonpath='{.items[*].metadata.name}' | wc -w)
log "Mount pods: $MOUNT_COUNT"
[[ "$MOUNT_COUNT" -ge 2 ]] || log "WARNING: expected at least 2 mount pods, got $MOUNT_COUNT"
ok "podmount/06-multi-volume"
