VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= kubeamber:dev
CLUSTER ?= kubeamber
LDFLAGS := -X github.com/kubeamber/kubeamber/internal/version.Version=$(VERSION)

CHART   := charts/kubeamber

# Who signs the public cgr.dev/chainguard images (Sigstore keyless).
CHAINGUARD_IDENTITY := https://github.com/chainguard-images/images/.github/workflows/release.yaml@refs/heads/main
CHAINGUARD_ISSUER   := https://token.actions.githubusercontent.com

.PHONY: build test lint fmt chart-sync chart-lint docker-build docker-build-local verify-base-images kind-up kind-deploy-helm kind-demo-helm kind-load kind-deploy kind-demo e2e kind-down clean

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

chart-sync: ## Copy deploy/crd.yaml into the Helm chart
	cp deploy/crd.yaml $(CHART)/files/crd.yaml

chart-lint: ## Lint the Helm chart and check its CRD matches deploy/
	@diff -q deploy/crd.yaml $(CHART)/files/crd.yaml >/dev/null || (echo "$(CHART)/files/crd.yaml is out of date: run make chart-sync" && exit 1)
	helm lint $(CHART)
	helm lint $(CHART) --set bridge.enabled=true --set webhook.tls.existingSecret=tls --set webhook.tls.certManagerCertificate=ns/cert
	helm lint $(CHART) --set metrics.serviceMonitor.enabled=true --set metrics.serviceMonitor.labels.release=prometheus
	helm lint $(CHART) --set metrics.enabled=false
	helm lint $(CHART) --set notify.enabled=true --set 'notify.config.channels[0].name=platform' --set 'notify.config.channels[0].slack.urlFile=/etc/kubeamber/notify-secrets/slack-url'

docker-build: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

docker-build-local: ## Build the image from a host-compiled binary (small and fast; used by the kind targets)
	CGO_ENABLED=0 GOOS=linux GOARCH=$$(go env GOARCH) go build -trimpath -ldflags "-s -w $(LDFLAGS)" -o bin/linux/kubeamber ./cmd/kubeamber
	docker build -f hack/Dockerfile.local -t $(IMAGE) .

verify-base-images: ## Check both Dockerfiles pin the same Chainguard base, and verify Chainguard's signature on it (needs cosign)
	@refs=$$(grep -h '^FROM' Dockerfile hack/Dockerfile.local | grep -o 'cgr\.dev/[^ ]*' | sort -u); \
	test "$$(echo "$$refs" | grep -c .)" -eq 1 || { echo "Dockerfile and hack/Dockerfile.local must pin the same cgr.dev base image:"; echo "$$refs"; exit 1; }; \
	echo "verifying $$refs"; \
	cosign verify "$$refs" --certificate-identity $(CHAINGUARD_IDENTITY) --certificate-oidc-issuer $(CHAINGUARD_ISSUER) >/dev/null

kind-up: ## Create a kind cluster
	kind get clusters | grep -qx $(CLUSTER) || kind create cluster --name $(CLUSTER) --wait 90s

kind-load: docker-build-local
	kind load docker-image $(IMAGE) --name $(CLUSTER)

kind-deploy: build kind-up kind-load ## Install KubeAmber into the kind cluster
	kubectl --context kind-$(CLUSTER) apply -k hack/dev
	kubectl --context kind-$(CLUSTER) -n kubeamber-system rollout restart deploy/kubeamber
	kubectl --context kind-$(CLUSTER) -n kubeamber-system rollout status deploy/kubeamber --timeout=120s

kind-deploy-helm: build kind-up kind-load ## Install KubeAmber into the kind cluster with the Helm chart
	@# Helm will not adopt resources from a kustomize install; remove one if present.
	-kubectl --context kind-$(CLUSTER) delete -k hack/dev --ignore-not-found --wait=true >/dev/null 2>&1
	helm upgrade --install kubeamber $(CHART) --kube-context kind-$(CLUSTER) \
	  --namespace kubeamber-system --create-namespace \
	  --set image.repository=kubeamber --set image.tag=dev --wait --timeout 3m
	kubectl --context kind-$(CLUSTER) -n kubeamber-system rollout restart deploy/kubeamber
	kubectl --context kind-$(CLUSTER) -n kubeamber-system rollout status deploy/kubeamber --timeout=120s

kind-demo-helm: kind-deploy-helm ## Install on kind with Helm and run the agent scenarios
	CONTEXT=kind-$(CLUSTER) KUBEAMBER=bin/kubeamber hack/e2e.sh

kind-demo: kind-deploy ## Install on kind and walk through agent scenarios
	CONTEXT=kind-$(CLUSTER) KUBEAMBER=bin/kubeamber hack/e2e.sh

e2e: kind-demo ## Same as kind-demo; fails if any scenario misbehaves

kind-down:
	kind delete cluster --name $(CLUSTER)

clean:
	rm -rf bin
