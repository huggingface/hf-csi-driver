#!/usr/bin/env bash
# Token file in shared emptyDir must be readable by the unprivileged sidecar (uid 65534).

set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

kubectl create secret generic hf-dummy-token \
  --from-literal=token="test-token-value" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: test-token-perms
spec:
  containers:
  - name: test
    image: $BUSYBOX_IMAGE
    command: ["sh", "-c", "ls /model && echo TOKEN_PERMS_OK"]
    volumeMounts:
    - name: vol
      mountPath: /model
      readOnly: true
  volumes:
  - name: vol
    csi:
      driver: hf.csi.huggingface.co
      nodePublishSecretRef:
        name: hf-dummy-token
      volumeAttributes:
        sourceType: repo
        sourceId: openai-community/gpt2
  restartPolicy: Never
EOF

wait_pod_succeeded test-token-perms 180s

TOKEN_LOGS=$(kubectl logs test-token-perms -c hf-mount 2>/dev/null || true)
if echo "$TOKEN_LOGS" | grep -q "Permission denied"; then
  echo "$TOKEN_LOGS"
  fail "token file not readable by sidecar"
fi

ok "sidecar/07-token-perms"
