# CAPHR — Cluster API Provider Hetzner Robot
# Standard CAPI provider Makefile

# Image
IMG ?= registry.sylphx.com/library/caphr:latest
CONTROLLER_GEN_VERSION ?= v0.16.0

# Go
GOBIN := $(shell go env GOPATH)/bin
CONTROLLER_GEN := $(GOBIN)/controller-gen

.PHONY: all
all: generate fmt vet build

##@ Development

.PHONY: fmt
fmt: ## Run go fmt
	go fmt ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: test
test: ## Run tests
	go test ./... -v -count=1

.PHONY: test-short
test-short: ## Run tests (short mode, skip slow tests)
	go test ./... -short -count=1

.PHONY: lint
lint: ## Run linters
	@which golangci-lint > /dev/null 2>&1 || (echo "Installing golangci-lint..." && go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)
	golangci-lint run ./...

##@ Build

.PHONY: build
build: ## Build manager binary
	CGO_ENABLED=0 go build -ldflags="-w -s" -o bin/manager ./main.go

.PHONY: run
run: ## Run against the configured Kubernetes cluster
	go run ./main.go

.PHONY: docker-build
docker-build: ## Build docker image
	docker build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push docker image
	docker push $(IMG)

##@ Code Generation

.PHONY: controller-gen
controller-gen: ## Install controller-gen if not present
	@test -f $(CONTROLLER_GEN) || (echo "Installing controller-gen $(CONTROLLER_GEN_VERSION)..." && \
		go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION))

.PHONY: generate
generate: controller-gen ## Generate CRD manifests and RBAC from Go types + kubebuilder markers
	$(CONTROLLER_GEN) crd \
		paths="./api/..." \
		output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=caphr-manager-role \
		paths="./controllers/..." \
		output:rbac:artifacts:config=config/rbac/generated
	@echo "CRDs generated in config/crd/bases/"
	@echo "RBAC generated in config/rbac/generated/"
	@echo "NOTE: config/rbac/rbac.yaml is the hand-maintained RBAC (includes SA + ClusterRoleBinding)"

.PHONY: manifests
manifests: generate ## Alias for generate

##@ Deployment

.PHONY: install
install: ## Install CRDs into the K8s cluster (current context)
	kubectl apply -f config/crd/bases/

.PHONY: uninstall
uninstall: ## Uninstall CRDs from the K8s cluster
	kubectl delete -f config/crd/bases/

.PHONY: deploy
deploy: ## Deploy controller to the K8s cluster
	kubectl apply -f config/crd/bases/
	kubectl apply -f config/rbac/rbac.yaml
	kubectl apply -f config/manager/deployment.yaml

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster
	kubectl delete -f config/manager/deployment.yaml
	kubectl delete -f config/rbac/rbac.yaml
	kubectl delete -f config/crd/bases/

##@ Help

.PHONY: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
