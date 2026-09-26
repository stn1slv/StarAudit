# Makefile for StarAudit project

BINARY_NAME=staraudit
DOCKER_IMAGE=stn1slv/staraudit

.PHONY: all
all: build test lint

.PHONY: setup
setup:
	@echo "Setting up development environment..."
	go mod download
	go mod tidy

.PHONY: test
test:
	@echo "Running tests..."
	go test -race -v ./...

.PHONY: lint
lint:
	@echo "Running linter..."
	@if command -v golangci-lint > /dev/null; then \
		golangci-lint run; \
	else \
		echo "golangci-lint is not installed. Skipping..."; \
	fi

.PHONY: format
format:
	@echo "Formatting code..."
	@if command -v gofumpt > /dev/null; then \
		gofumpt -extra -w .; \
	else \
		go fmt ./...; \
	fi

.PHONY: build
build:
	@echo "Building binary..."
	go build -o $(BINARY_NAME) main.go

.PHONY: docker
# The image copies a prebuilt binary. It must be static, because the base is scratch.
docker:
	@echo "Building docker image..."
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dist/staraudit-linux-amd64
	docker build -t $(DOCKER_IMAGE) .

.PHONY: run
run: build
	@echo "Running locally..."
	./$(BINARY_NAME) $(REPO)

.PHONY: upgrade-deps
upgrade-deps:
	@echo "Upgrading dependencies..."
	go get -u ./...
	go mod tidy
