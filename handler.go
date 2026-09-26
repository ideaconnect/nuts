// Package nuts exposes a retained NATS JetStream stream to browsers as
// Server-Sent Events through a Caddy HTTP handler.
//
//	Producer ──▶ NATS JetStream ──▶ NUTS (this module) ──▶ Browser (EventSource)
//
// External producers write to NATS directly; NUTS is a read-only bridge.
package nuts

import (
	"sync"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(&Handler{})
	httpcaddyfile.RegisterHandlerDirective("nuts", parseCaddyfile)
}

// Handler implements an HTTP handler that bridges NATS.io JetStream
// messages to Server-Sent Events (SSE) for browser clients.
//
// Exported fields are user-configurable through Caddyfile or JSON config.
type Handler struct {
	// NatsURL is the connection string for the NATS server.
	NatsURL string `json:"nats_url,omitempty"`

	// StreamName is the existing JetStream stream to subscribe to.
	StreamName string `json:"stream_name,omitempty"`

	// NatsCredentials is the filesystem path to a NATS .creds file.
	NatsCredentials string `json:"nats_credentials,omitempty"`

	// NatsToken is a shared-secret token for NATS authentication.
	NatsToken string `json:"nats_token,omitempty"`

	// NatsUser is the username for NATS user/password authentication.
	NatsUser string `json:"nats_user,omitempty"`

	// NatsPassword must be paired with NatsUser.
	NatsPassword string `json:"nats_password,omitempty"`

	// SubscriberJWTKey enables first-party subscriber JWT auth using HMAC-signed
	// tokens. Tokens must include a subscribe claim with allowed topics/filters.
	SubscriberJWTKey string `json:"subscriber_jwt_key,omitempty"`

	// SubscriberJWTCookie optionally names the cookie that carries the JWT for
	// browser EventSource clients. Authorization: Bearer is always accepted.
	SubscriberJWTCookie string `json:"subscriber_jwt_cookie,omitempty"`

	// TopicPrefix is prepended to every topic name before subscribing.
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

	// HealthPath is the legacy URL path (relative to the matched route) that
	// returns NATS / stream readiness as JSON. Empty uses the default.
	// Default: "/healthz".
	HealthPath string `json:"health_path,omitempty"`

	// LivePath is the URL path for a process-liveness probe. It returns 200
	// when the handler can serve HTTP, without checking NATS or JetStream.
	// Default: "/livez".
	LivePath string `json:"live_path,omitempty"`

	// ReadyPath is the URL path for a readiness probe. It checks the NATS
	// connection and configured JetStream stream before returning 200.
	// Default: "/readyz".
	ReadyPath string `json:"ready_path,omitempty"`

	// AllowedHeaders lists HTTP headers permitted by CORS preflight responses.
	// Default: ["Cache-Control", "Last-Event-ID"].
	AllowedHeaders []string `json:"allowed_headers,omitempty"`

	// AllowedMethods lists HTTP methods permitted by CORS preflight responses.
	// NUTS only serves GET streams and OPTIONS preflight requests.
	// Default: ["GET", "OPTIONS"].
	AllowedMethods []string `json:"allowed_methods,omitempty"`

	// MaxConnections caps the total number of concurrent SSE connections served
	// by this handler instance. 0 (default) disables the cap. Connections that
	// would exceed the cap receive HTTP 429 (Too Many Requests, RFC 6585) with
	// a Retry-After header.
	MaxConnections int `json:"max_connections,omitempty"`

	// MaxTopicsPerSubscription caps how many distinct topics a single SSE
	// request may subscribe to. 0 (or unset) uses the default. A negative
	// value disables the limit. Requests exceeding the cap receive HTTP 400.
	// Default: 32.
	MaxTopicsPerSubscription int `json:"max_topics_per_subscription,omitempty"`

	// ClientBufferSize is how many messages each connection's pull consumer
	// prefetches from JetStream. When the SSE writer falls behind, pulling
	// stops and the backlog waits in the stream; it bounds per-connection
	// memory at roughly ClientBufferSize × the largest message.
	// 0 (or unset) uses the default. Default: 64.
	ClientBufferSize int `json:"client_buffer_size,omitempty"`

	// DispatchTimeout is deprecated and has no effect since the pull
	// consumer replaced the push queue: nothing waits to hand a message to a
	// full queue any more. Stalled clients are bounded by WriteTimeout.
	// Setting it logs a deprecation warning; it will be removed in the next
	// major release.
	DispatchTimeout int `json:"dispatch_timeout,omitempty"`

	// WriteTimeout caps each SSE frame write and flush, in seconds. A write
	// that misses it means the client stopped reading, and the stream closes
	// with disconnect_reason=slow_client. 0 (or unset) uses the default of
	// 30; -1 disables the deadline and leaves stalled writes to the
	// surrounding HTTP server configuration.
	WriteTimeout int `json:"write_timeout,omitempty"`

	// ReplayMaxMessages caps replay delivery per reconnect. 0 disables the cap.
	ReplayMaxMessages int `json:"replay_max_messages,omitempty"`

	// ReplayWindow caps replay by time in seconds. 0 preserves retained replay.
	ReplayWindow int `json:"replay_window,omitempty"`

	// NatsIdleHeartbeat is the heartbeat interval, in seconds, that every
	// pull request asks JetStream for. When heartbeats stop arriving, the
	// ordered consumer recreates itself from the last delivered sequence,
	// so a consumer reaped during an outage or lost with a leafnode route
	// does not leave the stream silently stuck.
	//
	// Default: 10 (seconds). 0 (or unset) is normalised to the default
	// during Provision. -1 is accepted for compatibility but no longer
	// disables anything: the ordered consumer always uses heartbeats, so
	// the library default (5s) applies and Validate logs a warning. Other
	// negative values are rejected as likely typos.
	//
	// Constraint enforced by Validate when positive:
	// NatsIdleHeartbeat < defaultConsumerInactiveThreshold/2 (currently
	// 15 seconds) so two missed heartbeats are observable before the
	// server reaps the consumer.
	NatsIdleHeartbeat int `json:"nats_idle_heartbeat,omitempty"`

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

	// conn is opened during Provision and shared across HTTP requests.
	conn *nats.Conn

	// js is the JetStream API handle derived from conn.
	js jetstream.JetStream

	// logger is scoped to this handler instance.
	logger *zap.Logger

	// mu protects conn, js, shutdown and closing.
	mu sync.RWMutex

	// connCount enforces MaxConnections.
	connCount int64

	// shutdown wakes in-flight SSE handlers during Cleanup.
	shutdown chan struct{}

	// closing is set by Cleanup. No stream is tracked in streams after it.
	closing bool

	// streams counts SSE streams whose JetStream consumer has not been
	// deleted yet, so Cleanup can let those deletes reach the server before
	// it closes the connection.
	streams sync.WaitGroup
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
	Topic   string `json:"topic"`   // Topic name (without the prefix)
	Payload any    `json:"payload"` // Original message body; valid JSON is embedded without numeric coercion
	Time    string `json:"time"`    // ISO 8601 timestamp (from JetStream metadata when available)
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
