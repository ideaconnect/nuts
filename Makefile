.PHONY: all build test test-unit test-performance test-functional test-functional-dev test-functional-stress test-functional-matrix release-check docker-up wait-functional-stack docker-down docker-logs mutate mutate-pkg mutate-tools clean

DOCKER_COMPOSE := $(shell if docker compose version >/dev/null 2>&1; then echo docker compose; elif docker-compose version >/dev/null 2>&1; then echo docker-compose; fi)
FUNCTIONAL_TEST_STRESS_COUNT ?= 3
NATS_COMPAT_IMAGES ?= nats:2.9-alpine nats:2.12-alpine nats:2.14-alpine
GORELEASER_IMAGE ?= goreleaser/goreleaser:v2.8.2
GOLANGCI_LINT_IMAGE ?= golangci/golangci-lint:v2.11.4
GREMLINS_VERSION ?= v0.6.0
MUTATION_OUTPUT_DIR ?= docs/mutation/runs

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

# Run performance confidence tests and hot-path benchmarks.
test-performance:
	go test -run '^TestPerformance_' -timeout 180s .
	go test -run '^$$' -bench 'Benchmark(FormatMessageEvent|TryParseJSON|IsValidTopic|CommonSubjectFilter|MultiTopicRequestedMessageHandler)' -benchmem .

# Validate GoReleaser config without requiring a local GoReleaser install.
release-check:
	docker run --rm -v "$(CURDIR):/workspace" -w /workspace $(GORELEASER_IMAGE) check

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
		$(MAKE) wait-functional-stack; \
	fi

# Wait for the functional test stack when Docker Compose lacks --wait.
wait-functional-stack:
	@echo "Waiting for functional test stack health..."
	@attempt=1; \
	while [ $$attempt -le 60 ]; do \
		health=$$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}missing{{end}}' nuts-server 2>/dev/null || true); \
		if [ "$$health" = "healthy" ]; then \
			echo "Functional test stack is healthy"; \
			exit 0; \
		fi; \
		printf 'Waiting for nuts-server health (attempt %s/60, status=%s)\n' "$$attempt" "$$health"; \
		attempt=$$((attempt + 1)); \
		sleep 2; \
	done; \
	echo "Functional test stack did not become healthy"; \
	$(MAKE) docker-logs; \
	exit 1

# Stop Docker services
docker-down:
	@if [ -z "$(DOCKER_COMPOSE)" ]; then \
		echo "Docker Compose is not available. Nothing to stop."; \
	else \
		$(DOCKER_COMPOSE) down -v; \
	fi

# Print service logs for functional test diagnosis.
docker-logs:
	@if [ -z "$(DOCKER_COMPOSE)" ]; then \
		echo "Docker Compose is not available. No logs to print."; \
	else \
		$(DOCKER_COMPOSE) logs --no-color --timestamps nats nats-init nuts || true; \
	fi

# Run functional tests (requires Docker services).
# FUNCTIONAL_TEST_RACE=1 appends -race to the go test invocation so a
# CI job (or local run) can exercise the broker-timing-dependent
# concurrency paths under the race detector. Disabled by default
# because -race roughly doubles wall-clock and the unit suite already
# runs -race on every PR.
test-functional: docker-up
	@echo "Running functional tests..."
	@status=0; \
	race_flag=""; \
	if [ "$(FUNCTIONAL_TEST_RACE)" = "1" ]; then race_flag="-race"; echo "  (race detector enabled via FUNCTIONAL_TEST_RACE=1)"; fi; \
	(cd functional_test && go test $$race_flag -count=1 -v -timeout 240s ./...) || status=$$?; \
	if [ $$status -ne 0 ]; then $(MAKE) docker-logs; fi; \
	$(MAKE) docker-down || status=$$?; \
	exit $$status

# Run functional tests without stopping Docker (for development).
# Honours FUNCTIONAL_TEST_RACE=1 just like the production target.
test-functional-dev:
	@race_flag=""; \
	if [ "$(FUNCTIONAL_TEST_RACE)" = "1" ]; then race_flag="-race"; fi; \
	cd functional_test && go test $$race_flag -count=1 -v -timeout 240s ./...

# Run the functional suite repeatedly to smoke out flakes.
test-functional-stress: docker-up
	@echo "Running functional stress tests ($(FUNCTIONAL_TEST_STRESS_COUNT) passes)..."
	@status=0; \
	attempt=1; \
	while [ $$attempt -le $(FUNCTIONAL_TEST_STRESS_COUNT) ]; do \
		echo "Functional stress pass $$attempt/$(FUNCTIONAL_TEST_STRESS_COUNT)"; \
		(cd functional_test && go test -count=1 -v -timeout 120s ./...) || { status=$$?; break; }; \
		attempt=$$((attempt + 1)); \
	done; \
	if [ $$status -ne 0 ]; then $(MAKE) docker-logs; fi; \
	$(MAKE) docker-down || status=$$?; \
	exit $$status

