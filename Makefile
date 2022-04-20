# Image URL to use all building/pushing image targets
IMG ?= mercari/spanner-autoscaler:latest

# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION = 1.21

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Setting SHELL to bash allows bash commands to be executed by recipes.
# This is a requirement for 'setup-envtest.sh' in the test target.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk commands is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

clean: ## Remove any locally built or downloaded files
	rm -rf $(shell pwd)/bin

docs: crd-ref-docs ## Generate CRD reference docs
	$(CRD_REF_DOCS) --config docs/config/config.yaml --renderer markdown --output-path docs/crd-reference.md --source-path api/v1beta1

##@ Development

manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

fmt: ## Run go fmt against code.
	go fmt ./...

vet: ## Run go vet against code.
	go vet ./...

lint: golangci-lint ## Run golangci-lint against code.
	$(GOLANGCI_LINT) run

test: manifests generate fmt vet envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" go test ./... -coverprofile cover.out

KIND_CLUSTER_NAME = spanner-autoscaler
kind-cluster-create: kind ## Create a kind cluster for development
	@if [ -z $(shell $(KIND) get clusters | grep $(KIND_CLUSTER_NAME)) ]; then \
	  $(KIND) create cluster --name $(KIND_CLUSTER_NAME); \
	fi
	## Change context to use kind cluster as default
	kubectl config use-context kind-$(KIND_CLUSTER_NAME)
	## Install cert-manager for generating certifacetes for validation and defaulting webhooks
	kubectl apply -f https://github.com/jetstack/cert-manager/releases/download/v1.6.1/cert-manager.yaml

kind-cluster-delete: kind ## Delete the kind cluster created for development
	@$(KIND) delete cluster --name $(KIND_CLUSTER_NAME)

kind-cluster-reset: kind-cluster-delete kind-cluster-create ## Recreate the kind cluster for development

kind-load-docker-image: kind ## Load a local Docker image to the kind cluster for development
	@$(KIND) load docker-image --name $(KIND_CLUSTER_NAME) ${IMG}

##@ Build

build: generate fmt vet ## Build manager binary.
	go build -o bin/manager main.go

run: manifests generate fmt vet ## Run a controller from your host.
	go run ./main.go -zap-devel

docker-build: test ## Build docker image with the manager.
	docker build -t ${IMG} .

docker-push: ## Push docker image with the manager.
	docker push ${IMG}

##@ Deployment

install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | kubectl delete -f -

deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | kubectl apply -f -

deploy-dev: manifests kustomize ## Deploy controller (for development, see `docs/webhook_development.md` for details) to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/deploy-dev | kubectl apply -f -

undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/default | kubectl delete -f -

##@ Dependencies

deps: controller-gen kustomize kind kpt golangci-lint crd-ref-docs ## Download the following dependencies locally (in './bin') if necessary

CONTROLLER_GEN = $(shell pwd)/bin/controller-gen
controller-gen: ## Download controller-gen locally if necessary.
	$(call go-get-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen@v0.7.0)

KUSTOMIZE = $(shell pwd)/bin/kustomize
kustomize: ## Download kustomize locally if necessary.
	$(call go-get-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v3@v3.8.7)

ENVTEST = $(shell pwd)/bin/setup-envtest
.PHONY: envtest
envtest: ## Download envtest-setup locally if necessary.
	$(call go-get-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest@latest)

KIND = $(shell pwd)/bin/kind
kind: ## Downlaod 'kind' locally if necessary
	$(call go-get-tool,$(KIND),sigs.k8s.io/kind@v0.11.1)

KPT = $(shell pwd)/bin/kpt
kpt: ## Downlaod 'kpt' locally if necessary
	$(call go-get-tool,$(KPT),github.com/GoogleContainerTools/kpt@main)

GOLANGCI_LINT = $(shell pwd)/bin/golangci-lint
golangci-lint: ## Downlaod 'golangci-lint' locally if necessary
	$(call go-get-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/cmd/golangci-lint@v1.44.0)

CRD_REF_DOCS = $(shell pwd)/bin/crd-ref-docs
crd-ref-docs: ## Downlaod 'crd-ref-docs' locally if necessary
	$(call go-get-tool,$(CRD_REF_DOCS),github.com/elastic/crd-ref-docs@v0.0.8)

# go-get-tool will 'go get' any package $2 and install it to $1.
PROJECT_DIR := $(shell dirname $(abspath $(lastword $(MAKEFILE_LIST))))
define go-get-tool
@[ -f $(1) ] || { \
set -e ;\
TMP_DIR=$$(mktemp -d) ;\
cd $$TMP_DIR ;\
go mod init tmp ;\
echo "Downloading $(2)" ;\
GOBIN=$(PROJECT_DIR)/bin go get $(2) ;\
rm -rf $$TMP_DIR ;\
}
endef

