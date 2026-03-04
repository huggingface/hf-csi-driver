# Stage 1: Build hf-mount-fuse (Rust)
FROM rust:1.85-bookworm AS rust-builder
ARG GIT_AUTH_TOKEN
RUN git config --global url."https://x-access-token:${GIT_AUTH_TOKEN}@github.com/".insteadOf "https://github.com/"
WORKDIR /build
COPY hf-mount/ .
RUN cargo build --release --bin hf-mount-fuse

# Stage 2: Build CSI driver (Go)
FROM golang:1.24-bookworm AS go-builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY pkg/ pkg/
RUN CGO_ENABLED=0 go build -o /bin/hf-csi-driver ./cmd/hf-csi-driver/

# Stage 3: Runtime
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends fuse3 ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=rust-builder /build/target/release/hf-mount-fuse /usr/local/bin/
COPY --from=go-builder /bin/hf-csi-driver /bin/
ENTRYPOINT ["/bin/hf-csi-driver"]
