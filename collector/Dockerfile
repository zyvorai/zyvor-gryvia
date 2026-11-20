# Stage 1: Compile eBPF programs.
FROM ubuntu:24.04 AS ebpf-builder

RUN apt-get update && apt-get install -y --no-install-recommends \
    clang llvm libbpf-dev linux-libc-dev make \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build/ebpf
COPY ebpf/ .
RUN make

# Stage 2: Build the Go collector binary.
FROM golang:1.27-bookworm AS go-builder

WORKDIR /build/collector
COPY collector/go.mod collector/go.sum ./
RUN go mod download

COPY collector/ .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /gryvia-collector .

# Stage 3: Minimal runtime image.
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="Gryvia Flow Collector"
LABEL org.opencontainers.image.description="eBPF-based network flow collector for Kubernetes"
LABEL org.opencontainers.image.source="https://github.com/zyvorai/gryvia"

# Copy compiled eBPF programs.
COPY --from=ebpf-builder /build/ebpf/*.o /opt/gryvia/ebpf/

# Copy the collector binary.
COPY --from=go-builder /gryvia-collector /usr/local/bin/gryvia-collector

# The container must run as root (privileged) for eBPF operations.
USER root

EXPOSE 9090

ENTRYPOINT ["/usr/local/bin/gryvia-collector"]
CMD ["--metrics-addr=:9090", "--ebpf-dir=/opt/gryvia/ebpf"]
