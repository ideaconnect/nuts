// provision.go — Caddy lifecycle: setup, teardown, and config validation.
package nuts

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// defaultMaxEventSize is the SSE event size cap applied when MaxEventSize is 0.
const defaultMaxEventSize = 1048576 // 1 MiB

// defaultClientBufferSize is the per-connection NATS message buffer length
// used when ClientBufferSize is unset.
const defaultClientBufferSize = 64

// defaultHealthPath is used when no health_path directive is configured.
const defaultHealthPath = "/healthz"

// defaultLivePath is used when no live_path directive is configured.
const defaultLivePath = "/livez"

// defaultReadyPath is used when no ready_path directive is configured.
const defaultReadyPath = "/readyz"

// defaultConsumerInactiveThreshold is how long JetStream waits after the
// last delivery / activity before reaping an ephemeral NUTS consumer.
// Without an explicit threshold, nats-server falls back to its own
// default (5s) and a client disconnect followed by a tight reconnect
// loop can accumulate server-side consumer state. 30s is conservative
// enough for legitimate slow disconnect detection while bounding state
// accumulation under churn.
const defaultConsumerInactiveThreshold = 30 * time.Second

// defaultReadinessProbeTimeout bounds how long the readiness probe will
// wait on JetStream's StreamInfo call. Without it, a partially-degraded
// JetStream cluster can stall the probe up to nats.go's library default
// (5s), which is longer than the readiness budget most orchestrators
// use. 1s is well above realistic JetStream latency and well below
// kubelet's default 1s timeout.
const defaultReadinessProbeTimeout = time.Second

// defaultMetadataReadTimeout bounds the per-request JetStream metadata
// reads (StreamInfo + GetMsg in readStreamSnapshot) that run on the SSE
// connection-setup hot path. Without it, a partially-degraded cluster
// can stall every new subscription up to nats.go's library default
// (5s), holding a Caddy handler goroutine and a MaxConnections slot.
// 2s is more permissive than the readiness probe (which fires
// frequently) but still well below the library default. The readiness
// probe already establishes this pattern at serve.go's serveReadinessCheck.
const defaultMetadataReadTimeout = 2 * time.Second

// defaultMaxTopicsPerSubscription bounds how many distinct ?topic= filters
// a single SSE request may carry when MaxTopicsPerSubscription is 0. A
// strict default protects NATS from consumer-creation amplification by an
// attacker passing thousands of `?topic=` parameters.
const defaultMaxTopicsPerSubscription = 32

// minRecommendedJWTKeyLen is the floor below which Validate() warns that
// the configured subscriber_jwt_key is shorter than the SHA-256 output
// size assumed by HS256. Keys shorter than this still verify correctly
// but provide less than the algorithm's nominal security margin.
const minRecommendedJWTKeyLen = 32

