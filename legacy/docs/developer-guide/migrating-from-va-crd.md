# Migrating from VariantAutoscaling CRD

The `VariantAutoscaling` (VA) CRD is deprecated and will be removed in a future release.
The preferred path is to add WVA discovery annotations directly to your existing HPA or KEDA `ScaledObject`.

## Why migrate

The annotation-based path removes the need for a separate CRD and aligns WVA with standard Kubernetes autoscaling primitives.

## Before / After — HPA path

> **Note:** The YAML examples below show the minimal migration changes. For production-ready
> configuration including scale behavior policies, fallback settings, and TLS, refer to the
> ready-to-use samples in `config/samples/hpa/annotations/` and `config/samples/keda/annotations/`.

**Before (deprecated):** two objects

```yaml
# VariantAutoscaling (deprecated)
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: sample-deployment
  namespace: llm-d-sim
spec:
  scaleTargetRef:
    kind: Deployment
    name: sample-deployment
  modelID: default/default
  maxReplicas: 10
---
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: sample-deployment-hpa
  namespace: llm-d-sim
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: sample-deployment
  minReplicas: 1
  maxReplicas: 10
  metrics:
  - type: External
    external:
      metric:
        name: wva_desired_replicas
        selector:
          matchLabels:
            variant_name: sample-deployment
            exported_namespace: llm-d-sim
      target:
        type: AverageValue
        averageValue: "1"
```

**After (recommended):** one object

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: sample-deployment-hpa
  namespace: llm-d-sim
  annotations:
    llm-d.ai/managed: "true"
    llm-d.ai/model-id: "default/default"
    llm-d.ai/variant-cost: "10.0"   # optional, defaults to 10.0
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: sample-deployment
  minReplicas: 1
  maxReplicas: 10
  metrics:
  - type: External
    external:
      metric:
        name: wva_desired_replicas
        selector:
          matchLabels:
            variant_name: sample-deployment
            exported_namespace: llm-d-sim
      target:
        type: AverageValue
        averageValue: "1"
```

## Before / After — KEDA ScaledObject path

**Before (deprecated):** two objects

```yaml
# VariantAutoscaling (deprecated)
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: sample-deployment
  namespace: llm-d-sim
spec:
  scaleTargetRef:
    kind: Deployment
    name: sample-deployment
  modelID: default/default
  maxReplicas: 10
---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: sample-deployment-scaler
  namespace: llm-d-sim
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: sample-deployment
  maxReplicaCount: 10
  triggers:
  - type: prometheus
    name: wva-desired-replicas
    metadata:
      serverAddress: https://prometheus.example.com:9090
      query: |
        wva_desired_replicas{
          variant_name="sample-deployment",
          namespace="llm-d-sim"
        }
      threshold: '1'
      activationThreshold: '0'
      metricType: "Value"
```

**After (recommended):** one object

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: sample-deployment-scaler
  namespace: llm-d-sim
  annotations:
    llm-d.ai/managed: "true"
    llm-d.ai/model-id: "default/default"
    llm-d.ai/variant-cost: "10.0"   # optional, defaults to 10.0
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: sample-deployment
  maxReplicaCount: 10
  triggers:
  - type: prometheus
    name: wva-desired-replicas
    metadata:
      serverAddress: https://kube-prometheus-stack-prometheus.workload-variant-autoscaler-monitoring.svc.cluster.local:9090
      query: |
        wva_desired_replicas{
          variant_name="sample-deployment",
          namespace="llm-d-sim"
        }
      threshold: '1'
      activationThreshold: '0'
      metricType: "Value"
      unsafeSsl: "true"  # WARNING: set to "false" in production and provide a CA bundle via authenticationRef
```

> **Note on metric label differences:** The HPA path uses `exported_namespace` in the metric selector
> (added by Prometheus Adapter), while the KEDA path uses `namespace` (the label WVA emits directly).
> Use the label matching your setup — see the ready-to-use samples in `config/samples/hpa/annotations/`
> and `config/samples/keda/annotations/` for the exact selectors.

## Migration steps

1. **List existing VariantAutoscalings:**

   ```bash
   kubectl get variantautoscalings -A
   ```

2. **For each VA, extract its `modelID` and scale-target name:**

   ```bash
   kubectl get variantautoscaling <va-name> -n <namespace> \
     -o jsonpath='{.spec.modelID} {.spec.scaleTargetRef.name}{"\n"}'
   ```

3. **Annotate your HPA (or ScaledObject) with the values from step 2:**

   ```bash
   # Replace <hpa-name>, <namespace>, <modelID> with actual values
   kubectl annotate hpa <hpa-name> -n <namespace> \
     llm-d.ai/managed=true \
     llm-d.ai/model-id=<modelID> \
     --overwrite
   ```

   For KEDA ScaledObject:

   ```bash
   kubectl annotate scaledobject <so-name> -n <namespace> \
     llm-d.ai/managed=true \
     llm-d.ai/model-id=<modelID> \
     --overwrite
   ```

4. **Verify WVA is picking up the annotated resource:**

   Check controller logs for a line containing `"Discovered annotated HPA"` or `"Discovered annotated ScaledObject"` with your resource name.

   ```bash
   kubectl logs -n workload-variant-autoscaler-system \
     deployment/workload-variant-autoscaler-controller-manager \
     | grep -E "Discovered annotated|<hpa-or-so-name>"
   ```

   Alternatively, check that the `wva_desired_replicas` metric is emitted:

   ```bash
   kubectl exec -n workload-variant-autoscaler-system \
     deployment/workload-variant-autoscaler-controller-manager -- \
     wget -qO- http://localhost:9090/metrics | grep wva_desired_replicas
   ```

5. **Delete the legacy VariantAutoscaling once validated:**

   ```bash
   kubectl delete variantautoscaling <va-name> -n <namespace>
   ```

## Validation checklist

- [ ] Controller logs show the annotated HPA/ScaledObject being discovered.
- [ ] `wva_desired_replicas{variant_name="<name>", namespace="<ns>"}` metric is present and non-zero under load.
- [ ] HPA/ScaledObject is scaling as expected.
- [ ] No `Warning Deprecated` events on the deleted VA (it no longer exists).

## Sample manifests

Ready-to-use samples are in:
- `config/samples/hpa/annotations/` — annotation-based HPA
- `config/samples/keda/annotations/` — annotation-based ScaledObject
