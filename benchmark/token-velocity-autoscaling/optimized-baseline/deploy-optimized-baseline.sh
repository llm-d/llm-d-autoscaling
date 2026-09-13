#!/bin/bash
set -euo pipefail

# Deploy the llm-d "optimized-baseline" guide to the pd-test namespace:
#   - model inference server (Qwen3-32B, vLLM, TP=2, co-located kv_both)
#   - EPP router (llm-d-router-standalone chart, optimized-baseline overlay)
#   - a "router" Service that exposes the EPP metrics + inference ports
#
# This is the CO-LOCATED baseline (single pool, no prefill/decode split), so the
# EPP uses the optimized-baseline plugin set (prefix-cache-affinity + token-load),
# NOT the pd-disaggregation prefill-filter/decode-filter set. See:
#   llm-d/guides/optimized-baseline/router/optimized-baseline.values.yaml
#
# Env knobs:
#   HF_TOKEN                  optional; Qwen3-32B is ungated so an empty token works
#   WIPE_NAMESPACE=true       destructively delete+recreate the namespace (WIPES the
#                             model PVC → forces a full 65GB re-download). Default false:
#                             the namespace/PVC/model are preserved across runs.
#   SKIP_DOWNLOAD=true        skip the model-cache download job (model already cached)
#   PEAK_PREFILL_THROUGHPUT   V_P seeded into the prefix-cache-affinity-filter
#                             (default 15928, the guide's Qwen3-32B/H100 reference).
#                             Calibrate and patch this later in the workflow.
#   LLMD_REPO                 path to a checked-out llm-d repo (for base.values.yaml).
#                             Defaults to the sibling clone; cloned on demand if absent.

NAMESPACE="pd-test"
DEPLOYMENT_NAME="optimized-baseline"
ROUTER_RELEASE="${DEPLOYMENT_NAME}-router"   # → deployment/service optimized-baseline-router-epp
MODEL="Qwen/Qwen3-32B"
TENSOR_PARALLEL_SIZE=2
CHART_REPO="oci://ghcr.io/llm-d/charts"
ROUTER_CHART="llm-d-router-standalone"
ROUTER_VERSION="v0"

WIPE_NAMESPACE="${WIPE_NAMESPACE:-false}"
SKIP_DOWNLOAD="${SKIP_DOWNLOAD:-false}"
PEAK_PREFILL_THROUGHPUT="${PEAK_PREFILL_THROUGHPUT:-15644}"
# Resolve the llm-d checkout: honor $LLMD_REPO, else prefer an existing ./llm-d or
# ../llm-d, otherwise default to ./llm-d (cloned on demand below if absent).
LLMD_REPO="${LLMD_REPO:-}"
if [[ -z "$LLMD_REPO" ]]; then
  if [[ -d "${PWD}/llm-d/.git" ]]; then LLMD_REPO="${PWD}/llm-d"
  elif [[ -d "${PWD}/../llm-d/.git" ]]; then LLMD_REPO="$(cd "${PWD}/../llm-d" && pwd)"
  else LLMD_REPO="${PWD}/llm-d"; fi
fi
GAIE_URL="https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/latest/download/manifests.yaml"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Step 0: Prerequisite — the router chart renders an InferencePool
# (inference.networking.k8s.io/v1). Without the GAIE CRD, `helm install` fails
# halfway and leaves a partial release. Fail fast with a clear message instead.
log_info "Step 0: Checking GAIE InferencePool CRD (v1)..."
if ! kubectl get crd inferencepools.inference.networking.k8s.io >/dev/null 2>&1; then
    log_error "GAIE InferencePool CRD missing. Install it (cluster-admin) with:"
    log_error "  kubectl apply -f ${GAIE_URL}"
    exit 1
fi
if ! kubectl get crd inferencepools.inference.networking.k8s.io \
        -o jsonpath='{.spec.versions[*].name}' 2>/dev/null | grep -qw v1; then
    log_error "InferencePool CRD does not serve v1; the router chart needs v1. Upgrade the GAIE CRDs."
    exit 1
