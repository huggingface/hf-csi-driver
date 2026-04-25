#!/usr/bin/env bash
# Bucket write as non-root with fsGroup (requires HF_TOKEN).

set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

: "${HF_TOKEN:?HF_TOKEN is required for bucket tests}"
HUB_BUCKET=${HUB_BUCKET:-chris-rannou/csi-e2e-bucket}

kubectl delete pod test-bucket-rw --ignore-not-found --wait=false

kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: test-fsgroup-rw
spec:
  securityContext:
    runAsUser: 1000
    runAsGroup: 1000
    fsGroup: 1000
    runAsNonRoot: true
  containers:
  - name: test
    image: $BUSYBOX_IMAGE
    command:
    - sh
    - -c
    - |
      echo "uid=\$(id -u) gid=\$(id -g) groups=\$(id -G)"
      stat -c 'mount: uid=%u gid=%g mode=%a' /mnt/output
      echo fsgroup-test > /mnt/output/fsgroup-test.txt && echo FSGROUP_WRITE_OK
      cat /mnt/output/fsgroup-test.txt
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
        mountMode: mountpod
        sourceType: bucket
        sourceId: $HUB_BUCKET
  restartPolicy: Never
EOF

wait_pod_succeeded test-fsgroup-rw 180s
kubectl logs test-fsgroup-rw | grep FSGROUP_WRITE_OK
ok "podmount/10-fsgroup"
