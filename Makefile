.PHONY: all build test test-unit test-functional docker-up docker-down clean

DOCKER_COMPOSE := $(shell if command -v docker-compose >/dev/null 2>&1; then echo docker-compose; else echo docker compose; fi)

# Default target
all: build test

# Build the Caddy binary with nuts module
build:
	go build -o ./caddy ./cmd/caddy

# Run all tests
test: test-unit test-functional

# Run unit tests (uses embedded NATS server)
test-unit:
	go test -v -timeout 120s .

# Start Docker services for functional tests
docker-up:
	@if $(DOCKER_COMPOSE) up --help 2>/dev/null | grep -q -- --wait; then \
		echo "Starting Docker services with readiness checks..."; \
		$(DOCKER_COMPOSE) up -d --build --wait; \
	else \
		echo "Starting Docker services without native readiness checks..."; \
		$(DOCKER_COMPOSE) up -d --build; \
		echo "Waiting for services to be ready..."; \
		sleep 10; \
	fi

# Stop Docker services
docker-down:
	$(DOCKER_COMPOSE) down -v

# Run functional tests (requires Docker services)
test-functional: docker-up
	@echo "Running functional tests..."
	cd functional_test && go test -v -timeout 120s ./...
	@$(MAKE) docker-down

# Run functional tests without stopping Docker (for development)
test-functional-dev:
	cd functional_test && go test -v -timeout 120s ./...

# Run Godog with pretty output
godog: docker-up
	cd functional_test && go test -v -godog.format=pretty
	@$(MAKE) docker-down

# Install dependencies
deps:
	go mod download
	go get github.com/cucumber/godog/cmd/godog@latest

# Clean build artifacts
clean:
	rm -f ./caddy
	$(DOCKER_COMPOSE) down -v 2>/dev/null || true

# Format code
fmt:
	go fmt ./...

# Lint code
lint:
	golangci-lint run

# Show help
help:
	@echo "Available targets:"
	@echo "  build            - Build the Caddy binary"
	@echo "  test             - Run all tests (unit + functional)"
	@echo "  test-unit        - Run unit tests with embedded NATS"
	@echo "  test-functional  - Run functional/BDD tests with Docker"
	@echo "  docker-up        - Start Docker services"
	@echo "  docker-down      - Stop Docker services"
	@echo "  godog            - Run Godog with pretty output"
	@echo "  deps             - Install dependencies"
	@echo "  clean            - Clean build artifacts"
	@echo "  fmt              - Format code"
	@echo "  lint             - Run linter"
