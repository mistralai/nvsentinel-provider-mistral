GO ?= go
HELM ?= helm
PYTHON ?= python3
VERSION ?= dev
ENVTEST_K8S_VERSION ?= 1.37.0
SETUP_ENVTEST_VERSION := v0.25.0
LOCALBIN := $(CURDIR)/bin
CHART := charts/nvsentinel-provider-mistral

.PHONY: check fmt-check mod-check vet test test-integration chart-test envtest build

check: fmt-check mod-check vet test-integration chart-test

mod-check:
	$(GO) mod tidy -diff

fmt-check:
	@test -z "$$(gofmt -l $$(find cmd internal -name '*.go' -type f))" || { echo 'Run gofmt on Go sources'; exit 1; }

vet:
	$(GO) vet ./...

test:
	$(GO) test -race ./...

envtest: $(LOCALBIN)/setup-envtest

$(LOCALBIN)/setup-envtest:
	@mkdir -p $(LOCALBIN)
	GOBIN=$(LOCALBIN) $(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)

test-integration: envtest
	@set -eu; assets="$$($(LOCALBIN)/setup-envtest use $(ENVTEST_K8S_VERSION) -p path)"; \
	test -n "$$assets"; KUBEBUILDER_ASSETS="$$assets" $(GO) test -race ./...

chart-test:
	$(HELM) lint --strict $(CHART) -f charts/ci-values.yaml
	$(PYTHON) charts/tests/test_chart.py

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-X main.version=$(VERSION)" -o bin/provider ./cmd/provider
