VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= kubeamber:dev
CLUSTER ?= kubeamber
LDFLAGS := -X github.com/kubeamber/kubeamber/internal/version.Version=$(VERSION)

.PHONY: build test lint fmt docker-build docker-build-local kind-up kind-load kind-deploy kind-demo e2e kind-down clean

build: ## Build bin/kubeamber
	go build -ldflags "$(LDFLAGS)" -o bin/kubeamber ./cmd/kubeamber

test: ## Unit tests with the race detector
	go test -race ./...

lint: ## go vet, gofmt and golangci-lint (if installed)
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run: make fmt" && exit 1)
	@command -v golangci-lint >/dev/null && golangci-lint run || echo "golangci-lint not installed; skipped"

fmt:
	gofmt -w .

docker-build: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

docker-build-local: ## Build the image from a host-compiled binary (small and fast; used by the kind targets)
	CGO_ENABLED=0 GOOS=linux GOARCH=$$(go env GOARCH) go build -trimpath -ldflags "-s -w $(LDFLAGS)" -o bin/linux/kubeamber ./cmd/kubeamber
	docker build -f hack/Dockerfile.local -t $(IMAGE) .

kind-up: ## Create a kind cluster
	kind get clusters | grep -qx $(CLUSTER) || kind create cluster --name $(CLUSTER) --wait 90s

kind-load: docker-build-local
	kind load docker-image $(IMAGE) --name $(CLUSTER)

kind-deploy: build kind-up kind-load ## Install KubeAmber into the kind cluster
	kubectl --context kind-$(CLUSTER) apply -k hack/dev
	kubectl --context kind-$(CLUSTER) -n kubeamber-system rollout restart deploy/kubeamber
	kubectl --context kind-$(CLUSTER) -n kubeamber-system rollout status deploy/kubeamber --timeout=120s

kind-demo: kind-deploy ## Install on kind and walk through agent scenarios
	CONTEXT=kind-$(CLUSTER) KUBEAMBER=bin/kubeamber hack/e2e.sh

e2e: kind-demo ## Same as kind-demo; fails if any scenario misbehaves

kind-down:
	kind delete cluster --name $(CLUSTER)

clean:
	rm -rf bin