fi
log_info "InferencePool CRD present (v1)."

# Step 1: Namespace. By default this is NON-destructive so the model PVC (a ~65GB
# download) survives re-runs. Set WIPE_NAMESPACE=true only for a clean slate.
if [ "${WIPE_NAMESPACE}" = "true" ]; then
    log_warn "Step 1: WIPE_NAMESPACE=true — deleting namespace ${NAMESPACE} (this WIPES the model PVC)..."
    kubectl delete namespace "${NAMESPACE}" --ignore-not-found=true
    log_info "Waiting for namespace cleanup..."
    kubectl wait --for=delete namespace/"${NAMESPACE}" --timeout=180s 2>/dev/null || true
else
    log_info "Step 1: Preserving existing ${NAMESPACE} namespace/PVC (WIPE_NAMESPACE=false)."
fi

# Step 2: Ensure namespace exists and is labelled.
log_info "Step 2: Ensuring ${NAMESPACE} namespace exists and is labelled..."
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
kubectl label namespace "${NAMESPACE}" llm-d.ai/guide=optimized-baseline --overwrite

# Step 3: HF token secret (idempotent). The download Job references it; Qwen3-32B is
# ungated, so an empty value works.
log_info "Step 3: Ensuring HF token secret..."
if [ -z "${HF_TOKEN:-}" ]; then
    log_warn "HF_TOKEN not set. Creating llm-d-hf-token secret with an empty value."
fi
kubectl create secret generic llm-d-hf-token \
    --from-literal=HF_TOKEN="${HF_TOKEN:-}" \
    -n "${NAMESPACE}" \
    --dry-run=client -o yaml | kubectl apply -f -

# Step 4: Model cache PVC (idempotent — apply is a no-op if it already exists).
log_info "Step 4: Ensuring model cache PVC..."
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: model-cache
  namespace: ${NAMESPACE}
spec:
  accessModes:
    - ReadWriteMany
  resources:
    requests:
      storage: 500Gi
  storageClassName: ibm-spectrum-scale-fileset
EOF

# Step 5: Download the model into the PVC. `hf download` is idempotent (it skips
# shards already present), so re-running is cheap. A completed Job has immutable
# fields, so delete any prior Job before re-creating.
if [ "${SKIP_DOWNLOAD}" = "true" ]; then
    log_info "Step 5: SKIP_DOWNLOAD=true — skipping model download."
else
    log_info "Step 5: Creating model cache download job..."
    kubectl delete job model-cache-download -n "${NAMESPACE}" --ignore-not-found=true
    cat <<EOF | kubectl apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: model-cache-download
  namespace: ${NAMESPACE}
spec:
  template:
    spec:
      containers:
      - name: model-downloader
        image: python:3.12-slim
        imagePullPolicy: IfNotPresent
        env:
        - name: HF_TOKEN
          valueFrom:
            secretKeyRef:
              name: llm-d-hf-token
              key: HF_TOKEN
              optional: true
        # OpenShift restricted SCC runs this pod with a random non-root UID, so the
        # image's default HOME (/) and /root are not writable. Point HOME and HF_HOME
        # at /tmp so pip --user, the hf CLI, and HF token/metadata all land in a
        # writable location. Model WEIGHTS go to the PVC via --local-dir.
        - name: HF_HOME
          value: /tmp/huggingface
        - name: HOME
          value: /tmp
        command:
        - /bin/sh
        - -c
        - |
          set -e
          pip install -q -U --user huggingface_hub
          export PATH="\${PATH}:\${HOME}/.local/bin"
          if [ -n "\${HF_TOKEN:-}" ]; then hf auth login --token "\${HF_TOKEN}"; fi
          hf download ${MODEL} --local-dir /model-cache/models/${MODEL}
        volumeMounts:
        - name: model-cache
          mountPath: /model-cache
        resources:
          requests:
            memory: 16Gi
            cpu: 4
          limits:
            memory: 32Gi
            cpu: 8
      volumes:
      - name: model-cache
        persistentVolumeClaim:
          claimName: model-cache
      restartPolicy: Never
  backoffLimit: 3
