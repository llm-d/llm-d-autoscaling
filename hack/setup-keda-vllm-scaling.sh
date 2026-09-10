#!/usr/bin/env bash
#
# Create a KEDA ScaledObject to autoscale on raw vLLM metrics
# (vllm:kv_cache_usage_perc, vllm:num_requests_waiting) instead of EPP pool aggregates.
#
# Usage:
#   ./hack/setup-keda-vllm-scaling.sh <namespace> <target-resource-name> [--dry-run] [--target-kind KIND] [--target-api-version VERSION]
#
# Examples:
#   # Create ScaledObject for a Deployment
#   ./hack/setup-keda-vllm-scaling.sh my-namespace my-decode-deployment
#
#   # Create ScaledObject for a LeaderWorkerSet
#   ./hack/setup-keda-vllm-scaling.sh my-namespace my-lws-resource --target-kind LeaderWorkerSet --target-api-version leaderworkerset.x-k8s.io/v1alpha1
#
#   # Dry-run: show what would be created
#   ./hack/setup-keda-vllm-scaling.sh my-namespace my-decode-deployment --dry-run
#

set -euo pipefail

# === FUNCTIONS ===

show_help() {
    cat <<'HELP'
Setup KEDA autoscaling on raw vLLM metrics.

USAGE:
  setup-keda-vllm-scaling.sh <namespace> <target-name> [OPTIONS]

ARGS:
  <namespace>       Kubernetes namespace containing the target resource
  <target-name>     Name of the Deployment, LeaderWorkerSet, or other scalable resource

OPTIONS:
  --target-kind KIND              Kind of resource to scale (default: Deployment)
  --target-api-version VERSION    API version (default: apps/v1)
  --dry-run                       Show what would be created without applying
  --help                          Show this message

EXAMPLES:
  # Deployment
  setup-keda-vllm-scaling.sh prod my-model-decode

  # LeaderWorkerSet
  setup-keda-vllm-scaling.sh prod my-lws \
    --target-kind LeaderWorkerSet \
    --target-api-version leaderworkerset.x-k8s.io/v1alpha1

THRESHOLDS:
  - KV Cache: 60% (router score degrades to 0.4 at this point)
  - Queue Depth: 30 requests (based on 180s pod startup / 15s KEDA polling + 1.5x buffer)

REQUIREMENTS:
  - KEDA installed in cluster
  - TriggerAuthentication "prometheus-auth" exists in target namespace
  - vLLM pods exposing metrics to Prometheus
  - Bearer token with Thanos/Prometheus access

HELP
}

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() {
    echo -e "${GREEN}ℹ${NC} $*" >&2
}

log_warn() {
    echo -e "${YELLOW}⚠${NC} $*" >&2
}

log_error() {
    echo -e "${RED}✗${NC} $*" >&2
}

log_success() {
    echo -e "${GREEN}✓${NC} $*" >&2
}

# === PARSE ARGS ===

if [ $# -lt 2 ]; then
    show_help
    exit 1
fi

NAMESPACE="$1"
TARGET_NAME="$2"
shift 2

DRY_RUN=false
TARGET_KIND="Deployment"
TARGET_API_VERSION="apps/v1"

while [ $# -gt 0 ]; do
    case "$1" in
        --dry-run) DRY_RUN=true ;;
        --target-kind) TARGET_KIND="$2"; shift ;;
        --target-api-version) TARGET_API_VERSION="$2"; shift ;;
        --help) show_help; exit 0 ;;
        *) log_error "Unknown option: $1"; exit 1 ;;
    esac
    shift
done

# === VALIDATE PREREQUISITES ===

log_info "Validating prerequisites..."

for tool in kubectl jq; do
    if ! command -v "$tool" &>/dev/null; then
        log_error "Required tool not found: $tool"
        exit 1
    fi
done

# Check namespace exists
if ! kubectl get namespace "$NAMESPACE" &>/dev/null; then
    log_error "Namespace not found: $NAMESPACE"
    exit 1
fi

# Check TriggerAuthentication exists
if ! kubectl get triggerauthentication -n "$NAMESPACE" prometheus-auth &>/dev/null; then
    log_error "TriggerAuthentication 'prometheus-auth' not found in namespace/$NAMESPACE"
    log_info "Create it with:"
    log_info "  kubectl create secret generic prometheus-token -n $NAMESPACE --from-literal=token=<bearer-token>"
    log_info "  kubectl apply -f - <<EOF"
    log_info "apiVersion: keda.sh/v1alpha1"
    log_info "kind: TriggerAuthentication"
    log_info "metadata:"
    log_info "  name: prometheus-auth"
    log_info "  namespace: $NAMESPACE"
    log_info "spec:"
    log_info "  secretTargetRef:"
    log_info "    - parameter: bearerToken"
    log_info "      name: prometheus-token"
    log_info "      key: token"
    log_info "EOF"
    exit 1
fi

log_success "Prerequisites validated"

# === CHECK TARGET RESOURCE EXISTS ===

log_info "Looking for $TARGET_KIND: $TARGET_NAME in namespace/$NAMESPACE..."

RESOURCE_NAME=$(echo "$TARGET_KIND" | tr '[:upper:]' '[:lower:]' | sed 's/set$/sets/')

if ! kubectl get "$RESOURCE_NAME" -n "$NAMESPACE" "$TARGET_NAME" &>/dev/null; then
    log_error "$TARGET_KIND '$TARGET_NAME' not found in namespace/$NAMESPACE"
    exit 1
