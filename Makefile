.PHONY: build test lint clean setup dev generate proto proto-tools docs docker install coverage-report coverage-summary test-unit test-integration coverage-unit coverage help build-all build-linux-amd64 build-linux-arm64 build-darwin-amd64 build-darwin-arm64

# Tools and paths
GO ?= go
BIN_DIR ?= bin

# Project metadata
BINARY_NAME = rune
VERSION = $(shell git describe --tags --always --dirty 2>/dev/null || echo "unknown")
BUILD_TIME = $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
COMMIT = $(shell git rev-parse HEAD 2>/dev/null || echo "unknown")
LDFLAGS = -X github.com/runestack/rune/pkg/version.Version=$(VERSION) \
          -X github.com/runestack/rune/pkg/version.BuildTime=$(BUILD_TIME) \
          -X github.com/runestack/rune/pkg/version.Commit=$(COMMIT)

# Coverage files
UNIT_COVERAGE = coverage_unit.out

# Coverage threshold (default 60%, can be overridden via COVERAGE_THRESHOLD env var)
THRESHOLD ?= $(or $(COVERAGE_THRESHOLD),23)

# Default goal
.DEFAULT_GOAL := build

## Build binaries for current platform
build:
	@echo "Building $(BINARY_NAME) for current platform..."
	@echo "LDFLAGS: $(LDFLAGS)"
	@$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_NAME) ./cmd/rune
	@$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_NAME)d ./cmd/runed
	@echo "Build completed!"

## Build all platforms for release
build-all: build-linux-amd64 build-linux-arm64 build-darwin-amd64 build-darwin-arm64
	@echo "All platform builds completed!"

## Build for Linux AMD64
build-linux-amd64:
	@echo "Building $(BINARY_NAME) for Linux AMD64..."
	@mkdir -p $(BIN_DIR)/linux_amd64
	@GOOS=linux GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/linux_amd64/$(BINARY_NAME) ./cmd/rune
	@GOOS=linux GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/linux_amd64/$(BINARY_NAME)d ./cmd/runed
	@echo "Linux AMD64 build completed!"

## Build for Linux ARM64
build-linux-arm64:
	@echo "Building $(BINARY_NAME) for Linux ARM64..."
	@mkdir -p $(BIN_DIR)/linux_arm64
	@GOOS=linux GOARCH=arm64 $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/linux_arm64/$(BINARY_NAME) ./cmd/rune
	@GOOS=linux GOARCH=arm64 $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/linux_arm64/$(BINARY_NAME)d ./cmd/runed
	@echo "Linux ARM64 build completed!"

## Build for macOS AMD64
build-darwin-amd64:
	@echo "Building $(BINARY_NAME) for macOS AMD64..."
	@mkdir -p $(BIN_DIR)/darwin_amd64
	@GOOS=darwin GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/darwin_amd64/$(BINARY_NAME) ./cmd/rune
	@echo "macOS AMD64 build completed!"

## Build for macOS ARM64
build-darwin-arm64:
	@echo "Building $(BINARY_NAME) for macOS ARM64..."
	@mkdir -p $(BIN_DIR)/darwin_arm64
	@GOOS=darwin GOARCH=arm64 $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/darwin_arm64/$(BINARY_NAME) ./cmd/rune
	@echo "macOS ARM64 build completed!"

## Build for specific platform (usage: make build-platform GOOS=linux GOARCH=amd64)
build-platform:
	@echo "Building $(BINARY_NAME) for $(GOOS)/$(GOARCH)..."
	@mkdir -p $(BIN_DIR)/$(GOOS)_$(GOARCH)
	@GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(GOOS)_$(GOARCH)/$(BINARY_NAME) ./cmd/rune
	@if [ "$(GOOS)" = "linux" ]; then \
		GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(GOOS)_$(GOARCH)/$(BINARY_NAME)d ./cmd/runed; \
	fi
	@echo "$(GOOS)/$(GOARCH) build completed!"

## Install binaries to GOPATH/bin
install: build
	@echo "Installing $(BINARY_NAME)..."
	@install -m 755 $(BIN_DIR)/$(BINARY_NAME) $(GOPATH)/bin/
	@install -m 755 $(BIN_DIR)/$(BINARY_NAME)d $(GOPATH)/bin/
	@echo "Installation completed!"

## Run all tests
test: test-unit test-integration

## Run unit tests via script
test-unit:
	@bash scripts/run_unit_tests.sh
	@$(GO) test -coverprofile=$(UNIT_COVERAGE) ./... 

## Run integration tests via script (defaults to BadgerDB store)
test-integration:
	@bash scripts/run_integration_tests.sh

## Run integration tests with memory store (fast)
test-integration-memory:
	@echo "Running integration tests with memory store..."
	@cd test/integration/cmd && RUNE_TEST_STORE_TYPE=memory go test -v -tags=integration

## Run integration tests with BadgerDB store (real storage)
test-integration-badger:
	@echo "Running integration tests with BadgerDB store..."
	@cd test/integration/cmd && RUNE_TEST_STORE_TYPE=badger go test -v -tags=integration

## Run integration tests with specific storage type
test-integration-store:
	@echo "Running integration tests with $(STORE) store..."
	@cd test/integration/cmd && RUNE_TEST_STORE_TYPE=$(STORE) go test -v -tags=integration

## Run integration tests in Docker (GitHub Actions style)
test-integration-docker:
	@echo "Running integration tests in Docker environment..."
	@docker-compose -f docker-compose.test.yml --profile test up --abort-on-container-exit --exit-code-from test-runner

