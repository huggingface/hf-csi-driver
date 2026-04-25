#!/usr/bin/env bash
# PV-backed volume with secret -> webhook resolves PVC->PV->CSI driver and injects sidecar.

set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

kubectl delete pod test-ephemeral --ignore-not-found --wait=false

kubectl create secret generic hf-token-sidecar --from-literal=token='' \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolume
metadata:
  name: test-pv-sidecar
spec:
  capacity:
    storage: 1Gi
  accessModes: [ReadOnlyMany]
  persistentVolumeReclaimPolicy: Retain
  csi:
    driver: hf.csi.huggingface.co
    volumeHandle: test-gpt2-sidecar
    nodePublishSecretRef:
      name: hf-token-sidecar
      namespace: $NAMESPACE
    volumeAttributes:
      sourceType: repo
      sourceId: openai-community/gpt2
      revision: main
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: test-pvc-sidecar
spec:
  accessModes: [ReadOnlyMany]
  storageClassName: ""
  resources:
    requests:
      storage: 1Gi
  volumeName: test-pv-sidecar
---
apiVersion: v1
kind: Pod
metadata:
  name: test-pv-token
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
      claimName: test-pvc-sidecar
  restartPolicy: Never
EOF

wait_pod_succeeded test-pv-token 180s
kubectl logs test-pv-token | grep model_type
kubectl get pod test-pv-token -o jsonpath='{.spec.initContainers[*].name}' | grep -q hf-mount
ok "sidecar/04-pv-token"
