#!/usr/bin/env bash
# Put the plant in a run's starting state (benchmark/protocol/5-gpu-evaluation.md §5.2).
#
#   arm.sh <namespace> <scaledobject> pin <replicas>    pause autoscaling at <replicas>, wait Ready
#   arm.sh <namespace> <scaledobject> active [<window>] pin at minReplicaCount, wait <window> s
#                                                       (default 90) for rate windows to empty,
#                                                       then resume autoscaling
#   arm.sh <namespace> <scaledobject> zero [<window>]   pin at 0, wait, resume (minReplicaCount 0)
#   arm.sh <namespace> <scaledobject> status            print spec, Ready and the pause annotation
#
# Uses KEDA's autoscaling.keda.sh/paused-replicas annotation; while it is set KEDA
# holds the target at the given count and the HPA computes nothing.
# KUBECTL may carry --context, e.g. KUBECTL="kubectl --context foo".
set -euo pipefail

NS=${1:?usage: arm.sh <namespace> <scaledobject> pin|active|zero|status [arg]}
SO=${2:?scaledobject name}
CMD=${3:?pin|active|zero|status}
ARG=${4:-}
K=${KUBECTL:-kubectl}
PAUSE=autoscaling.keda.sh/paused-replicas

target() {
  $K -n "$NS" get scaledobject "$SO" \
    -o jsonpath='{.spec.scaleTargetRef.kind}/{.spec.scaleTargetRef.name}'
}

field() { $K -n "$NS" get "$(target)" -o jsonpath="$1"; }

wait_ready() {  # <replicas>
  local want=$1 ready spec
  for _ in $(seq 1 180); do
    ready=$(field '{.status.readyReplicas}')
    spec=$(field '{.spec.replicas}')
    if [[ "${ready:-0}" == "$want" && "$spec" == "$want" ]]; then
      echo "$(target): $want Ready"
      return 0
    fi
    sleep 10
  done
  echo "timed out waiting for $want Ready (spec ${spec:-?}, Ready ${ready:-0})" >&2
  return 1
}

pin() {  # <replicas>
  $K -n "$NS" annotate scaledobject "$SO" --overwrite "$PAUSE=$1" >/dev/null
  wait_ready "$1"
}

resume() {
  $K -n "$NS" annotate scaledobject "$SO" "$PAUSE-" >/dev/null
  echo "$SO: autoscaling resumed"
}

case $CMD in
  pin) pin "${ARG:?replicas}" ;;
  active)
    pin "$($K -n "$NS" get scaledobject "$SO" -o jsonpath='{.spec.minReplicaCount}')"
    sleep "${ARG:-90}"
    resume ;;
  zero)
    pin 0
    sleep "${ARG:-90}"
    resume ;;
  status)
    echo "target $(target): spec $(field '{.spec.replicas}'), Ready $(field '{.status.readyReplicas}')"
    echo "paused-replicas: $($K -n "$NS" get scaledobject "$SO" \
      -o jsonpath="{.metadata.annotations.autoscaling\.keda\.sh/paused-replicas}")" ;;
  *) echo "unknown command $CMD" >&2; exit 2 ;;
esac
