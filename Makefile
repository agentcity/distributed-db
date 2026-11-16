SHELL := /bin/bash
GO ?= go
BINARY := bin/db
CHAOS_ROUNDS ?= 3
CHAOS_TIMEOUT ?= 1200

.PHONY: all build test test-short vet fmt proto compose-up compose-down smoke chaos-nightly run-coordinator run-node1 run-node2 run-node3 dev clean

all: build

build:
	@mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o $(BINARY) ./cmd/db

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
	docker compose up -d --build

compose-down:
	docker compose down -v

smoke:
	./scripts/smoke.sh

# Nightly chaos/soak suite (Jepsen partitions + v2 snapshot/split tests).
# Override CHAOS_ROUNDS=1 for a local smoke run.
chaos-nightly:
	GO="$(GO)" GOFLAGS="$(GOFLAGS)" \
		CHAOS_ROUNDS="$(CHAOS_ROUNDS)" CHAOS_TIMEOUT="$(CHAOS_TIMEOUT)" \
		./scripts/nightly-chaos.sh

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
