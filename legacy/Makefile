# Image URL to use all building/pushing image targets
IMAGE_TAG_BASE ?= ghcr.io/llm-d
IMG_TAG ?= latest
IMG ?= $(IMAGE_TAG_BASE)/llm-d-workload-variant-autoscaler:$(IMG_TAG)
KIND_ARGS ?= -t mix -n 3 -g 2   # Default: 3 nodes, 2 GPUs per node, mixed vendors
CLUSTER_NAME ?= kind-wva-gpu-cluster
CLUSTER_GPU_TYPE ?= nvidia-mix
CLUSTER_NODES ?= 3
CLUSTER_GPUS ?= 4
KUBECONFIG ?= $(HOME)/.kube/config
K8S_VERSION ?= v1.32.0

WVA_NS              ?= workload-variant-autoscaler-system
CONTROLLER_NAMESPACE ?= workload-variant-autoscaler-system
MONITORING_NAMESPACE ?= openshift-user-workload-monitoring
LLMD_NAMESPACE       ?= llm-d-optimized-baseline
GATEWAY_NAME         ?= # discovered automatically in e2es
MODEL_ID             ?= e2ewva/dummy-model
DEPLOYMENT           ?= # discovered automatically in e2es
REQUEST_RATE         ?= 20
NUM_PROMPTS          ?= 3000

# E2E test configuration (for test/e2e/ suite)
ENVIRONMENT                 ?= kind-emulator
USE_SIMULATOR               ?= true
SCALE_TO_ZERO_ENABLED       ?= false
DEPLOY_ALERTING_RULES       ?= false
SCALER_BACKEND              ?= keda  # keda (ScaledObject) or none (skip, use pre-installed backend)
LLM_D_ROUTER_VERSION        ?= v0.9.0
GAIE_VERSION                ?= v1.5.0
KV_SPARE_TRIGGER           ?=
QUEUE_SPARE_TRIGGER         ?=
E2E_MONITORING_NAMESPACE    ?= workload-variant-autoscaler-monitoring
E2E_EMULATED_LLMD_NAMESPACE ?= llm-d-sim
E2E_KEDA_NAMESPACE          ?= keda-system
E2E_WVA_SECONDARY_OVERLAY_PATH ?= $(CURDIR)/test/e2e/testdata/secondary-controller
LLMD_SOURCE                 ?=
LLMD_SOURCE_SHA             ?=
# llm-d-benchmark CLI configuration
# Ensure brew-installed tools (helm >=3.19) take precedence over Rancher Desktop
export PATH := /opt/homebrew/bin:$(PATH)
BENCHMARK_REPO_URL   ?= https://github.com/llm-d/llm-d-benchmark.git
BENCHMARK_REPO_DIR   ?= $(CURDIR)/llm-d-benchmark
BENCHMARK_DIRECT_KEDA ?= false
BENCHMARK_REPO_REF   ?= $(if $(filter true,$(BENCHMARK_DIRECT_KEDA)),main,v0.7.0)
BENCHMARK_SPEC       ?= $(if $(filter true,$(BENCHMARK_DIRECT_KEDA)),guides/epp-keda-saturation,guides/workload-autoscaling)
BENCHMARK_NAMESPACE  ?= # set via BENCHMARK_NAMESPACE=<namespace>
BENCHMARK_GATEWAY_URL ?= http://infra-llmdbench-inference-gateway-istio.$(BENCHMARK_NAMESPACE).svc.cluster.local:80
BENCHMARK_WORKSPACE  ?= $(CURDIR)
BENCHMARK_HARNESS    ?= guidellm
BENCHMARK_WORKLOAD   ?= prefill_heavy
BENCHMARK_FORCE      ?= true
BENCHMARK_MONITORING ?= true
BENCHMARK_UV         ?= false
BENCHMARK_SCENARIOS_DIR ?= $(CURDIR)/test/benchmark/scenarios
BENCHMARK_MODEL_ID   ?= $(MODEL_ID)
BENCHMARK_DECODE_REPLICAS ?= 1
BENCHMARK_KEDA_MIN_REPLICAS ?= 1
BENCHMARK_KEDA_MAX_REPLICAS ?= 10
BENCHMARK_KEDA_SCALE_UP_PERIOD ?= 0
BENCHMARK_KEDA_SCALE_DOWN_PERIOD ?= 300

# Flags for deploy/install.sh (e2e / CI-style cluster infra; no chart VA/HPA).
CREATE_CLUSTER    ?= false
DELETE_CLUSTER    ?= false
DELETE_NAMESPACES ?= false


# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration and ClusterRole objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role webhook paths="./..." \
		output:rbac:artifacts:config=config/base/rbac
	# controller-gen writes `role.yaml`; rename to match the
	# (<app>-)?<kind>.yaml convention used under config/.
	mv config/base/rbac/role.yaml config/base/rbac/manager-clusterrole.yaml

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest helm ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" PATH="$(LOCALBIN):$(PATH)" go test $$(go list ./... | grep -v /e2e | grep -v /benchmark) -coverprofile cover.out

# Creates a multi-node Kind cluster
# Adds emulated GPU labels and capacities per node
.PHONY: create-kind-cluster
create-kind-cluster:
	export KIND=$(KIND) KUBECTL=$(KUBECTL) && \
		deploy/kind-emulator/setup.sh -c $(CLUSTER_NAME) -t $(CLUSTER_GPU_TYPE) -n $(CLUSTER_NODES) -g $(CLUSTER_GPUS)

# Destroys the Kind cluster created by `create-kind-cluster`
.PHONY: destroy-kind-cluster
destroy-kind-cluster:
	export KIND=$(KIND) KUBECTL=$(KUBECTL) KIND_NAME=$(CLUSTER_NAME) && \
        deploy/kind-emulator/teardown.sh


## Deploy WVA to OpenShift cluster with specified image.
.PHONY: deploy-wva-on-openshift
deploy-wva-on-openshift: manifests kustomize ## Deploy WVA to OpenShift cluster with specified image.
	@echo "Deploying WVA to OpenShift with image: $(IMG)"
	@echo "Target namespace: $(WVA_NS)"
	WVA_NS=$(WVA_NS) IMG=$(IMG) ENVIRONMENT=openshift ./deploy/install.sh

