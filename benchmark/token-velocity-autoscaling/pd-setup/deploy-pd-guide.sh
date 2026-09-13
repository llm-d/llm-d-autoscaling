#!/usr/bin/env bash
#
# deploy-pd-guide.sh — deploy the UPSTREAM llm-d P/D disaggregation guide, reproducibly.
#
#   https://github.com/llm-d/llm-d/tree/main/guides/pd-disaggregation
#
# This follows the guide's own installation path — `helm install` of the
# llm-d-router-standalone chart, then a kustomize overlay for the model server —
# rather than going through llm-d-benchmark's `llmdbenchmark standup`. Nothing
# about the guide's structure is bypassed: the router values, the EPP plugin
# config, the vLLM args, the NIXL KV-transfer config, the routing sidecar and the
# guide's own labels all come from the checkout, unedited.
#
# ---------------------------------------------------------------------------
# TWO DEVIATIONS FROM THE GUIDE, AND WHY
# ---------------------------------------------------------------------------
#
# 1. A LOCAL OVERLAY IS REQUIRED — the guide's own instruction cannot work.
#
#    The guide says:
#        export INFRA_PROVIDER=base
#        kubectl apply -n ${NAMESPACE} -k .../modelserver/gpu/vllm/${INFRA_PROVIDER}
#
#    But `modelserver/gpu/vllm/base` renders a literal placeholder:
#        image: REPLACE_MODEL_SERVER_IMAGE
#    which the API server rejects. base/kustomization.yaml says so on purpose:
#        "the model server image component is intentionally not set here.
#         Each overlay sets its own (e.g. gpu-vllm/release, gpu-vllm/aws-efa)"
#    Every sibling overlay (coreweave, aws, gke/base) adds
#    `components/images/gpu-vllm/release`; `base` does not, and there is no OCP
#    or generic-cluster overlay upstream. So this script generates the missing
#    overlay: base + the release image component + the topology below. Verify
#    with `kustomize build llm-d/guides/pd-disaggregation/modelserver/gpu/vllm/base
#    | grep REPLACE`.
#
# 2. MODEL AND TOPOLOGY ARE SCALED TO THIS CLUSTER'S GPU BUDGET.
#
#      guide default                    this script
#      -------------                    -----------
#      openai/gpt-oss-120b              Qwen/Qwen3-32B
#      prefill 8 x TP=1  =  8 GPU       prefill 1 x TP=2  = 2 GPU
#      decode  2 x TP=4  =  8 GPU       decode  1 x TP=2  = 2 GPU
#      total            = 16 GPU        total             = 4 GPU
#
#    Everything else in the pod spec — block size, kv-transfer-config, probes,
#    cpu/memory, env, volumes, the sidecar — is left at the guide's values, so
#    what is being tested stays the guide and not a rewrite of it. Each of those
#    is exposed as an environment variable below if it needs to move.
#
# NOT IN SCOPE: KEDA ScaledObjects, calibrate.sh, benchmarking. This script
# deploys the guide, enables monitoring, and verifies it serves. The EPP keeps the guide's
# peakPrefillThroughput=33821, which upstream measured on H200/gpt-oss-120b and
# which is therefore NOT the right figure for Qwen3-32B on H100 — it affects
# prefix-affinity routing decisions, not whether the stack works.
#
# ---------------------------------------------------------------------------
# USAGE
# ---------------------------------------------------------------------------
#   ./deploy-pd-guide.sh                 # full cycle: clean ns -> deploy -> verify
#   ./deploy-pd-guide.sh --dry-run       # preflight + render + assert, apply nothing
#   ./deploy-pd-guide.sh --render-only   # render + assert only, no cluster needed
#   ./deploy-pd-guide.sh --skip-clean    # deploy into the namespace as it is
#   ./deploy-pd-guide.sh --verify-only   # re-run verification against a live stack
#   ./deploy-pd-guide.sh --teardown      # helm uninstall + delete namespace
#   ./deploy-pd-guide.sh --namespace NS  # default pd-test
#   ./deploy-pd-guide.sh --ref COMMIT    # llm-d checkout ref, default main
#   ./deploy-pd-guide.sh --model-cache   # share one downloaded copy of the weights via
#                                        # a PVC instead of every pod downloading its own
#                                        # (see MODEL_CACHE_* below). Scoped to $NAMESPACE:
#                                        # a full clean run (the default) still deletes it
#                                        # along with the namespace; use --skip-clean to
#                                        # reuse a populated cache across reruns.
#
# Environment overrides (all optional):
#   MODEL PREFILL_REPLICAS PREFILL_TP DECODE_REPLICAS DECODE_TP
#   PREFILL_CPU PREFILL_MEM DECODE_CPU DECODE_MEM
#   PREFILL_GPU_MEM_UTIL  (default 0.85; headroom for cold-start CUDA graph capture)
#   HF_TOKEN  ROLLOUT_TIMEOUT
#   MODEL_CACHE (default false)  MODEL_CACHE_PVC_NAME  MODEL_CACHE_PVC_SIZE (default 100Gi)
#   MODEL_CACHE_STORAGE_CLASS  MODEL_CACHE_MOUNT (default /model-cache)
#   MODEL_DOWNLOAD_IMAGE (default python:3.12-slim)  MODEL_CACHE_DOWNLOAD_TIMEOUT (default 2400)
#
set -uo pipefail

cd "$(dirname "$0")" || exit 1

# ---------------------------------------------------------------------------
# configuration
# ---------------------------------------------------------------------------
NAMESPACE="${NAMESPACE:-pd-test}"
GUIDE_NAME="pd-disaggregation"
LLMD_REPO="https://github.com/llm-d/llm-d.git"
# Where the llm-d checkout lives. Overridable so the folder can be dropped next to an
# existing checkout instead of cloning a second copy: LLMD_DIR=/path/to/llm-d ./deploy-pd-guide.sh
# Default: ./llm-d inside this folder, falling back to ../llm-d when that already exists
# (the layout in the repo this folder was extracted from).
if [[ -z ${LLMD_DIR:-} ]]; then
  if [[ -d "${PWD}/llm-d/.git" ]]; then LLMD_DIR="${PWD}/llm-d"
  elif [[ -d "${PWD}/../llm-d/.git" ]]; then LLMD_DIR="$(cd "${PWD}/../llm-d" && pwd)"
  else LLMD_DIR="${PWD}/llm-d"; fi
fi
LLMD_REF="${LLMD_REF:-main}"

MODEL="${MODEL:-Qwen/Qwen3-32B}"
PREFILL_REPLICAS="${PREFILL_REPLICAS:-1}"
PREFILL_TP="${PREFILL_TP:-2}"
DECODE_REPLICAS="${DECODE_REPLICAS:-1}"
DECODE_TP="${DECODE_TP:-2}"

# Left empty => keep whatever the guide sets. Set to override.
PREFILL_CPU="${PREFILL_CPU:-}"
PREFILL_MEM="${PREFILL_MEM:-}"
DECODE_CPU="${DECODE_CPU:-}"
DECODE_MEM="${DECODE_MEM:-}"

# The guide's base overlay sets no --gpu-memory-utilization, so vLLM uses its
# own default (~0.9). That leaves too little headroom for a fresh replica's
# CUDA-graph capture on cold start: a prefill scale-out under
# launch-scaledobjects.sh has been observed OOMing at startup with ~77.2GiB/79.18GiB
# already in use before graph capture even runs. Cap it below the default so a
# KEDA-triggered scale-out has room to start. Set to empty to keep the guide's default.
PREFILL_GPU_MEM_UTIL="${PREFILL_GPU_MEM_UTIL:-0.85}"

# Opt-in shared model-weight cache (mirrors llm-d-benchmark's PVC + one-time download
# Job pattern) so pods stop re-downloading the same weights via the guide's emptyDir.
# Off by default: the guide's own behavior is left exactly as-is unless requested.
MODEL_CACHE="${MODEL_CACHE:-false}"
MODEL_CACHE_PVC_NAME="${MODEL_CACHE_PVC_NAME:-model-cache}"
MODEL_CACHE_PVC_SIZE="${MODEL_CACHE_PVC_SIZE:-100Gi}"          # ~65GB weights + headroom
MODEL_CACHE_STORAGE_CLASS="${MODEL_CACHE_STORAGE_CLASS:-}"     # "" = cluster default
MODEL_CACHE_MOUNT="${MODEL_CACHE_MOUNT:-/model-cache}"
MODEL_DOWNLOAD_IMAGE="${MODEL_DOWNLOAD_IMAGE:-python:3.12-slim}"
MODEL_CACHE_DOWNLOAD_TIMEOUT="${MODEL_CACHE_DOWNLOAD_TIMEOUT:-2400}"
MODEL_PATH="models/${MODEL}"                                  # local cache layout

