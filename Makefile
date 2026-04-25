IMAGE ?= ghcr.io/huggingface/hf-csi-driver
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X github.com/huggingface/hf-buckets-csi-driver/pkg/driver.Version=$(VERSION)"

.PHONY: build test docker-build docker-push clean \
        e2e-cluster-podmount e2e-cluster-sidecar \
        e2e-podmount e2e-sidecar e2e \
        e2e-clean-podmount e2e-clean-sidecar e2e-clean

build:
	CGO_ENABLED=0 go build $(LDFLAGS) -o bin/hf-csi-driver ./cmd/hf-csi-driver/

test:
	go test -v ./pkg/...

docker-build:
	docker build -t $(IMAGE):$(VERSION) .

docker-push: docker-build
	docker push $(IMAGE):$(VERSION)

clean:
	rm -rf bin/

# --- e2e ---------------------------------------------------------------------
# Spawns a kind cluster, builds & loads the driver image, helm-installs the
# chart, then runs the test suite. Set HF_TOKEN to run bucket tests.
#
# Override HF_MOUNT_IMAGE (default ghcr.io/huggingface/hf-mount-fuse:v0.3.1) or
# set HF_MOUNT_BUILD_DIR=/path/to/hf-mount to build hf-mount from source.

e2e-cluster-podmount:
	test/e2e/setup-cluster.sh podmount

e2e-cluster-sidecar:
	test/e2e/setup-cluster.sh sidecar

e2e-podmount: e2e-cluster-podmount
	test/e2e/podmount/run.sh

e2e-sidecar: e2e-cluster-sidecar
	test/e2e/sidecar/run.sh

e2e: e2e-sidecar e2e-podmount

e2e-clean-podmount:
	test/e2e/teardown-cluster.sh podmount

e2e-clean-sidecar:
	test/e2e/teardown-cluster.sh sidecar

e2e-clean: e2e-clean-podmount e2e-clean-sidecar
