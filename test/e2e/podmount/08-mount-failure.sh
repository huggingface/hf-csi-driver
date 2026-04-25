#!/usr/bin/env bash
# Mount of a non-existent repo -> FailedMount event.

set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: test-mount-fail
spec:
  containers:
  - name: test
    image: $BUSYBOX_IMAGE
    command: ["sleep", "300"]
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
        sourceId: this-repo/does-not-exist-at-all
  restartPolicy: Never
EOF

log "=== Waiting for FailedMount event ==="
EVENTS=""
for i in $(seq 1 60); do
  EVENTS=$(kubectl get events --field-selector involvedObject.name=test-mount-fail,reason=FailedMount -o jsonpath='{.items[*].message}' 2>/dev/null || true)
  if [[ -n "$EVENTS" ]]; then
    log "FailedMount event found after ${i}s: $EVENTS"
    break
  fi
  sleep 2
done

if [[ -z "$EVENTS" ]]; then
  kubectl describe pod test-mount-fail
  fail "no FailedMount event after 120s"
fi

if echo "$EVENTS" | grep -q "keeps crashing\|CrashLoopBackOff\|failed.*phase"; then
  ok "FailedMount event has descriptive crash message"
else
  log "WARNING: FailedMount present but message may not be specific: $EVENTS"
fi

kubectl delete pod test-mount-fail --ignore-not-found
ok "podmount/08-mount-failure"
