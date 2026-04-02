# Build CSI driver + sidecar mounter (Go)
FROM golang:1.25-bookworm AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY pkg/ pkg/
RUN CGO_ENABLED=0 go build -o /bin/hf-csi-driver ./cmd/hf-csi-driver/

# Runtime: needs /bin/mount for FUSE mount via mount-utils
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends mount && rm -rf /var/lib/apt/lists/*
COPY --from=builder /bin/hf-csi-driver /bin/
ENTRYPOINT ["/bin/hf-csi-driver"]
