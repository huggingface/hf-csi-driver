#!/usr/bin/env bash
# Re-mount the same volume after the previous workload was deleted.

set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: test-remount
spec:
  containers:
  - name: test
    image: $BUSYBOX_IMAGE
    command: ["sh", "-c", "cat /data/config.json"]
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

wait_pod_succeeded test-remount 120s
kubectl logs test-remount | grep model_type
ok "podmount/05-remount"