## Undeploy WVA from OpenShift.
.PHONY: undeploy-wva-on-openshift
undeploy-wva-on-openshift:
	@echo ">>> Undeploying workload-variant-autoscaler from OpenShift"
	export KIND=$(KIND) KUBECTL=$(KUBECTL) ENVIRONMENT=openshift WVA_NS=$(WVA_NS) && \
		deploy/install.sh --undeploy

## Deploy WVA on Kubernetes with the specified image.
.PHONY: deploy-wva-on-k8s
deploy-wva-on-k8s: manifests kustomize ## Deploy WVA on Kubernetes with the specified image.
	@echo "Deploying WVA on Kubernetes with image: $(IMG)"
	@echo "Target namespace: $(WVA_NS)"
	WVA_NS=$(WVA_NS) IMG=$(IMG) ENVIRONMENT=kubernetes ./deploy/install.sh

## Undeploy WVA from Kubernetes.
.PHONY: undeploy-wva-on-k8s
undeploy-wva-on-k8s:
	@echo ">>> Undeploying workload-variant-autoscaler from Kubernetes"
	export KIND=$(KIND) KUBECTL=$(KUBECTL) ENVIRONMENT=kubernetes WVA_NS=$(WVA_NS) && \
		deploy/install.sh --undeploy

# E2E tests on Kind cluster for saturation-based autoscaling
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# Supports FOCUS and SKIP variables for ginkgo test filtering.
# Setup options:
# - CERT_MANAGER_INSTALL_SKIP=true: Skip certManager installation during test setup.
# - IMAGE_BUILD_SKIP=true: Skip building the WVA docker image during test setup.
# - INFRA_SETUP_SKIP=true: Skip setting up the llm-d and the WVA controller manager during test setup. Reload the docker image if necessary.
# - INFRA_TEARDOWN_SKIP=true: Skip tearing down the Kind cluster during test teardown.

# Consolidated e2e test targets (environment-agnostic)
# These targets use the test/e2e/ suite that works on any Kubernetes cluster
# Supports FOCUS and SKIP variables for ginkgo test filtering.

# Deploys WVA + monitoring + scaler (install.sh), then EPP (install-epp.sh). No model server or VA/HPA.
# Works for all environments: kind-emulator (default), openshift, kubernetes.
# For OpenShift/Kubernetes: ENVIRONMENT=openshift LLMD_NS=<your-ns> make deploy-e2e-infra
# If IMG is set, builds the image locally first (unless SKIP_BUILD=true).
.PHONY: deploy-e2e-infra
deploy-e2e-infra: ## Deploy e2e test infrastructure (WVA + EPP; no model server or VA/HPA). Works for kind-emulator, openshift, kubernetes.
	@echo "Deploying e2e test infrastructure..."
	@if [ -n "$(IMG)" ]; then \
		echo "IMG is set to '$(IMG)'"; \
		if [ "$(SKIP_BUILD)" != "true" ]; then \
			echo "Building local image (SKIP_BUILD not set)..."; \
			$(MAKE) docker-build IMG=$(IMG); \
		else \
			echo "Skipping image build (SKIP_BUILD=true) - assuming image already exists"; \
		fi; \
		echo "Extracting image repo and tag from IMG..."; \
		if echo "$(IMG)" | grep -q ":"; then \
			IMAGE_REPO=$$(echo $(IMG) | cut -d: -f1); \
			IMAGE_TAG=$$(echo $(IMG) | cut -d: -f2); \
		else \
			IMAGE_REPO="$(IMG)"; \
			IMAGE_TAG="latest"; \
		fi; \
		echo "Using local image: $$IMAGE_REPO:$$IMAGE_TAG"; \
		ENVIRONMENT=$(ENVIRONMENT) \
		SCALER_BACKEND=$(SCALER_BACKEND) \
		ENABLE_SCALE_TO_ZERO=$(SCALE_TO_ZERO_ENABLED) \
		DEPLOY_ALERTING_RULES=$(DEPLOY_ALERTING_RULES) \
		WVA_IMAGE_REPO=$$IMAGE_REPO \
		WVA_IMAGE_TAG=$$IMAGE_TAG \
		WVA_IMAGE_PULL_POLICY=IfNotPresent \
		./deploy/install.sh; \
	else \
		echo "IMG not set - using default image from registry (latest)"; \
		ENVIRONMENT=$(ENVIRONMENT) \
		SCALER_BACKEND=$(SCALER_BACKEND) \
		ENABLE_SCALE_TO_ZERO=$(SCALE_TO_ZERO_ENABLED) \
		DEPLOY_ALERTING_RULES=$(DEPLOY_ALERTING_RULES) \
		./deploy/install.sh; \
	fi
	@ENVIRONMENT=$(ENVIRONMENT) \
		LLM_D_ROUTER_VERSION=$(LLM_D_ROUTER_VERSION) \
		GAIE_VERSION=$(GAIE_VERSION) \
		LLMD_NS=$${LLMD_NS:-$(E2E_EMULATED_LLMD_NAMESPACE)} \
		WVA_PROJECT=$(CURDIR) \
		ENABLE_SCALE_TO_ZERO=$(SCALE_TO_ZERO_ENABLED) \
		./deploy/install-epp.sh
	@NS=$${WVA_NS:-workload-variant-autoscaler-system}; \
	if [ -n "$(KV_SPARE_TRIGGER)" ] || [ -n "$(QUEUE_SPARE_TRIGGER)" ]; then \
		echo "Applying optional WVA capacity threshold overrides (KV_SPARE_TRIGGER / QUEUE_SPARE_TRIGGER)..."; \
		$(KUBECTL) patch configmap wva-saturation-scaling-config \
			-n "$$NS" --type=merge \
			-p "{\"data\":{\"default\":\"kvSpareTrigger: $(KV_SPARE_TRIGGER)\\nqueueSpareTrigger: $(QUEUE_SPARE_TRIGGER)\\n\"}}"; \
	fi


