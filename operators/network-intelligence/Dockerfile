# Build stage
FROM golang:1.27-alpine AS builder

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

# every root .go file is part of package main (main.go plus flag/registration helpers)
COPY *.go ./
COPY api/ api/
COPY controllers/ controllers/
COPY pkg/ pkg/

RUN CGO_ENABLED=0 go build -a -o manager .

# Runtime stage
FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /workspace/manager .

USER 65532:65532

ENTRYPOINT ["/manager"]