WORKDIR="${WORKDIR:-${PWD}/.pd-guide-workspace}"
OVERLAY_DIR="${WORKDIR}/overlay"
ROLLOUT_TIMEOUT="${ROLLOUT_TIMEOUT:-2400}"   # first start pulls a ~15GB image and downloads model weights
CURL_IMAGE="${CURL_IMAGE:-cfmanteiga/alpine-bash-curl-jq}"

DRY_RUN=false
RENDER_ONLY=false
SKIP_CLEAN=false
VERIFY_ONLY=false
TEARDOWN=false

# ---------------------------------------------------------------------------
# output helpers
# ---------------------------------------------------------------------------
if [[ -t 2 ]]; then B=$'\033[1m'; R=$'\033[31m'; G=$'\033[32m'; Y=$'\033[33m'; C=$'\033[36m'; Z=$'\033[0m'
else B=; R=; G=; Y=; C=; Z=; fi

stage()  { printf '\n%s══ %s %s\n' "$C$B" "$*" "$Z" >&2; }
info()   { printf '   %s\n' "$*" >&2; }
ok()     { printf '   %sPASS%s  %s\n' "$G" "$Z" "$*" >&2; }
warn()   { printf '   %sWARN%s  %s\n' "$Y" "$Z" "$*" >&2; }
die()    { printf '\n   %sFAIL%s  %s\n\n' "$R" "$Z" "$*" >&2; exit 1; }
run()    { info "\$ $*"; if [[ $DRY_RUN == true ]]; then return 0; fi; "$@"; }

usage() { sed -n '2,/^set -uo/p' "$0" | sed 's/^# \{0,1\}//' | head -n -1; exit 0; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run)      DRY_RUN=true; shift ;;
    --render-only)  RENDER_ONLY=true; DRY_RUN=true; shift ;;
    --skip-clean)   SKIP_CLEAN=true; shift ;;
    --verify-only)  VERIFY_ONLY=true; SKIP_CLEAN=true; shift ;;
    --teardown)     TEARDOWN=true; shift ;;
    --namespace|-n) NAMESPACE="${2:?--namespace needs a value}"; shift 2 ;;
    --ref)          LLMD_REF="${2:?--ref needs a value}"; shift 2 ;;
    --model)        MODEL="${2:?--model needs a value}"; MODEL_PATH="models/${MODEL}"; shift 2 ;;
    --model-cache)  MODEL_CACHE=true; shift ;;
    -h|--help)      usage ;;
    *)              die "unknown argument: $1  (try --help)" ;;
  esac
done

TOTAL_GPUS=$(( PREFILL_REPLICAS * PREFILL_TP + DECODE_REPLICAS * DECODE_TP ))
mkdir -p "$WORKDIR"
LOG="${WORKDIR}/deploy.$(date +%Y%m%d-%H%M%S).log"

# =========================================================================
# STAGE 0 — preflight
# =========================================================================
preflight() {
  stage "STAGE 0  preflight"

  for t in kubectl helm kustomize python3 git; do
    command -v "$t" >/dev/null 2>&1 || die "missing required tool: $t"
  done
  ok "client tools present: kubectl helm kustomize python3 git"

  # Credentials. An expired OCP token surfaces as "the server has asked for the
  # client to provide credentials" from every subsequent call, so fail here with
  # something actionable instead of 40 lines of memcache noise.
  kubectl get ns default >/dev/null 2>&1 \
    || die "not authenticated to the cluster. Run your 'oc login ...' and retry."
  local who
  who=$(oc whoami 2>/dev/null || kubectl config view --minify -o jsonpath='{.contexts[0].context.user}' 2>/dev/null || echo "unknown")
  ok "authenticated as ${who} @ $(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')"

  # Cluster-scoped prerequisites: asserted, never installed. The standalone
  # router chart renders an InferencePool (inference.networking.k8s.io/v1), so a
  # missing GAIE CRD makes `helm install` fail halfway with a partial release.
  if ! kubectl get crd inferencepools.inference.networking.k8s.io >/dev/null 2>&1; then
    die "GAIE CRDs missing. This script does not install cluster-scoped resources.
         Someone with cluster-admin must run:
           kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/latest/download/v1-manifests.yaml"
  fi
  local pool_versions
  pool_versions=$(kubectl get crd inferencepools.inference.networking.k8s.io \
                    -o jsonpath='{range .spec.versions[*]}{.name}{" "}{end}' 2>/dev/null)
  [[ $pool_versions == *v1* ]] \
    || die "InferencePool CRD is served at [${pool_versions}] but the chart needs v1. Upgrade the GAIE CRDs."
  ok "InferencePool CRD present, versions: ${pool_versions}"

  # The decode pod's routing sidecar is a native sidecar (initContainer with
  # restartPolicy: Always), which needs the SidecarContainers feature: beta and
  # on by default in k8s 1.29, GA in 1.33. On an older server the field is
  # dropped and the init container runs to completion, so the pod never starts.
  local kmajor kminor
  kmajor=$(kubectl version -o json 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin)["serverVersion"]["major"])' 2>/dev/null)
  kminor=$(kubectl version -o json 2>/dev/null | python3 -c 'import json,sys; import re; print(re.sub(r"[^0-9]","",json.load(sys.stdin)["serverVersion"]["minor"]))' 2>/dev/null)
  if [[ -n ${kminor:-} ]]; then
    if (( kmajor > 1 || kminor >= 29 )); then
      ok "server is k8s ${kmajor}.${kminor} — native sidecar containers supported"
    else
      die "server is k8s ${kmajor}.${kminor}; the guide's decode routing sidecar needs >= 1.29"
    fi
  else
    warn "could not read the server version; assuming native sidecars are supported"
  fi

  # Namespace-scoped authority in the target namespace.
  if [[ $DRY_RUN == false ]]; then
    kubectl auth can-i create deployments -n "$NAMESPACE" >/dev/null 2>&1 \
      || die "cannot create deployments in namespace ${NAMESPACE}"
    ok "can create workloads in ${NAMESPACE}"
  fi

  # GPU capacity. Tensor parallelism is intra-node: a TP=N pod needs N free GPUs
  # on ONE node, so a cluster-wide free count can look sufficient while nothing
  # schedules. This computes free GPUs per node and bin-packs the requested pods.
  if [[ $VERIFY_ONLY == false ]]; then
    check_gpu_capacity
  fi
}

