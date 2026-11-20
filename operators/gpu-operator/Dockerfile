# Build stage
FROM golang:1.27-alpine AS builder

WORKDIR /workspace

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
# every root .go file is part of package main (main.go plus flag/registration helpers)
COPY *.go ./
COPY api/ api/
COPY controllers/ controllers/
COPY pkg/ pkg/
COPY cmd/ cmd/

# Build
RUN CGO_ENABLED=0 go build -a -o manager . && \
    CGO_ENABLED=0 go build -a -o gpu-reset-agent ./cmd/gpu-reset-agent

# Runtime stage
FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/gpu-reset-agent .

USER 65532:65532

ENTRYPOINT ["/manager"]
