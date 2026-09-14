#!/usr/bin/env bash
#
# Shared Kubernetes-like adapter functions used by:
#   - deploy/kubernetes/install.sh
#   - deploy/kind-emulator/install.sh
# Requires funcs: create_namespaces_shared_loop(), deploy_prometheus_kube_stack(),
# undeploy_prometheus_kube_stack(), deploy_wva_prerequisites_kube_like().
# Requires var: KUBE_LIKE_VALUES_DEV_IF_PRESENT to be set by caller.
#
materialize_namespace() {
    kubectl create namespace "$1"
}

create_namespaces() {
    create_namespaces_shared_loop
}

deploy_prometheus_stack() {
    deploy_prometheus_kube_stack
}

undeploy_prometheus_stack() {
    undeploy_prometheus_kube_stack
}

deploy_wva_prerequisites() {
    deploy_wva_prerequisites_kube_like
}
