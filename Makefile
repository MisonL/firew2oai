.POSIX:
BINARY_NAME=firew2oai
BUILD_DIR=bin
VERSION?=1.0.0
LDFLAGS=-ldflags="-s -w -X main.Version=$(VERSION)"
GO=go

.PHONY: build run clean docker-build docker-up docker-down test lint build-all help

## build: Build for current platform
build:
	$(GO) build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/server/

## run: Build and run
run: build
	./$(BUILD_DIR)/$(BINARY_NAME)

## clean: Remove build artifacts
clean:
	rm -rf $(BUILD_DIR)

## test: Run tests
test:
	$(GO) test -v -race ./...

## lint: Run linter
lint:
	golangci-lint run ./...

## docker-build: Build Docker image
docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(BINARY_NAME):$(VERSION) -t $(BINARY_NAME):latest .

## docker-up: Start with docker compose
docker-up:
	docker compose up -d --build

## docker-down: Stop docker compose
docker-down:
	docker compose down

## build-all: Cross-compile for all platforms
build-all:
	@echo "==> Building for linux/amd64..."
	GOOS=linux   GOARCH=amd64 $(GO) build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/server/
	@echo "==> Building for linux/arm64..."
	GOOS=linux   GOARCH=arm64 $(GO) build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 ./cmd/server/
	@echo "==> Building for darwin/amd64..."
	GOOS=darwin  GOARCH=amd64 $(GO) build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 ./cmd/server/
	@echo "==> Building for darwin/arm64..."
	GOOS=darwin  GOARCH=arm64 $(GO) build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 ./cmd/server/
	@echo "==> Building for windows/amd64..."
	GOOS=windows GOARCH=amd64 $(GO) build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe ./cmd/server/
	@echo "==> All builds complete."

## docker-buildx: Multi-platform Docker build (requires buildx)
docker-buildx:
	docker buildx build --platform linux/amd64,linux/arm64 \
		-t $(BINARY_NAME):$(VERSION) \
		-t $(BINARY_NAME):latest \
		--push .

## help: Show this help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sort | sed 's/## //'

.DEFAULT_GOAL := help
