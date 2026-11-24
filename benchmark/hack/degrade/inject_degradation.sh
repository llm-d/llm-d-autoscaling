#!/bin/bash
# inject_degradation.sh -- lock GPU clocks on model-server pods to simulate
# a capacity-shaped degradation, then restore.
#
# Usage:
#   inject_degradation.sh -n <namespace> -l <label> [-c <clock_mhz>] [-d <duration_s>]
#
# Options:
#   -n  Namespace (required)
#   -l  Pod label selector (required, e.g. "llm-d.ai/role=prefill")
#   -c  Clock in MHz (default: 495, quarter of H100 1980 MHz max)
#   -d  Duration in seconds (default: 900)
#
# Prerequisites:
#   - Model-server pods are privileged (nvidia-smi clock control requires it)
#   - kubectl or oc is authenticated and on PATH
#
# The script:
#   1. Finds all running pods matching the label selector
#   2. Locks each pod's GPU clock to the specified value
#   3. Sleeps for the duration
#   4. Resets ALL running pods (including any added during the window)

set -euo pipefail

NAMESPACE=""
LABEL=""
CLOCK=495
DURATION=900

while getopts "n:l:c:d:h" opt; do
  case $opt in
    n) NAMESPACE="$OPTARG" ;;
    l) LABEL="$OPTARG" ;;
    c) CLOCK="$OPTARG" ;;
    d) DURATION="$OPTARG" ;;
    h) echo "Usage: $0 -n <namespace> -l <label> [-c <clock>] [-d <duration>]"; exit 0 ;;
    *) echo "Usage: $0 -n <namespace> -l <label> [-c <clock>] [-d <duration>]" >&2; exit 1 ;;
  esac
done

if [ -z "$NAMESPACE" ] || [ -z "$LABEL" ]; then
  echo "Error: -n and -l are required" >&2
  exit 1
fi

KUBECTL=$(command -v kubectl || command -v oc)
if [ -z "$KUBECTL" ]; then
  echo "Error: kubectl or oc not found" >&2
  exit 1
fi

get_running_pods() {
  $KUBECTL -n "$NAMESPACE" get pods -l "$LABEL" \
    --field-selector=status.phase=Running \
    -o json | python3 -c "
import json, sys
items = json.load(sys.stdin).get('items', [])
print(' '.join(p['metadata']['name'] for p in items))
"
}

echo "=== DEGRADE: locking all $LABEL pods to ${CLOCK} MHz ==="
PODS=$(get_running_pods)
if [ -z "$PODS" ]; then
  echo "Error: no running pods found with label $LABEL" >&2
  exit 1
fi
echo "Targets: $PODS"

for POD in $PODS; do
  echo "Locking $POD to ${CLOCK} MHz"
  $KUBECTL -n "$NAMESPACE" exec "$POD" -- nvidia-smi -lgc "$CLOCK" 2>&1 | tail -1
done

echo "DEGRADE-START $(date -u +%Y-%m-%dT%H:%M:%SZ)"

sleep "$DURATION"

echo "=== RESTORE: resetting all $LABEL pods ==="
ALL_PODS=$(get_running_pods)
for POD in $ALL_PODS; do
  echo "Resetting $POD"
  $KUBECTL -n "$NAMESPACE" exec "$POD" -- nvidia-smi -rgc 2>&1 | tail -1
done

echo "DEGRADE-END $(date -u +%Y-%m-%dT%H:%M:%SZ)"
