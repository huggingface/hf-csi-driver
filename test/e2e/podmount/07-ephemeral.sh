#!/usr/bin/env bash
# Inline ephemeral CSI volume (no PV/PVC).

set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

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
        mountMode: mountpod
        sourceType: repo
        sourceId: openai-community/gpt2
  restartPolicy: Never
EOF

wait_pod_succeeded test-ephemeral 120s
kubectl logs test-ephemeral | grep model_type
ok "podmount/07-ephemeral"