EOF
    log_info "Waiting for model cache download job to complete..."
    kubectl wait --for=condition=complete job/model-cache-download -n "${NAMESPACE}" --timeout=3600s \
        || log_warn "Model cache download job did not complete in time"
fi

# Step 6: Model inference server (Qwen, TP=2).
log_info "Step 6: Deploying model inference server (Qwen with TP=${TENSOR_PARALLEL_SIZE})..."
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${DEPLOYMENT_NAME}-modelserver
  namespace: ${NAMESPACE}
  labels:
    app: modelserver
    guide: ${DEPLOYMENT_NAME}
automountServiceAccountToken: false
---
apiVersion: v1
kind: Service
metadata:
  name: modelserver
  namespace: ${NAMESPACE}
  labels:
    app: modelserver
    guide: ${DEPLOYMENT_NAME}
spec:
  type: ClusterIP
  selector:
    app: modelserver
    guide: ${DEPLOYMENT_NAME}
  ports:
  - name: modelserver
    port: 8000
    targetPort: 8000
    protocol: TCP
  - name: nixl
    port: 5600
    targetPort: 5600
    protocol: TCP
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${DEPLOYMENT_NAME}-modelserver
  namespace: ${NAMESPACE}
  labels:
    app: modelserver
    guide: ${DEPLOYMENT_NAME}
spec:
  replicas: 1
  # Recreate (not RollingUpdate): the pod holds 2 GPUs for the whole node. A rolling
  # update would leave the new pod Pending forever waiting for GPUs the old (still
  # running) pod holds — a self-inflicted deadlock. Recreate tears the old pod down
  # first, freeing the GPUs before the new pod schedules.
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app: modelserver
      guide: ${DEPLOYMENT_NAME}
  template:
    metadata:
      labels:
        app: modelserver
        guide: ${DEPLOYMENT_NAME}
        # The EPP discovers model-server pods by this label (InferencePool selector /
        # router.modelServers.matchLabels). Without it the pool selects zero endpoints
        # and the router 503s. Matches guides/optimized-baseline/.../base/kustomization.yaml.
        llm-d.ai/guide: ${DEPLOYMENT_NAME}
    spec:
      serviceAccountName: ${DEPLOYMENT_NAME}-modelserver
      containers:
      - name: modelserver
        image: vllm/vllm-openai:latest
        args:
        - "/model-cache/models/${MODEL}"
        - "--served-model-name=${MODEL}"
        - "--disable-access-log-for-endpoints=/health,/metrics,/v1/models"
        - "--tensor-parallel-size=${TENSOR_PARALLEL_SIZE}"
        - "--block-size=128"
        - "--kv-transfer-config"
        - '{"kv_connector":"NixlConnector", "kv_role":"kv_both"}'
        - "--no-disable-hybrid-kv-cache-manager"
        - "--port=8000"
        # No \`command:\` override — the vllm/vllm-openai image's ENTRYPOINT is
        # \`vllm serve\`, so the args above are passed to it directly. Overriding
        # with \`python -m ...\` fails because the image has no \`python\` binary
        # (only python3), yielding CreateContainerError.
        env:
        # OpenShift's restricted SCC uses a random non-root UID whose HOME is /
        # (not writable). vLLM's runtime deps write to ~/.cache (FlashInfer),
        # ~/.config (usage stats), and ~/.triton (Triton kernel cache); with
        # HOME=/ these become /.cache, /.config, /.triton and crash the workers
        # with "Permission denied". Pointing HOME at the writable /tmp layer
        # redirects all of them at once. Mirrors the download job's HOME=/tmp.
        - name: HOME
          value: /tmp
        - name: HF_HOME
          value: /tmp/huggingface
        - name: HF_TOKEN
          valueFrom:
            secretKeyRef:
              name: llm-d-hf-token
              key: HF_TOKEN
              optional: true
        - name: VLLM_NIXL_SIDE_CHANNEL_HOST
          valueFrom:
            fieldRef:
              fieldPath: status.podIP
        - name: CUDA_VISIBLE_DEVICES
          value: "0,1"
        ports:
        - containerPort: 8000
          name: modelserver
          protocol: TCP
        - containerPort: 5600
          name: nixl
          protocol: TCP
        livenessProbe:
          httpGet:
            path: /health
            port: modelserver
          initialDelaySeconds: 300
          periodSeconds: 10
          failureThreshold: 3
          timeoutSeconds: 5
        readinessProbe:
          httpGet:
            path: /v1/models
            port: modelserver
          initialDelaySeconds: 60
          periodSeconds: 5
          failureThreshold: 3
          timeoutSeconds: 2
        startupProbe:
          httpGet:
            path: /health
            port: modelserver
          initialDelaySeconds: 0
          periodSeconds: 10
          failureThreshold: 120
          timeoutSeconds: 5
        resources:
          # vLLM is GPU-bound; the CPU request is kept modest so the pod can
          # schedule on a busy shared cluster (a large CPU request needlessly
          # excludes otherwise-suitable GPU nodes). GPU is the binding resource.
          limits:
            nvidia.com/gpu: "2"
            memory: 80Gi
            cpu: "16"
          requests:
            nvidia.com/gpu: "2"
            memory: 32Gi
            cpu: "8"
        volumeMounts:
        - name: model-cache
          mountPath: /model-cache
          readOnly: true
        # vLLM tensor-parallel (TP>1) workers communicate over POSIX shared
        # memory. A pod's default /dev/shm is only 64Mi, too small — it causes
        # "Insufficient space in /dev/shm" at engine startup. Back it with a
        # Memory emptyDir sized generously.
        - name: dshm
          mountPath: /dev/shm
        # Writable cache dir (belt-and-braces alongside HOME=/tmp), mirroring the
        # upstream guide's modelserver base.
        - name: cache
          mountPath: /.cache
      volumes:
      - name: model-cache
        persistentVolumeClaim:
          claimName: model-cache
      - name: dshm
        emptyDir:
          medium: Memory
          sizeLimit: 16Gi
      - name: cache
        emptyDir: {}
      # No nodeSelector/affinity: the nvidia.com/gpu resource request lands this on a
      # GPU node. The cluster's H100 nodes are labelled nvidia.com/gpu.present=true
      # (not nvidia.com/gpu=true) and carry no taints, so a literal selector/affinity
      # would match nothing and the pod would stay Pending. Mirrors deploy-pd-guide.sh.