# Run functional tests against old and current NATS server images.
test-functional-matrix:
	@echo "Running functional tests across NATS images: $(NATS_COMPAT_IMAGES)"
	@status=0; \
	for image in $(NATS_COMPAT_IMAGES); do \
		echo "Functional matrix image: $$image"; \
		NATS_IMAGE=$$image $(MAKE) test-functional || { status=$$?; break; }; \
	done; \
	exit $$status

# Install the gremlins mutation testing binary into $GOPATH/bin (or $GOBIN).
# Pinned to GREMLINS_VERSION so contributors and CI agree on the same mutator
# set and behaviour; bump deliberately and document the diff in CHANGELOG.
mutate-tools:
	go install github.com/go-gremlins/gremlins/cmd/gremlins@$(GREMLINS_VERSION)

# Full-module mutation testing run. Brings up the Docker NATS stack because
# gremlins runs `go test ./...` for coverage (hard-coded in gremlins), which
# transitively executes the godog suite under functional_test/.
# Output goes to $(MUTATION_OUTPUT_DIR)/run-<UTC-timestamp>.json.
mutate: docker-up
	@command -v gremlins >/dev/null 2>&1 || { echo "gremlins not installed. Run 'make mutate-tools' first."; $(MAKE) docker-down; exit 1; }
	@mkdir -p $(MUTATION_OUTPUT_DIR)
	@status=0; \
	stamp=$$(date -u +%Y%m%dT%H%M%SZ); \
	output=$(MUTATION_OUTPUT_DIR)/run-$$stamp.json; \
	echo "Running mutation testing on github.com/ideaconnect/nuts → $$output"; \
	gremlins unleash --output $$output . || status=$$?; \
	if [ $$status -ne 0 ]; then $(MAKE) docker-logs; fi; \
	$(MAKE) docker-down || status=$$?; \
	exit $$status

# Scoped mutation run. PKG accepts anything gremlins accepts as a path:
# a Go file (`make mutate-pkg PKG=auth.go`) or a directory.
mutate-pkg: docker-up
	@if [ -z "$(PKG)" ]; then echo "Error: PKG is required, e.g. \`make mutate-pkg PKG=auth.go\`"; $(MAKE) docker-down; exit 2; fi
	@command -v gremlins >/dev/null 2>&1 || { echo "gremlins not installed. Run 'make mutate-tools' first."; $(MAKE) docker-down; exit 1; }
	@mkdir -p $(MUTATION_OUTPUT_DIR)
	@status=0; \
	stamp=$$(date -u +%Y%m%dT%H%M%SZ); \
	scope=$$(echo "$(PKG)" | tr '/.' '__'); \
	output=$(MUTATION_OUTPUT_DIR)/run-$$scope-$$stamp.json; \
	echo "Running mutation testing on $(PKG) → $$output"; \
	gremlins unleash --output $$output $(PKG) || status=$$?; \
	if [ $$status -ne 0 ]; then $(MAKE) docker-logs; fi; \
	$(MAKE) docker-down || status=$$?; \
	exit $$status

# Run Godog with pretty output
godog: docker-up
	@status=0; \
	(cd functional_test && go test -count=1 -v -godog.format=pretty) || status=$$?; \
	if [ $$status -ne 0 ]; then $(MAKE) docker-logs; fi; \
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

# Lint code using the same major/minor tool version as CI.
lint:
	docker run --rm -v "$(CURDIR):/app" -w /app $(GOLANGCI_LINT_IMAGE) golangci-lint run

# Show help
help:
	@echo "Available targets:"
	@echo "  build            - Build the Caddy binary"
	@echo "  test             - Run all tests (unit + functional)"
	@echo "  test-unit        - Run unit tests with embedded NATS"
	@echo "  test-performance - Run performance confidence tests and benchmarks"
	@echo "  test-functional  - Run functional/BDD tests with Docker"
	@echo "  test-functional-stress - Run functional/BDD tests repeatedly with Docker"
	@echo "  test-functional-matrix - Run functional/BDD tests against old and current NATS images"
	@echo "  release-check    - Validate GoReleaser config in a container"
	@echo "  docker-up        - Start Docker services"
	@echo "  docker-down      - Stop Docker services"
	@echo "  docker-logs      - Print functional test Docker service logs"
	@echo "  godog            - Run Godog with pretty output"
	@echo "  deps             - Install dependencies"
	@echo "  clean            - Clean build artifacts"
	@echo "  fmt              - Format code"
	@echo "  lint             - Run linter"
	@echo "  mutate-tools     - Install gremlins (mutation testing binary)"
	@echo "  mutate           - Run gremlins on the whole module (Docker NATS up/down)"
	@echo "  mutate-pkg PKG=… - Run gremlins on a single file/dir (e.g. PKG=auth.go)"
