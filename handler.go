// Package nuts provides a Caddy module that bridges NATS.io JetStream
// messages to Server-Sent Events (SSE), similar to Mercure.rocks.
//
// The data flow is one-directional:
//
//	Producer  ──▶  NATS JetStream  ──▶  NUTS (this module)  ──▶  Browser (EventSource)
//
// External applications publish messages to NATS subjects. NUTS subscribes
// to those subjects through JetStream and pushes every message to connected
// browsers as SSE events in real time. JetStream persists messages, so a
// client that reconnects can replay everything it missed by providing its
// last received event ID.
//
// Source files:
//   - handler.go   — Module registration, Handler struct (config + runtime state)
//   - serve.go     — HTTP/SSE request handling: the main streaming loop
//   - provision.go — Caddy lifecycle: connect to NATS, create JetStream context, cleanup
//   - helpers.go   — Small utility functions (JSON, topic validation, SSE writing)
//   - caddyfile.go — Caddyfile directive parser
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

// init runs automatically when the package is imported. It tells Caddy about
// our module so that Caddy can create instances of it, and it registers the
// "nuts" keyword so users can use it in their Caddyfile.
func init() {
	// RegisterModule makes Caddy aware of our Handler type.
	caddy.RegisterModule(&Handler{})

	// RegisterHandlerDirective tells Caddy's config parser that when it sees
	// a "nuts" block it should call parseCaddyfile (defined in caddyfile.go)
	// to turn the text into a Handler.
	httpcaddyfile.RegisterHandlerDirective("nuts", parseCaddyfile)
}

// Handler implements an HTTP handler that bridges NATS.io JetStream
// messages to Server-Sent Events (SSE) for browser clients.
//
// Fields tagged with `json:"..."` are user-configurable — they come
// from the Caddyfile (via UnmarshalCaddyfile) or from Caddy's JSON API.
// The unexported fields at the bottom are runtime state set during
// Provision() and should never be set manually.
type Handler struct {
	// ── Required ──────────────────────────────────────────────────

	// NatsURL is the connection string for the NATS server.
	// Example: "nats://localhost:4222"
	NatsURL string `json:"nats_url,omitempty"`

	// StreamName is the JetStream stream to subscribe to (e.g., "EVENTS").
	// The stream must already exist — NUTS does not create streams.
	StreamName string `json:"stream_name,omitempty"`

	// ── Authentication (pick at most one method) ─────────────────

	// NatsCredentials is the filesystem path to a .creds file.
	// Used with NATS account-based security (JWT + NKey).
	NatsCredentials string `json:"nats_credentials,omitempty"`

	// NatsToken is a simple shared-secret token for NATS authentication.
	NatsToken string `json:"nats_token,omitempty"`

	// NatsUser is the username for basic NATS authentication.
	// Must be paired with NatsPassword.
	NatsUser string `json:"nats_user,omitempty"`

	// NatsPassword is the password for basic NATS authentication.
	// Must be paired with NatsUser.
	NatsPassword string `json:"nats_password,omitempty"`

	// ── Optional tuning ──────────────────────────────────────────

	// TopicPrefix is prepended to every topic name before subscribing.
	// For example, prefix "events." + client topic "orders" = NATS subject "events.orders".
	TopicPrefix string `json:"topic_prefix,omitempty"`

	// AllowedOrigins lists the browser origins allowed by CORS.
	// Use ["*"] to allow any origin (default). In production, prefer explicit domains.
	AllowedOrigins []string `json:"allowed_origins,omitempty"`

	// HeartbeatInterval is how often (in seconds) the server sends a keep-alive
	// comment to SSE clients. Prevents proxies from killing idle connections.
	// Default: 30.
	HeartbeatInterval int `json:"heartbeat_interval,omitempty"`

	// ReconnectWait is the delay in seconds between NATS reconnection attempts.
	// Default: 2.
	ReconnectWait int `json:"reconnect_wait,omitempty"`

	// MaxReconnects limits total NATS reconnection attempts.
	// 0 means "no reconnects", -1 means "unlimited".
	// When the directive is omitted from the Caddyfile the parser sets this
	// to -1 so the default behaviour stays "retry forever".
	MaxReconnects int `json:"max_reconnects,omitempty"`

	// MaxEventSize caps the size (in bytes) of a single formatted SSE event.
	// Events exceeding this are dropped with a warning log.
	// A negative value disables the limit. 0 (or unset) uses the default.
	// Default: 1048576 (1 MB).
	MaxEventSize int `json:"max_event_size,omitempty"`

	// HubURL is the URL advertised in the Link header for hub discovery.
	// When set, SSE responses include a Link: <url>; rel="nuts" header
	// so that clients and upstream APIs can discover the event hub automatically.
	// Leave empty to disable hub discovery (default).
	HubURL string `json:"hub_url,omitempty"`

	// HealthPath is the URL path (relative to the matched route) that
	// returns NATS / stream health as JSON. Empty disables the endpoint.
	// Default: "/healthz".
	HealthPath string `json:"health_path,omitempty"`

	// AllowedHeaders lists HTTP headers permitted by CORS preflight responses.
	// Default: ["Cache-Control", "Last-Event-ID"].
	AllowedHeaders []string `json:"allowed_headers,omitempty"`

	// AllowedMethods lists HTTP methods permitted by CORS preflight responses.
	// Default: ["GET", "OPTIONS"].
	AllowedMethods []string `json:"allowed_methods,omitempty"`

	// MaxConnections caps the total number of concurrent SSE connections served
	// by this handler instance. 0 (default) disables the cap. Connections that
	// would exceed the cap receive HTTP 503 with a Retry-After header.
	MaxConnections int `json:"max_connections,omitempty"`

	// ClientBufferSize is the size of the per-connection NATS message buffer.
	// When the buffer fills, the slow client is disconnected to avoid drops.
	// Default: 64.
	ClientBufferSize int `json:"client_buffer_size,omitempty"`

	// ── NATS TLS ─────────────────────────────────────────────────

	// NatsTLSCA is a path to a PEM-encoded CA bundle used to verify the
	// NATS server certificate.
	NatsTLSCA string `json:"nats_tls_ca,omitempty"`

	// NatsTLSCert is a path to a PEM-encoded client certificate for mTLS.
	// Must be paired with NatsTLSKey.
	NatsTLSCert string `json:"nats_tls_cert,omitempty"`

	// NatsTLSKey is a path to the PEM-encoded private key for the client
	// certificate. Must be paired with NatsTLSCert.
	NatsTLSKey string `json:"nats_tls_key,omitempty"`

	// NatsTLSInsecureSkipVerify disables NATS server certificate verification.
	// Use only for development against self-signed certs.
	NatsTLSInsecureSkipVerify bool `json:"nats_tls_insecure_skip_verify,omitempty"`

	// ── Runtime state (not user-configurable) ────────────────────

	// conn is the long-lived TCP connection to the NATS server.
	// Opened during Provision(), shared across all HTTP requests.
	conn *nats.Conn

	// js is a JetStream context derived from conn. It provides
	// Subscribe() and StreamInfo() used by ServeHTTP and Provision.
	js nats.JetStreamContext

	// logger is a structured logger scoped to this handler instance.
	logger *zap.Logger

	// mu protects conn and js. Request goroutines read-lock (RLock);
	// Provision and Cleanup write-lock (Lock).
	mu sync.RWMutex

	// connCount is the live SSE connection counter used to enforce
	// MaxConnections. Updated atomically.
	connCount int64

	// maxReconnectsSet is true when the Caddyfile contained an explicit
	// max_reconnects directive. Provision() uses this to distinguish
	// "user wrote 0" (no reconnects) from "directive omitted" (unlimited).
	maxReconnectsSet bool
}