# Deploys the canonical llm-d KEDA+EPP guide contract through the existing
# controller-free e2e infrastructure lifecycle. This target requires a fresh,
# uniquely named Kind cluster created separately with create-kind-cluster.
.PHONY: deploy-e2e-keda-epp-guide
deploy-e2e-keda-epp-guide: ## Deploy the llm-d KEDA+EPP guide contract on disposable Kind
	@echo "Deploying direct KEDA+EPP guide contract..."
	@KUBECONFIG=$(KUBECONFIG) \
		CLUSTER_NAME=$(CLUSTER_NAME) \
		LLMD_SOURCE="$(LLMD_SOURCE)" \
		LLMD_SOURCE_SHA="$(LLMD_SOURCE_SHA)" \
		LLMD_NS=llm-d-optimized-baseline \
		MONITORING_NAMESPACE=$(E2E_MONITORING_NAMESPACE) \
		KEDA_NAMESPACE=$(E2E_KEDA_NAMESPACE) \
		WVA_PROJECT=$(CURDIR) \
		MAKE="$(MAKE)" \
		./deploy/lib/deploy_keda_epp_guide.sh

# Owns the complete disposable lifecycle. The default cluster name is replaced
# with a unique guide-specific name; an explicitly supplied name must not exist.
.PHONY: test-e2e-keda-epp-guide-with-setup
test-e2e-keda-epp-guide-with-setup: ## Create fresh Kind, deploy, test, diagnose, and tear down the guide contract
	@set -Eeuo pipefail; \
	cluster_name="$(CLUSTER_NAME)"; \
	if [ "$$cluster_name" = "kind-wva-gpu-cluster" ]; then \
		cluster_name="wva-keda-epp-guide-$$(date +%s)-$$$$"; \
	fi; \
	if $(KIND) get clusters 2>/dev/null | grep -Fxq "$$cluster_name"; then \
		echo "ERROR: Kind cluster '$$cluster_name' already exists; choose a fresh unique name" >&2; \
		exit 1; \
	fi; \
	cleanup() { \
		$(MAKE) destroy-kind-cluster CLUSTER_NAME="$$cluster_name"; \
	}; \
	trap cleanup EXIT; \
	trap 'exit 130' INT; \
	trap 'exit 143' TERM; \
	$(MAKE) create-kind-cluster \
		CLUSTER_NAME="$$cluster_name" \
		CLUSTER_NODES=1 \
		CLUSTER_GPUS=0 \
		CLUSTER_GPU_TYPE=nvidia; \
	$(MAKE) deploy-e2e-keda-epp-guide \
		CLUSTER_NAME="$$cluster_name" \
		LLMD_SOURCE="$(LLMD_SOURCE)" \
		LLMD_SOURCE_SHA="$(LLMD_SOURCE_SHA)"; \
	$(MAKE) test-e2e-keda-epp-guide CLUSTER_NAME="$$cluster_name"

# Runs the smoke subset of the e2e suite. KEDA is the only scaler backend.
.PHONY: test-e2e-smoke
test-e2e-smoke: ## Run smoke e2e tests
	@echo "Running smoke e2e tests..."
	$(eval FOCUS_ARGS := $(if $(FOCUS),-ginkgo.focus="$(FOCUS)",))
	$(eval SKIP_ARGS := $(if $(SKIP),-ginkgo.skip="$(SKIP)",))
	KUBECONFIG=$(KUBECONFIG) \
	ENVIRONMENT=$(ENVIRONMENT) \
	WVA_NAMESPACE=$(CONTROLLER_NAMESPACE) \
	LLMD_NAMESPACE=$(E2E_EMULATED_LLMD_NAMESPACE) \
	MONITORING_NAMESPACE=$(E2E_MONITORING_NAMESPACE) \
	WVA_E2E_SECONDARY_OVERLAY_PATH=$${WVA_E2E_SECONDARY_OVERLAY_PATH:-$(E2E_WVA_SECONDARY_OVERLAY_PATH)} \
	USE_SIMULATOR=$(USE_SIMULATOR) \
	SCALE_TO_ZERO_ENABLED=$(SCALE_TO_ZERO_ENABLED) \
	DEPLOY_ALERTING_RULES=$(DEPLOY_ALERTING_RULES) \
	SCALER_BACKEND=keda \
	MODEL_ID=$(MODEL_ID) \
	go test ./test/e2e/ -timeout 35m -v -ginkgo.v \
		-ginkgo.label-filter="smoke" $(FOCUS_ARGS) $(SKIP_ARGS); \
	TEST_EXIT_CODE=$$?; \
	echo ""; \
	echo "=========================================="; \
	echo "Test execution completed. Exit code: $$TEST_EXIT_CODE"; \
	echo "=========================================="; \
	exit $$TEST_EXIT_CODE

# Runs only the direct KEDA+EPP guide contract against resources already applied
# from the llm-d repository. The WVA controller is intentionally not required.
# Ginkgo gets 65m and Go gets 70m so suite preflight, diagnostics, and cleanup
# remain inside the outer test-process boundary.
.PHONY: test-e2e-keda-epp-guide
test-e2e-keda-epp-guide: ## Run the llm-d KEDA+EPP guide contract
	@echo "Running direct KEDA+EPP guide contract..."
	KUBECONFIG=$(KUBECONFIG) \
	ENVIRONMENT=kind-emulator \
	DEPLOY_WVA=false \
	WVA_NAMESPACE=workload-variant-autoscaler-system \
	LLMD_NAMESPACE=llm-d-optimized-baseline \
	MONITORING_NAMESPACE=$(E2E_MONITORING_NAMESPACE) \
	KEDA_NAMESPACE=$(E2E_KEDA_NAMESPACE) \
	EPP_SERVICE_NAME=optimized-baseline-epp \
	USE_SIMULATOR=true \
	SCALE_TO_ZERO_ENABLED=false \
	SCALER_BACKEND=keda \
	MODEL_ID=Qwen/Qwen3-32B \
	POD_READY_TIMEOUT=120 \
	SCALE_UP_TIMEOUT=300 \
	E2E_EVENTUALLY_SHORT=30 \
	E2E_EVENTUALLY_MEDIUM=60 \
	E2E_EVENTUALLY_STANDARD=90 \
	E2E_EVENTUALLY_LONG=90 \
	E2E_EVENTUALLY_EXTENDED=180 \
	go test ./test/e2e/ -timeout 70m -v -ginkgo.v -ginkgo.timeout=65m \
		-ginkgo.label-filter="keda-epp-guide"

