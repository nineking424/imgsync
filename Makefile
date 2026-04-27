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
