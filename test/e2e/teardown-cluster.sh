#!/usr/bin/env bash
# Delete the kind cluster created by setup-cluster.sh.
#   ./teardown-cluster.sh podmount|sidecar

set -euo pipefail

MODE=${1:?Usage: $0 podmount|sidecar}
CLUSTER_NAME=${CLUSTER_NAME:-hfcsi-$MODE}

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  echo "Deleting kind cluster '$CLUSTER_NAME'"
  kind delete cluster --name "$CLUSTER_NAME"
else
  echo "Cluster '$CLUSTER_NAME' not found, nothing to do"
fi
