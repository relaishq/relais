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

.PHONY: all build clean test coverage deps lint lint-install vet fmt run bench profile test-streams test-stream-groups test-cluster help

all: test build

build: ensure_bin_dir ## Build the binary
	$(GOBUILD) -o $(BIN_DIR)/$(BINARY_NAME) -v $(LDFLAGS) ./cmd/relais-core
	$(GOBUILD) -o $(BIN_DIR)/$(BINARY_NAME)-ingress -v $(LDFLAGS) ./cmd/ingress-runner
	$(GOBUILD) -o $(BIN_DIR)/$(BINARY_NAME)-egress -v $(LDFLAGS) ./cmd/egress-runner
	$(GOBUILD) -o $(BIN_DIR)/$(BINARY_NAME)-transform -v $(LDFLAGS) ./cmd/transform-runner

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

lint-install: ## Install/update golangci-lint
	@echo "Installing golangci-lint..."
	$(GOCMD) install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
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

run: build ## Run the application
	./$(BIN_DIR)/$(BINARY_NAME)

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

help: ## Display this help screen
	@grep -h -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'