// Provision sets up the handler.
func (h *Handler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger(h)

	// Step 0: validate configuration BEFORE normalizing defaults or opening any
	// sockets so that a bad config can't leak resources.
	if err := h.validateRequiredFields(); err != nil {
		return err
	}
	if err := h.validateConfigValues(); err != nil {
		return err
	}

	// Step 1: Normalize optional settings before dialling NATS.
	if h.HeartbeatInterval <= 0 {
		h.HeartbeatInterval = 30
	}
	if h.ReconnectWait <= 0 {
		h.ReconnectWait = 2
	}
	// MaxReconnects nil (directive omitted in Caddyfile or absent from JSON)
	// defaults to -1 (unlimited). An explicit 0 from either source is honoured
	// as "no reconnects".
	if h.MaxReconnects == nil {
		defaultMaxReconnects := -1
		h.MaxReconnects = &defaultMaxReconnects
	}
	if len(h.AllowedOrigins) == 0 {
		h.AllowedOrigins = []string{"*"}
	}
	if len(h.AllowedHeaders) == 0 {
		h.AllowedHeaders = []string{"Cache-Control", "Last-Event-ID"}
	}
	if len(h.AllowedMethods) == 0 {
		h.AllowedMethods = []string{"GET", "OPTIONS"}
	}
	// MaxEventSize semantics:
	//   0  → use defaultMaxEventSize
	//   <0 → unlimited (sentinel preserved as-is)
	//   >0 → user-defined limit
	if h.MaxEventSize == 0 {
		h.MaxEventSize = defaultMaxEventSize
	}
	if h.ClientBufferSize <= 0 {
		h.ClientBufferSize = defaultClientBufferSize
	}
	if h.HealthPath == "" {
		h.HealthPath = defaultHealthPath
	}
	if h.LivePath == "" {
		h.LivePath = defaultLivePath
	}
	if h.ReadyPath == "" {
		h.ReadyPath = defaultReadyPath
	}
	// MaxTopicsPerSubscription semantics mirror MaxEventSize:
	//   0  → use defaultMaxTopicsPerSubscription (apply the cap)
	//   <0 → unlimited (sentinel preserved as-is)
	//   >0 → user-defined limit
	if h.MaxTopicsPerSubscription == 0 {
		h.MaxTopicsPerSubscription = defaultMaxTopicsPerSubscription
	}

	// Create the shutdown signal before opening any sockets so that if
	// connectNATS fails the deferred Cleanup() still has a channel to close.
	h.mu.Lock()
	h.shutdown = make(chan struct{})
	h.mu.Unlock()

	// Register the failure-cleanup deferred call BEFORE the first step that
	// can fail. Otherwise an early failure (e.g. connectNATS) returns before
	// the defer is registered and leaks the shutdown channel created above.
	var provisionErr error
	defer func() {
		if provisionErr != nil {
			_ = h.Cleanup()
		}
	}()

	// Step 2: Open the NATS connection.
	if err := h.connectNATS(); err != nil {
		provisionErr = fmt.Errorf("failed to connect to NATS: %w", err)
		return provisionErr
	}

	// Step 3: Create a JetStream context.
	h.mu.RLock()
	conn := h.conn
	if conn == nil {
		h.mu.RUnlock()
		provisionErr = fmt.Errorf("NATS connection is nil after connect")
		return provisionErr
	}
	js, err := conn.JetStream()
	h.mu.RUnlock()
	if err != nil {
		provisionErr = fmt.Errorf("failed to create JetStream context: %w", err)
		return provisionErr
	}

	h.mu.Lock()
	h.js = js
	h.mu.Unlock()

	// Step 4: Verify that the configured stream actually exists.
	_, err = js.StreamInfo(h.StreamName)
	if err != nil {
		provisionErr = fmt.Errorf("JetStream stream '%s' not found. Please create the stream first. See README for instructions. Error: %w", h.StreamName, err)
		return provisionErr
	}

	h.log().Info("nuts handler provisioned",
		zap.String("nats_url", redactURL(h.NatsURL)),
		zap.String("stream_name", h.StreamName),
		zap.String("topic_prefix", h.TopicPrefix),
		zap.String("health_path", h.HealthPath),
		zap.String("live_path", h.LivePath),
		zap.String("ready_path", h.ReadyPath),
	)

	return nil
}

// connectNATS opens a long-lived TCP connection to the NATS server.
func (h *Handler) connectNATS() error {
	// Provision normalises this to a non-nil pointer. We re-check here so
	// tests can call connectNATS() directly without going through Provision.
	maxReconnects := -1
	if h.MaxReconnects != nil {
		maxReconnects = *h.MaxReconnects
	}
	opts := []nats.Option{
		// nats.Name labels this connection on the NATS server side. It
		// surfaces in /connz output and in server-side slow-consumer
		// warnings so operators can attribute a connection to a NUTS
		// gateway instance among other workloads sharing the cluster.
		// Without it the connection is anonymous (IP:port only), which
		// is operationally painful when many Caddy pods share egress.
		nats.Name("nuts-caddy"),
		nats.ReconnectWait(time.Duration(h.ReconnectWait) * time.Second),
		nats.MaxReconnects(maxReconnects),

		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			metricsNATSConnectionEvents.WithLabelValues("disconnect").Inc()
			if err != nil {
				h.log().Warn("disconnected from NATS", zap.Error(err))
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			metricsNATSConnectionEvents.WithLabelValues("reconnect").Inc()
			h.log().Info("reconnected to NATS", zap.String("url", redactURL(nc.ConnectedUrl())))
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			metricsNATSConnectionEvents.WithLabelValues("closed").Inc()
			h.log().Info("NATS connection closed")
		}),
		// ErrorHandler captures async failures the nats.go client would
		// otherwise log to stderr via its default printer — most importantly
		// ErrSlowConsumer when a subscription's internal buffer overflows
		// and silently drops messages. Routing through h.logger + a metric
		// makes that failure mode observable in production.
		nats.ErrorHandler(func(nc *nats.Conn, sub *nats.Subscription, err error) {
			if err == nil {
				// nats.go is documented to always pass a non-nil err here;
				// guard defensively so a future library bug can't bump the
				// metric with an undocumented label or log a confusing
				// "<nil>"-error entry.
				return
			}
			kind := classifyNATSAsyncError(err)
			metricsNATSAsyncErrors.WithLabelValues(kind).Inc()
			fields := []zap.Field{zap.String("kind", kind), zap.Error(err)}
			if sub != nil {
				fields = append(fields, zap.String("subject", sub.Subject))
			}
			h.log().Warn("NATS async error", fields...)
		}),
	}

	// Apply auth (Validate ensures only one mode is configured).
	if h.NatsCredentials != "" {
		opts = append(opts, nats.UserCredentials(h.NatsCredentials))
	} else if h.NatsToken != "" {
		opts = append(opts, nats.Token(h.NatsToken))
	} else if h.NatsUser != "" && h.NatsPassword != "" {
		opts = append(opts, nats.UserInfo(h.NatsUser, h.NatsPassword))
	}

	// Apply TLS configuration if any TLS field is set.
	if h.NatsTLSCA != "" || h.NatsTLSCert != "" || h.NatsTLSKey != "" || h.NatsTLSInsecureSkipVerify {
		tlsCfg, err := h.buildTLSConfig()
		if err != nil {
			return err
		}
		opts = append(opts, nats.Secure(tlsCfg))
	}

	conn, err := nats.Connect(h.NatsURL, opts...)
	if err != nil {
		return err
	}

	h.mu.Lock()
	h.conn = conn
	h.mu.Unlock()
	return nil
}