EOF

log_info "Waiting for modelserver deployment to be ready..."
kubectl rollout status deployment/${DEPLOYMENT_NAME}-modelserver -n "${NAMESPACE}" --timeout=15m

# Step 7: EPP router (optimized-baseline overlay layered on the shared base values).
log_info "Step 7: Deploying EPP router (optimized-baseline plugin set)..."

# Ensure we have the shared base values file. It lives in the llm-d repo.
BASE_VALUES="${LLMD_REPO}/guides/recipes/router/base.values.yaml"
if [ ! -f "${BASE_VALUES}" ]; then
    log_warn "base.values.yaml not found at ${BASE_VALUES}; cloning llm-d repo..."
    git clone --depth 1 https://github.com/llm-d/llm-d.git "${LLMD_REPO}"
fi
[ -f "${BASE_VALUES}" ] || { log_error "missing ${BASE_VALUES}"; exit 1; }

# Guide overlay. This is the CO-LOCATED baseline plugin set (no prefill/decode
# split): prefix-cache-affinity-filter + token-load-scorer. peakPrefillThroughput
# is seeded here and patched later in the workflow after calibration.
cat <<EOF > /tmp/optimized-baseline-router-values.yaml
router:
  extraServicePorts:
    - name: http
      port: 80
      protocol: TCP
      targetPort: 8081
  epp:
    pluginsConfigFile: "optimized-baseline-plugins.yaml"
    pluginsCustomConfig:
      optimized-baseline-plugins.yaml: |
        apiVersion: llm-d.ai/v1alpha1
        kind: EndpointPickerConfig
        plugins:
        - type: approx-prefix-cache-producer
        - type: inflight-load-producer
        - type: prefix-cache-affinity-filter
          parameters:
            peakPrefillThroughput: ${PEAK_PREFILL_THROUGHPUT}
        - type: token-load-scorer
        schedulingProfiles:
        - name: default
          plugins:
          - pluginRef: prefix-cache-affinity-filter
          - pluginRef: token-load-scorer
  modelServers:
    matchLabels:
      llm-d.ai/guide: "${DEPLOYMENT_NAME}"
