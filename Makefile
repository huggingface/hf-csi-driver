IMAGE ?= ghcr.io/huggingface/hf-csi-driver
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X github.com/huggingface/hf-buckets-csi-driver/pkg/driver.Version=$(VERSION)"

.PHONY: build test docker-build docker-push clean

build:
	CGO_ENABLED=0 go build $(LDFLAGS) -o bin/hf-csi-driver ./cmd/hf-csi-driver/

test:
	go test -v ./pkg/...

docker-build:
	docker build -t $(IMAGE):$(VERSION) --build-arg GIT_AUTH_TOKEN=$(GIT_AUTH_TOKEN) .

docker-push: docker-build
	docker push $(IMAGE):$(VERSION)

clean:
	rm -rf bin/
