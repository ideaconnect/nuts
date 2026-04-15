// provision.go — Caddy lifecycle: setup, teardown, and config validation.
//
// Caddy calls three methods on our Handler during its lifecycle:
//
//	Provision() — called once when the config is loaded. Opens the NATS
//	              connection and verifies the JetStream stream exists.
//	Validate()  — called after Provision to sanity-check the config.
//	Cleanup()   — called when the config is unloaded or Caddy shuts down.
//	              Closes the NATS connection and nils the cached state.
package nuts

import (
	"fmt"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// Provision sets up the handler. It follows four sequential steps:
//
//	1. Normalize defaults — fill in sane values for optional fields.
//	2. Connect to NATS   — open a TCP connection with auth & callbacks.
//	3. JetStream context  — create a JetStream context from the connection.
//	4. Verify stream      — fail fast if the configured stream doesn't exist.
//
// If any step after 2 fails, the deferred cleanup closes the connection
// so we don’t leak sockets.
func (h *Handler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger(h)

	// Step 1: Normalize optional settings before dialling NATS.
	// Zero/negative values are replaced with production-friendly defaults.
	if h.HeartbeatInterval <= 0 {
		h.HeartbeatInterval = 30 // send a keep-alive comment every 30 seconds
	}
	if h.ReconnectWait <= 0 {
		h.ReconnectWait = 2 // wait 2 seconds between NATS reconnect attempts
	}
	if h.MaxReconnects == 0 {
		h.MaxReconnects = -1 // -1 = retry forever
	}
	if len(h.AllowedOrigins) == 0 {
		h.AllowedOrigins = []string{"*"} // allow any browser origin by default
	}
	if h.MaxEventSize == 0 {
		h.MaxEventSize = 1048576 // 1 MB — protects clients from huge events
	}

	// Step 2: Open the NATS connection.
	if err := h.connectNATS(); err != nil {
		return fmt.Errorf("failed to connect to NATS: %v", err)
	}

	// If steps 3 or 4 fail, we must close the connection we just opened.
	// The provisionErr variable is checked in the deferred function: if it
	// is non-nil, Cleanup() runs to release the socket.
	var provisionErr error
	defer func() {
		if provisionErr != nil {
			_ = h.Cleanup()
		}
	}()

	// Step 3: Create a JetStream context.
	// We hold a read-lock on mu so that a concurrent Cleanup() call cannot
	// nil the connection between our check and the JetStream() call.
	h.mu.RLock()
	conn := h.conn
	if conn == nil {
		h.mu.RUnlock()
		provisionErr = fmt.Errorf("NATS connection is nil after connect")
		return provisionErr
	}

	// Cache a JetStream context — all request goroutines will use this to
	// create subscriptions in ServeHTTP.
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
	// Starting subscriptions against a non-existent stream would fail per
	// request; catching it here gives the operator a clear startup error.
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
//
// It configures three lifecycle callbacks that surface connection state
// changes in Caddy’s log output, and applies whichever authentication
// mode the user configured (credentials file, token, or user/password).
func (h *Handler) connectNATS() error {
	opts := []nats.Option{
		nats.ReconnectWait(time.Duration(h.ReconnectWait) * time.Second),
		nats.MaxReconnects(h.MaxReconnects),

		// DisconnectErrHandler fires when the connection drops unexpectedly.
		// Logging the error helps operators diagnose network issues.
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			if err != nil {
				h.logger.Warn("disconnected from NATS", zap.Error(err))
			}
		}),

		// ReconnectHandler fires after a successful automatic reconnect.
		nats.ReconnectHandler(func(nc *nats.Conn) {
			h.logger.Info("reconnected to NATS", zap.String("url", nc.ConnectedUrl()))
		}),

		// ClosedHandler fires when the connection is permanently closed,
		// either by us calling conn.Close() or after max reconnects.
		nats.ClosedHandler(func(nc *nats.Conn) {
			h.logger.Info("NATS connection closed")
		}),
	}

	// Apply the configured auth mode. Only one is allowed — Validate()
	// rejects configs that specify more than one.
	if h.NatsCredentials != "" {
		// .creds file — contains a JWT and a private NKey.
		opts = append(opts, nats.UserCredentials(h.NatsCredentials))
	} else if h.NatsToken != "" {
		// Shared-secret token authentication.
		opts = append(opts, nats.Token(h.NatsToken))
	} else if h.NatsUser != "" && h.NatsPassword != "" {
		// Basic username/password authentication.
		opts = append(opts, nats.UserInfo(h.NatsUser, h.NatsPassword))
	}

	conn, err := nats.Connect(h.NatsURL, opts...)
	if err != nil {
		return err
	}

	// Store the connection under the write-lock so that ServeHTTP (which
	// reads under RLock) and Cleanup (which writes under Lock) stay safe.
	h.mu.Lock()
	h.conn = conn
	h.mu.Unlock()
	return nil
}

// Cleanup is called by Caddy when the config is unloaded or Caddy shuts
// down. It closes the NATS connection and nils the cached state so that
// any in-flight request goroutines that still hold a reference will see
// nil and fail cleanly rather than using a stale connection.
func (h *Handler) Cleanup() error {
	// Write-lock because we are mutating conn and js.
	h.mu.Lock()
	defer h.mu.Unlock()

	// Clear cached connection state so future reads fail cleanly.
	if h.conn != nil {
		h.conn.Close()
		h.conn = nil
	}
	h.js = nil
	return nil
}

// Validate is called by Caddy after Provision to sanity-check the config.
// It ensures required fields are present, at most one auth method is set,
// and warns about CORS misconfigurations.
func (h *Handler) Validate() error {
	if h.NatsURL == "" {
		return fmt.Errorf("nats_url is required")
	}
	if h.StreamName == "" {
		return fmt.Errorf("stream_name is required for JetStream support")
	}

	// Count how many auth methods were configured. We allow at most one
	// because the NATS client library applies them in order and having
	// multiple would be confusing / ambiguous.
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

	// Warn if allowed_origins contains "*" alongside NATS auth. Browsers
	// reject Access-Control-Allow-Origin: * when the request carries
	// credentials. Our handler echoes the real origin (see setCORSHeaders)
	// so it still works at runtime, but the wildcard config looks more
	// permissive than intended and may confuse security reviewers.
	for _, o := range h.AllowedOrigins {
		if o == "*" && authMethods > 0 {
			if h.logger != nil {
				h.logger.Warn("allowed_origins contains '*' alongside NATS auth; " +
					"consider listing explicit origins for credential-aware CORS")
			}
			break
		}
	}

	return nil
}
