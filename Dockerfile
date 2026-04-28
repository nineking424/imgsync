ARG GO_VERSION=1.25
ARG VERSION=dev

# ──────────────────────────────────────────────────────────────────────
# Builder
# ──────────────────────────────────────────────────────────────────────
FROM golang:${GO_VERSION}-alpine AS builder

ARG VERSION
ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64

WORKDIR /src

# Cache module downloads first
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy source
COPY . .

# Build the single binary; -trimpath for reproducibility, -s -w to shrink
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/imgsync \
        ./cmd/imgsync

# Bundle migrations as a separate copy (the binary references them via filesystem)
RUN mkdir -p /out/migrations && cp -r ./migrations/* /out/migrations/

# ──────────────────────────────────────────────────────────────────────
# Runtime
# ──────────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

# Binary
COPY --from=builder /out/imgsync /app/imgsync

# Migrations (read-only at runtime, baked in)
COPY --from=builder /out/migrations /app/migrations

# Default migrations dir resolved relative to /app
ENV IMGSYNC_MIGRATIONS_DIR=/app/migrations

USER nonroot:nonroot

# /app/imgsync is the entrypoint; subcommand passed via CMD or k8s args
ENTRYPOINT ["/app/imgsync"]
CMD ["--help"]
