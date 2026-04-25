#!/usr/bin/env bash
# 2 inline HF volumes -> 1 sidecar serving both.

set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

kubectl delete pod test-token-perms --ignore-not-found --wait=false

kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: test-multi-vol
spec:
  containers:
  - name: test
    image: $BUSYBOX_IMAGE
    command: ["sh", "-c", "cat /model1/config.json && cat /model2/config.json && echo MULTI_OK"]
    volumeMounts:
    - name: gpt2
      mountPath: /model1
      readOnly: true
    - name: bert
      mountPath: /model2
      readOnly: true
  volumes:
  - name: gpt2
    csi:
      driver: hf.csi.huggingface.co
      volumeAttributes:
        sourceType: repo
        sourceId: openai-community/gpt2
  - name: bert
    csi:
      driver: hf.csi.huggingface.co
      volumeAttributes:
        sourceType: repo
        sourceId: google-bert/bert-base-uncased
  restartPolicy: Never
EOF

wait_pod_succeeded test-multi-vol 180s
kubectl logs test-multi-vol | grep MULTI_OK
COUNT=$(kubectl get pod test-multi-vol -o jsonpath='{.spec.initContainers[*].name}' | tr ' ' '\n' | grep -c hf-mount)
[[ "$COUNT" -eq 1 ]] || fail "expected 1 sidecar, got $COUNT"
ok "sidecar/08-multi-volume"
