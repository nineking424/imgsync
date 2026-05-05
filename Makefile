VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= imgsync:$(VERSION)

.PHONY: build test lint streaming-check tidy ci

build:
	go build -o bin/imgsync ./cmd/imgsync

test:
	go test ./... -race -count=1

lint:
	golangci-lint run

streaming-check:
	./scripts/check-streaming.sh

tidy:
	go mod tidy

ci: lint streaming-check test

.PHONY: docker-build
docker-build: ## Build the production container image
	DOCKER_BUILDKIT=1 docker build \
		--build-arg VERSION=$(VERSION) \
		-t $(IMAGE) \
		-t imgsync:dev \
		.

.PHONY: docker-test
docker-test: ## Run the Dockerfile contract checks
	./scripts/test-docker-build.sh

.PHONY: docker-run-help
docker-run-help: docker-build ## Smoke test the built image
	docker run --rm imgsync:dev --help

.PHONY: dev-up
dev-up: docker-build ## Stand up the dev compose stack
	docker compose up -d

.PHONY: dev-down
dev-down: ## Tear down the dev compose stack
	docker compose down -v

.PHONY: dev-seed
dev-seed: ## Enqueue 10 smoke-test jobs into the dev stack
	./scripts/dev-seed.sh

.PHONY: dev-smoke
dev-smoke: ## Run dev stack end-to-end smoke test (assumes dev-up + dev-seed already ran)
	./scripts/dev-smoke-test.sh

HELM_CHART = deploy/helm/imgsync

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart
	helm lint $(HELM_CHART)

.PHONY: helm-template
helm-template: ## Render Helm chart with default values
	helm template t $(HELM_CHART)

.PHONY: helm-test
helm-test: ## Run Helm chart structural tests
	./$(HELM_CHART)/tests/template_test.sh

.PHONY: e2e-up
e2e-up: ## Bring up the kind+chart e2e environment
	./scripts/e2e-up.sh

.PHONY: e2e-down
e2e-down: ## Tear down the e2e environment
	./scripts/e2e-down.sh

.PHONY: e2e-throughput
e2e-throughput: ## Run C7 throughput E2E (kind cluster required)
	IMGSYNC_E2E=1 go test -tags e2e -timeout 35m -v ./e2e/... -run TestC7_ThroughputScaleOut

.PHONY: e2e-dirty-state
e2e-dirty-state: ## Run F5 dirty-state recovery E2E (added in Task 6)
	IMGSYNC_E2E=1 go test -tags e2e -timeout 30m -v ./e2e/... -run TestF5_DirtyStateRecovery

.PHONY: e2e-sniffer
e2e-sniffer: ## Run sniffer C5' E2E (kind cluster required)
	IMGSYNC_E2E=1 go test -tags e2e -timeout 20m -v ./e2e/... -run TestC5Prime_

.PHONY: e2e-up-real
e2e-up-real: ## Bring up the real-cluster e2e environment (requires kubectl context set)
	./scripts/e2e-up-real.sh

.PHONY: e2e-down-real
e2e-down-real: ## Tear down the real-cluster e2e environment
	./scripts/e2e-down-real.sh

.PHONY: e2e-seed-real
e2e-seed-real: ## Seed fixture files into the real-cluster localfs PVC (defaults: 1000 × 1KB)
	./scripts/e2e-seed-real.sh

.PHONY: test-integration-sniffer
test-integration-sniffer: ## Run sniffer integration tests S0-S3 (requires Docker)
	go test -tags integration -timeout 5m -run "TestS[0-3]_" -v ./internal/sniffer/

.PHONY: e2e-push-real
e2e-push-real: ## Build and push imgsync image to ghcr.io for real-cluster e2e
	./scripts/e2e-image-push.sh

docs-install: ## docs 빌드 의존성 설치 (venv 권장)
	pip install -r requirements-docs.txt

docs-serve: ## 로컬 라이브 미리보기 (http://localhost:8000)
	mkdocs serve --strict

docs-build: ## 정적 사이트 빌드 (--strict, 링크/레퍼런스 깨지면 실패)
	mkdocs build --strict

docs-clean:
	rm -rf site/

.PHONY: docs-install docs-serve docs-build docs-clean
