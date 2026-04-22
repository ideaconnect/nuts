// provision.go — Caddy lifecycle: setup, teardown, and config validation.
package nuts

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
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

// Provision sets up the handler.
func (h *Handler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger(h)

	// Step 0: validate required fields BEFORE opening any sockets so that a
	// bad config can't leak resources.
	if err := h.validateRequiredFields(); err != nil {
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

	// Create the shutdown signal before opening any sockets so that if
	// connectNATS fails the deferred Cleanup() still has a channel to close.
	h.mu.Lock()
	h.shutdown = make(chan struct{})
	h.mu.Unlock()

	// Step 2: Open the NATS connection.
	if err := h.connectNATS(); err != nil {
		return fmt.Errorf("failed to connect to NATS: %v", err)
	}

	var provisionErr error
	defer func() {
		if provisionErr != nil {
			_ = h.Cleanup()
		}
	}()

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
		provisionErr = fmt.Errorf("failed to create JetStream context: %v", err)
		return provisionErr
	}

	h.mu.Lock()
	h.js = js
	h.mu.Unlock()

	// Step 4: Verify that the configured stream actually exists.
	_, err = js.StreamInfo(h.StreamName)
	if err != nil {
		provisionErr = fmt.Errorf("JetStream stream '%s' not found. Please create the stream first. See README for instructions. Error: %v", h.StreamName, err)
		return provisionErr
	}

	h.logger.Info("nuts handler provisioned",
		zap.String("nats_url", redactURL(h.NatsURL)),
		zap.String("stream_name", h.StreamName),
		zap.String("topic_prefix", h.TopicPrefix),
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
		nats.ReconnectWait(time.Duration(h.ReconnectWait) * time.Second),
		nats.MaxReconnects(maxReconnects),

		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			if err != nil {
				h.logger.Warn("disconnected from NATS", zap.Error(err))
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			h.logger.Info("reconnected to NATS", zap.String("url", nc.ConnectedUrl()))
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			h.logger.Info("NATS connection closed")
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
			return nil, fmt.Errorf("read nats_tls_ca: %v", err)
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
			return nil, fmt.Errorf("load nats_tls_cert/key: %v", err)
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

// Validate is called by Caddy after Provision to sanity-check the config and
// surface warnings.
func (h *Handler) Validate() error {
	if err := h.validateRequiredFields(); err != nil {
		return err
	}

	for _, o := range h.AllowedOrigins {
		if o == "*" {
			if h.logger != nil {
				h.logger.Warn("allowed_origins contains '*': Access-Control-Allow-Credentials " +
					"is not advertised for wildcard-matched origins. If clients need credentialed " +
					"CORS (cookies, Authorization headers), list explicit origins instead.")
			}
			break
		}
	}

	if h.logger != nil && (h.NatsToken != "" || (h.NatsUser != "" && h.NatsPassword != "")) {
		if strings.HasPrefix(h.NatsURL, "nats://") {
			h.logger.Warn("NATS credentials sent over plaintext nats:// URL; consider tls:// or nats_tls_* directives",
				zap.String("nats_url", redactURL(h.NatsURL)))
		}
	}

	if h.logger != nil && h.NatsTLSInsecureSkipVerify {
		h.logger.Warn("nats_tls_insecure_skip_verify is enabled — server certificate is not verified")
	}

	return nil
}
