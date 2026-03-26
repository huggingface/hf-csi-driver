# Stage 1: Get hf-mount-fuse from pre-built image
FROM ghcr.io/huggingface/hf-mount-fuse:latest AS hf-mount

# Stage 2: Build CSI driver (Go)
FROM golang:1.25-bookworm AS go-builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY pkg/ pkg/
RUN CGO_ENABLED=0 go build -o /bin/hf-csi-driver ./cmd/hf-csi-driver/

# Stage 3: Runtime
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends libfuse3-3 ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=hf-mount /usr/local/bin/hf-mount-fuse /usr/local/bin/
COPY --from=go-builder /bin/hf-csi-driver /bin/
ENTRYPOINT ["/bin/hf-csi-driver"]