# Runs the complete e2e test suite (KEDA backend, excluding smoke and flaky tests).
.PHONY: test-e2e-full
test-e2e-full: ## Run full e2e test suite
	@echo "Running full e2e test suite..."
	$(eval FOCUS_ARGS := $(if $(FOCUS),-ginkgo.focus="$(FOCUS)",))
	$(eval SKIP_ARGS := $(if $(SKIP),-ginkgo.skip="$(SKIP)",))
	KUBECONFIG=$(KUBECONFIG) \
	ENVIRONMENT=$(ENVIRONMENT) \
	WVA_NAMESPACE=$(CONTROLLER_NAMESPACE) \
	WVA_E2E_SECONDARY_OVERLAY_PATH=$${WVA_E2E_SECONDARY_OVERLAY_PATH:-$(E2E_WVA_SECONDARY_OVERLAY_PATH)} \
	USE_SIMULATOR=$(USE_SIMULATOR) \
	SCALE_TO_ZERO_ENABLED=$(SCALE_TO_ZERO_ENABLED) \
	DEPLOY_ALERTING_RULES=$(DEPLOY_ALERTING_RULES) \
	SCALER_BACKEND=keda \
	KEDA_NAMESPACE=$(E2E_KEDA_NAMESPACE) \
	MODEL_ID=$(MODEL_ID) \
	go test ./test/e2e/ -timeout 35m -v -ginkgo.v \
		-ginkgo.label-filter="full && !smoke && !flaky" $(FOCUS_ARGS) $(SKIP_ARGS); \
	TEST_EXIT_CODE=$$?; \
	echo ""; \
	echo "=========================================="; \
	echo "Test execution completed. Exit code: $$TEST_EXIT_CODE"; \
	echo "=========================================="; \
	exit $$TEST_EXIT_CODE

# Convenience targets for local e2e testing

# Convenience target that deploys KEDA infra + runs smoke tests.
# Set DELETE_CLUSTER=true to delete Kind cluster after tests (default: keep cluster for debugging).
.PHONY: test-e2e-smoke-with-setup
test-e2e-smoke-with-setup:
	$(MAKE) deploy-e2e-infra DEPLOY_ALERTING_RULES=true SCALER_BACKEND=keda
	$(MAKE) test-e2e-smoke DEPLOY_ALERTING_RULES=true

# Runs only the multi-controller (dual namespace-scoped) e2e tests.
.PHONY: test-e2e-multi-controller
test-e2e-multi-controller: ## Run multi-controller e2e tests
	@echo "Running multi-controller e2e tests..."
	$(eval FOCUS_ARGS := $(if $(FOCUS),-ginkgo.focus="$(FOCUS)",))
	$(eval SKIP_ARGS := $(if $(SKIP),-ginkgo.skip="$(SKIP)",))
	KUBECONFIG=$(KUBECONFIG) \
	ENVIRONMENT=$(ENVIRONMENT) \
	WVA_NAMESPACE=$(CONTROLLER_NAMESPACE) \
	LLMD_NAMESPACE=$(E2E_EMULATED_LLMD_NAMESPACE) \
	MONITORING_NAMESPACE=$(E2E_MONITORING_NAMESPACE) \
	WVA_E2E_SECONDARY_OVERLAY_PATH=$${WVA_E2E_SECONDARY_OVERLAY_PATH:-$(E2E_WVA_SECONDARY_OVERLAY_PATH)} \
	USE_SIMULATOR=$(USE_SIMULATOR) \
	SCALE_TO_ZERO_ENABLED=$(SCALE_TO_ZERO_ENABLED) \
	DEPLOY_ALERTING_RULES=$(DEPLOY_ALERTING_RULES) \
	SCALER_BACKEND=$(SCALER_BACKEND) \
	MODEL_ID=$(MODEL_ID) \
	go test ./test/e2e/ -timeout 35m -v -ginkgo.v \
		-ginkgo.label-filter="multi-controller" $(FOCUS_ARGS) $(SKIP_ARGS); \
	TEST_EXIT_CODE=$$?; \
	echo ""; \
	echo "=========================================="; \
	echo "Test execution completed. Exit code: $$TEST_EXIT_CODE"; \
	echo "=========================================="; \
	exit $$TEST_EXIT_CODE

# Convenience target that deploys infra + runs multi-controller tests.
.PHONY: test-e2e-multi-controller-with-setup
test-e2e-multi-controller-with-setup: deploy-e2e-infra test-e2e-multi-controller

# Convenience target that deploys KEDA infra + runs full test suite.
# Set DELETE_CLUSTER=true to delete Kind cluster after tests (default: keep cluster for debugging).
# LWS is installed because the full suite includes LeaderWorkerSet scale-from-zero tests.
.PHONY: test-e2e-full-with-setup
test-e2e-full-with-setup:
	DEPLOY_LWS=true SCALER_BACKEND=keda $(MAKE) deploy-e2e-infra
	$(MAKE) test-e2e-full


##@ llm-d-benchmark CLI (standup / run / teardown)

# llmdbenchmark binary from the benchmark repo venv
BENCHMARK_VENV       = $(BENCHMARK_REPO_DIR)/.venv
LLMDBENCHMARK        = $(shell command -v llmdbenchmark 2>/dev/null || echo $(BENCHMARK_VENV)/bin/llmdbenchmark)

# Common llmdbenchmark flags (spec + workspace + base dir for config resolution)
BENCHMARK_CLI_FLAGS = --spec $(BENCHMARK_SPEC) --workspace $(BENCHMARK_WORKSPACE) --base-dir $(BENCHMARK_REPO_DIR)

.PHONY: benchmark-install
benchmark-install: ## Clone llm-d-benchmark at BENCHMARK_REPO_REF (default v0.7.0) and install the llmdbenchmark CLI
	@if [ ! -d "$(BENCHMARK_REPO_DIR)" ]; then \
		echo "Cloning llm-d-benchmark @ $(BENCHMARK_REPO_REF)..."; \
		git clone --branch $(BENCHMARK_REPO_REF) $(BENCHMARK_REPO_URL) $(BENCHMARK_REPO_DIR); \
	else \
		echo "llm-d-benchmark already cloned at $(BENCHMARK_REPO_DIR); checking out $(BENCHMARK_REPO_REF)..."; \
		cd $(BENCHMARK_REPO_DIR) && git fetch --tags && git checkout $(BENCHMARK_REPO_REF); \
	fi
	@cd $(BENCHMARK_REPO_DIR) && ./install.sh $(if $(filter true,$(BENCHMARK_UV)),--uv,--no-uv)
	@echo "Upgrading helm-diff to v3.15.10 for Helm 4 compatibility..."
	@helm plugin uninstall diff 2>/dev/null || true
	@helm plugin install https://github.com/databus23/helm-diff --version v3.15.10 --verify=false 2>&1