check_gpu_capacity() {
  local nodes pods
  nodes=$(kubectl get nodes -o json 2>/dev/null) || die "cannot list nodes"
  pods=$(kubectl get pods -A --field-selector=status.phase!=Succeeded,status.phase!=Failed -o json 2>/dev/null) \
    || die "cannot list pods (needed to compute free GPUs)"

  printf '%s' "$nodes" > "${WORKDIR}/nodes.json"
  printf '%s' "$pods"  > "${WORKDIR}/pods.json"

  python3 - "$PREFILL_REPLICAS" "$PREFILL_TP" "$DECODE_REPLICAS" "$DECODE_TP" \
           "${WORKDIR}/nodes.json" "${WORKDIR}/pods.json" <<'PY' >&2
import json, sys
pr, ptp, dr, dtp = (int(x) for x in sys.argv[1:5])
nodes = json.load(open(sys.argv[5]))
pods  = json.load(open(sys.argv[6]))
GPU = "nvidia.com/gpu"

free, eph = {}, {}
for n in nodes["items"]:
    name = n["metadata"]["name"]
    alloc = n.get("status", {}).get("allocatable", {})
    if GPU not in alloc:
        continue
    ready = any(c["type"] == "Ready" and c["status"] == "True"
                for c in n.get("status", {}).get("conditions", []))
    sched = not n.get("spec", {}).get("unschedulable", False)
    if not (ready and sched):
        continue
    free[name] = int(alloc[GPU])
    eph[name] = alloc.get("ephemeral-storage", "?")

for p in pods["items"]:
    node = p.get("spec", {}).get("nodeName")
    if node not in free:
        continue
    for c in p["spec"].get("containers", []):
        req = c.get("resources", {}).get("requests", {}) or {}
        lim = c.get("resources", {}).get("limits", {}) or {}
        free[node] -= int(req.get(GPU, lim.get(GPU, 0)) or 0)

# bin-pack: schedule the biggest TP first, it is the hardest to place
want = [("decode", dtp)] * dr + [("prefill", ptp)] * pr
want.sort(key=lambda x: -x[1])
avail = dict(free)
placed, failed = [], []
for role, tp in want:
    fit = sorted((v, k) for k, v in avail.items() if v >= tp)
    if not fit:
        failed.append((role, tp)); continue
    _, node = fit[0]          # tightest fit that still holds it
    avail[node] -= tp
    placed.append((role, tp, node))

total_free = sum(v for v in free.values() if v > 0)
print(f"   GPU nodes: {len(free)}, free GPUs cluster-wide: {total_free}")
for k in sorted(free, key=lambda k: -free[k])[:8]:
    if free[k] > 0:
        print(f"     {k:<48} free={free[k]:<3} ephemeral-storage={eph[k]}")
for role, tp, node in placed:
    print(f"   place {role:<8} TP={tp} -> {node}")
if failed:
    for role, tp in failed:
        print(f"   NO NODE with {tp} free GPUs for {role} (TP is intra-node)")
    sys.exit(1)
PY
  # shellcheck disable=SC2181
  if [[ $? -ne 0 ]]; then
    die "insufficient free GPUs on a single node for the requested topology (${TOTAL_GPUS} GPU total)"
  fi
  ok "topology fits: prefill ${PREFILL_REPLICAS}xTP${PREFILL_TP} + decode ${DECODE_REPLICAS}xTP${DECODE_TP} = ${TOTAL_GPUS} GPU"

  # The guide mounts emptyDir at /.cache, so every pod downloads its own copy of
  # the weights to node ephemeral storage. Qwen3-32B is ~65GB per pod.
  if [[ $MODEL_CACHE == true ]]; then
    ok "model cache enabled: pods will share one downloaded copy via PVC ${MODEL_CACHE_PVC_NAME}"
  else
    warn "the guide uses no model PVC: each of the $((PREFILL_REPLICAS + DECODE_REPLICAS)) pods downloads its own"
    warn "  copy of ${MODEL} (~65GB) to node ephemeral storage via the emptyDir at /.cache"
    warn "  use --model-cache to share one download across pods instead"
  fi
}

# =========================================================================
# STAGE 1 — llm-d checkout at a known ref
# =========================================================================
checkout_repo() {
  stage "STAGE 1  llm-d checkout @ ${LLMD_REF}"

  if [[ ! -d "${LLMD_DIR}/.git" ]]; then
    info "cloning ${LLMD_REPO} into ${LLMD_DIR}"
    [[ $DRY_RUN == true ]] || git clone --quiet "$LLMD_REPO" "$LLMD_DIR" \
      || die "git clone failed"
  fi

  if [[ $DRY_RUN == false ]]; then
    git -C "$LLMD_DIR" fetch --quiet origin "$LLMD_REF" 2>/dev/null \
      || git -C "$LLMD_DIR" fetch --quiet origin \
      || warn "git fetch failed; using the checkout as-is"
    # Detach so the tree is exactly the requested ref, whatever was there before.
    git -C "$LLMD_DIR" checkout --quiet --detach FETCH_HEAD 2>/dev/null \
      || git -C "$LLMD_DIR" checkout --quiet --detach "$LLMD_REF" \
      || die "cannot check out ref ${LLMD_REF}"
    local dirty
    dirty=$(git -C "$LLMD_DIR" status --porcelain | head -5)
    [[ -z $dirty ]] || { warn "checkout has local modifications:"; printf '     %s\n' "$dirty" >&2; }
  fi

  REPO_ROOT="$LLMD_DIR"
  export REPO_ROOT
  # shellcheck source=/dev/null
  source "${REPO_ROOT}/guides/env.sh" || die "cannot source guides/env.sh"

  GUIDE_DIR="${REPO_ROOT}/guides/${GUIDE_NAME}"
  [[ -d $GUIDE_DIR ]] || die "guide directory not found: ${GUIDE_DIR}"

  info "commit:              $(git -C "$LLMD_DIR" log -1 --format='%h %ad %s' --date=short 2>/dev/null)"
  info "ROUTER_CHART:        ${ROUTER_STANDALONE_CHART} @ ${ROUTER_CHART_VERSION}"
  info "GAIE CRD source:     ${GAIE_URL}"
  ok "guide checked out"
}

# =========================================================================
# STAGE 2 — clean namespace
# =========================================================================
clean_namespace() {
  stage "STAGE 2  clean namespace ${NAMESPACE}"

  if [[ $SKIP_CLEAN == true ]]; then
    warn "--skip-clean: leaving existing namespace contents in place"
    run kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml \
      > "${WORKDIR}/ns.yaml" 2>/dev/null
    [[ $DRY_RUN == true ]] || kubectl apply -f "${WORKDIR}/ns.yaml" >/dev/null
    return 0
  fi

  if kubectl get namespace "$NAMESPACE" >/dev/null 2>&1; then
    info "namespace exists; deleting it so the guide deploys onto nothing"
    if [[ $DRY_RUN == false ]]; then
      # helm uninstall first: leaving the release record behind makes a later
      # `helm install` of the same name fail even though the objects are gone.
      helm uninstall "$GUIDE_NAME" -n "$NAMESPACE" --wait --timeout 3m >/dev/null 2>&1 || true
      kubectl delete namespace "$NAMESPACE" --wait=false >/dev/null 2>&1 || true
      local waited=0
      while kubectl get namespace "$NAMESPACE" >/dev/null 2>&1; do
        (( waited >= 600 )) && die "namespace ${NAMESPACE} stuck terminating after 600s (check finalizers)"
        sleep 5; waited=$((waited + 5))
        (( waited % 30 == 0 )) && info "  still terminating (${waited}s)"
      done
      info "namespace deleted after ${waited}s"
    fi
  else
    info "namespace does not exist yet"
  fi

  if [[ $DRY_RUN == false ]]; then
    kubectl create namespace "$NAMESPACE" >/dev/null || die "cannot create namespace ${NAMESPACE}"
  fi
  ok "namespace ${NAMESPACE} is clean"
}

# =========================================================================
# STAGE 3 — HF token secret (guide prerequisite)
# =========================================================================
create_hf_secret() {
  stage "STAGE 3  llm-d-hf-token secret"

  local tok="${HF_TOKEN:-${LLMDBENCH_HF_TOKEN:-}}"
  if [[ -z $tok ]]; then
    # The deployments reference the secret unconditionally, so it must exist or
    # every pod fails with CreateContainerConfigError. Qwen3-32B is not gated,
    # so an empty value is enough to pull it.
    warn "HF_TOKEN not set; creating the secret with an empty value"
    warn "  ${MODEL} is not gated so this is fine, but a gated model would 401"
    tok=""
  fi

  if [[ $DRY_RUN == false ]]; then
    kubectl create secret generic llm-d-hf-token \
      --from-literal="HF_TOKEN=${tok}" -n "$NAMESPACE" \
      --dry-run=client -o yaml | kubectl apply -f - >/dev/null \
      || die "cannot create llm-d-hf-token secret"
  fi
  ok "secret llm-d-hf-token present in ${NAMESPACE}"
}