// buildTLSConfig assembles a *tls.Config from the configured TLS file paths.
func (h *Handler) buildTLSConfig() (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: h.NatsTLSInsecureSkipVerify, //nolint:gosec // operator opt-in
	}

	if h.NatsTLSCA != "" {
		pem, err := os.ReadFile(h.NatsTLSCA)
		if err != nil {
			return nil, fmt.Errorf("read nats_tls_ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("nats_tls_ca: no certificates found in %s", h.NatsTLSCA)
		}
		cfg.RootCAs = pool
	}

	if h.NatsTLSCert != "" && h.NatsTLSKey != "" {
		cert, err := tls.LoadX509KeyPair(h.NatsTLSCert, h.NatsTLSKey)
		if err != nil {
			return nil, fmt.Errorf("load nats_tls_cert=%s nats_tls_key=%s: %w", h.NatsTLSCert, h.NatsTLSKey, err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}

	return cfg, nil
}

// Cleanup is called by Caddy when the config is unloaded or Caddy shuts down.
func (h *Handler) Cleanup() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Signal in-flight SSE handlers first so they return promptly instead of
	// discovering the teardown via a heartbeat-write error or a NATS-side
	// subscription close. Idempotent: nil-ing after close prevents a panic
	// if Cleanup is called more than once.
	if h.shutdown != nil {
		close(h.shutdown)
		h.shutdown = nil
	}
	if h.conn != nil {
		h.conn.Close()
		h.conn = nil
	}
	h.js = nil
	return nil
}

// validateRequiredFields checks the presence of fields that are required
// before any side effect (opening a NATS connection) can be performed.
func (h *Handler) validateRequiredFields() error {
	if h.NatsURL == "" {
		return fmt.Errorf("nats_url is required")
	}
	if h.StreamName == "" {
		return fmt.Errorf("stream_name is required for JetStream support")
	}

	// Auth method check: at most one.
	authMethods := 0
	if h.NatsCredentials != "" {
		authMethods++
	}
	if h.NatsToken != "" {
		authMethods++
	}
	if h.NatsUser != "" || h.NatsPassword != "" {
		if h.NatsUser == "" || h.NatsPassword == "" {
			return fmt.Errorf("nats_user and nats_password must be provided together")
		}
		authMethods++
	}
	if authMethods > 1 {
		return fmt.Errorf("only one NATS authentication method can be configured")
	}

	// TLS pairing: cert and key must come together.
	if (h.NatsTLSCert == "") != (h.NatsTLSKey == "") {
		return fmt.Errorf("nats_tls_cert and nats_tls_key must be provided together")
	}

	return nil
}

// validateTopicPrefix rejects topic_prefix values that would silently
// compose with a request topic to produce a NATS wildcard subscription or
// system-subject — the cross-tenant fan-out class. isValidTopic only sees
// the unprefixed request token, so without this guard a one-character
// Caddyfile typo (`topic_prefix *.`) would subscribe every client to
// every first-token namespace under the configured stream.
//
// Allowed: empty (no prefix), or a string composed of isAllowedTopicByte
// characters with no `*`/`>`, no `..`, no leading `.` or `$`, and at
// most maxSubjectLen bytes. A trailing `.` is idiomatic per docs
// (`events.`, `tenants.a.`) and remains allowed.
func validateTopicPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if len(prefix) > maxSubjectLen {
		return fmt.Errorf("topic_prefix exceeds %d bytes", maxSubjectLen)
	}
	if strings.HasPrefix(prefix, "$") {
		return fmt.Errorf("topic_prefix may not address NATS system subjects ($-prefix)")
	}
	if strings.HasPrefix(prefix, ".") {
		return fmt.Errorf("topic_prefix may not start with '.'")
	}
	if strings.Contains(prefix, "..") {
		return fmt.Errorf("topic_prefix may not contain consecutive dots")
	}
	if strings.ContainsAny(prefix, "*>") {
		return fmt.Errorf("topic_prefix may not contain NATS wildcards ('*' or '>') — they would compose with the request topic and silently broaden the subscription")
	}
	for i := 0; i < len(prefix); i++ {
		if !isAllowedTopicByte(prefix[i]) {
			return fmt.Errorf("topic_prefix contains disallowed byte at position %d: 0x%02x", i, prefix[i])
		}
	}
	return nil
}

