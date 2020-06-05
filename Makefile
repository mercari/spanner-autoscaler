GOOS   := $(shell go env GOOS)
GOARCH := $(shell go env GOARCH)

# Image URL to use all building/pushing image targets
IMG ?= mercari/spanner-autoscaler:latest

KUBEBUILDER_VERSION ?= 2.1.0

BIN_DIR := $(shell pwd)/bin

KUBEBUILDER_ASSETS_DIR := ${BIN_DIR}/kubebuilder
KUBEBUILDER_ASSETS := ${KUBEBUILDER_ASSETS_DIR}/bin

CONTROLLER_GEN := bin/controller-gen
KIND           := bin/kind
KUBECTL        := bin/kubectl
SKAFFOLD       := bin/skaffold
KPT            := bin/kpt

KIND_NODE_VERSION := 1.18.2

GO_TEST ?= go test

all: manager

.PHONY: test
test: kubebuilder
	@KUBEBUILDER_ASSETS=${KUBEBUILDER_ASSETS} $(GO_TEST) -v -race ./...

# Run tests
coverage: generate fmt vet manifests
	@KUBEBUILDER_ASSETS=${KUBEBUILDER_ASSETS} $(GO_TEST) -v -race ./... -covermode=atomic -coverprofile=cover.out

# Build manager binary
manager: generate fmt vet
	go build -o bin/manager cmd/spanner-autoscaler/main.go

# Install CRDs into a cluster
install: manifests
	kustomize build config/crd | kubectl apply -f -

# Deploy controller in the configured Kubernetes cluster in ~/.kube/config
deploy: manifests
	cd config/manager && kustomize edit set image controller=${IMG}
	kustomize build config/default | kubectl apply -f -

.PHONY: fmt
fmt: ## Run go fmt against code
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code
	go vet ./...

.PHONY: lint
lint: lint/golangci-lint  ## Run all linters.

.PHONY: tools/golangci-lint
tools/golangci-lint:  # go get 'golangci-lint' binary
ifeq (, $(shell which golangci-lint))
	GO111MODULE=off go get -u github.com/golangci/golangci-lint/cmd/golangci-lint
endif

.PHONY: lint/golangci-lint
lint/golangci-lint: tools/golangci-lint .golangci.yml  ## Run golangci-lint.
	golangci-lint run

# Build the docker image
docker-build:
	docker image build . -t ${IMG}

# Push the docker image
docker-push:
	docker image push ${IMG}

kubebuilder: ${KUBEBUILDER_ASSETS}/kubebuilder
${KUBEBUILDER_ASSETS}/kubebuilder:
	@{ \
		set -e ;\
		TMP_DIR=./tmp/kubebuilder ;\
		mkdir -p $$TMP_DIR ;\
		curl -sSL https://github.com/kubernetes-sigs/kubebuilder/releases/download/v${KUBEBUILDER_VERSION}/kubebuilder_${KUBEBUILDER_VERSION}_${GOOS}_${GOARCH}.tar.gz | tar -xz -C $$TMP_DIR ;\
		mv $$TMP_DIR/kubebuilder_${KUBEBUILDER_VERSION}_${GOOS}_${GOARCH} ${KUBEBUILDER_ASSETS_DIR} ;\
		rm -rf $$TMP_DIR ;\
	}

.PHONY: run
run: $(SKAFFOLD)
	# NOTE: since skaffold uses kind executable from PATH directly, so this overrides PATH to use project local executable.
	@PATH=$${PWD}/bin:$${PATH} $(SKAFFOLD) dev --filename=./skaffold/skaffold.yaml

.PHONY: manifests
manifests: $(CONTROLLER_GEN)
	@$(CONTROLLER_GEN) crd:trivialVersions=true rbac:roleName=manager-role webhook paths="./pkg/api/..." output:crd:artifacts:config=config/crd/bases

.PHONY: deepcopy
deepcopy: $(CONTROLLER_GEN)
	 @$(CONTROLLER_GEN) object:headerFile=./hack/boilerplate.go.txt paths="./..."

controlller-gen: $(CONTROLLER_GEN)
$(CONTROLLER_GEN): go.sum
	@go build -o $(CONTROLLER_GEN) sigs.k8s.io/controller-tools/cmd/controller-gen

kind: $(KIND)
$(KIND): go.sum
	@go build -o ./bin/kind sigs.k8s.io/kind

kubectl: $(KUBECTL)
$(KUBECTL): .kubectl-version
	@curl -Lso $(KUBECTL) https://storage.googleapis.com/kubernetes-release/release/$(shell cat .kubectl-version)/bin/$(GOOS)/$(GOARCH)/kubectl
	@chmod +x $(KUBECTL)

skaffold: $(SKAFFOLD)
$(SKAFFOLD): .skaffold-version
	@curl -Lso $(SKAFFOLD) https://storage.googleapis.com/skaffold/releases/$(shell cat .skaffold-version)/skaffold-$(GOOS)-$(GOARCH)
	@chmod +x $(SKAFFOLD)

kpt: $(KPT)
$(KPT): go.sum
	@go build -o $(KPT) github.com/GoogleContainerTools/kpt

.PHONY: kind-cluster
kind-cluster: $(KIND) $(KUBECTL)
	@$(KIND) delete cluster --name spanner-autoscaler
	@$(KIND) create cluster --name spanner-autoscaler --image kindest/node:v${KIND_NODE_VERSION}
	@$(KUBECTL) apply -f ./config/crd/bases/spanner.mercari.com_spannerautoscalers.yaml
	@$(KUBECTL) apply -f ./kind/namespace.yaml
	@$(KUBECTL) apply -f ./kind/rbac

.PHONY: kpt/crd
kpt/crd: manifests kpt
	@kustomize build config/crd > ./kpt/crd/spannerautoscalers.spanner.mercari.com.yaml