## Run integration tests in Go container with Docker access
test-integration-docker-go:
	@echo "Running integration tests in Go container with Docker access..."
	@docker run --rm \
		-v $(PWD):/workspace \
		-v /var/run/docker.sock:/var/run/docker.sock \
		-e RUNE_TEST_STORE_TYPE=badger \
		-e RUNE_INTEGRATION_TESTS=1 \
		-e DOCKER_HOST=unix:///var/run/docker.sock \
		-w /workspace \
		golang:1.23-alpine \
		sh -c "apk add --no-cache git bash && go build -o bin/rune-test ./cmd/rune-test && bash scripts/integration/run_tests.sh"

## Run end-to-end tests (real CLI + test server)
test-e2e:
	@bash scripts/run_e2e_tests.sh

## Open unit test coverage report
coverage-unit:
	@$(GO) tool cover -html=$(UNIT_COVERAGE) -o $(UNIT_COVERAGE).html && open $(UNIT_COVERAGE).html

## Open coverage report (unit only)
coverage-report:
	@if [ -f $(UNIT_COVERAGE) ]; then $(GO) tool cover -html=$(UNIT_COVERAGE) -o unit_coverage.html && echo "Opened unit coverage report."; else echo "No unit coverage file found."; fi

## Show coverage summary (unit only)
coverage-summary:
	@if [ -f $(UNIT_COVERAGE) ]; then \
		echo "─ Unit Test Coverage ─"; \
		$(GO) tool cover -func=$(UNIT_COVERAGE) | grep total; \
	else \
		echo "No unit coverage file found: $(UNIT_COVERAGE)"; \
	fi

## Check coverage against threshold (unit only)
check-coverage:
	@echo "Checking unit test coverage threshold: $(THRESHOLD)%"
	@if [ -f $(UNIT_COVERAGE) ]; then \
		echo "Checking unit test coverage..."; \
		unit_coverage=$$(go tool cover -func=$(UNIT_COVERAGE) | grep total: | awk '{print $$3}' | sed 's/%//'); \
		echo "Unit test coverage: $$unit_coverage%"; \
		if [ $$(echo "$$unit_coverage >= $(THRESHOLD)" | bc -l 2>/dev/null || echo "$$unit_coverage >= $(THRESHOLD)" | awk '{print $$1 >= $$3}') -eq 1 ]; then \
			echo "✅ Unit coverage ($$unit_coverage%) meets threshold ($(THRESHOLD)%)"; \
		else \
			echo "❌ Unit coverage ($$unit_coverage%) below threshold ($(THRESHOLD)%)"; \
			exit 1; \
		fi; \
	else \
		echo "❌ No unit coverage file found: $(UNIT_COVERAGE)"; \
		exit 1; \
	fi

## Lint the project
lint:
	@echo "Running linters..."
	@golangci-lint run ./...

## Clean build and coverage artifacts
clean:
	@echo "Cleaning..."
	@rm -rf $(BIN_DIR) *.out *.html
	@echo "Clean completed!"

## Setup development environment
setup:
	@echo "Setting up development tools..."
	@$(GO) mod tidy
	@$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	@$(GO) install github.com/golang/mock/mockgen@latest
	@$(GO) install golang.org/x/tools/cmd/godoc@latest
	@echo "Setup completed!"

## Run code generation
generate:
	@echo "Running go generate..."
	@$(GO) generate ./...

## Generate protobuf files
proto:
	@bash scripts/generate-proto.sh

## Install protobuf tools
proto-tools:
	@bash scripts/install-proto-tools.sh

## Run documentation server
docs:
	@godoc -http=:6060 &
	@echo "Docs available at http://localhost:6060"

## Build docker image
docker:
	@docker build -t razorbill/$(BINARY_NAME):$(VERSION) .

## Show help
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Build:"
	@echo "  build             Build binaries for current platform"
	@echo "  build-all         Build for all platforms (Linux AMD64/ARM64, macOS AMD64/ARM64)"
	@echo "  build-linux-amd64 Build for Linux AMD64"
	@echo "  build-linux-arm64 Build for Linux ARM64"
	@echo "  build-darwin-amd64 Build for macOS AMD64"
	@echo "  build-darwin-arm64 Build for macOS ARM64"
	@echo "  build-platform    Build for specific platform (usage: make build-platform GOOS=linux GOARCH=amd64)"
	@echo "  install           Build and install to GOPATH/bin"
	@echo ""
	@echo "Testing:"
	@echo "  test              Run all tests"
	@echo "  test-unit         Run unit tests"
	@echo "  test-integration  Run integration tests (BadgerDB store by default)"
	@echo "  test-integration-memory  Run integration tests with memory store (fast)"
	@echo "  test-integration-badger  Run integration tests with BadgerDB store (real storage)"
	@echo "  test-integration-store STORE=<type>  Run integration tests with specific store type"
	@echo "  test-integration-docker              Run integration tests in Docker environment"
	@echo "  test-integration-docker-go           Run integration tests in Go container with Docker access"
	@echo "  test-e2e          Run end-to-end tests (real CLI + test server)"
	@echo "  coverage-report   Open unit coverage report"
	@echo "  coverage-summary  Show unit coverage summary"
	@echo "  check-coverage    Check unit coverage against threshold ($(THRESHOLD)%)"
	@echo ""
	@echo "Dev Tools:"
	@echo "  lint              Run linters"
	@echo "  clean             Clean all artifacts"
	@echo "  setup             Install dev tools"
	@echo ""
	@echo "Protobuf:"
	@echo "  proto             Generate Protobuf code"
	@echo "  proto-tools       Install Protobuf tools"
	@echo ""
	@echo "Docs & Docker:"
	@echo "  docs              Serve documentation"
	@echo "  docker            Build Docker image"
