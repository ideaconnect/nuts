// Package nuts implements an HTTP middleware for Caddy that exposes a
// JetStream stream to browser clients as a Server-Sent Events (SSE) feed.
//
// The module registers itself with Caddy as http.handlers.nuts and as the
// "nuts" Caddyfile directive. A single long-lived NATS connection is opened
// in Provision() and shared across every request handled by the module
// instance; per-request subscriptions are ephemeral JetStream consumers that
// are torn down when the SSE connection closes.
//
// Data flow is one-directional — NUTS does not publish:
//
//	Producer ──▶ NATS JetStream ──▶ NUTS (this module) ──▶ Browser (EventSource)
//
// External producers write to NATS subjects directly. Every request to the
// handler opens an ephemeral JetStream consumer scoped to the configured
// stream and the topics extracted from ?topic= (repeatable) or from the
// request path as a shorthand ("/a/b" → topic "a.b"). Incoming messages are
// wrapped as JSON ({topic, payload, time}) and written to the client as SSE
// events with the JetStream sequence number as the SSE id. Clients reconnect
// with either the browser-managed Last-Event-ID header or an explicit
// ?last-id= query parameter to resume from a specific sequence; if that
// sequence has already been purged from the stream NUTS falls back to a
// full-retention replay (bounded by replay_max_messages / replay_window when
// configured).
//
// Source files:
//   - handler.go   — Package godoc, Handler struct (config + runtime state),
//                    module registration, Caddy interface guards.
//   - provision.go — Provision/Validate/Cleanup: defaults, NATS dial,
//                    JetStream context, stream existence check, TLS config,
//                    teardown.
//   - serve.go     — ServeHTTP and its helpers: health endpoint, CORS
//                    preflight, topic extraction and validation, last-id
//                    parsing, connection-slot reservation, JetStream
//                    subscription (with purged-sequence fallback), the SSE
//                    streaming select loop, slow-client disconnect, and
//                    heartbeat.
//   - caddyfile.go — Caddyfile directive parser (UnmarshalCaddyfile).
//   - helpers.go   — Small utilities: JSON marshalling, topic validation,
//                    SSE frame writer, URL credential redaction.
//   - metrics.go   — Prometheus counters and gauges registered via promauto
//                    (nuts_* namespace).
//
// See cmd/caddy/main.go for the build entry point that compiles Caddy with
// this module baked in.
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
	// 0 means "no reconnects", -1 means "unlimited". Nil (omitted from
	// Caddyfile or JSON) defaults to unlimited so the historical "retry
	// forever" behaviour is preserved. The pointer type lets JSON config
	// express an explicit "max_reconnects": 0 — a plain int would collide
	// with Go's zero value and get silently rewritten to the default.
	MaxReconnects *int `json:"max_reconnects,omitempty"`

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

	// ReplayMaxMessages caps how many messages a single client can receive
	// during a DeliverAll fallback (triggered when the requested sequence
	// has been purged from the stream). 0 (default) disables the cap. When
	// the cap is reached the SSE connection is closed cleanly; the client
	// may reconnect with a fresher Last-Event-ID.
	ReplayMaxMessages int `json:"replay_max_messages,omitempty"`

	// ReplayWindow caps how far back in time a DeliverAll fallback reaches.
	// Value is in seconds. When > 0 the fallback subscribes with
	// StartTime(now - ReplayWindow) instead of DeliverAll, so the client
	// only sees recent retained messages. 0 (default) preserves the
	// "replay everything retained" behaviour.
	ReplayWindow int `json:"replay_window,omitempty"`

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

	// shutdown is closed by Cleanup() to wake in-flight SSE handlers
	// immediately instead of letting them linger until the next heartbeat
	// tick or NATS-side error. Created in Provision(), closed and nilled
	// in Cleanup(). Protected by mu. When nil (e.g. tests that bypass
	// Provision), the serve loop's select case is a never-firing nil
	// channel — harmless.
	shutdown chan struct{}
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
