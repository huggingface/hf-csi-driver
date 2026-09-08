#!/usr/bin/env bash
# Continuously snapshot hf-mount pod logs into a directory so they survive
# the driver deleting crashed mount pods. Run in the background; each pod's
# last readable logs are kept in $OUT_DIR/<pod>.log (and .previous.log for
# the pre-restart container).

set -uo pipefail
OUT_DIR=${1:-/tmp/hf-mount-logs}
mkdir -p "$OUT_DIR"

while true; do
  for pod in $(kubectl get pods -l hf.csi.huggingface.co/app=hf-mount \
      -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
    kubectl logs "$pod" --timestamps >"$OUT_DIR/$pod.log.tmp" 2>/dev/null \
      && mv "$OUT_DIR/$pod.log.tmp" "$OUT_DIR/$pod.log"
    kubectl logs "$pod" --previous --timestamps >"$OUT_DIR/$pod.previous.log.tmp" 2>/dev/null \
      && mv "$OUT_DIR/$pod.previous.log.tmp" "$OUT_DIR/$pod.previous.log"
  done
  rm -f "$OUT_DIR"/*.tmp
  sleep 2
done