# =========================================================================
# STAGE 4 — shared model-weight cache (opt-in, --model-cache)
# =========================================================================
provision_model_cache() {
  stage "STAGE 4  model cache PVC (optional)"

  if [[ $MODEL_CACHE != true ]]; then
    info "--model-cache not set; pods will download their own copies via the guide's emptyDir"
    return 0
  fi

  cat > "${WORKDIR}/model-cache-pvc.yaml" <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${MODEL_CACHE_PVC_NAME}
  namespace: ${NAMESPACE}
spec:
  accessModes:
    - ReadWriteMany
  resources:
    requests:
      storage: ${MODEL_CACHE_PVC_SIZE}
EOF
  [[ -n $MODEL_CACHE_STORAGE_CLASS ]] \
    && printf '  storageClassName: %s\n' "$MODEL_CACHE_STORAGE_CLASS" >> "${WORKDIR}/model-cache-pvc.yaml"

  run kubectl apply -n "$NAMESPACE" -f "${WORKDIR}/model-cache-pvc.yaml" \
    || die "cannot create model cache PVC ${MODEL_CACHE_PVC_NAME}"
  ok "PVC ${MODEL_CACHE_PVC_NAME} requested (${MODEL_CACHE_PVC_SIZE}, RWX)"

  # Idempotent across --skip-clean reruns: a full clean run deletes the namespace
  # (and this PVC/Job with it), so this only ever matters when the namespace survived.
  if [[ $DRY_RUN == false ]] && kubectl get job model-cache-download -n "$NAMESPACE" >/dev/null 2>&1; then
    local succeeded
    succeeded=$(kubectl get job model-cache-download -n "$NAMESPACE" -o jsonpath='{.status.succeeded}' 2>/dev/null)
    if [[ $succeeded == "1" ]]; then
      ok "model-cache-download already completed; reusing the PVC without re-downloading"
      return 0
    fi
    info "a previous download job exists but did not complete; deleting it and retrying"
    kubectl delete job model-cache-download -n "$NAMESPACE" --wait=true --timeout=60s >/dev/null 2>&1 || true
  fi

  cat > "${WORKDIR}/model-cache-download-job.yaml" <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: model-cache-download
  namespace: ${NAMESPACE}
spec:
  backoffLimit: 2
  template:
    spec:
      restartPolicy: OnFailure
      containers:
        - name: downloader
          image: ${MODEL_DOWNLOAD_IMAGE}
          # A :latest tag plus the default imagePullPolicy: Always would re-pull the
          # image every run; llm-d-benchmark hit exactly this and had a download job
          # time out mid-image-pull before transferring a single byte of weights.
          imagePullPolicy: IfNotPresent
          command: ["/bin/sh", "-c"]
          args:
            - |
              set -e
              mkdir -p "\${MOUNT_PATH}/\${MODEL_PATH}"
              pip install -q -U --user huggingface_hub
              export PATH="\${PATH}:\${HOME}/.local/bin"
              if [ -n "\${HF_TOKEN:-}" ]; then
                hf auth login --token "\${HF_TOKEN}"
              fi
              hf download "\${HF_MODEL_ID}" --local-dir "\${MOUNT_PATH}/\${MODEL_PATH}"
          env:
            - name: MODEL_PATH
              value: ${MODEL_PATH}
            - name: HF_MODEL_ID
              value: ${MODEL}
            - name: HF_TOKEN
              valueFrom:
                secretKeyRef:
                  name: llm-d-hf-token
                  key: HF_TOKEN
                  optional: true
            - name: HF_HOME
              value: /tmp/huggingface
            - name: HOME
              value: /tmp
            - name: MOUNT_PATH
              value: ${MODEL_CACHE_MOUNT}
          volumeMounts:
            - name: model-cache
              mountPath: ${MODEL_CACHE_MOUNT}
      volumes:
        - name: model-cache
          persistentVolumeClaim:
            claimName: ${MODEL_CACHE_PVC_NAME}
EOF

  run kubectl apply -n "$NAMESPACE" -f "${WORKDIR}/model-cache-download-job.yaml" \
    || die "cannot create model-cache-download job"

  if [[ $DRY_RUN == true ]]; then
    warn "--dry-run: not waiting for the download job"
    return 0
  fi

  info "waiting up to ${MODEL_CACHE_DOWNLOAD_TIMEOUT}s for ${MODEL} to download to the PVC"
  if ! kubectl wait --for=condition=complete job/model-cache-download -n "$NAMESPACE" \
        --timeout="${MODEL_CACHE_DOWNLOAD_TIMEOUT}s" >/dev/null 2>&1; then
    kubectl logs -n "$NAMESPACE" job/model-cache-download --tail=80 >&2 2>/dev/null
    die "model-cache-download job did not complete within ${MODEL_CACHE_DOWNLOAD_TIMEOUT}s"
  fi
  ok "weights for ${MODEL} downloaded to PVC ${MODEL_CACHE_PVC_NAME} at ${MODEL_CACHE_MOUNT}/${MODEL_PATH}"
}

# =========================================================================
# STAGE 5 — router (guide step 1, standalone mode, values verbatim)
# =========================================================================
install_router() {
  stage "STAGE 5  llm-d Router (standalone)"

  local base_values="${REPO_ROOT}/guides/recipes/router/base.values.yaml"
  local guide_values="${GUIDE_DIR}/router/${GUIDE_NAME}.values.yaml"
  [[ -f $base_values  ]] || die "missing ${base_values}"
  [[ -f $guide_values ]] || die "missing ${guide_values}"

  # Record what the EPP will actually run with, so a later "why did it route
  # that way" question has an artifact to read instead of a guess.
  if [[ $DRY_RUN == false ]]; then
    helm template "$GUIDE_NAME" "$ROUTER_STANDALONE_CHART" \
      -f "$base_values" -f "$guide_values" \
      -n "$NAMESPACE" --version "$ROUTER_CHART_VERSION" \
      > "${WORKDIR}/router.rendered.yaml" 2>/dev/null \
      || die "helm template of the router chart failed"
    python3 - "${WORKDIR}/router.rendered.yaml" "${WORKDIR}/pd-config.yaml" <<'PY' >&2
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
cfg = None
for d in docs:
    if d.get("kind") == "ConfigMap":
        for k, v in (d.get("data") or {}).items():
            if k.endswith("pd-config.yaml"):
                cfg = v
if cfg is None:
    print("   WARN  no pd-config.yaml in the rendered chart"); sys.exit(0)
open(sys.argv[2], "w").write(cfg)
parsed = yaml.safe_load(cfg)
plugins = [p["type"] for p in parsed.get("plugins", [])]
print(f"   EPP plugins ({len(plugins)}): {', '.join(plugins)}")
for p in parsed.get("plugins", []):
    ppt = (p.get("parameters") or {}).get("peakPrefillThroughput")
    if ppt is not None:
        print(f"   peakPrefillThroughput = {ppt}   (guide value, measured upstream on H200/gpt-oss-120b)")
for prof in parsed.get("schedulingProfiles", []):
    refs = [x["pluginRef"] for x in prof.get("plugins", [])]
    print(f"   profile {prof['name']:<8} {' -> '.join(refs)}")
PY
  fi

  # The guide's install command, unmodified.
  if helm status "$GUIDE_NAME" -n "$NAMESPACE" >/dev/null 2>&1; then
    info "release ${GUIDE_NAME} already exists; upgrading in place"
    run helm upgrade "$GUIDE_NAME" "$ROUTER_STANDALONE_CHART" \
      -f "$base_values" -f "$guide_values" \
      -n "$NAMESPACE" --version "$ROUTER_CHART_VERSION" --wait --timeout 5m \
      || die "helm upgrade failed"
  else
    run helm install "$GUIDE_NAME" "$ROUTER_STANDALONE_CHART" \
      -f "$base_values" -f "$guide_values" \
      -n "$NAMESPACE" --version "$ROUTER_CHART_VERSION" --wait --timeout 5m \
      || die "helm install of the router failed"
  fi
  ok "router installed"
}

