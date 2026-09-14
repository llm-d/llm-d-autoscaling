# Workload-Variant-Autoscaler Kubernetes Deployment Script

Automated deployment script for WVA, llm-d infrastructure, Prometheus, and HPA on Kubernetes clusters.

> **Note**: This guide covers Kubernetes-specific deployment details. For a complete overview of deployment methods and the full configuration reference, see the [main deployment guide](../README.md).

## Table of Contents

- [Overview](#overview)
- [Prerequisites](#prerequisites)
- [Quick Start](#quick-start)
- [Configuration Options](#configuration-options)
- [Usage Examples](#usage-examples)
- [Script Features](#script-features)
- [What Gets Deployed](#what-gets-deployed)
- [Troubleshooting](#troubleshooting)
- [Post-Deployment](#post-deployment)
- [Cleanup](#cleanup)
- [Metrics Validation Feature](#metrics-validation-feature)
- [Advanced Usage](#advanced-usage)
- [Performance Tuning](#performance-tuning)

## Overview

This script automates the complete deployment process on kubernetes cluster including:

- **kube-prometheus-stack** (Prometheus + Grafana + Alertmanager)
- **Workload-Variant-Autoscaler** controller with metrics validation
- **llm-d infrastructure** (Gateway, Scheduler, vLLM)
- **KEDA** for external metrics (ScaledObject-driven)
- **HPA** integration for autoscaling
- **ServiceMonitors** for metrics collection
- **VariantAutoscaling** custom resources
- All required ConfigMaps and RBAC

## Prerequisites

### Required Tools

- **kubectl** - Kubernetes CLI
- **helm** (v3+) - Package manager
- **git** - Version control
- **jq** - JSON processor (optional, for verification)
- **yq** (v4+) - YAML processor (optional, used if available)

### Required Access

- Kubernetes cluster with **cluster-admin** privileges (or sufficient RBAC)
- `kubectl` configured and connected to your cluster
- GPUs available in the cluster (or use emulator mode for demo)

### Required Tokens

- **HuggingFace token** for model downloads (required for llm-d deployment)

## Quick Start

### 1. Set Environment Variables

```bash
# Required: Set your HuggingFace token
export HF_TOKEN="your-hf-token-here"

# Optional: Customize deployment (basename under llm-d guides/; llm-d main uses optimized-baseline)
export GUIDE_NAME="optimized-baseline"                                  # Default guide name
export MODEL_ID="unsloth/Meta-Llama-3.1-8B"                               # Default
export WVA_IMAGE_REPO="ghcr.io/llm-d/llm-d-workload-variant-autoscaler"         # Default
export WVA_IMAGE_TAG="latest"                                             # Default
export ACCELERATOR_TYPE="H100"                                            # Auto-detected or default
```

### 2. Run the Deployment Script using Make

```bash
make deploy-wva-on-k8s
```

That's it! The script will:

1. Check prerequisites
2. Detect GPU types
3. Create namespaces  (by default)
4. Deploy Prometheus stack  (by default)
5. Deploy WVA controller  (by default)
6. Deploy llm-d infrastructure  (by default)
7. Deploy KEDA for external metrics (by default)
8. Create the VariantAutoscaling resource for the vLLM deployment  (by default)
9. Deploy HPA  (by default)
10. Verify the deployment process
11. Print a summary with next steps

## Configuration Options

For a complete list of environment variables and configuration options, see the [Configuration Reference](../README.md#configuration-reference) in the main deployment guide.

**Key environment variables for Kubernetes**:

```bash
export HF_TOKEN="hf_xxxxx"                  # Required: HuggingFace token
export MODEL_ID="unsloth/Meta-Llama-3.1-8B" # Model to deploy
export ACCELERATOR_TYPE="H100"              # GPU type
export WVA_IMAGE_TAG="latest"               # WVA version
# HPA stabilization: configure on the HPA resource directly, not install.sh

# Performance tuning (optional; set in llm-d ModelService manifest)
export ACCELERATOR_TYPE="A100"              # GPU type (auto-detected)
```

**Deployment flags** (`deploy/install.sh`) — VA/HPA are managed separately; llm-d is deployed via `deploy/install-epp.sh` or the [llm-d guides](https://github.com/llm-d/llm-d/tree/main/guides/optimized-baseline):

```bash
export DEPLOY_PROMETHEUS=true               # Deploy kube-prometheus-stack
export DEPLOY_OPERATIONAL_DASHBOARD=true    # Deploy kube-prometheus-stack-grafana
export DEPLOY_WVA=true                      # Deploy WVA controller
export SCALER_BACKEND=keda                  # Deploy KEDA for external metrics
```

## Usage Examples

### Example 1: Full Deployment (Default)

```bash
export HF_TOKEN="hf_xxxxx"
export DEPLOY_VA=true
export DEPLOY_HPA=true
make deploy-wva-on-k8s
```

### Example 2: Custom Model and Namespace

```bash
export HF_TOKEN="hf_xxxxx"
export BASE_NAME="my-inference"
export MODEL_ID="meta-llama/Llama-2-7b-hf"
export DEPLOY_VA=true
export DEPLOY_HPA=true
make deploy-wva-on-k8s
```

### Example 3: CI-style stack (WVA + llm-d)

```bash
export HF_TOKEN="hf_xxxxx"
make deploy-wva-on-k8s   # install.sh (WVA + monitoring + scaler + LWS)
```

### Example 4: Deploy only WVA + Prometheus (llm-d already deployed)

```bash
export DEPLOY_WVA=true
export DEPLOY_PROMETHEUS=true
export DEPLOY_OPERATIONAL_DASHBOARD=true  # Grafana and operational dashboard
export SCALER_BACKEND=none                # Skip scaler backend (KEDA already installed)
make deploy-wva-on-k8s
```

### Example 5: Demo Mode (No GPUs Available)

```bash
export HF_TOKEN="hf_xxxxx"
export USE_VLLM_EMULATOR=true
make deploy-wva-on-k8s
```

### Example 6: Parameter Estimation Setup

```bash
export HF_TOKEN="hf_xxxxx"
export MODEL_ID="unsloth/Meta-Llama-3.1-8B"
make deploy-wva-on-k8s
```

### Example 7: Deploy with Different WVA Image

```bash
export HF_TOKEN="hf_xxxxx"
export IMG="ghcr.io/yourorg/llm-d-workload-variant-autoscaler:latest"
make deploy-wva-on-k8s
```

## Script Features

### Automatic Detection

- **GPU Type**: Automatically detects A100, H100, L40S, etc.
- **GPU Availability**: Detects if GPUs are visible to Kubernetes
- **Emulator Mode**: Auto-enables if no GPUs detected
- **Kubernetes Connection**: Verifies cluster connectivity

### Error Handling

- Exits on any error (`set -e`)
- Validates prerequisites before starting
- Checks for required environment variables
- Provides detailed error messages

### Progress Tracking

- Color-coded output (INFO, SUCCESS, WARNING, ERROR)
- Step-by-step progress indicators
- Detailed logging of each operation

### Deployment Verification

After deployment, the script verifies:

- WVA controller is running
- Prometheus stack is deployed
- llm-d infrastructure is deployed
- VariantAutoscaling resource exists
- HPA is configured
- ServiceMonitors are created

### Summary Report

Displays:

- All deployed components
- Resource names and namespaces
- Next steps and useful commands
- Troubleshooting tips

## What Gets Deployed

### 1. kube-prometheus-stack

- **Namespace**: `workload-variant-autoscaler-monitoring`
- **Components**:
  - Prometheus server (HTTPS/TLS enabled)
  - Grafana dashboards
  - Alertmanager
  - kube-state-metrics
  - node-exporter
  - ServiceMonitor CRDs and operators

### 2. Workload-Variant-Autoscaler

- **Namespace**: `workload-variant-autoscaler-system`
- **Components**:
  - Controller manager deployment (2 replicas)
  - Service for metrics (port 8443)
  - ServiceMonitor for WVA metrics
  - ConfigMaps (service classes, accelerator costs, config)
  - RBAC (roles, bindings, service account)

### 3. llm-d Infrastructure

- **Namespace**: `llm-d-optimized-baseline` (default)
- **Components**:
  - Gateway
  - Inference Scheduler (GAIE/EPP)
  - vLLM deployment with model
  - Service for vLLM (port 8200)
  - ServiceMonitor for vLLM metrics
  - HuggingFace token secret

### 4. KEDA

- **Namespace**: `keda-system`
- **Components**:
  - KEDA operator and metrics-apiserver deployments
  - External metrics API (`external.metrics.k8s.io`)
  - Prometheus scaler triggers driven by ScaledObjects

### 5. Autoscaling Resources

- **VariantAutoscaling**: Custom resource for WVA optimization
- **HPA**: HorizontalPodAutoscaler for deployment scaling
- **ServiceMonitors**: Metrics collection configuration

## Troubleshooting

### Script Fails: Missing Prerequisites

```bash
[ERROR] Missing required tools: helm kubectl
```

**Solution**: Install missing tools:

```bash
# Using package manager
sudo apt-get install helm kubectl  # Debian/Ubuntu
brew install helm kubectl          # macOS

# Or download from official sites
```

### Script Fails: Cannot Connect to Kubernetes

```bash
[ERROR] Cannot connect to Kubernetes cluster
```

**Solution**: Check kubectl configuration:

```bash
kubectl cluster-info
kubectl get nodes
```

### Script Fails: HF_TOKEN Not Set

**Solution**: Set your HuggingFace token:

```bash
export HF_TOKEN="hf_xxxxxxxxxxxxxxxxxxxxx"
```

### GPUs Not Visible to Kubernetes

**Symptoms**:

```bash
[WARNING] No GPUs visible to Kubernetes
vLLM pods: Pending
Reason: Insufficient nvidia.com/gpu
```

**Diagnosis**:

```bash
# Check if GPUs exist on host
nvidia-smi

# Check if GPUs visible to Kubernetes
kubectl get nodes -o json | jq '.items[].status.allocatable["nvidia.com/gpu"]'
```

**Solution**:

**Option 1**: Install NVIDIA GPU Operator

```bash
helm repo add nvidia https://helm.ngc.nvidia.com/nvidia
helm repo update
helm install --wait --generate-name \
  -n gpu-operator --create-namespace \
  nvidia/gpu-operator
```

**Option 2**: Install NVIDIA Device Plugin

```bash
kubectl create -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.14.0/nvidia-device-plugin.yml
```

**Option 3**: Use Emulator Mode (for demo/testing)

```bash
export USE_VLLM_EMULATOR=true
make deploy-wva-on-k8s
```

**Note for KIND clusters**: KIND (Kubernetes IN Docker) requires special configuration for GPU passthrough. Consider using a real Kubernetes cluster or emulator mode.

### Metrics Not Available After Deployment

**Wait 1-2 minutes** for:

- vLLM pods to start
- Prometheus to scrape metrics
- KEDA to read them via its Prometheus scaler
- External metrics API to update

**Check metrics availability**:

```bash
# Check WVA logs
kubectl logs -n workload-variant-autoscaler-system -l control-plane=controller-manager --tail=50

# Look for metrics validation
kubectl logs -n workload-variant-autoscaler-system -l control-plane=controller-manager | grep "Metrics unavailable"

# Check if vLLM is exposing metrics
kubectl port-forward -n llm-d-optimized-baseline <vllm-pod> 8200:8200
curl http://localhost:8200/metrics | grep vllm:
```

### vLLM Pods Not Starting

**Check logs**:

```bash
kubectl logs -n llm-d-optimized-baseline deployment/optimized-baseline-nvidia-gpu-vllm-decode
kubectl describe pod -n llm-d-optimized-baseline -l llm-d.ai/model
```

**Common issues**:

- Insufficient GPU resources
- HuggingFace token invalid/expired
- Model download timeout
- Image pull errors

### ServiceMonitor Not Scraping

**Verify ServiceMonitor exists**:

```bash
kubectl get servicemonitor -n workload-variant-autoscaler-monitoring
```

**Check Prometheus targets**:

```bash
kubectl port-forward -n workload-variant-autoscaler-monitoring svc/kube-prometheus-stack-prometheus 9090:9090
# Visit http://localhost:9090/targets
```

**Verify service selector matches**:

```bash
kubectl get servicemonitor vllm-servicemonitor -n workload-variant-autoscaler-monitoring -o yaml
kubectl get svc -n llm-d-optimized-baseline --show-labels
```

## Post-Deployment

### Verify Deployment

```bash
# Check all namespaces
kubectl get pods -n workload-variant-autoscaler-system
kubectl get pods -n workload-variant-autoscaler-monitoring
kubectl get pods -n llm-d-optimized-baseline

# Check VariantAutoscaling (with NEW MetricsReady column!)
kubectl get variantautoscaling -n llm-d-optimized-baseline -o wide

# Check detailed status with conditions
kubectl describe variantautoscaling optimized-baseline-nvidia-gpu-vllm-decode -n llm-d-optimized-baseline

# Check HPA
kubectl get hpa -n llm-d-optimized-baseline

# Check external metrics
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/llm-d-optimized-baseline/wva_desired_replicas" | jq
```

### Monitor WVA Logs (See Metrics Validation!)

```bash
# Watch live logs
kubectl logs -n workload-variant-autoscaler-system \
  -l control-plane=controller-manager \
  -f

# Filter for metrics validation messages
kubectl logs -n workload-variant-autoscaler-system \
  -l control-plane=controller-manager | \
  jq 'select(.reason=="MetricsMissing" or .reason=="MetricsFound")'
```

### Access Prometheus UI

```bash
kubectl port-forward -n workload-variant-autoscaler-monitoring \
  svc/kube-prometheus-stack-prometheus 9090:9090

# Visit http://localhost:9090
# Query: vllm:request_success_total
# Query: wva_desired_replicas
```

### Access Grafana Dashboards

```bash
# Get admin password
kubectl get secret -n workload-variant-autoscaler-monitoring \
  kube-prometheus-stack-grafana \
  -o jsonpath="{.data.admin-password}" | base64 -d ; echo

# Port forward
kubectl port-forward -n workload-variant-autoscaler-monitoring \
  svc/kube-prometheus-stack-grafana 3000:80

# Visit http://localhost:3000
# Username: admin
# Password: <from above command>
```

### Generate Load (Test Autoscaling)

If using real vLLM with model loaded:

```bash
# Deploy guidellm load generator
kubectl apply -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: guidellm-load-test
  namespace: llm-d-optimized-baseline
spec:
  template:
    spec:
      containers:
      - name: guidellm
        image: quay.io/vishakharamani/guidellm:latest
        args:
          - benchmark
          - --target=http://optimized-baseline-inference-gateway:80
          - --rate-type=constant
          - --rate=10
          - --max-seconds=300
          - --model=unsloth/Meta-Llama-3.1-8B
      restartPolicy: Never
EOF
```

Watch the autoscaling:

```bash
# Watch VariantAutoscaling status update
kubectl get variantautoscaling -n llm-d-optimized-baseline -w

# Watch HPA scaling
kubectl get hpa -n llm-d-optimized-baseline -w

# Watch pod count change
kubectl get pods -n llm-d-optimized-baseline -w
```

## Cleanup

To remove all deployed components:

```bash
make undeploy-wva-on-k8s
```

Or manually:

```bash
# Delete llm-d infrastructure (v0.7.0+)
helm uninstall optimized-baseline -n llm-d-optimized-baseline
kubectl delete -k llm-d/guides/optimized-baseline/modelserver/gpu/vllm/base -n llm-d-optimized-baseline

# Delete KEDA
helm uninstall keda -n keda-system

# Delete kube-prometheus-stack
helm uninstall kube-prometheus-stack -n workload-variant-autoscaler-monitoring

# Delete WVA
cd /path/to/workload-variant-autoscaler
kubectl delete -k config/overlays/cluster-scoped/kubernetes

# Delete namespaces
kubectl delete namespace llm-d-optimized-baseline
kubectl delete namespace workload-variant-autoscaler-system
kubectl delete namespace workload-variant-autoscaler-monitoring
```

## Metrics Validation Feature

This deployment includes the **NEW metrics health monitoring system**:

### What It Does

1. **Validates vLLM metrics** before optimization
2. **Sets status conditions** on VariantAutoscaling:
   - `MetricsAvailable` - Are vLLM metrics available?
   - `OptimizationReady` - Can optimization run?
3. **Provides actionable errors** when ServiceMonitor isn't working
4. **Implements graceful degradation** when metrics unavailable

### Viewing Metrics Health

```bash
# See MetricsReady column
kubectl get variantautoscaling -n llm-d-optimized-baseline

# Example output:
# NAME        MODEL           ACCELERATOR  CURRENT  OPTIMIZED  METRICSREADY  AGE
# my-variant  llama-3-8b      A100         2        3          True          5m
```

### Viewing Detailed Conditions

```bash
kubectl describe variantautoscaling optimized-baseline-nvidia-gpu-vllm-decode \
  -n llm-d-optimized-baseline

# Look for:
# Status:
#   Conditions:
#     Type:     MetricsAvailable
#     Status:   True/False
#     Reason:   MetricsFound/MetricsMissing/MetricsStale/PrometheusError
#     Message:  Detailed troubleshooting information
```

### Understanding Metrics Validation Logs

When metrics are unavailable, you'll see structured logs like:

```json
{
  "level": "WARN",
  "ts": "2025-10-13T18:36:52.670Z",
  "msg": "Metrics unavailable, skipping optimization for variant",
  "variant": "optimized-baseline-nvidia-gpu-vllm-decode",
  "namespace": "llm-d-optimized-baseline",
  "model": "meta-llama/Llama-3.1-8B",
  "reason": "MetricsMissing",
  "troubleshooting": "Check: (1) ServiceMonitor exists in monitoring namespace..."
}
```

This means:

- WVA is working correctly
- Detecting no metrics available
- Skipping optimization gracefully
- Providing troubleshooting steps
- Will retry automatically

## Advanced Usage

### Custom Prometheus Configuration

```bash
# Use existing Prometheus
export DEPLOY_PROMETHEUS=false
export PROMETHEUS_URL="https://my-prometheus.monitoring.svc:9090"
make deploy-wva-on-k8s
```

### Deploy to Specific Cluster Context

```bash
kubectl config use-context my-cluster
make deploy-wva-on-k8s
```

### Debug Mode

```bash
# Enable debug logging in WVA
kubectl set env deployment/controller-manager \
  LOG_LEVEL=debug \
  -n workload-variant-autoscaler-system
```

### Update WVA Image

```bash
export IMG="ghcr.io/yourorg/llm-d-workload-variant-autoscaler:custom-tag"
export DEPLOY_PROMETHEUS=false
make deploy-wva-on-k8s   # WVA + monitoring + scaler + LWS; llm-d is managed separately
```

## Performance Tuning

### Optimization Interval

```bash
# Change how often WVA runs optimization (default: 15s, minimum 1s, unit required)
kubectl patch configmap wva-manager-config \
  -n workload-variant-autoscaler-system \
  --type merge \
  -p '{"data":{"GLOBAL_OPT_INTERVAL":"30s"}}'

# Restart WVA to apply
kubectl rollout restart deployment controller-manager \
  -n workload-variant-autoscaler-system
```

### HPA Tuning

```bash
# Faster scale-up
kubectl patch hpa vllm-deployment-hpa -n llm-d-optimized-baseline --type merge -p '
{
  "spec": {
    "behavior": {
      "scaleUp": {
        "stabilizationWindowSeconds": 0,
        "policies": [{"type": "Pods", "value": 10, "periodSeconds": 15}]
      }
    }
  }
}'
```
