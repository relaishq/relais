## Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod
BINARY_NAME=relais
BINARY_UNIX=$(BINARY_NAME)_unix
BIN_DIR=bin

# Build-time variables
VERSION ?= $(shell git describe --tags --always --dirty)
COMMIT ?= $(shell git rev-parse --short HEAD)
BUILD_TIME ?= $(shell date -u '+%Y-%m-%d_%H:%M:%S')

# Linker flags (populate pkg/buildinfo)
LDFLAGS=-ldflags "-X github.com/relais/pkg/buildinfo.Version=$(VERSION) -X github.com/relais/pkg/buildinfo.Commit=$(COMMIT) -X github.com/relais/pkg/buildinfo.Date=$(BUILD_TIME)"

.PHONY: all build clean test coverage deps lint lint-install vet fmt bench profile test-streams test-stream-groups test-cluster test-harness test-harness-timing demo demo-cluster crash-run help

all: test build

build: ensure_bin_dir ## Build plugin runners, relay, worker, control, crash-run and demo binaries
	$(GOBUILD) -o $(BIN_DIR)/$(BINARY_NAME)-ingress -v $(LDFLAGS) ./cmd/ingress-runner
	$(GOBUILD) -o $(BIN_DIR)/$(BINARY_NAME)-egress -v $(LDFLAGS) ./cmd/egress-runner
	$(GOBUILD) -o $(BIN_DIR)/$(BINARY_NAME)-transform -v $(LDFLAGS) ./cmd/transform-runner

	$(GOBUILD) -o $(BIN_DIR)/relais-relay $(LDFLAGS) ./cmd/relais-relay
	$(GOBUILD) -o $(BIN_DIR)/relais-worker $(LDFLAGS) ./cmd/relais-worker
	$(GOBUILD) -o $(BIN_DIR)/relais-control $(LDFLAGS) ./cmd/relais-control
	$(GOBUILD) -o $(BIN_DIR)/crash-run $(LDFLAGS) ./cmd/crash-run
	$(GOBUILD) -o $(BIN_DIR)/relais-demo $(LDFLAGS) ./cmd/relais-demo

DEMO_CLUSTER_FLAGS ?=
demo-cluster: build ## Run three separate workers and the browser move/drain/kill page on loopback; starts its own Redis (never 6379)
	./$(BIN_DIR)/relais-demo $(DEMO_CLUSTER_FLAGS)

CRASH_FLAGS ?=
crash-run: build ## Run ten real-process SIGKILL calls on a dedicated throwaway Redis; requires Redis and ffmpeg
	./$(BIN_DIR)/crash-run $(CRASH_FLAGS)

ensure_bin_dir: ## Create bin directory if it doesn't exist
	mkdir -p $(BIN_DIR)

clean: ## Remove build artifacts
	$(GOCLEAN)
	rm -rf $(BIN_DIR)

test: ## Run tests
	$(GOTEST) -v ./...

coverage: ## Run tests with coverage
	$(GOTEST) -coverprofile=coverage.out ./...
	$(GOCMD) tool cover -html=coverage.out

deps: ## Download dependencies
	$(GOMOD) download

lint-install: ## Install golangci-lint (the version CI uses)
	@echo "Installing golangci-lint..."
	$(GOCMD) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
	@echo "golangci-lint installed!"

lint: ## Run comprehensive linter
	@echo "Running golangci-lint..."
	@if command -v ~/go/bin/golangci-lint >/dev/null 2>&1; then \
		~/go/bin/golangci-lint run ./...; \
	elif command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not found. Install with: make lint-install"; \
		exit 1; \
	fi
	@echo "All linting checks passed!"

vet: ## Run go vet
	$(GOCMD) vet ./...

fmt: ## Run go fmt
	$(GOCMD) fmt ./...

bench: ## Run benchmarks
	$(GOTEST) -bench=. ./test/benchmark/...

profile: ## Run benchmarks with profiling
	$(GOTEST) -bench=. -cpuprofile=cpu.prof -memprofile=mem.prof ./test/benchmark/...

test-stream-groups: ## Run Redis Streams consumer group tests only
	$(GOTEST) -v -count=1 -run '^(Test(Stream|Track)StreamGroup_.*|TestStreamGroup_.*)$$' ./pkg/storage

test-streams: ## Run Redis Streams tests (non-cluster) in storage
	$(GOTEST) -v -count=1 -run '^(TestStreams.*|Test(Stream|Track)Stream(Group)?_.*)$$' ./pkg/storage

test-cluster: ## Run Redis Cluster integration tests (requires RELAIS_TEST_REDIS_CLUSTER_ADDRS)
	$(GOTEST) -v -count=1 -run '^TestCluster' ./pkg/storage

HARNESS_PKGS=./pkg/framecache/... ./pkg/controlplane/... ./pkg/mediaworker/... ./pkg/relay/... ./pkg/sessionstore/... ./pkg/callharness/... ./cmd/echo-demo/...
HARNESS_FLAGS ?=

test-harness: ## Run the call harness (audio+video echo calls, direct and through the relay) with the race detector; HARNESS_FLAGS=-short for 5 s calls; full VP8 decode needs ffmpeg on PATH
	$(GOTEST) -race -v -count=1 -timeout 30m $(HARNESS_FLAGS) $(HARNESS_PKGS)

test-harness-timing: ## Run the relay move, drain and takeover timing tests without the race detector, which enforce issues #6/#7/#8's gap targets (needs ffmpeg on PATH)
	$(GOTEST) -v -count=1 -timeout 30m -run '^TestRelay(PlannedHandover|DrainTenCalls|HardKillTakeover|StaleSnapshotTakeover|MidKeyframeTakeover|FirstKeyframeTakeover|FrameCacheModes|FrameCacheMidKeyframe|FrameCacheWithoutSource|FrameCacheLargeGroup|PlannedWrap|TakeoverWrap)$$' ./pkg/callharness/

DEMO_FLAGS ?=

demo: ## Run media workers A and B on one UDP socket plus the browser echo page on http://localhost:9101 (?source=test for the test pattern; "Move call" hands the call over); DEMO_FLAGS="-media-ip=IP -media-port=N -http=ADDR -relay" (-relay: one worker behind a relay on the media address instead; no moves)
	$(GOCMD) run ./cmd/echo-demo $(DEMO_FLAGS)

help: ## Display this help screen
	@grep -h -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'