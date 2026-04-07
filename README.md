# 🥜 NUTS - NATS to SSE for Caddy

A Caddy Server module that bridges NATS.io JetStream messages to Server-Sent Events (SSE), inspired by [Mercure.rocks](https://mercure.rocks).

## Features

- **Real-time Updates**: Stream NATS messages to web browsers via SSE/EventSource
- **JetStream Persistence**: Messages are persisted in NATS JetStream for replay
- **Message Replay**: Clients can reconnect and replay messages from a specific ID using `?last-id=` or the standard `Last-Event-ID` header. If the requested sequence is no longer available in JetStream retention, NUTS falls back to replaying all retained messages for that topic.
- **Multiple Topics**: Subscribe to multiple NATS subjects simultaneously
- **Automatic Reconnection**: Built-in NATS reconnection handling
- **CORS Support**: Configurable cross-origin resource sharing
- **Heartbeat**: Keep-alive mechanism to prevent connection timeouts
- **No Message Drops For Connected Clients**: When a client falls behind, NUTS disconnects that SSE session before dropping queued messages so the client can resume from the last delivered event ID
- **Flexible Authentication**: Support for credentials file, token, or user/password auth
- **Topic Prefixing**: Optional prefix for all NATS subscriptions

## Installation

### Using xcaddy (Recommended)

```bash
xcaddy build --with github.com/ideaconnect/nuts
```

### Building from Source

```bash
# Clone the repository
git clone https://github.com/ideaconnect/nuts.git
cd nuts

# Build custom Caddy with the module
go build -o caddy ./cmd/caddy
```

## Testing

The repository has two test layers:

- `make test-unit` runs the self-contained Go tests in the root package using an embedded NATS server.
- `make test-functional` starts the Docker-backed NATS and Caddy test stack, runs the Godog functional suite, then tears the stack down.
- `make test` runs both layers in order.

If you prefer to inspect the functional environment manually, use `make docker-up`, run `cd functional_test && go test -v -timeout 120s ./...`, then clean up with `make docker-down`.

## Quick Start

1. **Start NATS server with JetStream enabled**:
   ```bash
   docker run -p 4222:4222 nats:latest -js
   ```

2. **Create a JetStream stream** (using NATS CLI):
   ```bash
   # Install NATS CLI: https://github.com/nats-io/natscli
   nats stream add EVENTS \
     --subjects "events.>" \
     --storage file \
     --retention limits \
     --max-msgs 10000 \
     --max-age 24h \
     --discard old
   ```

3. **Create a Caddyfile**:
   ```caddyfile
   :8080 {
       route /events* {
           nuts {
               nats_url nats://localhost:4222
               stream_name EVENTS
               topic_prefix events.
           }
       }
   }
   ```

4. **Run Caddy**:
   ```bash
   ./caddy run
   ```

5. **Connect from JavaScript**:
   ```javascript
   const events = new EventSource('/events?topic=my-topic');

   events.addEventListener('message', (e) => {
       const data = JSON.parse(e.data);
       console.log('Received:', data, 'ID:', e.lastEventId);
   });
   ```

6. **Publish a message** (using NATS CLI):
   ```bash
   nats pub events.my-topic '{"hello": "world"}'
   ```

## Configuration

### Caddyfile Syntax

```caddyfile
nuts {
    # NATS server URL (required)
    nats_url <url>

    # JetStream stream name (required)
    stream_name <name>

    # Authentication (choose exactly one; user/password must be set together)
    nats_credentials <path>      # Path to .creds file
    nats_token <token>           # Token auth
    nats_user <username>         # User/password auth
    nats_password <password>

    # Optional settings
    topic_prefix <prefix>        # Prefix for all subscriptions
    allowed_origins <origins...> # CORS origins (default: *)
    heartbeat_interval <seconds> # Heartbeat interval (default: 30)
    reconnect_wait <seconds>     # Reconnect wait time (default: 2)
    max_reconnects <count>       # Max reconnects, -1=infinite (default: -1)
    max_event_size <bytes>       # Max SSE event size in bytes (default: 1048576)
}
```

#### `max_event_size`

Limits the total size (in bytes) of a single SSE event frame — including the `id:`, `event:`, and `data:` lines plus the JSON-encoded payload. Any event that exceeds the limit is silently dropped and a warning is logged. The client never sees it.

For example, setting `max_event_size 1000` means that if a NATS message produces an SSE frame larger than 1000 bytes once formatted, that frame is discarded. A typical overhead (id, event type, topic, timestamp) is roughly 120-150 bytes, so a 1000-byte limit leaves ~850 bytes for the raw message payload. Set to `0` to disable the limit entirely.
```

### JSON Configuration

```json
{
    "handler": "nuts",
    "nats_url": "nats://localhost:4222",
    "stream_name": "EVENTS",
    "nats_credentials": "/path/to/creds.creds",
    "topic_prefix": "events.",
    "allowed_origins": ["https://example.com"],
    "heartbeat_interval": 30,
    "reconnect_wait": 2,
    "max_reconnects": -1,
    "max_event_size": 1048576
}
```

`max_event_size` counts the **entire formatted SSE frame**, not just the raw NATS payload. See the Caddyfile section above for details.
```

## JetStream Setup

NUTS requires a pre-configured JetStream stream. The stream must be created before starting Caddy.

### Creating a Stream

Using the NATS CLI:

```bash
# Basic stream for events
nats stream add EVENTS \
  --subjects "events.>" \
  --storage file \
  --retention limits \
  --max-msgs 10000 \
  --max-age 24h \
  --discard old

# Or interactively
nats stream add
```

### Stream Configuration Options

| Option | Recommended | Description |
|--------|-------------|-------------|
| `--subjects` | Match your `topic_prefix` + `>` | Subjects the stream captures |
| `--storage` | `file` | Use `file` for persistence, `memory` for speed |
| `--retention` | `limits` | How messages are retained |
| `--max-msgs` | `10000` | Maximum messages to keep |
| `--max-age` | `24h` | Maximum age of messages |
| `--discard` | `old` | Discard oldest messages when limit reached |

### Example Streams

**Chat application:**
```bash
nats stream add CHAT \
  --subjects "chat.>" \
  --storage file \
  --max-msgs-per-subject 1000 \
  --max-age 7d
```

**Metrics/Dashboard:**
```bash
nats stream add METRICS \
  --subjects "metrics.>" \
  --storage memory \
  --max-msgs 5000 \
  --max-age 1h
```

## Client Usage

### JavaScript EventSource

```javascript
// Subscribe to a single topic
const events = new EventSource('/events?topic=notifications');

// Subscribe to multiple topics
const events = new EventSource('/events?topic=notifications&topic=updates');

// Using path-based topic
const events = new EventSource('/events/my-topic');

// Replay messages from a specific ID (e.g., after reconnection)
const lastId = localStorage.getItem('lastEventId') || '';
const events = new EventSource(`/events?topic=notifications&last-id=${lastId}`);

// Handle connection
events.addEventListener('connected', (e) => {
    const { topics } = JSON.parse(e.data);
    console.log('Connected to:', topics);
});

// Handle messages and track last ID for replay
events.addEventListener('message', (e) => {
    const { topic, payload, time } = JSON.parse(e.data);
    console.log(`[${topic}] at ${time}:`, payload);

    // Store last event ID for reconnection replay
    if (e.lastEventId) {
        localStorage.setItem('lastEventId', e.lastEventId);
    }
});

// Handle errors and reconnect with replay
events.onerror = (e) => {
    console.error('SSE error:', e);
    // EventSource will auto-reconnect and send Last-Event-ID automatically.
    // Custom clients should reconnect with the most recent event ID.
};
```

### Slow Clients And Replay

NUTS does not intentionally drop messages for an active SSE client.

If a client cannot read fast enough and its per-connection queue fills, NUTS closes that SSE connection instead of discarding queued messages. A reconnecting client can then resume from the last delivered SSE `id` using either:

- The browser-managed `Last-Event-ID` header
- The explicit `?last-id=` query parameter for custom clients

This means the delivery policy is effectively:

- No silent per-client message loss in the live stream path
- Slow clients must reconnect to continue
- Replay depends on the requested sequence still being retained in JetStream

### Message Replay with `last-id` or `Last-Event-ID`

The `last-id` query parameter and standard `Last-Event-ID` header allow clients to replay messages from a specific point:

```javascript
// Get the last received message ID
const lastId = '12345';

// Reconnect and get all messages after that ID
const events = new EventSource(`/events?topic=updates&last-id=${lastId}`);
```

**Behavior:**
- Messages with sequence numbers greater than `last-id` will be delivered
- If the requested sequence no longer exists (expired/deleted), all available messages are delivered
- **Replay storm caveat**: When the fallback fires, *all* retained messages for that topic are replayed. If the stream holds a large backlog, this may deliver many messages the client has already seen. Design your stream retention policy (max age, max messages) accordingly.
- Without `last-id`, only new messages are delivered
- Standard `EventSource` reconnects can use the `Last-Event-ID` header automatically
- When a slow client is disconnected, reconnecting with the last delivered event ID resumes from that point instead of losing messages silently

### Message Format

Messages are sent as SSE events with the following format:

```
id: 12345
event: message
data: {"topic":"my-topic","payload":{"your":"data"},"time":"2024-01-01T12:00:00Z"}
```

The `id` field contains the JetStream sequence number, which can be used with `last-id` or `Last-Event-ID` for replay.

## Example Scenarios

### Chat Application

```caddyfile
:8080 {
    route /chat/* {
        nuts {
            nats_url nats://localhost:4222
            stream_name CHAT
            topic_prefix chat.
            allowed_origins https://chat.example.com
        }
    }
}
```

```bash
# Create the stream first
nats stream add CHAT --subjects "chat.>" --storage file --max-age 7d
```

```javascript
// Client subscribes to a room
const room = 'room-123';
const events = new EventSource(`/chat/messages?topic=${room}`);
```

### Real-time Dashboard

```caddyfile
:8080 {
    route /dashboard/events {
        nuts {
            nats_url nats://localhost:4222
            stream_name METRICS
            topic_prefix metrics.
            heartbeat_interval 15
        }
    }
}
```

```bash
# Create the stream first
nats stream add METRICS --subjects "metrics.>" --storage memory --max-age 1h
```

### With NATS Authentication

```caddyfile
:8080 {
    route /secure/events {
        nuts {
            nats_url nats://nats.example.com:4222
            stream_name EVENTS
            nats_credentials /etc/nats/user.creds
        }
    }
}
```

## Comparison with Mercure

| Feature | NUTS | Mercure |
|---------|------|---------|
| Backend | NATS.io JetStream | Custom Hub |
| Protocol | SSE | SSE |
| Persistence | JetStream streams | Built-in |
| Message Replay | Yes (`last-id` param and `Last-Event-ID` header) | Yes (`Last-Event-ID` header) |
| Authorization | NATS auth | JWT |
| Clustering | NATS clustering | Mercure clustering |

## Development

### Prerequisites

- Go 1.25+
- Docker (for running NATS server)
- [NATS CLI](https://github.com/nats-io/natscli) (optional, for manual testing)

### Quick Setup

```bash
# Start NATS server with JetStream and create test stream
./scripts/setup-dev.sh

# Or manually with Docker Compose
make docker-up
```

### Running Tests

#### Unit Tests

Unit tests use an embedded NATS server, so no external dependencies are required:

```bash
# Run unit tests
go test -v -timeout 120s .

# Run specific test
go test -v -run TestHandler_ServeHTTP_Integration .
```

#### Functional/BDD Tests

Functional tests use [Godog](https://github.com/cucumber/godog) (Cucumber for Go) with Gherkin syntax.
They require Docker services to be running:

```bash
# Using Make (recommended)
make test-functional

# Or step by step:
docker compose up -d --build
sleep 5  # Wait for services
cd functional_test && go test -v -timeout 120s ./...
docker compose down -v
```

The BDD tests are defined in `features/sse_streaming.feature` using Gherkin syntax:

```gherkin
Feature: SSE Streaming with JetStream
  Scenario: Connect to SSE endpoint and receive messages
    Given I am connected to SSE endpoint "/events?topic=notifications"
    When I publish message '{"alert": "test"}' to subject "events.notifications"
    Then I should receive an SSE event with topic "notifications"
    And the event should have an ID
```

#### All Tests

```bash
# Run both unit and functional tests
make test
```

### Building

```bash
# Build custom Caddy with the module
go build -o caddy ./cmd/caddy

# Build with race detector (requires CGO)
CGO_ENABLED=1 go build -race -o caddy ./cmd/caddy

# Format code
go fmt ./...
```

### Docker Compose

The project includes a `docker-compose.yml` for running the full stack (NATS + nuts server):

```bash
# Start all services
docker compose up -d --build

# View logs
docker compose logs -f

# Stop and clean up
docker compose down -v
```

### Makefile Commands

```bash
make build           # Build the Caddy binary
make test            # Run all tests (unit + functional)
make test-unit       # Run unit tests with embedded NATS
make test-functional # Run BDD tests with Docker
make docker-up       # Start Docker services
make docker-down     # Stop Docker services
make clean           # Clean build artifacts
make help            # Show all available commands
```

## License

BSD 4-Clause License - see [LICENSE](LICENSE) file for details.

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.
