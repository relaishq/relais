## Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod
GOLANGCI_LINT_VERSION := v2.14.0
# Explicit-version go run ignores the main module toolchain directive. Reuse
# the module-selected toolchain instead of downloading a newer one for lint.
GOLANGCI_LINT = GOTOOLCHAIN=$(shell $(GOCMD) env GOVERSION) $(GOCMD) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
BINARY_NAME=relais
BINARY_UNIX=$(BINARY_NAME)_unix
BIN_DIR=bin

# Build-time variables
VERSION ?= $(shell git describe --tags --always --dirty)
COMMIT ?= $(shell git rev-parse --short HEAD)
BUILD_TIME ?= $(shell date -u '+%Y-%m-%d_%H:%M:%S')

# Linker flags (populate pkg/buildinfo)
LDFLAGS=-ldflags "-X github.com/relaishq/relais/pkg/buildinfo.Version=$(VERSION) -X github.com/relaishq/relais/pkg/buildinfo.Commit=$(COMMIT) -X github.com/relaishq/relais/pkg/buildinfo.Date=$(BUILD_TIME)"

.PHONY: check check-quality check-rules test-external-module test-ci-scripts test-demo-page test-unit-local all build clean test coverage deps lint lint-install vet fmt bench profile test-streams test-stream-groups test-cluster test-unit test-harness test-harness-short test-harness-timing test-harness-smoke test-full demo demo-cluster demo-agent crash-run release-pair release-pair-source help

all: test build

build: ensure_bin_dir ## Build plugin runners, relay, worker, control, signal, crash-run and demo/example binaries
	$(GOBUILD) -o $(BIN_DIR)/$(BINARY_NAME)-ingress -v $(LDFLAGS) ./cmd/ingress-runner
	$(GOBUILD) -o $(BIN_DIR)/$(BINARY_NAME)-egress -v $(LDFLAGS) ./cmd/egress-runner
	$(GOBUILD) -o $(BIN_DIR)/$(BINARY_NAME)-transform -v $(LDFLAGS) ./cmd/transform-runner

	$(GOBUILD) -o $(BIN_DIR)/relais-relay $(LDFLAGS) ./cmd/relais-relay
	$(GOBUILD) -o $(BIN_DIR)/relais-worker $(LDFLAGS) ./cmd/relais-worker
	$(GOBUILD) -o $(BIN_DIR)/relais-control $(LDFLAGS) ./cmd/relais-control
	$(GOBUILD) -o $(BIN_DIR)/relais-signal $(LDFLAGS) ./cmd/relais-signal
	$(GOBUILD) -o $(BIN_DIR)/crash-run $(LDFLAGS) ./cmd/crash-run
	$(GOBUILD) -o $(BIN_DIR)/relais-load $(LDFLAGS) ./cmd/relais-load
	$(GOBUILD) -o $(BIN_DIR)/relais-demo $(LDFLAGS) ./cmd/relais-demo

	$(GOBUILD) -o $(BIN_DIR)/delayed-loopback $(LDFLAGS) ./examples/delayed-loopback
	$(GOBUILD) -o $(BIN_DIR)/voice-agent $(LDFLAGS) ./examples/voice-agent

DEMO_CLUSTER_FLAGS ?=
demo-cluster: build ## Run three separate workers and the browser move/drain/kill page on loopback; starts its own Redis (never 6379)
	./$(BIN_DIR)/relais-demo $(DEMO_CLUSTER_FLAGS)

demo-agent: build ## Run the scripted tone agent through moves and crashes; owns throwaway Redis
	./$(BIN_DIR)/relais-demo -agent=demo $(DEMO_CLUSTER_FLAGS)

CRASH_FLAGS ?=
crash-run: build ## Compare ten real-process SIGKILL calls per cache/PLI mode on throwaway Redis; requires ffmpeg
	./$(BIN_DIR)/crash-run $(CRASH_FLAGS)