// messageEventPayload is the JSON structure sent inside the "data:" field of
// each SSE event. Browsers receive it as a string and typically JSON.parse()
// it to access the topic, the original message body, and the timestamp.
//
// Example of what the browser sees on the wire:
//
//	id: 42
//	event: message
//	data: {"topic":"orders","payload":{"id":1},"time":"2024-01-01T12:00:00Z"}
type messageEventPayload struct {
	Topic   string      `json:"topic"`   // Topic name (without the prefix)
	Payload interface{} `json:"payload"` // Original message body; parsed as JSON when valid
	Time    string      `json:"time"`    // ISO 8601 timestamp (from JetStream metadata when available)
}

// CaddyModule returns metadata that tells Caddy about this module.
// The ID "http.handlers.nuts" places it in the HTTP handler namespace.
// The New function is a factory — Caddy calls it to create a fresh Handler
// instance every time the config is loaded.
func (*Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.nuts",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Interface guards — compile-time checks that Handler implements every
// Caddy interface it needs. If you accidentally remove a required method,
// the compiler will fail here with a clear message instead of panicking
// at runtime.
//
//   - caddy.Module:                CaddyModule() — identity & factory
//   - caddy.Provisioner:           Provision() — setup on startup
//   - caddy.Validator:             Validate() — config validation
//   - caddy.CleanerUpper:          Cleanup() — teardown on shutdown
//   - caddyhttp.MiddlewareHandler: ServeHTTP() — handle HTTP requests
//   - caddyfile.Unmarshaler:       UnmarshalCaddyfile() — parse Caddyfile
var (
	_ caddy.Module                = (*Handler)(nil)
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddy.Validator             = (*Handler)(nil)
	_ caddy.CleanerUpper          = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
	_ caddyfile.Unmarshaler       = (*Handler)(nil)
)
