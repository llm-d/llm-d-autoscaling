# Setup KEDA Autoscaling on vLLM Metrics

This script creates a KEDA `ScaledObject` to autoscale deployments on raw vLLM metrics (`vllm:kv_cache_usage_perc`, `vllm:num_requests_waiting`) instead of pool-averaged EPP metrics.

## Why

Raw vLLM metrics provide finer-grained autoscaling signals than pool averages. This enables proactive scaling based on:
- **KV Cache Utilization** (60%): Before endpoints become deprioritized by the router
- **Queue Depth** (30 requests): Before queue exceeds capacity during pod startup

## Prerequisites

- KEDA installed in the cluster
- `TriggerAuthentication` named `prometheus-auth` in the target namespace (with bearer token for Prometheus)
- vLLM pods exposing metrics (scraped into Prometheus)
- Target resource must exist: `Deployment`, `StatefulSet`, `LeaderWorkerSet`, or other scalable resource

## Usage

### Create ScaledObject for a Deployment

```bash
./hack/setup-keda-vllm-scaling.sh my-namespace my-decode-deployment
```

### Create ScaledObject for a LeaderWorkerSet

```bash
./hack/setup-keda-vllm-scaling.sh my-namespace my-lws-resource \
  --target-kind LeaderWorkerSet \
  --target-api-version leaderworkerset.x-k8s.io/v1alpha1
```

### Dry-run (preview without creating)

```bash
./hack/setup-keda-vllm-scaling.sh my-namespace my-deployment --dry-run
```

## What it does

1. Validates prerequisites (kubectl, jq, namespace, target resource, TriggerAuthentication)
2. Checks if a ScaledObject already exists (if so, asks you to delete it first)
3. Generates a `ScaledObject` manifest with vLLM-based triggers:
   - **KV Cache Utilization**: 0.6 (60%) threshold using sum aggregation normalized by pod count
   - **Queue Depth**: 30 requests threshold (average across pods)
4. Shows the manifest and asks for confirmation before applying
5. Verifies creation and shows status

## Cleanup

To remove autoscaling:

```bash
kubectl delete scaledobject -n <namespace> <deployment-name>-saturation
```

## Thresholds explained

### KV Cache @ 0.6 (60%)

The metric `vllm:kv_cache_usage_perc` is a 0–1 fraction, not 0–100. The script uses sum aggregation divided by pod count to get the average, triggering at 0.6. The router's load-balancing uses a KV cache scorer: `score = 1 - usage`. At 0.6 (60%) cache utilization, this score drops to 0.4, indicating meaningful degradation in routing quality. This threshold triggers scale-up before endpoints become strongly deprioritized (0.7+), allowing proactive capacity increases.

### Queue Depth @ 30 requests

Calculated from GPU pod startup time (typically 180s) divided by KEDA polling interval (15s), with 1.5× buffer:
```
(180s / 15s) × 1.5 = 18 requests ≈ 30 (with margin)
```

Setting to 30 ensures new pods arrive before the queue explodes, while respecting the router's soft load-balancing.

## SSL/TLS Configuration

The script includes `tlsCertFile: "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"` in the TriggerAuthentication metadata for secure connections to Thanos querier. If the pod cannot access the service CA bundle, you can alternatively set `unsafeSsl: "true"` in the metadata (not recommended for production).

## Troubleshooting

**"TriggerAuthentication 'prometheus-auth' not found"**

Create it:
```bash
kubectl create secret generic prometheus-token -n <namespace> --from-literal=token=<bearer-token>
kubectl apply -f - <<EOF
apiVersion: keda.sh/v1alpha1
kind: TriggerAuthentication
metadata:
  name: prometheus-auth
  namespace: <namespace>
spec:
  secretTargetRef:
    - parameter: bearerToken
      name: prometheus-token
      key: token
EOF
```

**"ScaledObject already exists"**

If you need to recreate with new triggers:
```bash
kubectl delete scaledobject -n <namespace> <deployment-name>-saturation
# Then run the script again
```

**HPA shows `<unknown>` for metrics**

Check the `pod=~"..."` regex matches your actual pod names. The script uses the pattern `^<deployment-name>-[a-z0-9]{5,}$` to match Kubernetes-generated pod names (at least 5 alphanumeric characters after the deployment name). Verify:
```bash
kubectl get pods -n <namespace> | grep <deployment-name>
```

Verify the namespace label in the Prometheus query:
```bash
kubectl get pods -n <namespace> --show-labels
```

**KEDA operator logs show errors**

```bash
kubectl logs -n keda deploy/keda-operator | grep -i <namespace>
```