PAIR_A ?=
PAIR_B ?=
PAIR_FLAGS ?=
release-pair: ensure_bin_dir ## Upgrade release archive PAIR_A to PAIR_B and roll back with live calls, gated by each target's relais-compat; owns throwaway Redis
	$(GOBUILD) -o $(BIN_DIR)/crash-run $(LDFLAGS) ./cmd/crash-run
	./$(BIN_DIR)/crash-run -release-pair -pair-a "$(PAIR_A)" -pair-b "$(PAIR_B)" $(PAIR_FLAGS)

PAIR_A_VERSION ?=
PAIR_A_SOURCE ?=
PAIR_B_VERSION ?=
PAIR_B_SOURCE ?=
PAIR_ARCHIVES = $(BIN_DIR)/release-pair-archives
release-pair-source: ensure_bin_dir ## Build A and B with build-archives.sh from refs or clean checkouts (PAIR_*_SOURCE, stamped PAIR_*_VERSION), then run release-pair
	rm -rf $(PAIR_ARCHIVES)
	./scripts/release/pair-archives.sh build "$(PAIR_A_VERSION)" "$(PAIR_A_SOURCE)" $(PAIR_ARCHIVES)/a
	./scripts/release/pair-archives.sh build "$(PAIR_B_VERSION)" "$(PAIR_B_SOURCE)" $(PAIR_ARCHIVES)/b
	$(MAKE) release-pair PAIR_A=$(PAIR_ARCHIVES)/a PAIR_B=$(PAIR_ARCHIVES)/b

ensure_bin_dir: ## Create bin directory if it doesn't exist
	mkdir -p $(BIN_DIR)

clean: ## Remove build artifacts
	$(GOCLEAN)
	rm -rf $(BIN_DIR)

test: ## Run all tests (Redis integration requires RELAIS_TEST_REDIS_ADDR)
	$(GOTEST) -v ./...

coverage: ## Run tests with coverage
	$(GOTEST) -coverprofile=coverage.out ./...
	$(GOCMD) tool cover -html=coverage.out

deps: ## Download dependencies
	$(GOMOD) download

# Keep these targets in the same order as the PR quality job.
check: ## Run PR quality and race unit gates with a disposable Redis
	$(MAKE) check-quality
	$(MAKE) test-unit-local

check-quality: ## Run every PR quality gate in CI order
	$(MAKE) deps
	$(MAKE) vet
	$(MAKE) lint
	$(MAKE) test-external-module
	$(MAKE) build
	$(MAKE) test-ci-scripts
	$(MAKE) test-demo-page
	$(MAKE) check-rules

check-rules: ## Enforce behaviour names, Redis safety, doc paths and lint version
	python3 scripts/ci/check_rules.py

test-external-module: ## Build the supported external-module API without CGO
	$(GOTEST) -count=1 -run '^TestExternalModuleBuild$$' ./pkg/workerapp

test-ci-scripts: ## Test CI evidence and rule scripts
	python3 -m unittest discover -s scripts/ci

