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

# Linker flags
LDFLAGS=-ldflags "-X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildTime=$(BUILD_TIME)"

.PHONY: all build clean test coverage deps lint vet fmt run help

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

lint: ## Run linter
	golangci-lint run

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

help: ## Display this help screen
	@grep -h -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'