# =========================================================================
# STAGE 5 — model server overlay: generate, assert, apply (guide step 2)
# =========================================================================
BASE_OVERLAY=
render_overlay() {
  stage "STAGE 6  model server overlay"

  BASE_OVERLAY="${GUIDE_DIR}/modelserver/gpu/vllm/base"
  [[ -d $BASE_OVERLAY ]] || die "missing ${BASE_OVERLAY}"

  # Prove the deviation is necessary rather than asserting it in a comment.
  if kustomize build "$BASE_OVERLAY" 2>/dev/null | grep -q 'REPLACE_MODEL_SERVER_IMAGE'; then
    info "confirmed: 'kubectl apply -k .../vllm/base' would apply image: REPLACE_MODEL_SERVER_IMAGE"
    info "           -> layering components/images/gpu-vllm/release, as every sibling overlay does"
  else
    warn "the base overlay no longer emits REPLACE_MODEL_SERVER_IMAGE — upstream may have fixed this;"
    warn "  re-read the guide before trusting this script's overlay"
  fi

  mkdir -p "$OVERLAY_DIR"
  kustomize build "$BASE_OVERLAY" > "${WORKDIR}/base.rendered.yaml" 2>/dev/null \
    || die "kustomize build of the guide's base overlay failed"

  # Discover the deployment names (the base applies a namePrefix) and the index
  # of --tensor-parallel-size inside args, so the JSON patches below do not
  # depend on upstream keeping its current argument order.
  python3 - "${WORKDIR}/base.rendered.yaml" > "${WORKDIR}/targets.env" <<'PY' || die "cannot inspect the base render"
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
out = {}
for d in docs:
    if d.get("kind") != "Deployment":
        continue
    name = d["metadata"]["name"]
    role = "PREFILL" if name.endswith("prefill") else "DECODE" if name.endswith("decode") else None
    if not role:
        continue
    c = d["spec"]["template"]["spec"]["containers"][0]
    args = c.get("args", [])
    tp = [i for i, a in enumerate(args) if str(a).startswith("--tensor-parallel-size")]
    if not tp:
        sys.exit(f"no --tensor-parallel-size arg on {name}")
    out[f"{role}_NAME"] = name
    out[f"{role}_TP_IDX"] = tp[0]
    out[f"{role}_MODEL_IDX"] = 0     # vllm serve <model> is positional
    out[f"{role}_BASE_MODEL"] = args[0]
for k in ("PREFILL_NAME", "DECODE_NAME"):
    if k not in out:
        sys.exit(f"could not find the {k.split('_')[0].lower()} Deployment in the base render")
for k, v in out.items():
    print(f"{k}={v}")
PY
  # shellcheck source=/dev/null
  source "${WORKDIR}/targets.env"
  info "base deployments:    ${PREFILL_NAME}, ${DECODE_NAME}"
  info "guide's model:       ${PREFILL_BASE_MODEL}  ->  ${MODEL}"

  # relpath must be computed from RESOLVED paths: kustomize resolves symlinks before
  # loading a resource, so a logical relative path breaks wherever a symlinked parent is
  # involved (on macOS /tmp -> /private/tmp, which turned ../../../../Users/... into
  # /private/Users/... and failed with "evalsymlink failure").
  local relpath_py='import os,sys; print(os.path.relpath(os.path.realpath(sys.argv[1]), os.path.realpath(sys.argv[2])))'
  local rel_base rel_component
  rel_base=$(python3 -c "$relpath_py" "$BASE_OVERLAY" "$OVERLAY_DIR")
  rel_component=$(python3 -c "$relpath_py" \
                "${REPO_ROOT}/guides/recipes/modelserver/components/images/gpu-vllm/release" "$OVERLAY_DIR")

  {
    cat <<EOF
# GENERATED by deploy-pd-guide.sh — do not edit, it is rewritten every run.
#
# The guide's own overlay (${rel_base}) plus:
#   * the model server image component, which base deliberately omits and every
#     sibling overlay (coreweave/aws/gke) supplies
#   * this cluster's topology: ${MODEL}, prefill ${PREFILL_REPLICAS}xTP${PREFILL_TP}, decode ${DECODE_REPLICAS}xTP${DECODE_TP}
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
  - ${rel_base}

components:
  - ${rel_component}

patches:
EOF
    _role_patch prefill "$PREFILL_NAME" "$PREFILL_REPLICAS" "$PREFILL_TP" \
                "$PREFILL_TP_IDX" "$PREFILL_CPU" "$PREFILL_MEM" "$PREFILL_GPU_MEM_UTIL"
    _role_patch decode  "$DECODE_NAME"  "$DECODE_REPLICAS"  "$DECODE_TP" \
                "$DECODE_TP_IDX"  "$DECODE_CPU"  "$DECODE_MEM" ""
  } > "${OVERLAY_DIR}/kustomization.yaml"

  kustomize build "$OVERLAY_DIR" > "${WORKDIR}/modelserver.rendered.yaml" \
    || die "kustomize build of the generated overlay failed"
  ok "overlay rendered -> ${WORKDIR}/modelserver.rendered.yaml"

  assert_render
}

# emit one JSON6902 patch block for a role
_role_patch() {
  local role="$1" name="$2" replicas="$3" tp="$4" tpidx="$5" cpu="$6" mem="$7" gpu_mem_util="${8:-}"
  local model_arg="${MODEL}"
  [[ $MODEL_CACHE == true ]] && model_arg="${MODEL_CACHE_MOUNT}/${MODEL_PATH}"
  cat <<EOF
  - target:
      kind: Deployment
      name: ${name}
    patch: |-
      - op: replace
        path: /spec/replicas
        value: ${replicas}
      - op: replace
        path: /spec/template/spec/containers/0/args/0
        value: ${model_arg}
      - op: replace
        path: /spec/template/spec/containers/0/args/${tpidx}
        value: --tensor-parallel-size=${tp}
      - op: replace
        path: /spec/template/spec/containers/0/resources/limits/nvidia.com~1gpu
        value: "${tp}"
      - op: replace
        path: /spec/template/spec/containers/0/resources/requests/nvidia.com~1gpu
        value: "${tp}"
EOF
  [[ -n $cpu ]] && cat <<EOF
      - op: replace
        path: /spec/template/spec/containers/0/resources/limits/cpu
        value: "${cpu}"
      - op: replace
        path: /spec/template/spec/containers/0/resources/requests/cpu
        value: "${cpu}"
EOF
  [[ -n $mem ]] && cat <<EOF
      - op: replace
        path: /spec/template/spec/containers/0/resources/limits/memory
        value: "${mem}"
      - op: replace
        path: /spec/template/spec/containers/0/resources/requests/memory
        value: "${mem}"
EOF
  [[ -n $gpu_mem_util ]] && cat <<EOF
      - op: add
        path: /spec/template/spec/containers/0/args/-
        value: --gpu-memory-utilization=${gpu_mem_util}
EOF
  if [[ $MODEL_CACHE == true ]]; then
    cat <<EOF
      - op: add
        path: /spec/template/spec/containers/0/args/-
        value: --served-model-name=${MODEL}
      - op: add
        path: /spec/template/spec/containers/0/volumeMounts/-
        value:
          name: model-cache
          mountPath: ${MODEL_CACHE_MOUNT}
          readOnly: true
      - op: add
        path: /spec/template/spec/volumes/-
        value:
          name: model-cache
          persistentVolumeClaim:
            claimName: ${MODEL_CACHE_PVC_NAME}
EOF
  fi
  return 0
}

