#!/usr/bin/env bash
set -euo pipefail

echo "=== Verifying metrics pipeline before running tests ==="
echo ""

# 1. Verify vLLM pods are serving /metrics endpoint
echo "--- Step 1: Checking vLLM /metrics endpoint ---"
for ns in "$LLMD_NAMESPACE"; do
  VLLM_POD=$(kubectl get pods -n "$ns" -l llm-d.ai/inference-serving=true -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  if [ -n "$VLLM_POD" ]; then
    PORT="${VLLM_SVC_PORT:-8000}"
    echo "  Checking vLLM pod $VLLM_POD in $ns (port $PORT)..."
    METRICS=$(kubectl exec -n "$ns" "$VLLM_POD" -- curl -s "http://localhost:${PORT}/metrics" 2>/dev/null | head -5 || true)
    if [ -n "$METRICS" ]; then
      echo "  ✅ vLLM metrics endpoint responding in $ns"
    else
      echo "  ⚠️  vLLM metrics endpoint not responding in $ns (may still be loading)"
    fi
    # Show pod labels for debugging
    echo "  Pod labels:"
    kubectl get pod "$VLLM_POD" -n "$ns" -o jsonpath='{.metadata.labels}' | jq -r 'to_entries[] | "    \(.key)=\(.value)"' 2>/dev/null || true
  else
    echo "  ⚠️  No vLLM pods found with label llm-d.ai/inference-serving=true in $ns"
    echo "  All pods in $ns:"
    kubectl get pods -n "$ns" --show-labels 2>/dev/null || true
  fi
done

# 1b. Verify vllm-service has endpoints (critical for ServiceMonitor scraping)
echo ""
echo "--- Step 1b: Checking vllm-service endpoints ---"
for ns in "$LLMD_NAMESPACE"; do
  SVC_NAME=$(kubectl get svc -n "$ns" -l app.kubernetes.io/name=workload-variant-autoscaler -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  if [ -n "$SVC_NAME" ]; then
    ENDPOINTS=$(kubectl get endpoints "$SVC_NAME" -n "$ns" -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null || true)
    if [ -n "$ENDPOINTS" ]; then
      echo "  ✅ Service $SVC_NAME in $ns has endpoints: $ENDPOINTS"
    else
      echo "  ❌ Service $SVC_NAME in $ns has NO endpoints — label selector mismatch!"
      echo "  Service selector:"
      kubectl get svc "$SVC_NAME" -n "$ns" -o jsonpath='{.spec.selector}' 2>/dev/null | jq . || true
    fi
  else
    echo "  ⚠️  No vllm-service found in $ns"
  fi
done

# 1c. Check PodMonitors (llm-d guide deploys these for direct pod scraping)
echo ""
echo "--- Step 1c: PodMonitor configuration ---"
for ns in "$LLMD_NAMESPACE"; do
  PM_COUNT=$(kubectl get podmonitor -n "$ns" --no-headers 2>/dev/null | wc -l | tr -d ' ')
  echo "  PodMonitors in $ns: $PM_COUNT"
  kubectl get podmonitor -n "$ns" 2>/dev/null || true
done

# 2. Check WVA controller health
echo ""
echo "--- Step 2: WVA controller status ---"
kubectl get pods -n "$WVA_NAMESPACE" -l app.kubernetes.io/name=workload-variant-autoscaler
WVA_POD=$(kubectl get pods -n "$WVA_NAMESPACE" -l app.kubernetes.io/name=workload-variant-autoscaler -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
if [ -n "$WVA_POD" ]; then
  echo "  Recent WVA controller logs:"
  kubectl logs "$WVA_POD" -n "$WVA_NAMESPACE" --tail=20 | grep -E "reconcil|metrics|error|saturation" || echo "  (no matching log lines)"
fi

# 3. Check managed autoscalers (annotation-based discovery surface)
echo ""
echo "--- Step 3: Managed HorizontalPodAutoscalers ---"
kubectl get hpa -A -o wide 2>/dev/null || echo "  No HorizontalPodAutoscalers found"

# 4. Check ServiceMonitors exist
echo ""
echo "--- Step 4: ServiceMonitor configuration ---"
for ns in "$LLMD_NAMESPACE" "$WVA_NAMESPACE"; do
  SM_COUNT=$(kubectl get servicemonitor -n "$ns" --no-headers 2>/dev/null | wc -l | tr -d ' ')
  echo "  ServiceMonitors in $ns: $SM_COUNT"
  kubectl get servicemonitor -n "$ns" 2>/dev/null || true
done

# 5. Wait for WVA to start processing metrics (up to 3 minutes)
# WVA discovers variants from annotated HPAs and emits the wva_desired_replicas
# external metric; the managed HPA then reports it under status.currentMetrics.
# Presence of a current metric value is our signal that the pipeline is live.
echo ""
echo "--- Step 5: Waiting for WVA to detect metrics (up to 3 minutes) ---"
METRICS_READY=false
for i in $(seq 1 18); do
  HPA_METRIC=$(kubectl get hpa -n "$LLMD_NAMESPACE" -o jsonpath='{.items[0].status.currentMetrics}' 2>/dev/null || true)
  if [ -n "$HPA_METRIC" ] && [ "$HPA_METRIC" != "null" ]; then
    echo "  ✅ WVA optimization active — HPA currentMetrics: $HPA_METRIC"
    METRICS_READY=true
    break
  fi
  echo "  Attempt $i/18: WVA not yet optimizing, waiting 10s..."
  sleep 10
done

if [ "$METRICS_READY" = "false" ]; then
  echo "  ⚠️  WVA has not started optimizing after 3 minutes"
  echo "  This may cause test timeouts — dumping diagnostics:"
  echo ""
  echo "  === WVA controller logs (last 50 lines) ==="
  kubectl logs "$WVA_POD" -n "$WVA_NAMESPACE" --tail=50 2>/dev/null || true
  echo ""
  echo "  === HPA status ==="
  kubectl get hpa -A 2>/dev/null || true
  echo ""
  echo "  Continuing to tests anyway (they have their own timeouts)..."
fi

echo ""
echo "=== Metrics pipeline verification complete ==="
