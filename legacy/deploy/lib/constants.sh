#!/usr/bin/env bash
#
# Shared deploy script constants.
# Sourced by deploy/install.sh before runtime helper libs.
# No function dependencies.
# Exposes shared names for waits, selectors, and common Kubernetes resources.
#

# Poll/wait defaults
WAIT_INTERVAL_10S=10
DEFAULT_VERIFY_STARTUP_SLEEP_SECONDS=10

# Shared resource names
EXTERNAL_METRICS_APISERVICE_NAME='v1beta1.external.metrics.k8s.io'
KEDA_RELEASE_NAME='keda'

# Common Kubernetes label selectors
WVA_CONTROLLER_LABEL_SELECTOR='app.kubernetes.io/name=workload-variant-autoscaler'
KEDA_OPERATOR_LABEL_SELECTOR='app.kubernetes.io/name=keda-operator'