.PHONY: benchmark-standup
benchmark-standup: ## Stand up the benchmark environment (set BENCHMARK_NAMESPACE=<namespace>, MODEL_ID=<model>; BENCHMARK_DIRECT_KEDA=true for controller-free EPP+KEDA autoscaling instead of WVA)
	@if [ -z "$(BENCHMARK_NAMESPACE)" ]; then \
		echo "ERROR: BENCHMARK_NAMESPACE is required. Usage: make benchmark-standup BENCHMARK_NAMESPACE=<namespace>"; \
		exit 1; \
	fi
	@if [ "$(BENCHMARK_DIRECT_KEDA)" = "true" ]; then \
		echo "Direct-KEDA mode: this feature isn't in a released llm-d-benchmark tag yet — upgrading the llm-d-benchmark checkout to '$(BENCHMARK_REPO_REF)' (unreleased)..."; \
		if ! kubectl get crd scaledobjects.keda.sh >/dev/null 2>&1; then \
			echo "ERROR: KEDA is not installed on this cluster (scaledobjects.keda.sh CRD not found)."; \
			echo "Install KEDA first (e.g. 'make deploy-e2e-infra SCALER_BACKEND=keda ENVIRONMENT=$(ENVIRONMENT)', or your platform's KEDA operator) and re-run."; \
			exit 1; \
		fi; \
		echo "KEDA ScaledObject CRD found — proceeding with direct-KEDA standup (no WVA controller)."; \
	fi
	@if [ -d "$(BENCHMARK_REPO_DIR)" ]; then \
		cd $(BENCHMARK_REPO_DIR) && git checkout -- config/scenarios config/specification config/templates 2>/dev/null || true; \
	fi
	@$(MAKE) benchmark-install BENCHMARK_REPO_REF=$(BENCHMARK_REPO_REF)
	@cd $(BENCHMARK_REPO_DIR) && git reset --hard origin/$(BENCHMARK_REPO_REF) 2>/dev/null || true
	@if [ -f "$(CURDIR)/hack/benchmark/scenarios/$(BENCHMARK_SPEC).yaml" ]; then \
		echo "Copying local scenario: hack/benchmark/scenarios/$(BENCHMARK_SPEC).yaml -> $(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml"; \
		mkdir -p "$(BENCHMARK_REPO_DIR)/config/scenarios/$$(dirname $(BENCHMARK_SPEC))"; \
		cp "$(CURDIR)/hack/benchmark/scenarios/$(BENCHMARK_SPEC).yaml" \
		   "$(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml"; \
	fi
	@if [ -f "$(CURDIR)/hack/benchmark/scenarios/$(BENCHMARK_SPEC).yaml.j2" ]; then \
		echo "Copying local specification: hack/benchmark/scenarios/$(BENCHMARK_SPEC).yaml.j2 -> $(BENCHMARK_REPO_DIR)/config/specification/$(BENCHMARK_SPEC).yaml.j2"; \
		mkdir -p "$(BENCHMARK_REPO_DIR)/config/specification/$$(dirname $(BENCHMARK_SPEC))"; \
		cp "$(CURDIR)/hack/benchmark/scenarios/$(BENCHMARK_SPEC).yaml.j2" \
		   "$(BENCHMARK_REPO_DIR)/config/specification/$(BENCHMARK_SPEC).yaml.j2"; \
	fi
	@if [ "$(BENCHMARK_SKIP_PROMETHEUS_ADAPTER)" = "true" ]; then \
		echo "Stubbing prometheus-adapter-resource-reader ClusterRole so standup's existing-PA probe passes..."; \
		kubectl create clusterrole prometheus-adapter-resource-reader \
			--verb=get,list,watch --resource=pods,nodes 2>/dev/null || true; \
		kubectl annotate --overwrite clusterrole prometheus-adapter-resource-reader \
			meta.helm.sh/release-name=prometheus-adapter \
			meta.helm.sh/release-namespace=$(WVA_MONITORING_NAMESPACE); \
		kubectl label --overwrite clusterrole prometheus-adapter-resource-reader \
			app.kubernetes.io/managed-by=Helm; \
	fi
	@echo "Injecting PYTORCH_ALLOC_CONF, decode replicas, and KEDA config into scenario YAML ($(BENCHMARK_SPEC).yaml)..."
	@sed -i.bak 's/extraEnvVars: \[\]/extraEnvVars:\n        - name: PYTORCH_ALLOC_CONF\n          value: "expandable_segments:True"/' \
		$(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml
	@sed -i.bak 's/replicas: 2$$/replicas: $(BENCHMARK_DECODE_REPLICAS)/' \
		$(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml
	@awk ' \
		/scaledObject:/ { in_keda=1 } \
		in_keda && /^    [a-z]/ && !/scaledObject:/ { in_keda=0 } \
		in_keda && /minReplicas: / { gsub(/minReplicas: [0-9]+/, "minReplicas: $(BENCHMARK_KEDA_MIN_REPLICAS)"); } \
		in_keda && /maxReplicas: / { gsub(/maxReplicas: [0-9]+/, "maxReplicas: $(BENCHMARK_KEDA_MAX_REPLICAS)"); } \
		in_keda && /scaleUp:/ { scale_section="up"; } \
		in_keda && /scaleDown:/ { scale_section="down"; } \
		in_keda && scale_section=="up" && /periodSeconds: 180/ { gsub(/periodSeconds: 180/, "periodSeconds: $(BENCHMARK_KEDA_SCALE_UP_PERIOD)"); scale_section=""; } \
		in_keda && scale_section=="down" && /periodSeconds: 300/ { gsub(/periodSeconds: 300/, "periodSeconds: $(BENCHMARK_KEDA_SCALE_DOWN_PERIOD)"); scale_section=""; } \
		{ print } \
	' $(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml > $(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml.tmp && \
	mv $(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml.tmp $(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml
	$(LLMDBENCHMARK) $(BENCHMARK_CLI_FLAGS) standup \
		-p $(BENCHMARK_NAMESPACE) \
		$(if $(BENCHMARK_MODEL_ID),-m $(BENCHMARK_MODEL_ID),) \
		$(if $(filter true,$(BENCHMARK_MONITORING)),--monitoring,); \
	rc=$$?; \
	mv $(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml.bak \
	   $(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml; \
	if [ $$rc -eq 0 ] && [ "$(BENCHMARK_MONITORING)" = "true" ]; then \
		echo "Enabling user-workload monitoring for namespace $(BENCHMARK_NAMESPACE)..."; \
		oc label namespace $(BENCHMARK_NAMESPACE) openshift.io/user-workload-monitoring=enabled --overwrite 2>/dev/null && \
		echo "✅ Monitoring label applied. Prometheus will begin scraping ServiceMonitors in this namespace."; \
	fi; \
	exit $$rc

.PHONY: benchmark-run
benchmark-run: ## Run a single benchmark workload (set BENCHMARK_NAMESPACE=<namespace>, MODEL_ID=<model>, BENCHMARK_HARNESS=guidellm|inference-perf)
	@if [ -z "$(BENCHMARK_NAMESPACE)" ]; then \
		echo "ERROR: BENCHMARK_NAMESPACE is required. Usage: make benchmark-run BENCHMARK_NAMESPACE=<namespace>"; \
		exit 1; \
	fi
	@mkdir -p "$(BENCHMARK_SCENARIOS_DIR)"
	@if [ "$(BENCHMARK_DIRECT_KEDA)" = "true" ] && [ -f "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD)" ]; then \
		echo "Injecting external model endpoint for direct-KEDA mode..."; \
		sed -i.bak 's|base_url: .*|base_url: http://infra-llmdbench-inference-gateway.$(BENCHMARK_NAMESPACE).svc.cluster.local:80|' \
			"$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD)"; \
		rm -f "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD).bak"; \
	fi
	@# Fetch workload from inference-perf catalog if not found locally and harness is inference-perf
	@if [ "$(BENCHMARK_HARNESS)" = "inference-perf" ] && [ ! -f "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD)" ] && [ ! -f "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD).in" ]; then \
		echo "Fetching $(BENCHMARK_WORKLOAD) from inference-perf workload-catalog..."; \
		if curl -sfL "https://raw.githubusercontent.com/kubernetes-sigs/inference-perf/main/workload-catalog/$(BENCHMARK_WORKLOAD)/inference-perf.yaml" \
			-o "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD)"; then \
			echo "Successfully fetched $(BENCHMARK_WORKLOAD)"; \
		else \
			echo "ERROR: Could not fetch $(BENCHMARK_WORKLOAD) from inference-perf workload-catalog"; \
			echo "Available workloads: interactive-chat, code-generation, deep-research, reasoning, batch-summarization-rag, batch-synthetic-data-generation"; \
			exit 1; \
		fi; \
	fi
	@if [ -f "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD).in" ]; then \
		cp "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD).in" \
		   "$(BENCHMARK_REPO_DIR)/workload/profiles/$(BENCHMARK_HARNESS)/$(BENCHMARK_WORKLOAD).in"; \
		cp "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD).in" \
		   "$(BENCHMARK_REPO_DIR)/workload/profiles/$(BENCHMARK_HARNESS)/$(BENCHMARK_WORKLOAD)"; \
	elif [ -f "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD)" ]; then \
		echo "Copying local workload from $(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD) to harness..."; \
		cp "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD)" \
		   "$(BENCHMARK_REPO_DIR)/workload/profiles/$(BENCHMARK_HARNESS)/$(BENCHMARK_WORKLOAD).yaml"; \
		if [ -n "$(BENCHMARK_MODEL_ID)" ]; then \
			echo "Injecting MODEL_ID=$(BENCHMARK_MODEL_ID) into workload profile..."; \
			sed -i.bak 's|model_name: .*|model_name: $(BENCHMARK_MODEL_ID)|' \
				"$(BENCHMARK_REPO_DIR)/workload/profiles/$(BENCHMARK_HARNESS)/$(BENCHMARK_WORKLOAD).yaml"; \
			rm -f "$(BENCHMARK_REPO_DIR)/workload/profiles/$(BENCHMARK_HARNESS)/$(BENCHMARK_WORKLOAD).yaml.bak"; \
		fi; \
	fi
	$(LLMDBENCHMARK) $(BENCHMARK_CLI_FLAGS) run \
		-p $(BENCHMARK_NAMESPACE) \
		-l $(BENCHMARK_HARNESS) \
		-w $(BENCHMARK_WORKLOAD).yaml \
		$(if $(BENCHMARK_MODEL_ID),-m $(BENCHMARK_MODEL_ID),) \
		$(if $(filter true,$(BENCHMARK_MONITORING)),--monitoring,) \
		--wait-timeout $(BENCHMARK_WAIT_TIMEOUT)
	@echo ""
	@echo "========================================="
	@echo "  Generating benchmark report..."
	@echo "========================================="
	@$(MAKE) benchmark-report
	@$(MAKE) benchmark-plot-two-variant || true

.PHONY: benchmark-report
benchmark-report: ## Generate a markdown table from the latest benchmark results
	@LATEST_DIR=$$(ls -td $(BENCHMARK_WORKSPACE)/$${USER}-*/results/$(BENCHMARK_HARNESS)-*_* 2>/dev/null | head -1); \
	if [ -z "$$LATEST_DIR" ]; then \
		echo "ERROR: No benchmark results found in $(BENCHMARK_WORKSPACE)"; \
		exit 1; \
	fi; \
	echo "Results directory: $$LATEST_DIR"; \
	echo ""; \
	if [ -n "$(BENCHMARK_TWO_VARIANT_SECONDARY_SUFFIX)" ]; then \
		python3 $(CURDIR)/hack/benchmark/postprocess.py \
			--secondary-suffix $(BENCHMARK_TWO_VARIANT_SECONDARY_SUFFIX) \
			--scenario-yaml $(CURDIR)/hack/benchmark/scenarios/$(BENCHMARK_SPEC).yaml \
			--variant-config $(VARIANT_CONFIG) \
			$$LATEST_DIR; \
	else \
		python3 $(CURDIR)/hack/benchmark/postprocess.py $$LATEST_DIR; \
	fi

BENCHMARK_TWO_VARIANT_SECONDARY_SUFFIX ?= v2

.PHONY: benchmark-plot-two-variant
benchmark-plot-two-variant: ## Plot two-variant replica/latency/throughput graph from the latest results (no-op for single-variant runs)
	@LATEST_DIR=$$(ls -td $(BENCHMARK_WORKSPACE)/$${USER}-*/results/$(BENCHMARK_HARNESS)-*_* 2>/dev/null | head -1); \
	if [ -z "$$LATEST_DIR" ]; then \
		echo "No benchmark results found, skipping two-variant plot"; \
		exit 0; \
	fi; \
	python3 $(CURDIR)/hack/benchmark/plot_two_variant_pipeline.py \
		$$LATEST_DIR && \
	echo "Two-variant plot: $$LATEST_DIR/metrics/graphs/two_variant_v2_full_pipeline.png"

VARIANT_CONFIG ?= $(CURDIR)/hack/benchmark/scenarios/guides/variants/v2-tp1-cheaper.yaml
WVA_V2_SATURATION_CONFIGMAP ?= $(CURDIR)/hack/benchmark/scenarios/wva_threshold/wva_saturation_v2_config.yaml
WVA_CONTROLLER_DEPLOY ?= deploy/workload-variant-autoscaler-controller-manager
WVA_ROLLOUT_TIMEOUT ?= 120s
WVA_MONITORING_NAMESPACE ?= workload-variant-autoscaler-monitoring

.PHONY: benchmark-add-variant
benchmark-add-variant: ## Add a secondary WVA variant to the running benchmark (set BENCHMARK_NAMESPACE=<namespace>, optional VARIANT_CONFIG=<path>)
	@if [ -z "$(BENCHMARK_NAMESPACE)" ]; then \
		echo "ERROR: BENCHMARK_NAMESPACE is required. Usage: make benchmark-add-variant BENCHMARK_NAMESPACE=<namespace>"; \
		exit 1; \
	fi
	python3 $(CURDIR)/hack/benchmark/add_variant.py \
		-n $(BENCHMARK_NAMESPACE) \
		--config $(VARIANT_CONFIG)

.PHONY: benchmark-enable-v2-saturation
benchmark-enable-v2-saturation: ## Enable WVA saturation V2 analyzer (apply configmap + restart controller)
	@if [ -z "$(BENCHMARK_NAMESPACE)" ]; then \
		echo "ERROR: BENCHMARK_NAMESPACE is required. Usage: make benchmark-enable-v2-saturation BENCHMARK_NAMESPACE=<namespace>"; \
		exit 1; \
	fi
	kubectl apply -n $(BENCHMARK_NAMESPACE) -f $(WVA_V2_SATURATION_CONFIGMAP)
	$(MAKE) benchmark-restart-controller BENCHMARK_NAMESPACE=$(BENCHMARK_NAMESPACE)

.PHONY: benchmark-restart-controller
benchmark-restart-controller: ## Restart WVA controller to flush in-memory state (e.g., k2 history between runs)
	@if [ -z "$(BENCHMARK_NAMESPACE)" ]; then \
		echo "ERROR: BENCHMARK_NAMESPACE is required. Usage: make benchmark-restart-controller BENCHMARK_NAMESPACE=<namespace>"; \
		exit 1; \
	fi
	kubectl rollout restart -n $(BENCHMARK_NAMESPACE) $(WVA_CONTROLLER_DEPLOY)
	kubectl rollout status -n $(BENCHMARK_NAMESPACE) $(WVA_CONTROLLER_DEPLOY) --timeout=$(WVA_ROLLOUT_TIMEOUT)

BURSTY_WORKLOAD    ?= bursty.yaml
BENCHMARK_WAIT_TIMEOUT ?= 7200
BENCHMARK_HARNESS_MEMORY ?= 40Gi

.PHONY: benchmark-run-bursty
benchmark-run-bursty: ## Run bursty traffic benchmark using inference-perf multi-stage rates (set BENCHMARK_NAMESPACE=<namespace>, MODEL_ID=<model>)
	@if [ -z "$(BENCHMARK_NAMESPACE)" ]; then \
		echo "ERROR: BENCHMARK_NAMESPACE is required. Usage: make benchmark-run-bursty BENCHMARK_NAMESPACE=<namespace>"; \
		exit 1; \
	fi
	@if [ -f "$(BENCHMARK_SCENARIOS_DIR)/$(BURSTY_WORKLOAD).in" ]; then \
		cp "$(BENCHMARK_SCENARIOS_DIR)/$(BURSTY_WORKLOAD).in" \
		   "$(BENCHMARK_REPO_DIR)/workload/profiles/inference-perf/$(BURSTY_WORKLOAD).in"; \
	fi
	@echo "Patching harness memory to $(BENCHMARK_HARNESS_MEMORY)..."
	@sed -i.bak 's/memory: 32Gi/memory: $(BENCHMARK_HARNESS_MEMORY)/' \
		$(BENCHMARK_REPO_DIR)/config/templates/values/defaults.yaml
	$(LLMDBENCHMARK) $(BENCHMARK_CLI_FLAGS) run \
		-p $(BENCHMARK_NAMESPACE) \
		-l inference-perf \
		-w $(BURSTY_WORKLOAD) \
		-U $(BENCHMARK_GATEWAY_URL) \
		$(if $(BENCHMARK_MODEL_ID),-m $(BENCHMARK_MODEL_ID),) \
		$(if $(filter true,$(BENCHMARK_MONITORING)),--monitoring,); \
	rc=$$?; \
	mv $(BENCHMARK_REPO_DIR)/config/templates/values/defaults.yaml.bak \
	   $(BENCHMARK_REPO_DIR)/config/templates/values/defaults.yaml; \
	exit $$rc

.PHONY: benchmark-run-all
benchmark-run-all: ## Run all scenarios: teardown → standup → run per scenario (set BENCHMARK_NAMESPACE=<namespace>, MODEL_ID=<model>)
	@if [ -z "$(BENCHMARK_NAMESPACE)" ]; then \
		echo "ERROR: BENCHMARK_NAMESPACE is required. Usage: make benchmark-run-all BENCHMARK_NAMESPACE=<namespace>"; \
		exit 1; \
	fi
	@for scenario in $(BENCHMARK_SCENARIOS_DIR)/*.yaml.in; do \
		scenario_name=$$(basename "$$scenario" .in); \
		echo ""; \
		echo "=========================================="; \
		echo "[1/3] Tearing down before: $$scenario_name"; \
		echo "=========================================="; \
		$(LLMDBENCHMARK) $(BENCHMARK_CLI_FLAGS) teardown \
			-p $(BENCHMARK_NAMESPACE) || true; \
		echo ""; \
		echo "=========================================="; \
		echo "[2/3] Standing up for: $$scenario_name"; \
		echo "=========================================="; \
		$(LLMDBENCHMARK) $(BENCHMARK_CLI_FLAGS) standup \
			-p $(BENCHMARK_NAMESPACE) \
			$(if $(BENCHMARK_MODEL_ID),-m $(BENCHMARK_MODEL_ID),) \
			$(if $(filter true,$(BENCHMARK_MONITORING)),--monitoring,) || { \
			echo "ERROR: Standup failed for $$scenario_name"; \
			exit 1; \
		}; \
		echo ""; \
		echo "=========================================="; \
		echo "[3/3] Running scenario: $$scenario_name"; \
		echo "=========================================="; \
		$(LLMDBENCHMARK) $(BENCHMARK_CLI_FLAGS) run \
			-p $(BENCHMARK_NAMESPACE) \
			-l $(BENCHMARK_HARNESS) \
			-w "$$scenario_name" \
			$(if $(BENCHMARK_MODEL_ID),-m $(BENCHMARK_MODEL_ID),) || { \
			echo "ERROR: Scenario $$scenario_name failed"; \
			exit 1; \
		}; \
	done
	@echo ""
	@echo "=========================================="
	@echo "All scenarios completed successfully"
	@echo "=========================================="

.PHONY: benchmark-teardown
benchmark-teardown: ## Tear down the benchmark environment (set BENCHMARK_NAMESPACE=<namespace>)
	@if [ -z "$(BENCHMARK_NAMESPACE)" ]; then \
		echo "ERROR: BENCHMARK_NAMESPACE is required. Usage: make benchmark-teardown BENCHMARK_NAMESPACE=<namespace>"; \
		exit 1; \
	fi
	$(LLMDBENCHMARK) $(BENCHMARK_CLI_FLAGS) teardown \
		-p $(BENCHMARK_NAMESPACE)

.PHONY: benchmark-full
benchmark-full: benchmark-standup benchmark-run-all benchmark-teardown ## Full lifecycle: standup -> run all scenarios -> teardown

# Stub for llm-d nightly reusable workflows (test_target=nightly-test-llm-d)
# No-op; temporarily satisfies nightly CI make invocation
# TODO: add nightly guide tests here
.PHONY: nightly-test-llm-d
nightly-test-llm-d: ## Nightly CI: noop; use as test_target instead of empty string
	@:

# Canonical target for llm-d-infra nightly reusables: ENVIRONMENT=openshift|kubernetes
# Deploys WVA + monitoring + scaler backend only. llm-d model serving is deployed separately
# by the nightly workflow's custom_deploy_script (kustomize + GAIE helm from llm-d/llm-d guide).
.PHONY: nightly-deploy-wva-guide
nightly-deploy-wva-guide: ## Nightly: WVA controller + monitoring stack from job env (WVA_NS <- WVA_NAMESPACE or CONTROLLER_NAMESPACE)
	# Note: CKS callers with resource constraints should disable nodeExporter by patching kube-prometheus-stack post-install.
	@WVA_NS="$${WVA_NS:-$${WVA_NAMESPACE:-$${CONTROLLER_NAMESPACE:-}}}" \
	ENVIRONMENT="$${ENVIRONMENT:-openshift}" \
	./deploy/install.sh

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-deploy-scripts
lint-deploy-scripts: ## Run bash -n for deploy/install.sh, deploy/lib/*.sh, and deploy plugins
	@echo "Syntax-checking deploy shell scripts..."
	@bash -n deploy/install.sh
	@bash -n deploy/install-epp.sh
	@for script in deploy/lib/*.sh; do bash -n "$$script"; done
	@for script in deploy/*/install.sh; do if [ -f "$$script" ]; then bash -n "$$script"; fi; done
	@for script in deploy/kind-emulator/*.sh; do if [ -f "$$script" ]; then bash -n "$$script"; fi; done
	@echo "deploy script syntax OK"

.PHONY: smoke-deploy-scripts
smoke-deploy-scripts: lint-deploy-scripts ## Non-interactive deploy script smoke check (source order + arg parsing)
	@echo "Running deploy script smoke check..."
	@SKIP_CHECKS=true ENVIRONMENT=kubernetes ./deploy/install.sh --help >/dev/null
	@echo "deploy script smoke OK"

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	$(GOLANGCI_LINT) config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64
BUILDER_NAME ?= workload-variant-autoscaler-builder

.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name workload-variant-autoscaler-builder
	$(CONTAINER_TOOL) buildx use workload-variant-autoscaler-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm workload-variant-autoscaler-builder
	rm Dockerfile.cross

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif


##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
HELM ?= $(LOCALBIN)/helm

## Tool Versions
KUSTOMIZE_VERSION ?= v5.6.0
CONTROLLER_TOOLS_VERSION ?= v0.17.2
#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')
#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')
GOLANGCI_LINT_VERSION ?= v2.10.0
HELM_VERSION ?= v3.17.1

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))


.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	@[ -f "$(LOCALBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)" ] || { \
	set -e; \
	echo "Downloading golangci-lint $(GOLANGCI_LINT_VERSION)"; \
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(LOCALBIN) $(GOLANGCI_LINT_VERSION); \
	if [ -f "$(LOCALBIN)/golangci-lint" ]; then \
		mv $(LOCALBIN)/golangci-lint $(LOCALBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION); \
	fi; \
	} ;\
	ln -sf golangci-lint-$(GOLANGCI_LINT_VERSION) $(GOLANGCI_LINT)

.PHONY: helm
helm: $(HELM) ## Download helm locally if necessary.
$(HELM): $(LOCALBIN)
	@[ -f "$(LOCALBIN)/helm-$(HELM_VERSION)" ] || { \
	set -e; \
	echo "Downloading helm $(HELM_VERSION)"; \
	curl -sSfL https://get.helm.sh/helm-$(HELM_VERSION)-$(shell go env GOOS)-$(shell go env GOARCH).tar.gz | tar xz --no-same-owner -C $(LOCALBIN) --strip-components=1 $(shell go env GOOS)-$(shell go env GOARCH)/helm; \
	mv $(LOCALBIN)/helm $(LOCALBIN)/helm-$(HELM_VERSION); \
	} ;\
	ln -sf helm-$(HELM_VERSION) $(HELM)

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef


include config/samples/hpa/co-ordinator/poc.mk
