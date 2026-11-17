SHELL := /bin/bash
GO ?= go
BINARY := bin/db
CHAOS_ROUNDS ?= 3
CHAOS_TIMEOUT ?= 1200

# --- image release -----------------------------------------------------------
# The tag is derived from the checkout (git describe), not from the clock, so
# the same commit yields the same tag. Every value can be overridden:
#   make image IMAGE_REPOSITORY=ghcr.io/acme/distributed-db IMAGE_TAG=v2.0.0
IMAGE_REPOSITORY ?= distributed-db
GIT_VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE_TAG ?= $(GIT_VERSION)
VERSION ?= $(IMAGE_TAG)
REVISION ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
DOCKER ?= docker
PLATFORM ?=
IMAGE ?= $(IMAGE_REPOSITORY):$(IMAGE_TAG)

# Multi-arch knobs. `image-multiarch` writes an offline OCI archive (no
# registry needed); `image-multiarch-push` publishes a multi-platform index
# and requires an authenticated `docker login`.
PLATFORMS ?= linux/amd64,linux/arm64
OCI_DEST ?= /tmp/$(subst :,_,$(subst /,_,$(IMAGE_REPOSITORY)))-$(IMAGE_TAG)-multiarch.oci.tar
BUILDER ?=
BUILDX_AUTO_CREATE ?= 1
PULL ?= 0

# --- Helm validation ---------------------------------------------------------
HELM ?= helm
CHART_DIR ?= deploy/helm/distributed-db
RELEASE ?= distributed-db
NAMESPACE ?= distributed-db

.PHONY: all build build-cli test test-short vet fmt proto compose-up compose-down compose-cluster-up compose-cluster-down smoke chaos-nightly run-coordinator run-node1 run-node2 run-node3 dev clean image image-push image-multiarch image-multiarch-push test-image helm-lint helm-template release-check

all: build

build:
	@mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o $(BINARY) ./cmd/db

build-cli:
	@mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o bin/kvctl ./cmd/kvctl

test:
	$(GO) test -race ./...

test-short:
	$(GO) test -short ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './.git/*')

# Requires protoc, protoc-gen-go and protoc-gen-go-grpc in PATH.
proto:
	protoc -I proto \
		--go_out=. --go_opt=module=distributed-db \
		--go-grpc_out=. --go-grpc_opt=module=distributed-db \
		proto/db.proto

compose-up:
	docker compose --profile demo up -d --build

compose-down:
	docker compose --profile demo down -v

compose-cluster-up:
	docker compose -f docker-compose.cluster.yml --profile cluster up -d --build

compose-cluster-down:
	docker compose -f docker-compose.cluster.yml --profile cluster down -v

smoke:
	./scripts/smoke.sh

# Nightly chaos/soak suite (Jepsen partitions + v2 snapshot/split tests).
# Override CHAOS_ROUNDS=1 for a local smoke run.
chaos-nightly:
	GO="$(GO)" GOFLAGS="$(GOFLAGS)" \
		CHAOS_ROUNDS="$(CHAOS_ROUNDS)" CHAOS_TIMEOUT="$(CHAOS_TIMEOUT)" \
		./scripts/nightly-chaos.sh

# --- container image ---------------------------------------------------------
# Build the multi-binary image. Build args VERSION/REVISION and the tag all
# come from the checkout (or from the overrides above).
image:
	IMAGE_REPOSITORY="$(IMAGE_REPOSITORY)" \
	IMAGE_TAG="$(IMAGE_TAG)" \
	VERSION="$(VERSION)" \
	REVISION="$(REVISION)" \
	DOCKER="$(DOCKER)" \
	PLATFORM="$(PLATFORM)" \
	./scripts/build-image.sh

# Optional publishing step. Requires an authenticated `docker login`; the
# target builds and pushes in one script invocation and never stores
# credentials.
image-push:
	IMAGE_REPOSITORY="$(IMAGE_REPOSITORY)" \
	IMAGE_TAG="$(IMAGE_TAG)" \
	VERSION="$(VERSION)" \
	REVISION="$(REVISION)" \
	DOCKER="$(DOCKER)" \
	PLATFORM="$(PLATFORM)" \
	PUSH=1 \
	./scripts/build-image.sh

