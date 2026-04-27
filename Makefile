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
