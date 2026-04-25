#!/usr/bin/env bash
# Delete the kind cluster created by setup-cluster.sh.
#   ./teardown-cluster.sh podmount|sidecar

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
MODE=${1:?Usage: $0 podmount|sidecar}
export CLUSTER_NAME=${CLUSTER_NAME:-hfcsi-$MODE}
# shellcheck source=lib.sh
source "$SCRIPT_DIR/lib.sh"

if cluster_exists; then
  echo "Deleting kind cluster '$CLUSTER_NAME'"
  kind delete cluster --name "$CLUSTER_NAME"
else
  echo "Cluster '$CLUSTER_NAME' not found, nothing to do"
fi
