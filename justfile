# FDB Record Layer Go - Task Runner
# Just is a command runner (https://github.com/casey/just)
# Usage: just <task>
# List all tasks: just --list

# Default recipe runs help
default:
    @just --list

# Display this help
help:
    @echo "FDB Record Layer Go - Available Tasks"
    @echo ""
    @just --list

# ==============================================================================
# Setup & Installation
# ==============================================================================

# Install all development dependencies
install-deps:
    @echo "Installing Go dependencies..."
    go mod download
    @echo "Installing Ginkgo test runner..."
    go install github.com/onsi/ginkgo/v2/ginkgo@latest
    @echo "Installing golangci-lint..."
    curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(go env GOPATH)/bin
    @echo "Installing buf..."
    go install github.com/bufbuild/buf/cmd/buf@latest
    @echo "✅ All dependencies installed!"

# Install FoundationDB client libraries (Linux/macOS)
install-fdb:
    #!/usr/bin/env bash
    set -euo pipefail
    if [[ "$OSTYPE" == "linux-gnu"* ]]; then
        echo "Installing FoundationDB client for Linux..."
        wget -q https://github.com/apple/foundationdb/releases/download/7.3.43/foundationdb-clients_7.3.43-1_amd64.deb
        sudo dpkg -i foundationdb-clients_7.3.43-1_amd64.deb
        rm foundationdb-clients_7.3.43-1_amd64.deb
        echo "✅ FoundationDB client installed!"
    elif [[ "$OSTYPE" == "darwin"* ]]; then
        echo "Installing FoundationDB client for macOS..."
        brew install foundationdb
        echo "✅ FoundationDB client installed!"
    else
        echo "❌ Unsupported OS: $OSTYPE"
        exit 1
    fi

# ==============================================================================
# Code Generation
# ==============================================================================

# Generate all protobuf code
generate:
    @echo "Generating protobuf code..."
    buf generate
    @echo "✅ Protobuf generation complete!"

# Lint protobuf files
proto-lint:
    @echo "Linting protobuf files..."
    buf lint
    @echo "✅ Protobuf lint complete!"

# Format protobuf files
proto-format:
    @echo "Formatting protobuf files..."
    buf format -w
    @echo "✅ Protobuf format complete!"

# ==============================================================================
# Building
# ==============================================================================

# Build all packages
build:
    @echo "Building all packages..."
    go build -v ./...
    @echo "✅ Build complete!"

# Build with race detector
build-race:
    @echo "Building with race detector..."
    go build -race -v ./...
    @echo "✅ Race build complete!"

# Clean build artifacts
clean:
    @echo "Cleaning build artifacts..."
    go clean -cache -testcache -modcache
    rm -rf coverage.txt coverage.html
    @echo "✅ Clean complete!"

# ==============================================================================
# Testing
# ==============================================================================

# Run all tests
test:
    @echo "Running all tests..."
    go test -v -race ./...
    @echo "✅ All tests passed!"

# Run tests with coverage
test-coverage:
    @echo "Running tests with coverage..."
    go test -v -race -coverprofile=coverage.txt -covermode=atomic ./...
    go tool cover -html=coverage.txt -o coverage.html
    @echo "✅ Coverage report generated: coverage.html"

# Run only unit tests (pkg/...)
test-unit:
    @echo "Running unit tests..."
    go test -v -race -coverprofile=coverage.txt -covermode=atomic ./pkg/...
    @echo "✅ Unit tests passed!"

# Run only conformance tests
test-conformance:
    @echo "Running conformance tests..."
    cd conformance && ginkgo -v --timeout 10m
    @echo "✅ Conformance tests passed!"

# Run tests matching a pattern
test-match pattern:
    @echo "Running tests matching: {{pattern}}"
    go test -v -race -run {{pattern}} ./...

# Run a specific test file
test-file file:
    @echo "Running test file: {{file}}"
    go test -v -race {{file}}

# Run tests in watch mode (requires ginkgo)
test-watch:
    @echo "Running tests in watch mode..."
    ginkgo watch -r -v

# ==============================================================================
# Linting & Formatting
# ==============================================================================

# Run golangci-lint
lint:
    @echo "Running golangci-lint..."
    golangci-lint run ./...
    @echo "✅ Lint complete!"

# Run golangci-lint and auto-fix issues
lint-fix:
    @echo "Running golangci-lint with auto-fix..."
    golangci-lint run --fix ./...
    @echo "✅ Lint fix complete!"

# Format Go code
fmt:
    @echo "Formatting Go code..."
    go fmt ./...
    @echo "✅ Format complete!"

# Run goimports
imports:
    @echo "Running goimports..."
    goimports -w .
    @echo "✅ Imports organized!"

# Run all formatters
format: fmt imports proto-format
    @echo "✅ All formatting complete!"

# ==============================================================================
# Code Quality
# ==============================================================================

# Run static analysis
vet:
    @echo "Running go vet..."
    go vet ./...
    @echo "✅ Vet complete!"

# Check for security issues (requires gosec)
security:
    @echo "Running security scan..."
    gosec ./...
    @echo "✅ Security scan complete!"

# Run all quality checks
quality: vet lint proto-lint
    @echo "✅ All quality checks passed!"

