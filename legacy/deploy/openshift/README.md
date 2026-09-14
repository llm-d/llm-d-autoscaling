# Workload-Variant-Autoscaler OpenShift Deployment Script

Automated deployment script for WVA and llm-d infrastructure on OpenShift clusters.

> **Note**: This guide covers OpenShift-specific deployment details. For a complete overview of deployment methods and the full configuration reference, see the [main deployment guide](../README.md).

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

## Overview

This script automates the complete deployment process on OpenShift cluster including:

- Workload-Variant-Autoscaler controller
- llm-d infrastructure (Gateway, Scheduler, vLLM)
- KEDA / Custom Metrics Autoscaler (CMA) for external metrics
- HPA integration
- All required ConfigMaps and RBAC
- Automatic GPU detection
- Deployment verification

## Prerequisites

### Required Tools

- **oc** (OpenShift CLI)
- **kubectl**
- **helm** (v3+)
- **yq** (v4+)
- **jq**
- **git**

### Required Access

- OpenShift cluster with **admin** privileges
- Logged in via `oc login`
- GPUs available in the cluster (H100, A100, L40S...)
*Note* to check the available GPU types on your OCP cluster, you can run:

```bash
kubectl get nodes -o jsonpath='{range .items[?(@.status.allocatable.nvidia\.com/gpu)]}{.metadata.name}{"\t"}{.metadata.labels.nvidia\.com/gpu\.product}{"\n"}{end}'
```

### Required Tokens

- **HuggingFace token** for model downloads

## Quick Start

### 1. Set Environment Variables

```bash
# Required: Set your HuggingFace token
export HF_TOKEN="your-hf-token-here"

# Optional: Customize deployment
export MODEL_ID="unsloth/Meta-Llama-3.1-8B"         # Default
export WVA_IMAGE="ghcr.io/llm-d/llm-d-workload-variant-autoscaler:latest"  # Default
```

### 2. Deploy the Workload Variant Autoscaler and llm-d using Make

```bash
make deploy-wva-on-openshift
```

That's it! The script will:

1. Check prerequisites

2. Detect GPU types on your OpenShift cluster

3. Deploy all components, including WVA, llm-d, and KEDA / CMA for external metrics

4. Verify the deployment

5. Print a summary with next steps

## Configuration Options

