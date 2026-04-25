#!/usr/bin/env bash
# Run all sidecar e2e tests in order. Skips bucket tests if HF_TOKEN is unset.

set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)

TESTS=(
  01-ephemeral.sh
  02-sidecar-injected.sh
  03-no-mount-pods.sh
  04-pv-token.sh
)

if [[ -n "${HF_TOKEN:-}" ]]; then
  TESTS+=(05-bucket-rw.sh 06-fsgroup.sh)
else
  echo "[sidecar] HF_TOKEN not set, skipping bucket-rw and fsgroup tests" >&2
fi

TESTS+=(
  07-token-perms.sh
  08-multi-volume.sh
  09-stuck-sidecar.sh
)

trap '$SCRIPT_DIR/99-debug.sh' ERR

for t in "${TESTS[@]}"; do
  echo
  echo "=================================================="
  echo "  $t"
  echo "=================================================="
  bash "$SCRIPT_DIR/$t"
done

echo
echo "All sidecar e2e tests passed."
