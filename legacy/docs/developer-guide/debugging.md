# Run WVA locally for debugging (connects to Cluster & Prometheus)

This guide shows how to run the Workload Variant Autoscaler (WVA) locally while letting it communicate with your Kubernetes/OpenShift cluster and Prometheus using an SSH tunnel and a ServiceMonitor.

### Quick summary
- Purpose: Run WVA locally (IDE or terminal) and let Prometheus in-cluster scrape the local /metrics endpoint via an SSH tunnel.
- Outcome: Prometheus scrapes your local WVA instance's metrics and WVA can query Prometheus in the cluster.

[![debugging-with-remote-clusters.png](debugging-with-remote-clusters.png)](debugging-with-remote-clusters.png)

### Prerequisites
- kubectl configured to talk to the target cluster (set KUBECONFIG if needed)
- A local shell that can run ssh and kubectl
- WVA is installed but scaled down to zero replicas in the cluster

### Steps

1) Create a secret `ssh-key` in a namespace of your choice (e.g., `debugging`):
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: ssh-key
type: Opaque
stringData:
  id_ed25519.pub: |
    <your-ssh-public-key-here>
```

2) Deploy helper manifests

Apply the manifests that create the SSH gateway (SSH server) and the RBAC objects in the cluster. See `debugging-ssh-tunnel.yaml` in this directory for an example manifest:

```shell
kubectl apply -f debugging-ssh-tunnel.yaml
```

3) Create a token for the `ssh-gateway` ServiceAccount

This token will be used by the ServiceMonitor and by in-cluster curl checks to authenticate to your local WVA metrics endpoint:

```shell
kubectl -n debugging create token ssh-gateway --duration=24h > /tmp/wva.token
```

4) Port-forward Prometheus (thanos-querier) and the SSH gateway service

We forward the thanos-querier so WVA (running locally) can query Prometheus via localhost, and forward the SSH gateway service so an SSH tunnel can be established.

```shell
# forward thanos-querier => local 9091
kubectl port-forward -n openshift-monitoring svc/thanos-querier 9091:9091

# forward ssh-gateway service => local 2222 (run in background if desired)
kubectl port-forward -n debugging svc/ssh-gateway 2222:22 &
```

5) Create a reverse SSH tunnel (cluster -> local)

From your local machine, open an SSH connection to the SSH Gateway pod and set up reverse port forwarding so the cluster can reach your WVA metrics endpoint at https://localhost:8443.

```shell
ssh -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -p 2222 \
    -R 0.0.0.0:8443:localhost:8443 \
    linuxserver.io@localhost
```

Notes:
- The `-R 0.0.0.0:8443:localhost:8443` exposes port 8443 on the remote (cluster) side so other pods (and Prometheus) can reach your local WVA.
- Keep the SSH session open while debugging.

6) Create a ServiceMonitor (example)

Use this ServiceMonitor (or adapt it) so the in-cluster Prometheus scrapes the SSH Gateway service which forwards to your local WVA metrics:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: tunnel-metrics-monitor
  namespace: openshift-user-workload-monitoring
spec:
  endpoints:
    - bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
      interval: 10s
      path: /metrics
      port: metrics
      scheme: https
      tlsConfig:
        insecureSkipVerify: true
  namespaceSelector:
    matchNames:
      - debugging
  selector:
    matchLabels:
      app: ssh-gateway
```

Apply it (if you need to):

```shell
kubectl apply -f path/to/your/servicemonitor.yaml
```

7) Environment variables — set these before running WVA locally

Example (adjust paths and values as needed):

```shell
export GLOBAL_OPT_INTERVAL=60s
export KUBECONFIG=/path/to/your/kubeconfig
export LOG_LEVEL=DEBUG
export PROMETHEUS_BASE_URL=https://127.0.0.1:9091
export PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true
export PROMETHEUS_BEARER_TOKEN="$(</tmp/wva.token)"
export WVA_SCALE_TO_ZERO=false
```

8) Run WVA locally

Start WVA from your IDE or terminal. With the environment variables above, WVA should query Prometheus via the forwarded thanos-querier and expose metrics on https://localhost:8443, reachable from the cluster via the SSH tunnel at https://ssh-gateway.debugging.svc.cluster.local:8443.

```shell
/path/to/wva/binary --metrics-bind-address :8443 --metrics-secure=true
```

### Troubleshooting / verification


- Check the local metrics endpoint (from your machine):

```shell
curl -k https://localhost:8443/metrics -H "Authorization: Bearer $(</tmp/wva.token)"
```

- From inside the SSH Gateway pod (verify it reaches your local WVA):

```shell
kubectl exec -n debugging deploy/ssh-gateway -- \
  curl -k https://localhost:8443/metrics -H "Authorization: Bearer $(</tmp/wva.token)"
```

