# Mini Fleet Tracker — backend Makefile
# Cross-platform: assumes a POSIX shell (Linux, macOS, or WSL/Git Bash on Windows).
# Tested on GNU Make 3.81+.

SHELL          := /bin/sh
GO             ?= go
BIN_DIR        ?= bin
APP_NAME       ?= api
DOCKER_IMAGE   ?= mini-fleet-tracker-backend
DOCKER_TAG     ?= dev
GIT_COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS        := -s -w -X main.gitCommit=$(GIT_COMMIT)

.DEFAULT_GOAL := help
.PHONY: help dev build test lint docker-build clean tidy

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Available targets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  %-14s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

dev: ## Run the API locally with race detector (reads .env via Viper later)
	$(GO) run ./cmd/api

build: ## Build a statically linked binary into ./bin/api
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/$(APP_NAME) ./cmd/api

test: ## Run all tests with race detector and coverage
	$(GO) test -race -cover ./...

lint: ## Run golangci-lint (install hint printed if missing)
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not found on PATH."; \
		echo "Install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; \
		exit 1; \
	fi

docker-build: ## Build the production Docker image (stamps GIT_COMMIT)
	docker build \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		-t $(DOCKER_IMAGE):$(DOCKER_TAG) \
		.

tidy: ## go mod tidy
	$(GO) mod tidy

clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)
