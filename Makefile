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