- From another pod in the cluster (replace service/namespace if needed):

```
https://ssh-gateway.debugging.svc.cluster.local:8443/metrics
```

#### Troubleshooting checklist

- Are port-forward sessions running? Use `ps` or re-run the `kubectl port-forward` commands.
- Is the SSH tunnel open and active? The SSH client should be connected and not exited.
- Inspect local WVA logs for errors and TLS/auth issues.
- Check Prometheus / Prometheus Operator logs in `openshift-monitoring` and `openshift-user-workload-monitoring`.
- If the ServiceMonitor returns 401/403, ensure the token at `/tmp/wva.token` matches the ServiceAccount used by the ServiceMonitor.

#### Security notes

- The examples use insecure TLS settings (insecureSkipVerify) for convenience in a local debug flow — do not use these in production.
- The token created is short-lived in the example (24h). Rotate or revoke tokens as appropriate.

### FAQ / tips

- Q: Why forward thanos-querier? A: WVA queries Prometheus via the thanos-querier endpoint in the cluster; forwarding makes that endpoint available at localhost for local debugging.
- Q: Why use reverse SSH (-R)? A: It lets the cluster reach your local service without exposing your machine directly to the cluster network. The WVA exposes metrics to actuate scaling decisions.
- Q: I can't create the service account or the related cluster-wide roles and rolebindings as I do not have sufficient permissions in the cluster. A: You can use an existing service account with sufficient permissions instead. Just ensure the SSH Gateway deployment uses that service account and creates a token for that service account for running the WVA.

---

## Debugging Event Handling

WVA uses Kubernetes event watches to trigger reconciliation. Understanding how events flow helps debug timing and synchronization issues.

### Enable Debug Logging for Events

Add the `-v=2` flag to see debug-level event logs:

```yaml
# In deployment manifest
args:
- --health-probe-bind-address=:8081
- --metrics-bind-address=:8443
- --leader-elect
- -v=2  # Add this for debug logging
```

### Trace Deployment Events

When debugging deployment lifecycle issues, watch for these log messages:

```bash
# Deployment created - VA should trigger immediately
kubectl logs -n workload-variant-autoscaler-system deployment/controller-manager | grep "Deployment created"

# Example output:
# "Deployment created, triggering VA reconciliation" deployment="llama-8b" va="llama-8b-autoscaler"
```

```bash
# Deployment deleted - VA should update status
kubectl logs -n workload-variant-autoscaler-system deployment/controller-manager | grep -i "deployment.*delete\|DeploymentNotFound"
```

### Check VA Status After Deployment Deletion

```bash
# Check if VA detected deployment deletion
kubectl get va llama-8b-autoscaler -o jsonpath='{.status.conditions[?(@.type=="Ready")]}'

# Expected output when deployment is missing:
# {"type":"Ready","status":"False","reason":"DeploymentNotFound","message":"Target deployment 'llama-8b' no longer exists"}
```

### Verify Event Filtering

Check that the controller's event predicates are working correctly:

```bash
# Count reconciliation events in logs
kubectl logs -n workload-variant-autoscaler-system deployment/controller-manager | grep "Reconciling" | wc -l

# If this number is extremely high, event filtering may be misconfigured
```

### Test Race Condition Handling

To test that VA creation before deployment works correctly:

```bash
# 1. Create VA first
kubectl apply -f variantautoscaling.yaml

# 2. Verify VA status shows deployment not found
kubectl get va llama-8b-autoscaler -o jsonpath='{.status.conditions[?(@.type=="Ready")]}'

# 3. Create deployment
kubectl apply -f deployment.yaml

# 4. Verify VA status updates to Ready (should happen within seconds, not 60s)
kubectl get va llama-8b-autoscaler -o jsonpath='{.status.conditions[?(@.type=="Ready")]}'
```

### Common Event-Related Issues

**Issue: VA doesn't respond to deployment creation**

Check:
1. Deployment name matches VA's `scaleTargetRef.name` or inferred name from `modelId`
2. Controller logs show "Deployment created" message
3. No errors in controller logs about listing VAs

**Issue: VA takes 60 seconds to detect deployment**

This suggests deployment Create events aren't triggering reconciliation. Check:
1. Controller has RBAC permissions for Deployment watch (should be automatic)
2. No errors in controller startup logs about setting up watches

**Issue: Stale metrics after deployment deletion**

If metrics persist after deployment deletion:
1. Check VA status shows `Ready=False`
2. Verify controller logs show deployment deletion was detected
3. Check if metrics collection is working (query Prometheus directly)

For detailed information on event handling architecture, see [Controller Behavior Documentation](../design/controller-behavior.md).

