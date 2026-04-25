#!/usr/bin/env bash
# Bucket read-write through sidecar (requires HF_TOKEN).

set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

: "${HF_TOKEN:?HF_TOKEN is required for bucket tests}"
HUB_BUCKET=${HUB_BUCKET:-xcid/csi-e2e-bucket}
HUB_API=${HUB_API:-https://huggingface.co/api}

kubectl delete pod test-pv-token --ignore-not-found --wait=false

curl -sf "$HUB_API/whoami-v2" -H "Authorization: Bearer $HF_TOKEN" | head -1
curl -sf -X POST "$HUB_API/buckets/$HUB_BUCKET" \
  -H "Authorization: Bearer $HF_TOKEN" || true

kubectl create secret generic hf-ci-token \
  --from-literal=token="$HF_TOKEN" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: test-bucket-rw
spec:
  containers:
  - name: test
    image: $BUSYBOX_IMAGE
    command: ["sh", "-c", "echo hello > /mnt/output/test.txt && cat /mnt/output/test.txt && echo BUCKET_RW_OK"]
    volumeMounts:
    - name: bucket
      mountPath: /mnt/output
  volumes:
  - name: bucket
    csi:
      driver: hf.csi.huggingface.co
      nodePublishSecretRef:
        name: hf-ci-token
      volumeAttributes:
        sourceType: bucket
        sourceId: $HUB_BUCKET
  restartPolicy: Never
EOF

wait_pod_succeeded test-bucket-rw 180s
kubectl logs test-bucket-rw | grep BUCKET_RW_OK
ok "sidecar/05-bucket-rw"
