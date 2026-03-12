# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.23-alpine AS builder

WORKDIR /workspace

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY cmd/        cmd/
COPY api/        api/
COPY internal/   internal/
COPY docs/       docs/

# Build a statically linked binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -a -ldflags="-s -w" -o /workspace/orchestrator ./cmd/main.go

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /workspace/orchestrator /orchestrator

# REST API   :8082
# Metrics    :8080
# Health     :8081
EXPOSE 8080 8081 8082

USER 65532:65532

ENTRYPOINT ["/orchestrator"]