For a complete list of environment variables and configuration options, see the [Configuration Reference](../README.md#configuration-reference) in the main deployment guide.

**Key environment variables for OpenShift**:

```bash
export HF_TOKEN="hf_xxxxx"                  # Required: HuggingFace token
export MODEL_ID="unsloth/Meta-Llama-3.1-8B" # Model to deploy
export ACCELERATOR_TYPE="H100"              # GPU type (auto-detected)
export GATEWAY_PROVIDER="istio"             # Gateway: istio or kgateway

# Performance tuning (optional)
export HPA_STABILIZATION_SECONDS=240        # HPA stabilization window
```

**Deployment flags** (`deploy/install.sh`) — llm-d is deployed separately via `deploy/install-epp.sh` or the [llm-d guides](https://github.com/llm-d/llm-d/tree/main/guides/optimized-baseline):

```bash
export DEPLOY_WVA=true                    # Deploy WVA controller
export SCALER_BACKEND=keda                # Use the platform Custom Metrics Autoscaler (CMA/KEDA) for external metrics
```

**Note**: OpenShift uses the built-in User Workload Monitoring (Thanos) instead of deploying a separate Prometheus stack. KEDA is provided by the platform Custom Metrics Autoscaler (CMA) operator and is not Helm-installed by `install.sh`; install the CMA operator beforehand.

## Usage Examples

### Example 1: Full Deployment (Default)

```bash
export HF_TOKEN="hf_xxxxx"
make deploy-wva-on-openshift
```

### Example 2: Custom Model and Namespace

```bash
export HF_TOKEN="hf_xxxxx"
export BASE_NAME="my-inference"
export MODEL_ID="meta-llama/Llama-2-7b-hf"
make deploy-wva-on-openshift
```

### Example 3: CI-style stack (WVA + llm-d)

```bash
export HF_TOKEN="hf_xxxxx"
make deploy-wva-on-openshift   # install.sh (WVA + monitoring + scaler + LWS)
```

### Example 4: Deploy Only WVA (llm-d already deployed)

```bash
export DEPLOY_WVA=true
export SCALER_BACKEND=none
make deploy-wva-on-openshift
```

### Example 5: Parameter Estimation Setup

```bash
export HF_TOKEN="hf_xxxxx"
export MODEL_ID="unsloth/Meta-Llama-3.1-8B"
make deploy-wva-on-openshift
```

### Example 6: Re-run with Different GPU Type

```bash
export HF_TOKEN="hf_xxxxx"
export ACCELERATOR_TYPE="A100"
make deploy-wva-on-openshift
```

## Script Features

### Automatic Detection

- **GPU Type**: Automatically detects H100, A100, L40S etc... GPUs
- **Thanos URL**: Finds the correct Prometheus/Thanos endpoint
- **OpenShift Connection**: Verifies cluster connectivity

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

- llm-d infrastructure is deployed

- KEDA / CMA is running

- VariantAutoscaling resource exists

- HPA is configured

- External metrics API is accessible

### Summary Report

Displays:

- All deployed components

- Resource names and namespaces

- Next steps and useful commands

- How to verify and test

## What Gets Deployed

### 1. Workload-Variant-Autoscaler

- **Namespace**: `workload-variant-autoscaler-system`
- **Components**:
  - Controller manager deployment
  - Service for metrics
  - ServiceMonitor for Prometheus
  - ConfigMaps (service classes, accelerator costs)
  - RBAC (roles, bindings, service account)

### 2. llm-d Infrastructure

- **Namespace**: `llm-d-optimized-baseline` (default)
- **Components**:
  - Gateway (kgateway)
  - Inference Scheduler (GAIE)
  - vLLM deployment with model
  - Service for vLLM
  - ServiceMonitor for vLLM metrics
  - HuggingFace token secret

### 3. KEDA / Custom Metrics Autoscaler (CMA)

- **Provided by**: the OpenShift Custom Metrics Autoscaler operator (platform-managed)
- **Components**:
  - KEDA operator and metrics-apiserver
  - External metrics API (`external.metrics.k8s.io`)
  - Prometheus scaler triggers driven by ScaledObjects

### 4. Autoscaling Resources

- **VariantAutoscaling**: Custom resource for WVA optimization
- **HPA**: HorizontalPodAutoscaler for deployment scaling
- **Probes**: Health checks for vLLM pods

## Troubleshooting

### Script Fails: Missing Prerequisites

```bash
[ERROR] Missing required tools: yq helm
```

**Solution**: Install missing tools:

```bash
# macOS
brew install yq helm

# Linux
# Follow official installation guides for yq and helm
```

### Script Fails: Not Logged Into OpenShift

```bash
[ERROR] Not logged into OpenShift cluster
```

**Solution**: Log in first:

```bash
oc login --token=<your-token> --server=<your-server>
```

### Script Fails: HF_TOKEN Not Set

```bash
[ERROR] HF_TOKEN environment variable is not set
```

**Solution**: Set your HuggingFace token:

```bash
export HF_TOKEN="hf_xxxxxxxxxxxxxxxxxxxxx"
```

### Deployment Succeeds But Metrics Not Available

**Wait 1-2 minutes** for:

- Prometheus to scrape metrics

- KEDA to read them via its Prometheus scaler

- External metrics API to update

**Check status**:

```bash
kubectl get pods -n keda-system
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/llm-d-optimized-baseline/wva_desired_replicas" | jq
```

### vLLM Pods Not Starting

**Check logs**:

```bash
kubectl logs -n llm-d-optimized-baseline deployment/optimized-baseline-nvidia-gpu-vllm-decode
```

**Common issues**:

- Insufficient GPU resources

- HuggingFace token invalid/expired

- Model download timeout

- Inappropriate SLOs for the deployed model and GPU types: update the `SLO_TPOT` and `SLO_TTFT` variables with appropriate SLOs given the model and employed GPU type

## Post-Deployment

### Verify Deployment

```bash
# Check all components
kubectl get pods -n workload-variant-autoscaler-system
kubectl get pods -n llm-d-optimized-baseline
kubectl get variantautoscaling -n llm-d-optimized-baseline
kubectl get hpa -n llm-d-optimized-baseline

# Check external metrics
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/llm-d-optimized-baseline/wva_desired_replicas" | jq
```

### Monitor WVA Logs

```bash
kubectl logs -n workload-variant-autoscaler-system \
  deployment/controller-manager \
  -f
```

### Run E2E Tests on OpenShift

```bash
export ENVIRONMENT=openshift
make test-e2e-full
```

### Generate Load

```bash
# Create a load generation job
kubectl apply -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: vllm-bench-test
  namespace: llm-d-optimized-baseline
spec:
  template:
    spec:
      containers:
      - name: vllm-bench
        image: vllm/vllm-openai:latest
        command: ["/bin/sh", "-c"]
        args:
        - |
          python3 -m vllm.entrypoints.cli.main bench serve \
            --backend openai \
            --base-url http://optimized-baseline-inference-gateway:80 \
            --model unsloth/Meta-Llama-3.1-8B \
            --request-rate 20 \
            --num-prompts 1000
      restartPolicy: Never
EOF
```

## Cleanup

To remove all deployed components:

```bash
# Delete llm-d infrastructure (v0.7.0+: single GAIE Helm release + Kustomize model server)
helm uninstall optimized-baseline -n llm-d-optimized-baseline
kubectl delete -k llm-d/guides/optimized-baseline/modelserver/gpu/vllm/base -n llm-d-optimized-baseline

# KEDA / CMA is platform-managed on OpenShift; remove ScaledObjects, not the operator
kubectl delete scaledobject --all -n llm-d-optimized-baseline

# Delete WVA
make undeploy

# Delete namespaces
kubectl delete namespace llm-d-optimized-baseline
```
