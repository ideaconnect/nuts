// provision.go — Caddy lifecycle: setup, teardown, and config validation.
package nuts

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
)

// defaultMaxEventSize is the SSE event size cap applied when MaxEventSize is 0.
const defaultMaxEventSize = 1048576 // 1 MiB

// defaultClientBufferSize is the per-connection NATS message buffer length
// used when ClientBufferSize is unset.
const defaultClientBufferSize = 64

// defaultWriteTimeoutSeconds is the per-frame write deadline applied when
// write_timeout is unset. With backpressure in the pull consumer, a write that
// cannot complete is the only sign of a client that stopped reading, so it
// needs a bound by default. writeTimeoutDisabledSentinel (-1) turns the
// deadline off.
const (
	defaultWriteTimeoutSeconds   = 30
	writeTimeoutDisabledSentinel = -1
)

// defaultHealthPath is used when no health_path directive is configured.
const defaultHealthPath = "/healthz"

// defaultLivePath is used when no live_path directive is configured.
const defaultLivePath = "/livez"

// defaultReadyPath is used when no ready_path directive is configured.
const defaultReadyPath = "/readyz"

// cleanupStreamsTimeout bounds how long Cleanup waits for in-flight streams to
// delete their consumers before it closes the NATS connection. Each delete is
// itself bounded by defaultMetadataReadTimeout.
const cleanupStreamsTimeout = 3 * time.Second

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

// defaultConsumerCreateTimeout bounds creating a request's ordered consumer.
// Creation is a single attempt; a timeout or refusal becomes a 503.
const defaultConsumerCreateTimeout = 5 * time.Second

// defaultConsumerMaxResetAttempts bounds how often an ordered consumer tries
// to recreate itself after a gap, a reconnect or missed heartbeats. The
// library backs off 1s, 2s, 4s, 8s, then 10s per attempt, so ten attempts give
// up after roughly 75 seconds of failures; the SSE stream then closes with
// disconnect_reason=consumer_unrecoverable and the client reconnects.
const defaultConsumerMaxResetAttempts = 10

// consumerNamePrefix marks the consumers NUTS creates so operators can tell
// them apart in `nats consumer ls`. A random suffix keeps names unique across
// NUTS instances sharing a stream.
const consumerNamePrefix = "nuts_"

