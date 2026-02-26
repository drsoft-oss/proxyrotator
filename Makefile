BINARY  := proxybench
VERSION ?= dev
LDFLAGS := -ldflags "-s -w -X github.com/drsoft-oss/proxyrotator/cmd.version=$(VERSION)"

.DEFAULT_GOAL := help

.PHONY: help build test lint clean release

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'

build: ## Build binary for current platform
	go build $(LDFLAGS) -o $(BINARY) .

test: ## Run all tests
	go test ./...

lint: ## Run golangci-lint (requires golangci-lint installed)
	golangci-lint run ./...

clean: ## Remove build artifacts
	rm -f $(BINARY)