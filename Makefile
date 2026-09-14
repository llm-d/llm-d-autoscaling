# llm-d-autoscaling — KEDA manifest blueprints and autoscaling evaluation.
#
# This repository is not a Go project: it ships KEDA autoscaling blueprints and
# the llm-d-benchmark based evaluation test bed under benchmark/. The deprecated
# Workload-Variant-Autoscaler controller (and its own Makefile) lives under
# legacy/ while its removal is staged.

CLUSTER_NAME     ?= kind-gpu-cluster
CLUSTER_GPU_TYPE ?= nvidia-mix
CLUSTER_NODES    ?= 3
CLUSTER_GPUS     ?= 4
KUBECONFIG       ?= $(HOME)/.kube/config

KIND    ?= kind
KUBECTL ?= kubectl

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-24s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Evaluation clusters

# Creates a multi-node Kind cluster with emulated GPU labels and capacities per
# node, suitable for running the benchmark/ test bed without real accelerators.
.PHONY: create-kind-cluster
create-kind-cluster: ## Create a Kind cluster with emulated GPUs
	export KIND=$(KIND) KUBECTL=$(KUBECTL) && \
		hack/kind-emulator/setup.sh -c $(CLUSTER_NAME) -t $(CLUSTER_GPU_TYPE) -n $(CLUSTER_NODES) -g $(CLUSTER_GPUS)

.PHONY: destroy-kind-cluster
destroy-kind-cluster: ## Destroy the Kind cluster created by create-kind-cluster
	export KIND=$(KIND) KUBECTL=$(KUBECTL) KIND_NAME=$(CLUSTER_NAME) && \
		hack/kind-emulator/teardown.sh

##@ Lint

.PHONY: lint-scripts
lint-scripts: ## Syntax-check the shell scripts in this repository (excluding legacy/)
	@for script in hack/kind-emulator/*.sh benchmark/hack/*.sh; do \
		[ -f "$$script" ] || continue; \
		echo "bash -n $$script"; \
		bash -n "$$script"; \
	done