fi

log_success "Found $TARGET_KIND: $TARGET_NAME"

# === CHECK FOR EXISTING SCALEDOBJECT ===

SCALEDOBJECT_NAME="${TARGET_NAME}-saturation"
log_info "Checking for existing ScaledObject: $SCALEDOBJECT_NAME..."

if kubectl get scaledobject -n "$NAMESPACE" "$SCALEDOBJECT_NAME" &>/dev/null; then
    log_error "ScaledObject already exists: $SCALEDOBJECT_NAME"
    log_warn "To recreate with new triggers, first delete the existing one:"
    log_warn "  kubectl delete scaledobject -n $NAMESPACE $SCALEDOBJECT_NAME"
    exit 1
fi

log_success "No existing ScaledObject found"

# === BUILD SCALEDOBJECT MANIFEST ===

POD_REGEX="^${TARGET_NAME}-[a-z0-9]{5,}$"

MANIFEST=$(cat <<EOF
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: $SCALEDOBJECT_NAME
  namespace: $NAMESPACE
  annotations:
    llm-d.ai/managed: "true"
    llm-d.ai/description: "Autoscales on vLLM raw metrics: KV cache @ 60%, queue depth @ 30 requests"
spec:
  scaleTargetRef:
    apiVersion: $TARGET_API_VERSION
    kind: $TARGET_KIND
    name: $TARGET_NAME
  minReplicaCount: 1
  maxReplicaCount: 10
  pollingInterval: 15
  triggers:
    # KV Cache Utilization Trigger (0.6 = 60%)
    # Rationale: vllm:kv_cache_usage_perc is a 0–1 fraction (not 0–100).
    # At 0.6 (60%), the router's scorer (1 - usage) = 0.4, indicating degradation.
    # Using sum aggregation to get the total across all pods in the deployment,
    # divided by pod count, scales the deployment as a whole unit.
    # TriggerAuthentication must include the service CA bundle or have unsafeSsl: "true".
    - type: prometheus
      name: kv-cache
      metricType: AverageValue
      authenticationRef:
        name: prometheus-auth
      metadata:
        serverAddress: "https://thanos-querier.openshift-monitoring.svc.cluster.local:9091"
        authModes: bearer
        tlsCertFile: "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
        query: |
          sum(vllm:kv_cache_usage_perc{namespace="$NAMESPACE",pod=~"$POD_REGEX"}) / count(vllm:kv_cache_usage_perc{namespace="$NAMESPACE",pod=~"$POD_REGEX"})
        threshold: "0.6"
        activationThreshold: "0"
    # Queue Depth Trigger (30 requests)
    # Rationale: Calculated from pod startup time: (180s / 15s polling) × 1.5 buffer = ~18 reqs.
    # Set to 30 to provide margin for GPU pod boot latency (typically 180 seconds).
    # By the time a new pod is ready, queue should not exceed capacity.
    # Router continues to load balance; this threshold prevents queue explosion.
    - type: prometheus
      name: queue-size
      metricType: AverageValue
      authenticationRef:
        name: prometheus-auth
      metadata:
        serverAddress: "https://thanos-querier.openshift-monitoring.svc.cluster.local:9091"
        authModes: bearer
        tlsCertFile: "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
        query: |
          avg(vllm:num_requests_waiting{namespace="$NAMESPACE",pod=~"$POD_REGEX"})
        threshold: "30"
        activationThreshold: "0"
  advanced:
    horizontalPodAutoscalerConfig:
      behavior:
        scaleUp:
          stabilizationWindowSeconds: 0
          policies:
            - type: Pods
              value: 1
              periodSeconds: 180
        scaleDown:
          stabilizationWindowSeconds: 300
          policies:
            - type: Pods
              value: 1
              periodSeconds: 300
EOF
)

# === VALIDATE MANIFEST ===

log_info "Validating ScaledObject manifest..."
if ! echo "$MANIFEST" | kubectl apply -f - --dry-run=client -o yaml &>/dev/null; then
    log_error "Generated manifest is invalid"
    exit 1
fi
log_success "Manifest is valid"

# === OUTPUT OR APPLY ===

if [ "$DRY_RUN" = true ]; then
    log_info "Dry-run mode: showing ScaledObject"
    echo ""
    echo "$MANIFEST"
    exit 0
fi

# Interactive confirm
echo ""
echo "ScaledObject to create:"
echo "$MANIFEST"
echo ""
read -p "Create this ScaledObject? (yes/no): " CONFIRM

if [ "$CONFIRM" != "yes" ] && [ "$CONFIRM" != "y" ]; then
    log_warn "Cancelled"
    exit 0
fi

# Apply
echo "$MANIFEST" | kubectl apply -f -
log_success "ScaledObject created!"

# === VERIFICATION ===

log_info "Verifying ScaledObject..."
sleep 2

kubectl get scaledobject -n "$NAMESPACE" "$SCALEDOBJECT_NAME" -o yaml | head -50

log_success "Done. KEDA will reconcile the underlying HPA. Monitors may take ~30s to resolve (one polling interval)"
log_info "To check status: kubectl get scaledobject -n $NAMESPACE $SCALEDOBJECT_NAME"
log_info "To verify metrics: kubectl describe hpa -n $NAMESPACE"
