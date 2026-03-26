# Build CSI driver (Go)
FROM golang:1.25-bookworm AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY pkg/ pkg/
RUN CGO_ENABLED=0 go build -o /bin/hf-csi-driver ./cmd/hf-csi-driver/

# Runtime (static binary, no deps needed)
FROM gcr.io/distroless/static-debian12
COPY --from=builder /bin/hf-csi-driver /bin/
ENTRYPOINT ["/bin/hf-csi-driver"]