test-demo-page: ## Test demo page verdict logic
	node --test cmd/relais-demo/*.test.cjs

test-unit-local: ## Run race units with an owned Redis and CI test requirements
	python3 scripts/dev/with-redis.py $(MAKE) test-unit

lint-install: ## Install golangci-lint (the version CI uses)
	$(GOCMD) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

lint: ## Run the pinned linter without installing a binary
	$(GOLANGCI_LINT) run ./...

vet: ## Run go vet
	$(GOCMD) vet ./...

fmt: ## Apply configured formatters, including import grouping
	$(GOLANGCI_LINT) fmt ./...

bench: ## Run benchmarks
	$(GOTEST) -bench=. ./test/benchmark/...

profile: ## Run benchmarks with profiling
	$(GOTEST) -bench=. -cpuprofile=cpu.prof -memprofile=mem.prof ./test/benchmark/...

test-stream-groups: ## Run Redis Streams consumer group tests only
	$(GOTEST) -v -count=1 -run '^(Test(Stream|Track)StreamGroup_.*|TestStreamGroup_.*)$$' ./pkg/storage

test-streams: ## Run Redis Streams tests (non-cluster) in storage
	$(GOTEST) -v -count=1 -run '^(TestStreams.*|Test(Stream|Track)Stream(Group)?_.*)$$' ./pkg/storage

test-cluster: ## Run opt-in Redis Cluster tests (RELAIS_TEST_REDIS_CLUSTER=1; build process binaries first)
	$(GOTEST) -race -p 1 -v -count=1 -timeout 10m -run '^Test(Cluster|RedisCluster|Redis.*Slot|SlotCRC16)' ./pkg/storage ./pkg/sessionstore ./pkg/framecache ./internal/processrun ./internal/redistestcluster ./cmd/crash-run ./cmd/relais-control

# Cover all packages except the call harness, which has a separate CI job.
# Include the plugin and pipeline suites previously covered by make test.
UNIT_PKGS=$(shell $(GOCMD) list ./pkg/... ./internal/... ./cmd/... ./examples/... ./plugins/... ./test/... | awk '$$0 != "github.com/relaishq/relais/pkg/callharness"')

test-unit: ## Run all non-harness tests with the race detector; set RELAIS_TEST_REDIS_ADDR for Redis suites
	$(GOTEST) -race -v -count=1 -timeout 10m $(UNIT_PKGS)

# test-unit already covers every package outside the call harness under -race.
HARNESS_PKGS=./pkg/callharness/...
HARNESS_FLAGS ?=

test-harness: ## Run the call harness (audio+video echo calls, direct and through the relay) with the race detector; HARNESS_FLAGS=-short for 5 s calls; full VP8 decode needs ffmpeg on PATH
	$(GOTEST) -race -v -count=1 -timeout 45m $(HARNESS_FLAGS) $(HARNESS_PKGS)

# Disable Redis explicitly, including deeply nested uncertainty subtests,
# even if the invoking shell has Redis configured and required.
# The suite ran at 99% of the former 10-minute timeout before v2 coverage.
test-harness-short: ## Run the PR call harness with -race and -short (5 s baseline calls), memory only; requires ffmpeg
	RELAIS_TEST_REDIS_ADDR= RELAIS_TEST_REDIS_REQUIRE=0 $(GOTEST) -race -short -v -count=1 -timeout 15m ./pkg/callharness/

test-harness-smoke: ## Enforce PR timing thresholds without -race: 20 moves, 20 kills and planned wrap, memory only; requires ffmpeg
	RELAIS_TEST_REDIS_ADDR= RELAIS_TEST_REDIS_REQUIRE=0 $(GOTEST) -v -count=1 -timeout 10m -run '^TestRelay(PlannedHandover|HardKillTakeover|PlannedWrap)$$' ./pkg/callharness/

test-harness-timing: ## Run the relay move, drain and takeover timing tests without the race detector, which enforce issues #6/#7/#8's gap targets, plus the 20-call #127 reorder stress (needs ffmpeg on PATH)
	$(GOTEST) -v -count=1 -timeout 30m -run '^TestAcceptance|^TestRelay(PlannedHandover|DrainTenCalls|HardKillTakeover|StaleSnapshotTakeover|MidKeyframeTakeover|FirstKeyframeTakeover|FrameCacheModes|FrameCacheMidKeyframe|FrameCacheWithoutSource|FrameCacheLargeGroup|PlannedWrap|TakeoverWrap)$$|^TestPlannedDrainReorderedPrivateInput$$' ./pkg/callharness/

test-full: ## Run race units and the full in-process race/timing matrix; requires dedicated Redis and ffmpeg
	$(MAKE) test-unit
	$(MAKE) test-harness
	$(MAKE) test-harness-timing

DEMO_FLAGS ?=

demo: ## Run media workers A and B on one UDP socket plus the browser echo page on http://localhost:9101 (?source=test for the test pattern; "Move call" hands the call over); DEMO_FLAGS="-media-ip=IP -media-port=N -http=ADDR -relay" (-relay: one worker behind a relay on the media address instead; no moves)
	$(GOCMD) run ./cmd/echo-demo $(DEMO_FLAGS)

help: ## Display this help screen
	@grep -h -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'

DIST_VERSION ?= 0.0.0-dev+$(COMMIT)
DIST_DIR ?= dist
.PHONY: dist
dist: ## Build release archives for Linux and macOS on amd64 and arm64
	./scripts/release/build-archives.sh "$(DIST_VERSION)" "$(DIST_DIR)"