# The rendered manifest is the last artifact before the cluster sees anything,
# so every property the topology depends on is asserted here. A silently wrong
# render is the failure mode that costs the most time downstream.
assert_render() {
  python3 - "${WORKDIR}/modelserver.rendered.yaml" \
      "$MODEL" "$PREFILL_REPLICAS" "$PREFILL_TP" "$DECODE_REPLICAS" "$DECODE_TP" \
      "$MODEL_CACHE" "$MODEL_CACHE_MOUNT" "$MODEL_PATH" "$MODEL_CACHE_PVC_NAME" <<'PY' >&2 \
    || die "the rendered manifest does not match the requested topology — nothing applied"
import sys, yaml
path, model = sys.argv[1], sys.argv[2]
pr, ptp, dr, dtp = (int(x) for x in sys.argv[3:7])
model_cache = sys.argv[7] == "true"
cache_mount, model_path, cache_pvc = sys.argv[8], sys.argv[9], sys.argv[10]
want = {"prefill": (pr, ptp), "decode": (dr, dtp)}
want_model_arg = f"{cache_mount}/{model_path}" if model_cache else model
docs = [d for d in yaml.safe_load_all(open(path)) if d]
raw = open(path).read()
bad, seen = [], {}

if "REPLACE_" in raw:
    bad.append("rendered manifest still contains a REPLACE_ placeholder")

for d in docs:
    if d.get("kind") != "Deployment":
        continue
    name = d["metadata"]["name"]
    role = "prefill" if name.endswith("prefill") else "decode" if name.endswith("decode") else None
    if not role:
        continue
    seen[role] = name
    spec = d["spec"]
    ms = [c for c in spec["template"]["spec"]["containers"] if c["name"] == "modelserver"]
    if not ms:
        bad.append(f"{role}: no container named modelserver"); continue
    c, args = ms[0], ms[0].get("args", [])
    wrep, wtp = want[role]

    got_rep = spec.get("replicas")
    got_model = args[0] if args else None
    got_tp = next((a.split("=", 1)[1] for a in args if str(a).startswith("--tensor-parallel-size")), None)
    lim = c["resources"]["limits"]; req = c["resources"]["requests"]
    got_gpu_l, got_gpu_r = lim.get("nvidia.com/gpu"), req.get("nvidia.com/gpu")
    img = c.get("image", "")
    kv = next((args[i+1] for i, a in enumerate(args) if a == "--kv-transfer-config"), None)
    pod = spec["template"]["spec"]
    # The routing sidecar is a NATIVE Kubernetes sidecar: an initContainer with
    # restartPolicy: Always (k8s >= 1.29), not a regular container. Looking only
    # at .containers finds nothing and the check silently passes on a broken pod.
    sidecars = [x["name"] for x in pod.get("containers", []) if x["name"] != "modelserver"] + \
               [x["name"] + "(init,restart=" + str(x.get("restartPolicy")) + ")"
                for x in pod.get("initContainers", [])]

    if got_rep != wrep:            bad.append(f"{role}: replicas={got_rep}, want {wrep}")
    if got_model != want_model_arg: bad.append(f"{role}: model={got_model}, want {want_model_arg}")
    if got_tp != str(wtp):         bad.append(f"{role}: tensor-parallel-size={got_tp}, want {wtp}")
    if str(got_gpu_l) != str(wtp): bad.append(f"{role}: gpu limit={got_gpu_l}, want {wtp}")
    if str(got_gpu_r) != str(wtp): bad.append(f"{role}: gpu request={got_gpu_r}, want {wtp}")
    if not img or "REPLACE" in img: bad.append(f"{role}: unusable image {img!r}")
    if not kv or "NixlConnector" not in kv:
        bad.append(f"{role}: kv-transfer-config missing NixlConnector: {kv!r}")

    if model_cache:
        served = next((a.split("=", 1)[1] for a in args if str(a).startswith("--served-model-name")), None)
        if served != model:
            bad.append(f"{role}: --served-model-name={served}, want {model}")
        mounts = c.get("volumeMounts", [])
        cm = next((m for m in mounts if m.get("name") == "model-cache"), None)
        if not cm:
            bad.append(f"{role}: no model-cache volumeMount on the modelserver container")
        elif cm.get("mountPath") != cache_mount or not cm.get("readOnly"):
            bad.append(f"{role}: model-cache volumeMount={cm}, want mountPath={cache_mount} readOnly=true")
        vols = pod.get("volumes", [])
        pv = next((v for v in vols if v.get("name") == "model-cache"), None)
        got_claim = (pv or {}).get("persistentVolumeClaim", {}).get("claimName")
        if got_claim != cache_pvc:
            bad.append(f"{role}: model-cache PVC claim={got_claim}, want {cache_pvc}")

    print(f"   {role:<8} replicas={got_rep} tp={got_tp} gpu={got_gpu_l} "
          f"kv=NixlConnector sidecars={sidecars or '-'}")
    print(f"   {'':<8} image={img}")

for role in ("prefill", "decode"):
    if role not in seen:
        bad.append(f"no {role} Deployment in the render")

# The decode pod must carry the routing sidecar: it is what calls the prefill
# pod named in the EPP's header and pulls the KV cache back over NIXL. Without
# it the deployment comes up healthy and silently never disaggregates.
for d in docs:
    if d.get("kind") == "Deployment" and d["metadata"]["name"].endswith("decode"):
        pod = d["spec"]["template"]["spec"]
        allc = pod.get("containers", []) + pod.get("initContainers", [])
        proxy = [c for c in allc if "proxy" in c["name"] or "sidecar" in c["name"]]
        if not proxy:
            bad.append("decode has no routing sidecar: " + str([c["name"] for c in allc]))
        else:
            pc = proxy[0]
            init_names = [c["name"] for c in pod.get("initContainers", [])]
            # A native sidecar without restartPolicy: Always runs to completion
            # and the pod never starts; with it, it stays up alongside vLLM.
            if pc["name"] in init_names and pc.get("restartPolicy") != "Always":
                bad.append("decode sidecar " + pc["name"] + " is an initContainer "
                           "without restartPolicy: Always")
            kvc = [a for a in pc.get("args", []) if "kv-connector" in str(a)]
            print("   sidecar  " + pc["name"] + " image=" + pc.get("image", "?")
                  + " " + " ".join(kvc))

if bad:
    print("\n   rendered manifest assertions FAILED:")
    for b in bad:
        print(f"     - {b}")
    sys.exit(1)
print("   all render assertions passed")
PY
  ok "render matches the requested topology"
}

apply_modelserver() {
  stage "STAGE 7  apply model server"
  # Equivalent to the guide's `kubectl apply -k <overlay>`, except the exact
  # bytes that were just asserted are what get applied.
  run kubectl apply -n "$NAMESPACE" -f "${WORKDIR}/modelserver.rendered.yaml" \
    || die "kubectl apply of the model server failed"
  ok "model server applied"
}

# =========================================================================
# STAGE 7b — monitoring (PodMonitors + EPP ServiceMonitor)
# =========================================================================
enable_monitoring() {
  stage "STAGE 7b  monitoring (PodMonitors + EPP ServiceMonitor)"
  [[ $DRY_RUN == true ]] && { warn "--dry-run: not applying monitoring resources"; return 0; }

  # OpenShift User Workload Monitoring only discovers ServiceMonitors/PodMonitors
  # in namespaces with this label.
  run kubectl label namespace "$NAMESPACE" openshift.io/user-monitoring=true --overwrite \
    || warn "could not label namespace for user-workload-monitoring"

  # vLLM PodMonitors: one per role. The upstream kustomize component
  # (components/monitoring-pd) carries both, but its commonlabels config injects
  # extra selectors we don't control. Apply them directly with the labels the
  # guide's model server already sets (llm-d.ai/role).
  for role in prefill decode; do
    cat <<EOPM | run kubectl apply -n "$NAMESPACE" -f -
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: ${role}-podmonitor
  namespace: ${NAMESPACE}
spec:
  selector:
    matchLabels:
      llm-d.ai/role: ${role}
  podMetricsEndpoints:
    - port: modelserver
      path: /metrics
      interval: 30s
EOPM
  done
  ok "PodMonitors: prefill-podmonitor, decode-podmonitor"

  # The EPP uses controller-runtime's secure metrics serving, which validates
  # callers via SubjectAccessReview. The EPP SA needs tokenreviews + SAR create
  # permissions, and the scraping SA needs get /metrics. The helm chart's
  # ClusterRole has all three but may be bound to a generated SA name that
  # differs from the deployment's SA; ensure the deployment SA has them too.
  local epp_sa epp_cr
  epp_sa=$(kubectl get deploy "${GUIDE_NAME}-epp" -n "$NAMESPACE" \
             -o jsonpath='{.spec.template.spec.serviceAccountName}' 2>/dev/null)
  epp_cr=$(kubectl get clusterrole -o name 2>/dev/null \
             | grep -m1 "${NAMESPACE}.*epp" | cut -d/ -f2)
  if [[ -n $epp_sa && -n $epp_cr ]]; then
    cat <<EOCRB | run kubectl apply -f -
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ${GUIDE_NAME}-${NAMESPACE}-epp
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: ${epp_cr}
subjects:
  - kind: ServiceAccount
    name: ${epp_sa}
    namespace: ${NAMESPACE}
EOCRB
    ok "EPP SA ${epp_sa} bound to ClusterRole ${epp_cr}"
  else
    warn "could not detect EPP SA (${epp_sa:-?}) or ClusterRole (${epp_cr:-?}); skipping CRB"
  fi

  # Create a token secret for the EPP SA so the ServiceMonitor can authenticate.
  cat <<EOSEC | run kubectl apply -n "$NAMESPACE" -f -
apiVersion: v1
kind: Secret
metadata:
  name: ${GUIDE_NAME}-epp-token
  namespace: ${NAMESPACE}
  annotations:
    kubernetes.io/service-account.name: ${epp_sa:-${GUIDE_NAME}-epp}
type: kubernetes.io/service-account-token
EOSEC
  ok "EPP SA token secret: ${GUIDE_NAME}-epp-token"

  # EPP ServiceMonitor: created directly rather than layering monitoring.values.yaml
  # into the helm install. Reason: calibrate-peak-prefill.sh does a helm upgrade
  # without that file, which would silently drop a chart-managed ServiceMonitor.
  # A directly-applied resource survives helm upgrades of the router.
  cat <<EOSM | run kubectl apply -n "$NAMESPACE" -f -
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: ${GUIDE_NAME}-epp-monitor
  namespace: ${NAMESPACE}
spec:
  endpoints:
    - interval: 10s
      port: http-metrics
      path: /metrics
      authorization:
        credentials:
          key: token
          name: ${GUIDE_NAME}-epp-token
  namespaceSelector:
    matchNames:
      - ${NAMESPACE}
  selector:
    matchLabels:
      app.kubernetes.io/name: ${GUIDE_NAME}-epp
EOSM
  ok "ServiceMonitor: ${GUIDE_NAME}-epp-monitor (with bearer auth)"
}

