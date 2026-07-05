---
layout: docs
title: Contributing
description: "How to contribute to NUTS — development setup, testing, code style, and pull request guidelines"
permalink: /docs/contributing/
---

## We Welcome Contributions

NUTS is open source and we appreciate contributions of all kinds — bug reports, feature ideas, documentation improvements, and code.

## Getting Started

1. **Fork** the [NUTS repository](https://github.com/ideaconnect/nuts)
2. **Clone** your fork:

   ```bash
   git clone https://github.com/YOUR-USERNAME/nuts.git
   cd nuts
   ```

3. **Set up** the development environment:

   ```bash
   ./scripts/setup-dev.sh
   ```

   Or manually:

   ```bash
   make docker-up
   ```

## Running Tests

NUTS has two test layers:

### Unit Tests

Unit tests use an embedded NATS server — no external dependencies needed:

```bash
make test-unit
```

Or directly:

```bash
go test -v -timeout 120s .
```

### Functional / BDD Tests

Functional tests use [Godog](https://github.com/cucumber/godog) (Cucumber for Go) with Gherkin syntax. They require Docker services:

```bash
make test-functional
```

Or step by step:

```bash
docker compose up -d --build
sleep 5
cd functional_test && go test -v -timeout 120s ./...
docker compose down -v
```

The BDD scenarios are in `features/sse_streaming.feature`:

```gherkin
Feature: SSE Streaming with JetStream
  Scenario: Connect to SSE endpoint and receive messages
    Given I am connected to SSE endpoint "/events?topic=notifications"
    When I publish message '{"alert": "test"}' to subject "events.notifications"
    Then I should receive an SSE event with topic "notifications"
    And the event should have an ID
```

### All Tests

```bash
make test
```

This runs both unit and functional tests in order.

## Code Style

- Format with `go fmt ./...` before committing
- Follow standard Go conventions and idioms
- Keep the Caddy module API surface clean and consistent

## Submitting a Pull Request

1. Create a feature branch from `main`
2. Make your changes with clear, focused commits
3. Ensure all tests pass: `make test`
4. Push your branch and open a pull request against `main`
5. Describe what your PR does and why

## Reporting Issues

Found a bug or have a feature request? Please [open an issue](https://github.com/ideaconnect/nuts/issues) with:

- A clear title and description
- Steps to reproduce (for bugs)
- Your environment (Go version, OS, NATS version)
- Expected vs. actual behavior

## License

By contributing, you agree that your contributions will be licensed under the [BSD 4-Clause License](https://github.com/ideaconnect/nuts/blob/main/LICENSE).