EOF

helm upgrade --install "${ROUTER_RELEASE}" "${CHART_REPO}/${ROUTER_CHART}" \
    --version "${ROUTER_VERSION}" \
    -f "${BASE_VALUES}" \
    -f /tmp/optimized-baseline-router-values.yaml \
    -n "${NAMESPACE}" \
    --wait \
    --timeout 5m

log_info "Waiting for EPP router to be ready..."
kubectl rollout status deployment/${ROUTER_RELEASE}-epp -n "${NAMESPACE}" --timeout=5m

# Step 8: "router" Service alias. The chart's own service (${ROUTER_RELEASE}-epp)
# exposes http-metrics=9090 and http=80(→8081, inference). The autoscaling workflow
# (KEDA) and test-metrics.sh look for a Service named "router" and scrape EPP metrics
# on :8000, so map 8000→9090 (metrics) and keep 80→8081 (inference) here.
log_info "Step 8: Creating 'router' Service (metrics on :8000, inference on :80)..."
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: router
  namespace: ${NAMESPACE}
  labels:
    guide: ${DEPLOYMENT_NAME}
    app.kubernetes.io/name: ${ROUTER_RELEASE}-epp
spec:
  type: ClusterIP
  # The EPP pods are labelled llm-d-router-gateway=<release>-epp (the chart's own
  # service selects on this, NOT app.kubernetes.io/name). The metadata label above
  # keeps -l app.kubernetes.io/name=... Service lookups in the sibling scripts working.
  selector:
    llm-d-router-gateway: ${ROUTER_RELEASE}-epp
  ports:
  - name: metrics
    port: 8000
    targetPort: 9090
    protocol: TCP
  - name: inference
    port: 80
    targetPort: 8081
    protocol: TCP
EOF

# Step 8b: EPP metrics auth. The EPP's /metrics endpoint requires a bearer token and
# authorizes callers via SubjectAccessReview, so a plain scrape gets 401. Give the EPP
# SA a ClusterRole with tokenreviews+subjectaccessreviews (to validate/authorize
# scrapers) plus get /metrics, mint an SA-token secret, and point a ServiceMonitor at
# the http-metrics port with that token. Applied directly (not via helm) so a later
# `helm upgrade` of the router can't drop it. Mirrors deploy-pd-guide.sh.
log_info "Step 8b: Setting up EPP metrics auth (ClusterRole + token + ServiceMonitor)..."
EPP_SA=$(kubectl get deploy ${ROUTER_RELEASE}-epp -n "${NAMESPACE}" \
           -o jsonpath='{.spec.template.spec.serviceAccountName}' 2>/dev/null)
EPP_SA="${EPP_SA:-${ROUTER_RELEASE}-epp}"
cat <<EOF | kubectl apply -f -
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ${ROUTER_RELEASE}-epp
  labels:
    app.kubernetes.io/instance: ${ROUTER_RELEASE}
rules:
- apiGroups: ["authentication.k8s.io"]
  resources: ["tokenreviews"]
  verbs: ["create"]
- apiGroups: ["authorization.k8s.io"]
  resources: ["subjectaccessreviews"]
  verbs: ["create"]
