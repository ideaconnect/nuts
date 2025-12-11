# 🥜 NUTS - NATS to SSE for Caddy

A Caddy Server module that bridges NATS.io Pub/Sub messages to Server-Sent Events (SSE), similar to [Mercure.rocks](https://mercure.rocks).

## Features

- **Real-time Updates**: Stream NATS messages to web browsers via SSE/EventSource
- **Multiple Topics**: Subscribe to multiple NATS subjects simultaneously
- **Automatic Reconnection**: Built-in NATS reconnection handling
- **CORS Support**: Configurable cross-origin resource sharing
- **Heartbeat**: Keep-alive mechanism to prevent connection timeouts
- **Flexible Authentication**: Support for credentials file, token, or user/password auth
- **Topic Prefixing**: Optional prefix for all NATS subscriptions

## Installation

### Using xcaddy (Recommended)

```bash
xcaddy build --with github.com/idct/nuts
```

### Building from Source

```bash
# Clone the repository
git clone https://github.com/idct/nuts.git
cd nuts

# Build custom Caddy with the module
go build -o caddy ./cmd/caddy
```

## Quick Start

1. **Start NATS server** (if not already running):
   ```bash
   docker run -p 4222:4222 nats:latest
   ```

2. **Create a Caddyfile**:
   ```caddyfile
   :8080 {
       route /events* {
           nuts {
               nats_url nats://localhost:4222
           }
       }
   }
   ```

3. **Run Caddy**:
   ```bash
   ./caddy run
   ```

4. **Connect from JavaScript**:
   ```javascript
   const events = new EventSource('/events?topic=my-topic');
   
   events.addEventListener('message', (e) => {
       const data = JSON.parse(e.data);
       console.log('Received:', data);
   });
   ```

5. **Publish a message** (using NATS CLI):
   ```bash
   nats pub my-topic '{"hello": "world"}'
   ```

## Configuration

### Caddyfile Syntax

```caddyfile
nuts {
    # NATS server URL (default: nats://localhost:4222)
    nats_url <url>
    
    # Authentication (choose one)
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
}
```

### JSON Configuration

```json
{
    "handler": "nuts",
    "nats_url": "nats://localhost:4222",
    "nats_credentials": "/path/to/creds.creds",
    "topic_prefix": "app.events.",
    "allowed_origins": ["https://example.com"],
    "heartbeat_interval": 30,
    "reconnect_wait": 2,
    "max_reconnects": -1
}
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

// Handle connection
events.addEventListener('connected', (e) => {
    const { topics } = JSON.parse(e.data);
    console.log('Connected to:', topics);
});

// Handle messages
events.addEventListener('message', (e) => {
    const { topic, payload, time } = JSON.parse(e.data);
    console.log(`[${topic}] at ${time}:`, payload);
});

// Handle errors
events.onerror = (e) => {
    console.error('SSE error:', e);
};
```

### Message Format

Messages are sent as SSE events with the following JSON structure:

```json
{
    "topic": "my-topic",
    "payload": { "your": "data" },
    "time": "2024-01-01T12:00:00Z"
}
```

## Example Scenarios

### Chat Application

```caddyfile
:8080 {
    route /chat/* {
        nuts {
            nats_url nats://localhost:4222
            topic_prefix chat.
            allowed_origins https://chat.example.com
        }
    }
}
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
            topic_prefix metrics.
            heartbeat_interval 15
        }
    }
}
```

### With NATS Authentication

```caddyfile
:8080 {
    route /secure/events {
        nuts {
            nats_url nats://nats.example.com:4222
            nats_credentials /etc/nats/user.creds
        }
    }
}
```

## Comparison with Mercure

| Feature | NUTS | Mercure |
|---------|------|---------|
| Backend | NATS.io | Custom Hub |
| Protocol | SSE | SSE |
| Authorization | NATS auth | JWT |
| Clustering | NATS clustering | Mercure clustering |
| Persistence | NATS JetStream* | Built-in |

\* JetStream support planned for future versions

## Development

```bash
# Run tests
go test ./...

# Build with race detector
go build -race -o caddy ./cmd/caddy

# Format code
go fmt ./...
```

## License

MIT License - see [LICENSE](LICENSE) file for details.

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.