# Build a real multi-platform image index for PLATFORMS (default amd64+arm64)
# and write it to a local OCI archive. Needs Docker buildx but no registry
# login and no push: useful offline and in CI without credentials.
image-multiarch:
	IMAGE_REPOSITORY="$(IMAGE_REPOSITORY)" \
	IMAGE_TAG="$(IMAGE_TAG)" \
	VERSION="$(VERSION)" \
	REVISION="$(REVISION)" \
	DOCKER="$(DOCKER)" \
	MULTIARCH=1 \
	PLATFORMS="$(PLATFORMS)" \
	OUTPUT=oci \
	OCI_DEST="$(OCI_DEST)" \
	BUILDER="$(BUILDER)" \
	BUILDX_AUTO_CREATE="$(BUILDX_AUTO_CREATE)" \
	PULL="$(PULL)" \
	./scripts/build-image.sh
	@echo "multiarch OCI archive: $(OCI_DEST)"

# Build and push the multi-platform image index in one buildx invocation.
# Requires `docker login` for IMAGE_REPOSITORY; the script never stores or
# logs credentials and verifies the pushed manifest with imagetools.
image-multiarch-push:
	IMAGE_REPOSITORY="$(IMAGE_REPOSITORY)" \
	IMAGE_TAG="$(IMAGE_TAG)" \
	VERSION="$(VERSION)" \
	REVISION="$(REVISION)" \
	DOCKER="$(DOCKER)" \
	MULTIARCH=1 \
	PUSH=1 \
	PLATFORMS="$(PLATFORMS)" \
	BUILDER="$(BUILDER)" \
	BUILDX_AUTO_CREATE="$(BUILDX_AUTO_CREATE)" \
	PULL="$(PULL)" \
	./scripts/build-image.sh

# Shell-only self-test for the image build script (no registry, no image
# build by default). Set MULTIARCH_REAL=1 to also run a real OCI build.
test-image:
	./scripts/test-build-image.sh

# --- Helm validation ---------------------------------------------------------
# Render the default values and parse every document as YAML. Fails before the
# lint if a template renders invalid YAML.
helm-template:
	@command -v "$(HELM)" >/dev/null || { echo "helm not found: install Helm 3.14+ or set HELM=" >&2; exit 1; }
	"$(HELM)" template "$(RELEASE)" "$(CHART_DIR)" --namespace "$(NAMESPACE)" >/tmp/$(RELEASE)-rendered.yaml
	@echo "rendered /tmp/$(RELEASE)-rendered.yaml"

helm-lint: helm-template
	"$(HELM)" lint --strict "$(CHART_DIR)"

# Local release gate: shell syntax, rendered YAML, helm lint/template, and an
# optional image build (WITH_IMAGE=1). See scripts/validate-release.sh.
release-check:
	./scripts/validate-release.sh

# --- local 3-node cluster + coordinator (separate terminals) -----------------
run-coordinator:
	$(GO) run ./cmd/db coordinator -http-addr=:8080 -grpc-addr=:9090 \
		-shards='shard-1=127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003' \
		-auth-user=admin -auth-pass=admin -log-level=debug

run-node1:
	$(GO) run ./cmd/db node -id=127.0.0.1:7001 -shard=shard-1 \
		-raft-addr=127.0.0.1:7001 -http-addr=:8001 \
		-peers=127.0.0.1:7002,127.0.0.1:7003 -data-dir=./data/node1 \
		-auth-user=admin -auth-pass=admin -log-level=debug

run-node2:
	$(GO) run ./cmd/db node -id=127.0.0.1:7002 -shard=shard-1 \
		-raft-addr=127.0.0.1:7002 -http-addr=:8002 \
		-peers=127.0.0.1:7001,127.0.0.1:7003 -data-dir=./data/node2 \
		-auth-user=admin -auth-pass=admin -log-level=debug

run-node3:
	$(GO) run ./cmd/db node -id=127.0.0.1:7003 -shard=shard-1 \
		-raft-addr=127.0.0.1:7003 -http-addr=:8003 \
		-peers=127.0.0.1:7001,127.0.0.1:7002 -data-dir=./data/node3 \
		-auth-user=admin -auth-pass=admin -log-level=debug

# Starts a local 3-node cluster and the coordinator in the background.
dev:
	./scripts/dev-cluster.sh

clean:
	rm -rf bin data coverage.txt