- nonResourceURLs: ["/metrics"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ${ROUTER_RELEASE}-${NAMESPACE}-epp
  labels:
    app.kubernetes.io/instance: ${ROUTER_RELEASE}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: ${ROUTER_RELEASE}-epp
subjects:
- kind: ServiceAccount
  name: ${EPP_SA}
  namespace: ${NAMESPACE}
---
apiVersion: v1
kind: Secret
metadata:
  name: ${ROUTER_RELEASE}-epp-token
  namespace: ${NAMESPACE}
  annotations:
    kubernetes.io/service-account.name: ${EPP_SA}
type: kubernetes.io/service-account-token
EOF

# ServiceMonitor is optional — only meaningful if the Prometheus Operator CRD exists.
if kubectl get crd servicemonitors.monitoring.coreos.com >/dev/null 2>&1; then
    cat <<EOF | kubectl apply -f -
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: ${ROUTER_RELEASE}-epp-monitor
  namespace: ${NAMESPACE}
spec:
  endpoints:
    - interval: 10s
      port: http-metrics
      path: /metrics
      authorization:
        credentials:
          key: token
          name: ${ROUTER_RELEASE}-epp-token
  namespaceSelector:
    matchNames: [${NAMESPACE}]
  selector:
    matchLabels:
      app.kubernetes.io/name: ${ROUTER_RELEASE}-epp
EOF
    log_info "ServiceMonitor ${ROUTER_RELEASE}-epp-monitor created."
else
    log_warn "ServiceMonitor CRD absent — skipping (metrics still scrapeable with the SA token)."
fi

# Step 9: Smoke test — confirm the router routes a real completion to the modelserver
# through the EPP (proves endpoint discovery + InferencePool selection work). This is
# NOT the throughput calibration; run calibrate-peak-prefill.sh separately for V_P.
log_info "Step 9: Router → modelserver smoke test..."
kubectl wait --for=condition=Ready pod \
    -l app=modelserver,guide=${DEPLOYMENT_NAME} -n "${NAMESPACE}" --timeout=15m
kubectl wait --for=condition=Ready pod \
    -l app.kubernetes.io/name=${ROUTER_RELEASE}-epp -n "${NAMESPACE}" --timeout=5m

kubectl port-forward -n "${NAMESPACE}" svc/router 18080:80 > /tmp/router-portforward.log 2>&1 &
PORTFORWARD_PID=$!
trap "kill ${PORTFORWARD_PID} 2>/dev/null || true" EXIT
sleep 4

SMOKE_OK=false
for i in $(seq 1 30); do
    if RESP=$(curl -s --max-time 30 -X POST "http://localhost:18080/v1/completions" \
        -H "Content-Type: application/json" \
        -d "{\"model\":\"${MODEL}\",\"prompt\":\"The quick brown fox\",\"max_tokens\":16}" 2>/dev/null) \
        && echo "${RESP}" | grep -q '"text"'; then
        log_info "Smoke test OK — router returned a completion:"
        echo "${RESP}" | head -c 400; echo
        SMOKE_OK=true
        break
    fi
    sleep 3
done
[ "${SMOKE_OK}" = "true" ] || log_warn "Smoke test did not get a completion. Check: kubectl logs -l app.kubernetes.io/name=${ROUTER_RELEASE}-epp -n ${NAMESPACE} -c epp"

# Step 10: Summary
log_info ""
log_info "=========================================="
log_info "Deployment Summary"
log_info "=========================================="
log_info "Namespace:            ${NAMESPACE}"
log_info "Model:                ${MODEL}"
log_info "Tensor Parallel Size: ${TENSOR_PARALLEL_SIZE}"
log_info "Router release:       ${ROUTER_RELEASE} (deploy/${ROUTER_RELEASE}-epp)"
log_info "Modelserver:          ${DEPLOYMENT_NAME}-modelserver"
log_info "peakPrefillThroughput seed: ${PEAK_PREFILL_THROUGHPUT} (calibrate + patch next)"
log_info ""
kubectl get deploy,svc,inferencepool -n "${NAMESPACE}" 2>/dev/null || kubectl get deploy,svc -n "${NAMESPACE}"
log_info ""
log_info "Inference:  kubectl port-forward -n ${NAMESPACE} svc/router 8080:80   # POST /v1/completions"
log_info "EPP metrics: kubectl port-forward -n ${NAMESPACE} svc/router 8000:8000  # GET /metrics"
log_info "=========================================="