// validateConfigValues checks semantic constraints that must be identical
// whether config was supplied through a Caddyfile or Caddy's JSON API.
func (h *Handler) validateConfigValues() error {
	if h.MaxReconnects != nil && *h.MaxReconnects < -1 {
		return fmt.Errorf("max_reconnects must be >= -1")
	}
	if h.MaxConnections < 0 {
		return fmt.Errorf("max_connections must be >= 0")
	}
	if h.ClientBufferSize < 0 {
		return fmt.Errorf("client_buffer_size must be >= 0")
	}
	if h.DispatchTimeout < 0 {
		return fmt.Errorf("dispatch_timeout must be >= 0")
	}
	if h.WriteTimeout < 0 {
		return fmt.Errorf("write_timeout must be >= 0")
	}
	if h.ReplayMaxMessages < 0 {
		return fmt.Errorf("replay_max_messages must be >= 0")
	}
	if h.ReplayWindow < 0 {
		return fmt.Errorf("replay_window must be >= 0")
	}
	// HeartbeatInterval and ReconnectWait are silently rewritten to
	// defaults at Provision-time when <= 0 (see provision.go:77-82),
	// but a negative value is a likely typo (e.g. `heartbeat_interval -30`
	// intended as `30`). Rejecting it surfaces the mistake instead of
	// quietly using the default cadence. The <= 0 normalization stays so
	// `0` still means "use the default" for forward compatibility.
	if h.HeartbeatInterval < 0 {
		return fmt.Errorf("heartbeat_interval must be >= 0")
	}
	if h.ReconnectWait < 0 {
		return fmt.Errorf("reconnect_wait must be >= 0")
	}
	if h.SubscriberJWTCookie != "" && h.SubscriberJWTKey == "" {
		return fmt.Errorf("subscriber_jwt_cookie requires subscriber_jwt_key")
	}
	if h.SubscriberJWTCookie != "" && !isValidCookieName(h.SubscriberJWTCookie) {
		return fmt.Errorf("subscriber_jwt_cookie contains invalid characters")
	}
	if err := validateTopicPrefix(h.TopicPrefix); err != nil {
		return err
	}
	for _, method := range h.AllowedMethods {
		switch strings.ToUpper(method) {
		case http.MethodGet, http.MethodOptions:
		default:
			return fmt.Errorf("allowed_methods may only include GET and OPTIONS; got %q", method)
		}
	}
	return nil
}

// Validate is called by Caddy after Provision to sanity-check the config and
// surface warnings.
func (h *Handler) Validate() error {
	if err := h.validateRequiredFields(); err != nil {
		return err
	}
	if err := h.validateConfigValues(); err != nil {
		return err
	}

	for _, o := range h.AllowedOrigins {
		if o == "*" {
			h.log().Warn("allowed_origins contains '*': Access-Control-Allow-Credentials " +
				"is not advertised for wildcard-matched origins. If clients need credentialed " +
				"CORS (cookies, Authorization headers), list explicit origins instead.")
			break
		}
	}

	if (h.NatsCredentials != "" || h.NatsToken != "" || (h.NatsUser != "" && h.NatsPassword != "")) &&
		strings.HasPrefix(h.NatsURL, "nats://") {
		h.log().Warn("NATS credentials sent over plaintext nats:// URL; consider tls:// or nats_tls_* directives",
			zap.String("nats_url", redactURL(h.NatsURL)))
	}

	if h.NatsTLSInsecureSkipVerify {
		h.log().Warn("nats_tls_insecure_skip_verify is enabled — server certificate is not verified")
	}

	if h.SubscriberJWTKey != "" && len(h.SubscriberJWTKey) < minRecommendedJWTKeyLen {
		h.log().Warn("subscriber_jwt_key is shorter than recommended; HS256 assumes a key of at least 32 bytes",
			zap.Int("key_length", len(h.SubscriberJWTKey)),
			zap.Int("recommended_minimum", minRecommendedJWTKeyLen))
	}

	return nil
}