# =========================================================================
# STAGE 8 — wait for readiness
# =========================================================================
wait_ready() {
  stage "STAGE 8  wait for pods (timeout ${ROLLOUT_TIMEOUT}s)"
  [[ $DRY_RUN == true ]] && { warn "--dry-run: not waiting"; return 0; }

  local deploys waited=0 interval=15
  deploys=$(kubectl get deploy -n "$NAMESPACE" -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}')
  info "deployments: ${deploys}"

  while :; do
    local all_ready=true line=""
    for d in $deploys; do
      local want got
      want=$(kubectl get deploy "$d" -n "$NAMESPACE" -o jsonpath='{.spec.replicas}' 2>/dev/null)
      got=$(kubectl get deploy "$d" -n "$NAMESPACE" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)
      got=${got:-0}
      line+="${d}=${got}/${want} "
      [[ ${got:-0} -lt ${want:-1} ]] && all_ready=false
    done

    if [[ $all_ready == true ]]; then
      ok "all pods ready after ${waited}s  (${line})"
      return 0
    fi

    # Surface hard failures immediately instead of burning the whole timeout.
    local broken
    broken=$(kubectl get pods -n "$NAMESPACE" -o json | python3 -c '
import json, sys
bad = []
FATAL = ("ErrImagePull", "ImagePullBackOff", "CrashLoopBackOff",
         "CreateContainerConfigError", "CreateContainerError", "InvalidImageName")
for p in json.load(sys.stdin)["items"]:
    n = p["metadata"]["name"]
    st = p.get("status", {})
    for cs in (st.get("containerStatuses") or []) + (st.get("initContainerStatuses") or []):
        cn = cs.get("name", "?")
        w = cs.get("state", {}).get("waiting") or {}
        t = cs.get("lastState", {}).get("terminated") or {}
        if w.get("reason") in FATAL:
            bad.append(n + "/" + cn + ": " + w["reason"] + " " + w.get("message", "")[:160])
        elif t.get("reason") == "OOMKilled":
            bad.append(n + "/" + cn + ": OOMKilled (raise the memory limit)")
    if st.get("phase") == "Pending":
        for c in (st.get("conditions") or []):
            if c.get("reason") == "Unschedulable":
                bad.append(n + ": Unschedulable " + c.get("message", "")[:160])
print("\n".join(bad))')
    if [[ -n $broken ]]; then
      printf '     %s\n' "$broken" >&2
      dump_diagnostics
      die "pods are failing, not just slow (see above)"
    fi

    (( waited >= ROLLOUT_TIMEOUT )) && { dump_diagnostics; die "timed out after ${waited}s (${line})"; }
    (( waited % 60 == 0 )) && info "  ${waited}s  ${line}"
    sleep $interval; waited=$((waited + interval))
  done
}

dump_diagnostics() {
  local out="${WORKDIR}/diagnostics.$(date +%H%M%S).txt"
  {
    echo "=== pods ===";   kubectl get pods -n "$NAMESPACE" -o wide
    echo; echo "=== events ==="; kubectl get events -n "$NAMESPACE" --sort-by=.lastTimestamp | tail -40
    for p in $(kubectl get pods -n "$NAMESPACE" -o name 2>/dev/null); do
      echo; echo "=== describe ${p} ==="; kubectl describe -n "$NAMESPACE" "$p" | tail -30
      echo; echo "=== logs ${p} (last 60) ==="
      kubectl logs -n "$NAMESPACE" "$p" --all-containers --tail=60 2>&1 | tail -60
    done
  } > "$out" 2>&1
  warn "diagnostics written to ${out}"
  kubectl get pods -n "$NAMESPACE" -o wide >&2 2>/dev/null
}

# =========================================================================
# STAGE 8 — verification (the guide's own, plus a P/D assertion)
# =========================================================================
verify() {
  stage "STAGE 9  verification"
  [[ $DRY_RUN == true ]] && { warn "--dry-run: not verifying"; return 0; }

  # 8a — the guide's step 1: resolve the standalone endpoint.
  local ip
  ip=$(kubectl get service "${GUIDE_NAME}-epp" -n "$NAMESPACE" -o jsonpath='{.spec.clusterIP}' 2>/dev/null) \
    || die "service ${GUIDE_NAME}-epp not found"
  [[ -n $ip && $ip != None ]] || die "service ${GUIDE_NAME}-epp has no clusterIP"
  ok "endpoint (standalone): http://${ip}"

  # 8b — the guide runs ONE InferencePool holding both roles; prefill-filter and
  # decode-filter split it by the llm-d.ai/role label. So the check that matters
  # is not "are there endpoints" (the model servers have no Service at all —
  # GAIE selects pods directly) but "does the pool select at least one ready pod
  # of EACH role". Miss a role and requests 503 while every pod looks healthy.
  local pool sel
  pool=$(kubectl get inferencepool -n "$NAMESPACE" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
  [[ -n $pool ]] || die "no InferencePool in ${NAMESPACE} — the router chart did not install cleanly"
  sel=$(kubectl get inferencepool "$pool" -n "$NAMESPACE" \
        -o jsonpath='{range .spec.selector.matchLabels}{@}{end}' 2>/dev/null)
  sel=$(kubectl get inferencepool "$pool" -n "$NAMESPACE" -o json 2>/dev/null | python3 -c '
import json, sys
ml = json.load(sys.stdin)["spec"]["selector"]["matchLabels"]
print(",".join(k + "=" + v for k, v in ml.items()))')
  info "InferencePool ${pool} selects: ${sel}"

  local roles
  roles=$(kubectl get pods -n "$NAMESPACE" -l "$sel" -o json 2>/dev/null | python3 -c '
import json, sys
from collections import Counter
c = Counter()
for p in json.load(sys.stdin)["items"]:
    role = p["metadata"]["labels"].get("llm-d.ai/role", "<none>")
    ready = any(cd["type"] == "Ready" and cd["status"] == "True"
                for cd in (p.get("status", {}).get("conditions") or []))
    if ready:
        c[role] += 1
print(" ".join(f"{k}={v}" for k, v in sorted(c.items())) or "NONE")
print(",".join(sorted(c)))')
  local counts pool_roles
  counts=$(printf '%s' "$roles" | head -1)
  pool_roles=$(printf '%s' "$roles" | tail -1)
  info "ready pods in the pool: ${counts}"
  [[ $pool_roles == *prefill* ]] || die "the InferencePool selects no ready prefill pod — P/D cannot route"
  [[ $pool_roles == *decode*  ]] || die "the InferencePool selects no ready decode pod — P/D cannot route"
  ok "pool holds both roles, as prefill-filter/decode-filter require"

  # 8c — mark the prefill log position, so 8e can tell whether THIS request
  # went through prefill rather than reading a stale line.
  local prefill_pod decode_pod
  prefill_pod=$(kubectl get pods -n "$NAMESPACE" -l llm-d.ai/role=prefill \
                  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
  [[ -n $prefill_pod ]] || prefill_pod=$(kubectl get pods -n "$NAMESPACE" -o name 2>/dev/null \
                  | grep -m1 prefill | cut -d/ -f2)
  decode_pod=$(kubectl get pods -n "$NAMESPACE" -o name 2>/dev/null | grep -m1 decode | cut -d/ -f2)
  info "prefill pod: ${prefill_pod:-<none>}"
  info "decode pod:  ${decode_pod:-<none>}"
  local prefill_lines_before=0
  [[ -n $prefill_pod ]] && prefill_lines_before=$(kubectl logs -n "$NAMESPACE" "$prefill_pod" \
      -c modelserver 2>/dev/null | wc -l | tr -d ' ')

  # 8d — the guide's step 2: send a completion request from inside the cluster.
  info "sending POST /v1/completions from an in-cluster pod"
  local resp
  resp=$(kubectl run "pd-verify-$$" --rm -i --restart=Never -n "$NAMESPACE" \
           --image="$CURL_IMAGE" --quiet --command -- \
           sh -c "curl -sS -m 180 -w '\nHTTP_CODE:%{http_code}\n' -X POST http://${ip}/v1/completions \
                  -H 'Content-Type: application/json' \
                  -d '{\"model\":\"${MODEL}\",\"prompt\":\"How are you today?\",\"max_tokens\":32}'" 2>&1)
  printf '%s\n' "$resp" > "${WORKDIR}/verify.response.txt"

  local code
  code=$(printf '%s' "$resp" | sed -n 's/^HTTP_CODE:\([0-9]*\)$/\1/p' | tail -1)
  if [[ $code != 200 ]]; then
    printf '%s\n' "$resp" | tail -20 >&2
    dump_diagnostics
    die "completion request returned HTTP ${code:-<none>} (full body in ${WORKDIR}/verify.response.txt)"
  fi

  local text
  text=$(printf '%s' "$resp" | sed '/^HTTP_CODE:/d' | python3 -c '
import json, sys
raw = sys.stdin.read()
i = raw.find("{")
try:
    d = json.loads(raw[i:])
    print(d["choices"][0].get("text", "")[:200].replace("\n", " "))
except Exception as e:
    print("")' 2>/dev/null)
  [[ -n $text ]] || { printf '%s\n' "$resp" | tail -20 >&2; die "HTTP 200 but no completion text in the response"; }
  ok "HTTP 200, completion returned: \"${text}\""

  # 8e — did it actually disaggregate? A P/D stack that quietly serves
  # everything from decode passes 8d and is still broken. Prefill's access log
  # is enabled for /v1/completions (only /health,/metrics,/v1/models are muted),
  # so a new prefill log line for this request is direct evidence.
  if [[ -n $prefill_pod ]]; then
    sleep 3
    local after new
    after=$(kubectl logs -n "$NAMESPACE" "$prefill_pod" -c modelserver 2>/dev/null | wc -l | tr -d ' ')
    new=$(kubectl logs -n "$NAMESPACE" "$prefill_pod" -c modelserver --tail=$(( after - prefill_lines_before < 1 ? 1 : after - prefill_lines_before )) 2>/dev/null)
    if printf '%s' "$new" | grep -qE 'POST /v1/(completions|chat/completions)|Received request'; then
      ok "prefill pod served this request — disaggregation is live"
    else
      warn "no new prefill activity observed for this request; it may have been served"
      warn "  without disaggregating. Check: kubectl logs -n ${NAMESPACE} ${decode_pod} -c routing-proxy"
    fi
  fi

  # 8f — the decode sidecar is the component that pulls KV over NIXL; an error
  # there is the classic silent P/D failure.
  if [[ -n $decode_pod ]]; then
    local sc
    sc=$(kubectl get pod "$decode_pod" -n "$NAMESPACE" \
          -o jsonpath='{range .spec.containers[*]}{.name}{" "}{end}{range .spec.initContainers[*]}{.name}{"(sidecar) "}{end}' 2>/dev/null)
    info "decode containers: ${sc}"
    # The sidecar is a native sidecar, so it starts BEFORE vLLM and proxies to
    # localhost:8200 while nothing is listening — a burst of "connection
    # refused" during model load is expected and means nothing. Only errors
    # logged after the pod went Ready indicate a real KV-transfer problem, so
    # date them against the Ready transition instead of counting blindly.
    local ready_at
    ready_at=$(kubectl get pod "$decode_pod" -n "$NAMESPACE" \
        -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.lastTransitionTime}{end}' 2>/dev/null)
    kubectl logs -n "$NAMESPACE" "$decode_pod" -c routing-proxy --tail=400 2>/dev/null \
      | READY_AT="$ready_at" python3 -c '
import sys, os, json, datetime as dt
raw = os.environ.get("READY_AT", "")
try:
    ready = dt.datetime.fromisoformat(raw.replace("Z", "+00:00")).timestamp()
except Exception:
    ready = 0
errs = []
for line in sys.stdin:
    try:
        d = json.loads(line)
    except Exception:
        continue
    if d.get("level") == "error":
        errs.append((d.get("ts", 0), d.get("msg", "")[:120]))
after = [e for e in errs if e[0] > ready + 2]
pre = len(errs) - len(after)
if not after:
    print("   PASS  routing-proxy: no errors since the pod went Ready"
          + (" (%d during startup, expected)" % pre if pre else ""))
else:
    print("   WARN  routing-proxy logged %d error(s) AFTER becoming Ready:" % len(after))
    for _, m in after[:5]:
        print("           " + m)
' >&2
  fi
}

# =========================================================================
# teardown
# =========================================================================
teardown() {
  stage "TEARDOWN ${NAMESPACE}"
  run helm uninstall "$GUIDE_NAME" -n "$NAMESPACE" --wait --timeout 3m || true
  run kubectl delete namespace "$NAMESPACE" --wait=true --timeout=10m || true
  ok "namespace ${NAMESPACE} removed"
  exit 0
}

summary() {
  stage "SUMMARY"
  local cache_line
  if [[ $MODEL_CACHE == true ]]; then
    cache_line="PVC ${MODEL_CACHE_PVC_NAME} (${MODEL_CACHE_PVC_SIZE}) at ${MODEL_CACHE_MOUNT} — persists only across --skip-clean reruns"
  else
    cache_line="disabled (per-pod emptyDir download; use --model-cache to share one)"
  fi
  cat >&2 <<EOF
   guide:      upstream ${GUIDE_NAME} @ $(git -C "$LLMD_DIR" log -1 --format='%h' 2>/dev/null || echo '?')
   namespace:  ${NAMESPACE}
   model:      ${MODEL}
   topology:   prefill ${PREFILL_REPLICAS} x TP=${PREFILL_TP} + decode ${DECODE_REPLICAS} x TP=${DECODE_TP} = ${TOTAL_GPUS} GPU
   router:     standalone (EPP + envoy sidecar), release "${GUIDE_NAME}"
   monitoring: PodMonitors (prefill + decode) + ServiceMonitor (EPP)
   model cache: ${cache_line}
   artifacts:  ${WORKDIR}/
                 modelserver.rendered.yaml   exactly what was applied
                 router.rendered.yaml        the helm output
                 pd-config.yaml              the EPP plugin config in force
                 verify.response.txt         the completion response

   endpoint:   export IP=\$(kubectl get service ${GUIDE_NAME}-epp -n ${NAMESPACE} -o jsonpath='{.spec.clusterIP}')
   re-verify:  ./$(basename "$0") --verify-only
   teardown:   ./$(basename "$0") --teardown
EOF
}

# =========================================================================
main() {
  printf '%s\n' "logging to ${LOG}" >&2
  # --render-only exercises the checkout + overlay + assertions with no cluster
  # at all, which is how you validate a change to this script safely.
  if [[ $RENDER_ONLY == true ]]; then
    checkout_repo; render_overlay; summary; exit 0
  fi
  preflight
  checkout_repo
  [[ $TEARDOWN == true ]] && teardown
  if [[ $VERIFY_ONLY == true ]]; then
    verify; summary; exit 0
  fi
  clean_namespace
  create_hf_secret
  provision_model_cache
  install_router
  render_overlay
  apply_modelserver
  enable_monitoring
  wait_ready
  verify
  summary
}

main 2>&1 | tee "$LOG"
exit "${PIPESTATUS[0]}"