# ==============================================================================
# CI/CD Simulation
# ==============================================================================

# Run all CI checks locally
ci: quality test-unit build
    @echo "✅ All CI checks passed!"

# Run full CI pipeline (including conformance)
ci-full: quality test-unit test-conformance build
    @echo "✅ Full CI pipeline passed!"

# Pre-commit checks (fast)
pre-commit: fmt lint-fix test-unit
    @echo "✅ Pre-commit checks passed!"

# ==============================================================================
# Development Helpers
# ==============================================================================

# Run Go mod tidy
tidy:
    @echo "Running go mod tidy..."
    go mod tidy
    @echo "✅ Go mod tidy complete!"

# Update all dependencies
update-deps:
    @echo "Updating dependencies..."
    go get -u ./...
    go mod tidy
    @echo "✅ Dependencies updated!"

# Check for outdated dependencies
check-updates:
    @echo "Checking for outdated dependencies..."
    go list -u -m all | grep '\['

# Verify dependencies
verify:
    @echo "Verifying dependencies..."
    go mod verify
    @echo "✅ Dependencies verified!"

# ==============================================================================
# Documentation
# ==============================================================================

# Generate Go documentation
docs:
    @echo "Starting documentation server..."
    @echo "Visit: http://localhost:6060/pkg/github.com/birdayz/fdb-record-layer-go/"
    godoc -http=:6060

# View coverage in browser
view-coverage: test-coverage
    @echo "Opening coverage report in browser..."
    open coverage.html || xdg-open coverage.html

# ==============================================================================
# Git Helpers
# ==============================================================================

# Show git status with nice formatting
status:
    @git status --short --branch

# Commit with conventional commit message
commit message: pre-commit
    @echo "Committing with message: {{message}}"
    git add -A
    git commit -m "{{message}}"

# Commit and push
push message: pre-commit
    @echo "Committing and pushing..."
    git add -A
    git commit -m "{{message}}"
    git push

# ==============================================================================
# Benchmarking
# ==============================================================================

# Run benchmarks
bench:
    @echo "Running benchmarks..."
    go test -bench=. -benchmem ./...

# Run benchmarks with CPU profiling
bench-cpu:
    @echo "Running benchmarks with CPU profiling..."
    go test -bench=. -benchmem -cpuprofile=cpu.prof ./...
    @echo "View profile with: go tool pprof cpu.prof"

# Run benchmarks with memory profiling
bench-mem:
    @echo "Running benchmarks with memory profiling..."
    go test -bench=. -benchmem -memprofile=mem.prof ./...
    @echo "View profile with: go tool pprof mem.prof"

# ==============================================================================
# Docker (for CI/testing)
# ==============================================================================

# Build Docker image for testing
docker-build:
    @echo "Building Docker image..."
    docker build -t fdb-record-layer-go:latest .

# Run tests in Docker
docker-test:
    @echo "Running tests in Docker..."
    docker run --rm fdb-record-layer-go:latest just test

# ==============================================================================
# Phase-specific Tasks
# ==============================================================================

# Run Phase 1 tests only
test-phase1:
    @echo "Running Phase 1 tests..."
    go test -v -race ./pkg/recordlayer -run "TestRecordExists|TestInsert|TestUpdate|TestDelete" ./...
    go test -v ./conformance -run "Isolation|Conflict|ExistenceCheck"

# Generate Phase 1 statistics
stats-phase1:
    @echo "Phase 1 Statistics:"
    @echo "=================="
    @wc -l pkg/recordlayer/*.go | tail -1 | awk '{print "Implementation lines:", $1}'
    @wc -l pkg/recordlayer/*_test.go conformance/*_test.go | tail -1 | awk '{print "Test lines:", $1}'
    @find conformance pkg/recordlayer -name "*_test.go" | wc -l | awk '{print "Test files:", $1}'

# ==============================================================================
# Troubleshooting
# ==============================================================================

# Show environment info
env-info:
    @echo "Environment Information:"
    @echo "======================="
    @echo "Go version: $(go version)"
    @echo "GOPATH: $(go env GOPATH)"
    @echo "GOOS: $(go env GOOS)"
    @echo "GOARCH: $(go env GOARCH)"
    @which fdbcli > /dev/null && echo "FoundationDB: $(fdbcli --version)" || echo "FoundationDB: not installed"
    @which buf > /dev/null && echo "Buf: $(buf --version)" || echo "Buf: not installed"
    @which golangci-lint > /dev/null && echo "golangci-lint: $(golangci-lint --version)" || echo "golangci-lint: not installed"

# Check if all tools are installed
check-tools:
    @echo "Checking required tools..."
    @which go > /dev/null && echo "✅ Go" || echo "❌ Go"
    @which buf > /dev/null && echo "✅ Buf" || echo "❌ Buf"
    @which golangci-lint > /dev/null && echo "✅ golangci-lint" || echo "❌ golangci-lint"
    @which ginkgo > /dev/null && echo "✅ Ginkgo" || echo "❌ Ginkgo"
    @which fdbcli > /dev/null && echo "✅ FoundationDB Client" || echo "❌ FoundationDB Client"
    @echo ""
    @echo "Install missing tools with: just install-deps"
