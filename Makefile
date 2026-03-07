# ── Configuration ─────────────────────────────────────────────────────────────
IMAGE_REPO   ?= ghcr.io/jacekmyjkowski/k8s-agent-orchestrator
IMAGE_TAG    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
IMAGE        := $(IMAGE_REPO):$(IMAGE_TAG)

HELM_RELEASE ?= agent-orchestrator
HELM_NS      ?= orchestrator

GOFLAGS      ?= -v

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} \
	     /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-25s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

# ── Go ────────────────────────────────────────────────────────────────────────
.PHONY: build
build: ## Build the orchestrator binary
	go build $(GOFLAGS) -o bin/orchestrator ./cmd/main.go

.PHONY: run
run: tidy ## Run the orchestrator locally (requires KUBECONFIG)
	go run ./cmd/main.go --debug=true

.PHONY: test
test: ## Run unit tests
	go test ./... -count=1 -race -short

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run ./...

.PHONY: fmt
fmt: ## Format Go source
	gofmt -l -w .
	goimports -l -w .

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: tidy
tidy: ## Tidy go.mod (use -e to skip unreachable test-only transitive deps)
	go mod tidy -e

# ── Docker ────────────────────────────────────────────────────────────────────
.PHONY: docker-build
docker-build: ## Build the Docker image
	docker build -t $(IMAGE) .

.PHONY: docker-push
docker-push: ## Push the Docker image
	docker push $(IMAGE)

.PHONY: docker-build-push
docker-build-push: docker-build docker-push ## Build and push the Docker image

# ── Helm ──────────────────────────────────────────────────────────────────────
.PHONY: helm-lint
helm-lint: ## Lint the Helm chart
	helm lint helm/k8s-agent-orchestrator

.PHONY: helm-template
helm-template: ## Render Helm templates to stdout
	helm template $(HELM_RELEASE) helm/k8s-agent-orchestrator \
	  --namespace $(HELM_NS)

.PHONY: helm-install
helm-install: ## Install (or upgrade) the Helm chart
	helm upgrade --install $(HELM_RELEASE) helm/k8s-agent-orchestrator \
	  --namespace $(HELM_NS) \
	  --create-namespace \
	  --set image.repository=$(IMAGE_REPO) \
	  --set image.tag=$(IMAGE_TAG)

.PHONY: helm-uninstall
helm-uninstall: ## Uninstall the Helm release (CRD is retained)
	helm uninstall $(HELM_RELEASE) --namespace $(HELM_NS)

# ── CRD ───────────────────────────────────────────────────────────────────────
.PHONY: install-crd
install-crd: ## Apply the Agent CRD directly
	kubectl apply -f config/crd/bases/orchestrator.dev_agents.yaml

.PHONY: uninstall-crd
uninstall-crd: ## Delete the Agent CRD (destroys all Agent instances!)
	kubectl delete -f config/crd/bases/orchestrator.dev_agents.yaml

# ── Manifests (generate CRD from kubebuilder markers) ─────────────────────────
.PHONY: manifests
manifests: ## Generate CRD manifests from kubebuilder markers (requires controller-gen)
	controller-gen rbac:roleName=agent-role crd output:crd:artifacts:config=config/crd/bases \
	  paths="./..."

.PHONY: generate
generate: ## Generate deepcopy stubs
	controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."

# ── Dev workflow ──────────────────────────────────────────────────────────────
.PHONY: dev
dev: install-crd tidy run ## Install CRD and run orchestrator locally

.PHONY: all
all: fmt vet build ## Format, vet, build
