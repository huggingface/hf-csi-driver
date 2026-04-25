#!/usr/bin/env bash
# Inline ephemeral CSI volume with webhook-injected sidecar.

set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

kubectl get pods -l app=hf-csi-webhook
kubectl get mutatingwebhookconfiguration
kubectl get csidriver hf.csi.huggingface.co

kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: test-ephemeral
spec:
  containers:
  - name: test
    image: $BUSYBOX_IMAGE
    command: ["sh", "-c", "ls /model && cat /model/config.json"]
    volumeMounts:
    - name: model
      mountPath: /model
      readOnly: true
  volumes:
  - name: model
    csi:
      driver: hf.csi.huggingface.co
      volumeAttributes:
        sourceType: repo
        sourceId: openai-community/gpt2
  restartPolicy: Never
EOF

wait_pod_succeeded test-ephemeral 180s
kubectl logs test-ephemeral | grep model_type
ok "sidecar/01-ephemeral"