// defaultNatsIdleHeartbeatSeconds is the M9 Batch A default for the
// server-side IdleHeartbeat interval on every JetStream push consumer
// NUTS opens. Default-on at 10s so the IdleHeartbeat-miss detector
// can surface a reaped or lost ephemeral consumer without operator
// intervention. The value is in seconds (not time.Duration) to match
// the int-seconds shape of NatsIdleHeartbeat on Handler — Validate
// requires it to stay strictly below defaultConsumerInactiveThreshold/2
// so two missed heartbeats can be detected before the server reaps.
//
// natsIdleHeartbeatDisabledSentinel is the explicit operator-disable
// value. Validate accepts exactly this value and rejects any other
// negative input as a likely typo (matching how HeartbeatInterval
// and ReconnectWait are validated — see validateConfigValues).
const (
	defaultNatsIdleHeartbeatSeconds   = 10
	natsIdleHeartbeatDisabledSentinel = -1
)

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
	// Caddy hands every config load a fresh Handler, so the logger is only
	// set beforehand by tests that need to observe Provision.
	if h.logger == nil {
		h.logger = ctx.Logger(h)
	}

	// Step 0: validate configuration BEFORE normalizing defaults or opening any
	// sockets so that a bad config can't leak resources.
	if err := h.validateRequiredFields(); err != nil {
		return err
	}
	if err := h.validateConfigValues(); err != nil {
		return err
	}
	// Warn before the connection is opened: by the time Validate runs, the
	// credentials and the first JetStream requests have already been sent.
	h.warnAboutTransportSecurity()

	// Step 1: Normalize optional settings before dialling NATS.
	if h.HeartbeatInterval <= 0 {
		h.HeartbeatInterval = 30
	}
	if h.ReconnectWait <= 0 {
		h.ReconnectWait = 2
	}
	// NatsIdleHeartbeat: 0 means "use default" (10s); the explicit
	// operator-disable sentinel (-1) is preserved as-is so
	// subscriptionOptions can skip the SubOpt append. Validate has
	// already rejected positive values >= InactiveThreshold/2 AND any
	// negative value other than the sentinel by this point, so by the
	// time the value is consumed it is guaranteed to be either a safe
	// positive interval or the disable sentinel exactly.
	if h.NatsIdleHeartbeat == 0 {
		h.NatsIdleHeartbeat = defaultNatsIdleHeartbeatSeconds
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
	// WriteTimeout semantics: 0 → default, -1 → no deadline (sentinel kept).
	if h.WriteTimeout == 0 {
		h.WriteTimeout = defaultWriteTimeoutSeconds
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
	if h.MaxTopicsPerSubscription == maxTopicsDisabledSentinel {
		h.log().Info("max_topics_per_subscription is -1: requests may subscribe to any number of topics")
	}

	// Create the shutdown signal before opening any sockets so that if
	// connectNATS fails the deferred Cleanup() still has a channel to close.
	h.mu.Lock()
	h.shutdown = make(chan struct{})
	h.closing = false
	h.shared = newSharedRegistry()
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

	// Step 3: Create the JetStream API handle. WithDefaultTimeout bounds any
	// API call that is not given its own deadline.
	h.mu.RLock()
	conn := h.conn
	if conn == nil {
		h.mu.RUnlock()
		provisionErr = fmt.Errorf("NATS connection is nil after connect")
		return provisionErr
	}
	js, err := jetstream.New(conn, jetstream.WithDefaultTimeout(defaultMetadataReadTimeout))
	serverVersion := conn.ConnectedServerVersion()
	h.mu.RUnlock()
	h.warnAboutServerVersion(serverVersion)
	if err != nil {
		provisionErr = fmt.Errorf("failed to create JetStream context: %w", err)
		return provisionErr
	}

	h.mu.Lock()
	h.js = js
	h.mu.Unlock()

	// Step 4: Verify that the configured stream actually exists. Bounded so a
	// degraded JetStream API cannot stall a Caddy reload.
	streamCtx, cancelStream := context.WithTimeout(context.Background(), defaultMetadataReadTimeout)
	stream, err := js.Stream(streamCtx, h.StreamName)
	cancelStream()
	if err != nil {
		provisionErr = fmt.Errorf("JetStream stream '%s' not found. Please create the stream first. See README for instructions. Error: %w", h.StreamName, err)
		return provisionErr
	}
	h.logStreamLimits(stream.CachedInfo())

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

// maxTopicsDisabledSentinel turns max_topics_per_subscription off. Other
// negative values are rejected as typos.
const maxTopicsDisabledSentinel = -1

// natsSchemes are the nats_url schemes nats.go dials: nats and tls over TCP,
// ws and wss over WebSocket.
var natsSchemes = []string{"nats", "tls", "ws", "wss"}

// natsServerSchemes validates nats_url, a comma-separated server list as
// nats.go accepts it, and returns the scheme of each server. A server
// without a scheme is dialled as nats://, as nats.go does. nats.go itself
// dials unknown schemes such as http:// or a mistyped tls:// as plain
// nats://, so they are rejected here rather than silently downgraded.
func natsServerSchemes(raw string) ([]string, error) {
	var schemes []string
	for _, server := range strings.Split(raw, ",") {
		server = strings.TrimSpace(server)
		if server == "" {
			return nil, fmt.Errorf("nats_url contains an empty server entry")
		}
		server = withNATSScheme(server)
		u, err := url.Parse(server)
		if err != nil {
			// url.Error repeats the URL, credentials included; keep only
			// the reason.
			var urlErr *url.Error
			if errors.As(err, &urlErr) {
				err = urlErr.Err
			}
			return nil, fmt.Errorf("nats_url entry %q is not a valid URL: %w", redactURL(server), err)
		}
		if !slices.Contains(natsSchemes, u.Scheme) {
			return nil, fmt.Errorf("nats_url scheme %q is not supported: use nats://, tls://, ws:// or wss://", u.Scheme)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("nats_url entry %q has no host", redactURL(server))
		}
		schemes = append(schemes, u.Scheme)
	}
	return schemes, nil
}

// warnAboutTransportSecurity flags NATS settings that expose the connection.
// Provision calls it before dialling (#82).
func (h *Handler) warnAboutTransportSecurity() {
	if h.NatsTLSInsecureSkipVerify {
		h.log().Warn("nats_tls_insecure_skip_verify is enabled: the NATS server certificate will not be verified")
	}
	if h.natsCredentialsConfigured() && h.natsPlaintextServer() {
		h.log().Warn("NATS credentials will be sent unencrypted; use tls:// or wss://, or the nats_tls_* directives",
			zap.String("nats_url", redactURL(h.NatsURL)))
	}
}

// natsCredentialsConfigured reports whether any credentials go to the server:
// a directive, or user information embedded in nats_url.
func (h *Handler) natsCredentialsConfigured() bool {
	if h.NatsCredentials != "" || h.NatsToken != "" || h.NatsUser != "" || h.NatsPassword != "" {
		return true
	}
	for _, server := range strings.Split(h.NatsURL, ",") {
		if u, err := url.Parse(withNATSScheme(strings.TrimSpace(server))); err == nil && u.User != nil {
			return true
		}
	}
	return false
}

// natsPlaintextServer reports whether any configured server is dialled
// without TLS: nats:// or ws:// with none of the nats_tls_* directives, which
// make nats.go use TLS on those schemes too.
func (h *Handler) natsPlaintextServer() bool {
	if h.NatsTLSCA != "" || h.NatsTLSCert != "" || h.NatsTLSKey != "" || h.NatsTLSInsecureSkipVerify {
		return false
	}
	schemes, err := natsServerSchemes(h.NatsURL)
	if err != nil {
		return false
	}
	return slices.Contains(schemes, "nats") || slices.Contains(schemes, "ws")
}

// withNATSScheme adds the nats:// scheme nats.go assumes for a server given
// as host:port.
func withNATSScheme(server string) string {
	if strings.Contains(server, "://") {
		return server
	}
	return "nats://" + server
}

// multiFilterPurgeFixed is the first nats-server release whose multi-filter
// consumers keep their pending messages when one of their subjects is purged
// or rolled up (nats-server#8572). Older servers skip them silently, and the
// client's cursor moves past the hole.
var multiFilterPurgeFixed = [3]int{2, 14, 7}

// warnAboutServerVersion flags a nats-server that silently loses messages on
// multi-topic subscriptions (#111). It only warns: behaviour does not depend
// on the version, and single-topic subscriptions are not affected.
func (h *Handler) warnAboutServerVersion(version string) {
	if serverVersionBefore(version, multiFilterPurgeFixed) {
		h.log().Warn("this nats-server can skip messages on multi-topic subscriptions when one of their subjects is purged or rolled up; upgrade to 2.14.7 or later, or subscribe to one topic per connection",
			zap.String("server_version", version),
		)
	}
}

// serverVersionBefore reports whether a server version such as "2.12.15" or
// "2.15.0-beta.1" is older than want. Unparseable versions report false.
func serverVersionBefore(version string, want [3]int) bool {
	core, _, _ := strings.Cut(version, "-")
	parts := strings.Split(core, ".")
	if len(parts) != len(want) {
		return false
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return false
		}
		if n != want[i] {
			return n < want[i]
		}
	}
	return false
}

// logStreamLimits points out stream settings that limit what NUTS can do, so
// operators see them at startup rather than as failed requests.
func (h *Handler) logStreamLimits(info *jetstream.StreamInfo) {
	if info == nil {
		return
	}
	if limit := info.Config.ConsumerLimits.InactiveThreshold; limit > 0 && limit < defaultConsumerInactiveThreshold {
		h.log().Info("consumer inactive threshold lowered to the stream's consumer limit",
			zap.Duration("inactive_threshold", limit),
			zap.Duration("default_inactive_threshold", defaultConsumerInactiveThreshold),
		)
	}
	if maxConsumers := info.Config.MaxConsumers; maxConsumers > 0 && h.MaxConnections > maxConsumers {
		h.log().Warn("the stream's max_consumers is below max_connections; every SSE connection needs its own consumer, so connections beyond it will be rejected",
			zap.Int("max_consumers", maxConsumers),
			zap.Int("max_connections", h.MaxConnections),
		)
	}
}

// connectNATS opens a long-lived TCP connection to the NATS server.
func (h *Handler) connectNATS() error {
	// Provision normalises this to a non-nil pointer. We re-check here so
	// tests can call connectNATS() directly without going through Provision.
	maxReconnects := -1
	if h.MaxReconnects != nil {
		maxReconnects = *h.MaxReconnects
	}
	// The callbacks run on nats.go's own goroutine for the life of the
	// connection, so they use the logger the handler has now rather than
	// reading the field later.
	log := h.log()
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
				log.Warn("disconnected from NATS", zap.Error(err))
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			metricsNATSConnectionEvents.WithLabelValues("reconnect").Inc()
			log.Info("reconnected to NATS", zap.String("url", redactURL(nc.ConnectedUrl())))
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			metricsNATSConnectionEvents.WithLabelValues("closed").Inc()
			log.Info("NATS connection closed")
		}),
		// Cleanup closes the connection itself; without this option the
		// final callbacks run after Close returns, logging through a handler
		// Caddy has already unloaded while its replacement is serving.
		nats.NoCallbacksAfterClientClose(),
		// A server in lame duck mode is about to shut down; clients move to
		// another server of the cluster and the ordered consumers follow.
		nats.LameDuckModeHandler(func(nc *nats.Conn) {
			metricsNATSConnectionEvents.WithLabelValues("lame_duck").Inc()
			log.Warn("NATS server entered lame duck mode; the connection will move to another server",
				zap.String("url", redactURL(nc.ConnectedUrl())),
			)
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
			log.Warn("NATS async error", fields...)
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
	// Signal in-flight SSE handlers first so they return promptly instead of
	// discovering the teardown via a heartbeat-write error or a NATS-side
	// subscription close. Idempotent: nil-ing after close prevents a panic
	// if Cleanup is called more than once.
	h.closing = true
	if h.shutdown != nil {
		close(h.shutdown)
		h.shutdown = nil
	}
	conn := h.conn
	h.conn = nil
	h.js = nil
	h.mu.Unlock()

	// The streams end as soon as shutdown closes. Let their consumer deletes
	// reach the server before the connection goes, so a reload does not
	// leave every stream's consumer behind until InactiveThreshold (#75).
	h.waitForStreams(cleanupStreamsTimeout)
	if conn != nil {
		conn.Close()
	}
	return nil
}

// waitForStreams waits, at most timeout, until every tracked stream has
// deleted its consumer.
func (h *Handler) waitForStreams(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		h.streams.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		h.log().Warn("closing the NATS connection before every SSE stream deleted its consumer",
			zap.Duration("waited", timeout),
		)
	}
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

// validateAllowedOrigins accepts "*" or origins exactly as browsers send them
// in the Origin header: scheme://host[:port] in lowercase, with no path,
// query or trailing slash. setCORSHeaders compares origins literally, so any
// other spelling would silently never match (#79).
func validateAllowedOrigins(origins []string) error {
	for _, origin := range origins {
		if origin == "*" {
			continue
		}
		if origin == "" {
			return fmt.Errorf("allowed_origins contains an empty entry")
		}
		if strings.ContainsAny(origin, ", \t\r\n") {
			return fmt.Errorf("allowed_origins entry %q contains a comma or whitespace; list origins as separate arguments", origin)
		}
		u, err := url.Parse(origin)
		if err != nil || u.Scheme == "" || u.Host == "" || u.Opaque != "" || u.User != nil ||
			u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(origin, "?") || strings.HasSuffix(origin, "#") {
			return fmt.Errorf("allowed_origins entry %q is not an origin: use scheme://host[:port], or *", origin)
		}
		if origin != strings.ToLower(origin) {
			return fmt.Errorf("allowed_origins entry %q must be lowercase, as browsers send it", origin)
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
	if h.WriteTimeout < writeTimeoutDisabledSentinel {
		return fmt.Errorf("write_timeout must be >= 0, or -1 to disable")
	}
	if h.ReplayMaxMessages < 0 {
		return fmt.Errorf("replay_max_messages must be >= 0")
	}
	if h.ReplayWindow < 0 {
		return fmt.Errorf("replay_window must be >= 0")
	}
	if h.MaxTopicsPerSubscription < maxTopicsDisabledSentinel {
		return fmt.Errorf("max_topics_per_subscription (%d) is invalid: the only accepted negative value is -1 (no limit); other negatives are rejected as typos",
			h.MaxTopicsPerSubscription)
	}
	if h.NatsURL != "" {
		if _, err := natsServerSchemes(h.NatsURL); err != nil {
			return err
		}
	}
	if h.NatsTLSInsecureSkipVerify && h.NatsTLSCA != "" {
		return fmt.Errorf("nats_tls_ca cannot be combined with nats_tls_insecure_skip_verify: without verification the CA bundle is ignored and any certificate is accepted; remove one of the two")
	}
	if err := validateAllowedOrigins(h.AllowedOrigins); err != nil {
		return err
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
	// nats_idle_heartbeat upper bound: must leave room for at least two
	// missed heartbeats before the server reaps the ephemeral via
	// InactiveThreshold. We pin to the same constant subscriptionOptions
	// uses (defaultConsumerInactiveThreshold = 30s), so the valid range
	// is (0, 15). The only accepted negative value is the explicit
	// operator-disable sentinel (-1) — any other negative is rejected as
	// a likely typo, matching how heartbeat_interval and reconnect_wait
	// are validated above. Zero is normalised by Provision to the
	// default (defaultNatsIdleHeartbeatSeconds = 10), well inside the
	// bound.
	idleHeartbeatUpperBound := int(defaultConsumerInactiveThreshold/time.Second) / 2
	switch {
	case h.NatsIdleHeartbeat > 0 && h.NatsIdleHeartbeat >= idleHeartbeatUpperBound:
		return fmt.Errorf("nats_idle_heartbeat (%d) must be less than half of InactiveThreshold (%d seconds): two missed heartbeats must be detectable before the server reaps the consumer",
			h.NatsIdleHeartbeat, int(defaultConsumerInactiveThreshold/time.Second))
	case h.NatsIdleHeartbeat < 0 && h.NatsIdleHeartbeat != natsIdleHeartbeatDisabledSentinel:
		return fmt.Errorf("nats_idle_heartbeat (%d) is invalid: the only accepted negative value is %d (operator-disable sentinel) — other negatives are rejected as typos",
			h.NatsIdleHeartbeat, natsIdleHeartbeatDisabledSentinel)
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

	if h.DispatchTimeout > 0 {
		h.log().Warn("dispatch_timeout is deprecated and has no effect: the pull consumer applies backpressure instead of queueing, and stalled clients are bounded by write_timeout",
			zap.Int("dispatch_timeout", h.DispatchTimeout))
	}

	if h.NatsIdleHeartbeat == natsIdleHeartbeatDisabledSentinel {
		h.log().Warn("nats_idle_heartbeat -1 no longer disables consumer health checks: the ordered consumer always uses heartbeats, so the library default (5s) applies")
	}

	if h.SubscriberJWTKey != "" && len(h.SubscriberJWTKey) < minRecommendedJWTKeyLen {
		h.log().Warn("subscriber_jwt_key is shorter than recommended; HS256 assumes a key of at least 32 bytes",
			zap.Int("key_length", len(h.SubscriberJWTKey)),
			zap.Int("recommended_minimum", minRecommendedJWTKeyLen))
	}

	return nil
}
