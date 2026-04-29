#!/usr/bin/env bash
# Run all podmount e2e tests in order. Skips bucket tests if HF_TOKEN is unset.

set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
export KUBECONFIG="$SCRIPT_DIR/../.kubeconfig-hfcsi-podmount"
set -a
[ -f "$SCRIPT_DIR/../.env" ] && source "$SCRIPT_DIR/../.env"
set +a

TESTS=(
  01-mount.sh
  02-resilience-driver-restart.sh
  03-kill-mount-pod.sh
  04-unmount-cleanup.sh
  05-remount.sh
  06-multi-volume.sh
  07-ephemeral.sh
  08-mount-failure.sh
)

if [[ -n "${HF_TOKEN:-}" ]]; then
  TESTS+=(09-bucket-rw.sh 10-fsgroup.sh)
else
  echo "[podmount] HF_TOKEN not set, skipping bucket-rw and fsgroup tests" >&2
fi

trap '$SCRIPT_DIR/99-debug.sh' ERR

for t in "${TESTS[@]}"; do
  echo
  echo "=================================================="
  echo "  $t"
  echo "=================================================="
  bash "$SCRIPT_DIR/$t"
done

echo
echo "All podmount e2e tests passed."
