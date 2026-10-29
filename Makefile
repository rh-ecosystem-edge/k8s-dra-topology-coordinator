# Makefile for Node Partition Topology Coordinator

.PHONY: build test test-coverage lint setup-envtest e2e clean deps fmt vet dev

# Build variables
BINARY_DIR=bin
BINARY=$(BINARY_DIR)/nodepartition-controller

# Go variables
GO=go
GOARCH=$(shell go env GOARCH)

# Build the controller
build: $(BINARY_DIR)
	CGO_ENABLED=0 $(GO) build -ldflags="-s -w -extldflags=-static" -trimpath -o $(BINARY) ./cmd/nodepartition-controller
	CGO_ENABLED=0 $(GO) build -ldflags="-s -w -extldflags=-static" -trimpath -o $(BINARY_DIR)/nodepartitionctl ./cmd/nodepartitionctl

# Create binary directory
$(BINARY_DIR):
	mkdir -p $(BINARY_DIR)

# Run tests (integration tests require envtest binaries)
test:
	KUBEBUILDER_ASSETS=$$($(GO) run sigs.k8s.io/controller-runtime/tools/setup-envtest use -p path --arch $(GOARCH) 1.34) $(GO) test -v ./...

# Run tests with coverage
test-coverage:
	KUBEBUILDER_ASSETS=$$($(GO) run sigs.k8s.io/controller-runtime/tools/setup-envtest use -p path --arch $(GOARCH) 1.34) $(GO) test -v -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html

# Run linting
lint:
	golangci-lint run

# Setup envtest binaries
setup-envtest:
	$(GO) run sigs.k8s.io/controller-runtime/tools/setup-envtest use -p path --arch $(GOARCH) 1.34

# Run live E2E profiles against an explicitly disposable Kubernetes cluster.
e2e:
	./test/e2e/run-e2e.sh

# Clean build artifacts
clean:
	rm -rf $(BINARY_DIR)
	rm -f coverage.out coverage.html

# Install dependencies
deps:
	$(GO) mod download
	$(GO) mod tidy

# Format code
fmt:
	$(GO) fmt ./...

# Vet code
vet:
	$(GO) vet ./...

# All-in-one development target
dev: deps fmt vet lint test build
