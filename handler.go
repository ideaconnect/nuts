// Package nuts provides a Caddy module that bridges NATS.io Pub/Sub with
// Server-Sent Events (SSE), similar to Mercure.rocks functionality.
package nuts

import (
	"sync"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(&Handler{})
	httpcaddyfile.RegisterHandlerDirective("nuts", parseCaddyfile)
}

// Handler implements an HTTP handler that bridges NATS.io messages
// to Server-Sent Events for browser clients.
type Handler struct {
	// NatsURL is the URL to connect to NATS server (e.g., "nats://localhost:4222")
	NatsURL string `json:"nats_url,omitempty"`

	// NatsCredentials is the path to NATS credentials file (optional)
	NatsCredentials string `json:"nats_credentials,omitempty"`

	// NatsToken is the authentication token for NATS (optional)
	NatsToken string `json:"nats_token,omitempty"`

	// NatsUser is the username for NATS authentication (optional)
	NatsUser string `json:"nats_user,omitempty"`

	// NatsPassword is the password for NATS authentication (optional)
	NatsPassword string `json:"nats_password,omitempty"`

	// TopicPrefix is a prefix added to all topic subscriptions
	TopicPrefix string `json:"topic_prefix,omitempty"`

	// AllowedOrigins is a list of allowed CORS origins (* for all)
	AllowedOrigins []string `json:"allowed_origins,omitempty"`

	// HeartbeatInterval is the interval for sending heartbeat comments (in seconds)
	HeartbeatInterval int `json:"heartbeat_interval,omitempty"`

	// ReconnectWait is the time to wait before reconnecting to NATS (in seconds)
	ReconnectWait int `json:"reconnect_wait,omitempty"`

	// MaxReconnects is the maximum number of reconnection attempts (-1 for infinite)
	MaxReconnects int `json:"max_reconnects,omitempty"`

	// StreamName is the name of the JetStream stream to use (required)
	StreamName string `json:"stream_name,omitempty"`

	// MaxEventSize is the maximum size in bytes of a single SSE event payload.
	// Messages exceeding this limit are dropped with a warning. 0 means no limit.
	// Default: 1048576 (1 MB).
	MaxEventSize int `json:"max_event_size,omitempty"`

	conn   *nats.Conn
	js     nats.JetStreamContext
	logger *zap.Logger
	mu     sync.RWMutex
}

type messageEventPayload struct {
	Topic   string      `json:"topic"`
	Payload interface{} `json:"payload"`
	Time    string      `json:"time"`
}

// CaddyModule returns the Caddy module information.
func (*Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.nuts",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Interface guards
var (
	_ caddy.Module                = (*Handler)(nil)
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddy.Validator             = (*Handler)(nil)
	_ caddy.CleanerUpper          = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
	_ caddyfile.Unmarshaler       = (*Handler)(nil)
)
