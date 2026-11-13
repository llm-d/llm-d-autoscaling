#!/usr/bin/env bash
# Generate the npd-fcoff request traces and copy them onto the namespace's
# workload PVC, where the harness pod sees them under /requests/datasets/.
#
#   benchmark/hack/npd-fcoff/stage-traces.sh <namespace> [kind|gpu|all]
#
# Then point a run at one with --dataset /requests/datasets/<name>.csv (the
# profile's REPLACE_ENV_LLMDBENCH_RUN_DATASET_{DIR,FILE} resolve from it).
#
# The PVC is `workload-pvc` (llm-d-benchmark's default storage.workloadPvc.name)
# and exists after `standup`. A short-lived pod mounts it for the copy and is
# deleted afterwards; it is pinned to the node of the data-access pod when one
# exists, since kind's local-path volumes are ReadWriteOnce.
set -euo pipefail

NS=${1:?usage: $0 <namespace> [kind|gpu|all]}
SET=${2:-all}
PVC=${PVC:-workload-pvc}
POD=npd-fcoff-stage-traces
HERE=$(cd "$(dirname "$0")" && pwd)
OUT=$(mktemp -d)
trap 'rm -rf "$OUT"; kubectl -n "$NS" delete pod "$POD" --ignore-not-found --wait=false >/dev/null 2>&1 || true' EXIT

python3 -I "$HERE/gen_traces.py" --set "$SET" --out-dir "$OUT"

NODE=$(kubectl -n "$NS" get pod -l role=llm-d-benchmark-data-access \
  -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null || true)

kubectl -n "$NS" apply -f - <<EOF
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
EOF
kubectl -n "$NS" wait --for=condition=Ready "pod/$POD" --timeout=300s
kubectl -n "$NS" exec "$POD" -- mkdir -p /requests/datasets
for f in "$OUT"/*; do
  kubectl -n "$NS" cp "$f" "$POD:/requests/datasets/$(basename "$f")"
done
kubectl -n "$NS" exec "$POD" -- ls -l /requests/datasets
