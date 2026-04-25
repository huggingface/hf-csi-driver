#!/usr/bin/env bash
# PR-23 regression test: a pod with a fake hf-mount init container that never
# connects to the CSI fd-passing socket leaves the driver in its 5-minute
# accept() wait, holding the FUSE fd. The kernel mount is in place but no
# daemon will ever respond to FUSE_INIT, so any os.Lstat on the target blocks.
#
# Without the PR-23 fix, NodeUnpublishVolume hangs on IsMountPoint -> Lstat
# until the 5min publish timeout fires AND every subsequent kubelet retry
# extends the hang. Pods stay in Terminating for hours/days.
#
# With the fix, the sidecar fast path skips Lstat and calls umount2(target,
# MNT_DETACH) directly, which never touches the FUSE daemon and detaches the
# mount immediately.
#
# This test reproduces the bug condition (FUSE mount + blocking lstat) and
# verifies that umount2(MNT_DETACH) — the exact kernel call the fix makes —
# releases it without blocking. The Go-level path (NodeUnpublishVolume calling
# fuseUnmountFn instead of IsMountPoint) is covered by node_test.go.

set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

POD=hf-stuck-sidecar
kubectl delete pod "$POD" --ignore-not-found --grace-period=0 --force --wait=false

# Pre-define our own hf-mount init container so the webhook hasSidecar()
# check skips injection. Container sleeps without ever connecting to the
# fd-passing socket.
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $POD
spec:
  restartPolicy: Never
  initContainers:
  - name: hf-mount
    image: $BUSYBOX_IMAGE
    restartPolicy: Always
    command: ["sh", "-c", "echo 'fake hf-mount, never connects to socket' && sleep 99999"]
    volumeMounts:
    - name: hf-csi-tmp
      mountPath: /hf-csi-tmp
  containers:
  - name: app
    image: $BUSYBOX_IMAGE
    command: ["sh", "-c", "ls /mnt/hf && sleep 3600"]
    volumeMounts:
    - name: hf-vol
      mountPath: /mnt/hf
  volumes:
  - name: hf-csi-tmp
    emptyDir:
      medium: Memory
  - name: hf-vol
    csi:
      driver: hf.csi.huggingface.co
      readOnly: true
      volumeAttributes:
        sourceType: repo
        sourceId: sshleifer/tiny-gpt2
        revision: main
EOF

log "Waiting up to 60s for NodePublishVolume to open /dev/fuse and block on the sidecar socket..."
POD_UID=""
TARGET=""
NODE=""
for i in $(seq 1 60); do
  POD_UID=$(kubectl get pod "$POD" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)
  NODE=$(kubectl get pod "$POD" -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)
  if [[ -n "$POD_UID" && -n "$NODE" ]]; then
    TARGET="/var/lib/kubelet/pods/$POD_UID/volumes/kubernetes.io~csi/hf-vol/mount"
    if docker exec "$NODE" sh -c "grep -q $POD_UID /proc/self/mountinfo" 2>/dev/null; then
      log "FUSE mount appeared after ${i}s: $TARGET"
      break
    fi
  fi
  sleep 1
done
[[ -n "$POD_UID" && -n "$NODE" ]] || fail "pod did not schedule"
docker exec "$NODE" sh -c "grep -q $POD_UID /proc/self/mountinfo" \
  || fail "FUSE mount never appeared in /proc/self/mountinfo"

# Confirm the bug condition: lstat blocks because no daemon will respond.
set +e
docker exec "$NODE" sh -c "timeout 3 stat $TARGET >/dev/null 2>&1"
STAT_EXIT=$?
set -e
if [[ $STAT_EXIT -eq 124 ]]; then
  ok "stat $TARGET timed out (exit 124) — kernel waiting for FUSE_INIT, bug condition reproduced"
else
  log "WARNING: stat returned $STAT_EXIT (expected 124). Bug condition may not be active."
fi

# Apply the fix's mechanism: umount2(target, MNT_DETACH). util-linux's
# `umount -l` is exactly that syscall, no Lstat first. This is what the PR-23
# fix invokes via syscall.Unmount(target, syscall.MNT_DETACH) in fuseUnmount.
log "Calling umount -l (MNT_DETACH) — same syscall the PR-23 fix invokes"
START_NS=$(date +%s%N)
docker exec "$NODE" timeout 5 umount -l "$TARGET"
END_NS=$(date +%s%N)
ELAPSED_MS=$(( (END_NS - START_NS) / 1000000 ))
log "umount -l returned in ${ELAPSED_MS}ms (without the fix, IsMountPoint/Lstat would block here)"

# MNT_DETACH is lazy: the mountinfo entry stays until every open fd closes
# (the CSI driver still holds the FUSE fd in its accept() goroutine). What
# matters is that the path is no longer a FUSE mountpoint -- stat must work
# without blocking and must report a regular directory.
set +e
docker exec "$NODE" sh -c "timeout 3 stat $TARGET >/dev/null 2>&1"
STAT_EXIT=$?
set -e
[[ $STAT_EXIT -eq 0 ]] || fail "stat still failing after MNT_DETACH (exit $STAT_EXIT) — fix mechanism broken"
ok "stat $TARGET works after MNT_DETACH (FUSE detached from path lookup)"

# Cleanup. Force-delete the pod (NodePublishVolume will remain blocked but the
# CSI driver socket goroutine will time out at most 5min later in the
# background; we don't wait for it).
kubectl delete pod "$POD" --grace-period=0 --force --wait=false >/dev/null 2>&1 || true

ok "sidecar/09-stuck-sidecar"
