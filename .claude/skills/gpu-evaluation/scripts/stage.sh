#!/usr/bin/env bash
# Copy a directory of traces (gen_load.py output) onto the namespace's workload
# PVC, where the harness pod sees them under /requests/datasets/.
#
#   stage.sh <namespace> <dir>
#
# Then point a run at one with --dataset /requests/datasets/<name>.csv.
# The PVC is `workload-pvc` (llm-d-benchmark's default storage.workloadPvc.name)
# and exists after `standup`. A short-lived pod mounts it for the copy and is
# deleted afterwards. KUBECTL may carry --context.
set -euo pipefail

NS=${1:?usage: stage.sh <namespace> <dir>}
DIR=${2:?trace directory}
PVC=${PVC:-workload-pvc}
POD=step5-stage-traces
K=${KUBECTL:-kubectl}
trap '$K -n "$NS" delete pod "$POD" --ignore-not-found --wait=false >/dev/null 2>&1 || true' EXIT

# RWO volumes: run on the node of the data-access pod when there is one
NODE=$($K -n "$NS" get pod -l role=llm-d-benchmark-data-access \
  -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null || true)

$K -n "$NS" apply -f - <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $POD
spec:
  restartPolicy: Never
  ${NODE:+nodeName: $NODE}
  containers:
    - name: copy
      image: quay.io/fedora/fedora:latest
      imagePullPolicy: IfNotPresent
      command: ["sleep", "600"]
      volumeMounts:
        - name: requests
          mountPath: /requests
  volumes:
    - name: requests
      persistentVolumeClaim:
        claimName: $PVC
YAML
$K -n "$NS" wait --for=condition=Ready "pod/$POD" --timeout=300s
$K -n "$NS" exec "$POD" -- mkdir -p /requests/datasets
for f in "$DIR"/*.csv; do
  $K -n "$NS" cp "$f" "$POD:/requests/datasets/$(basename "$f")"
done
$K -n "$NS" exec "$POD" -- ls -l /requests/datasets
