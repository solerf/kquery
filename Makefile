.PHONY: build install clean test vet fmt deps smoke all help

BINARY_NAME=kquery
BUILD_DIR=bin
INSTALL_DIR=$(GOPATH)/bin

help: ## Show this help message
	@echo 'Usage: make [target]'
	@echo ''
	@echo 'Available targets:'
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the binary
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	@go build -o $(BUILD_DIR)/$(BINARY_NAME) .
	@echo "✓ Binary built: $(BUILD_DIR)/$(BINARY_NAME)"

install: build ## Install to $GOPATH/bin
	@echo "Installing to $(INSTALL_DIR)..."
	@cp $(BUILD_DIR)/$(BINARY_NAME) $(INSTALL_DIR)/
	@echo "✓ Installed: $(INSTALL_DIR)/$(BINARY_NAME)"

clean: ## Remove build artifacts
	@echo "Cleaning..."
	@rm -rf $(BUILD_DIR)
	@echo "✓ Clean complete"

test: ## Run unit tests
	@echo "Running tests..."
	@go test ./...
	@echo "✓ Tests passed"

smoke: build ## Build and run --help
	@$(BUILD_DIR)/$(BINARY_NAME) --help
	@echo "✓ Smoke test passed"

deps: ## Download dependencies
	@echo "Downloading dependencies..."
	@go mod download
	@echo "✓ Dependencies downloaded"

fmt: ## Format code
	@echo "Formatting code..."
	@go fmt ./...
	@echo "✓ Code formatted"

vet: ## Run go vet
	@echo "Running go vet..."
	@go vet ./...
	@echo "✓ Vet passed"

all: clean deps fmt vet test build ## Clean, download deps, format, vet, test, and build

.DEFAULT_GOAL := help
