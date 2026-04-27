.PHONY: all build test test-unit test-functional docker-up docker-down clean

DOCKER_COMPOSE := $(shell if docker compose version >/dev/null 2>&1; then echo docker compose; elif docker-compose version >/dev/null 2>&1; then echo docker-compose; fi)

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
	@if [ -z "$(DOCKER_COMPOSE)" ]; then \
		echo "Docker Compose is not available. Install Docker Compose or enable Docker integration for this environment."; \
		exit 1; \
	fi
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
	@if [ -z "$(DOCKER_COMPOSE)" ]; then \
		echo "Docker Compose is not available. Nothing to stop."; \
	else \
		$(DOCKER_COMPOSE) down -v; \
	fi

# Run functional tests (requires Docker services)
test-functional: docker-up
	@echo "Running functional tests..."
	@status=0; \
	(cd functional_test && go test -v -timeout 120s ./...) || status=$$?; \
	$(MAKE) docker-down || status=$$?; \
	exit $$status

# Run functional tests without stopping Docker (for development)
test-functional-dev:
	cd functional_test && go test -v -timeout 120s ./...

# Run Godog with pretty output
godog: docker-up
	@status=0; \
	(cd functional_test && go test -v -godog.format=pretty) || status=$$?; \
	$(MAKE) docker-down || status=$$?; \
	exit $$status

# Install dependencies
deps:
	go mod download
	go install github.com/cucumber/godog/cmd/godog@v0.15.1

# Clean build artifacts
clean:
	rm -f ./caddy
	@if [ -n "$(DOCKER_COMPOSE)" ]; then \
		$(DOCKER_COMPOSE) down -v 2>/dev/null || true; \
	fi

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
