package nuts

import (
	"fmt"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// Provision sets up the handler.
func (h *Handler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger(h)

	// Normalize optional settings before the handler dials NATS.
	if h.HeartbeatInterval <= 0 {
		h.HeartbeatInterval = 30
	}
	if h.ReconnectWait <= 0 {
		h.ReconnectWait = 2
	}
	if h.MaxReconnects == 0 {
		h.MaxReconnects = -1 // infinite by default
	}
	if len(h.AllowedOrigins) == 0 {
		h.AllowedOrigins = []string{"*"}
	}
	if h.MaxEventSize == 0 {
		h.MaxEventSize = 1048576 // 1 MB
	}

	// Establish the underlying NATS connection first; JetStream is created from it.
	if err := h.connectNATS(); err != nil {
		return fmt.Errorf("failed to connect to NATS: %v", err)
	}

	// If any step after connection fails, clean up the open connection.
	var provisionErr error
	defer func() {
		if provisionErr != nil {
			_ = h.Cleanup()
		}
	}()

	// Hold the read-lock through JetStream context creation so Cleanup on
	// another goroutine cannot nil the connection in between.
	h.mu.RLock()
	conn := h.conn
	if conn == nil {
		h.mu.RUnlock()
		provisionErr = fmt.Errorf("NATS connection is nil after connect")
		return provisionErr
	}

	// Cache a JetStream context for request handling once the connection is ready.
	js, err := conn.JetStream()
	h.mu.RUnlock()
	if err != nil {
		provisionErr = fmt.Errorf("failed to create JetStream context: %v", err)
		return provisionErr
	}

	h.mu.Lock()
	h.js = js
	h.mu.Unlock()

	// Fail provision early if the configured stream does not exist.
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

// connectNATS establishes a connection to the NATS server.
func (h *Handler) connectNATS() error {
	// These callbacks surface connection lifecycle changes in Caddy logs.
	opts := []nats.Option{
		nats.ReconnectWait(time.Duration(h.ReconnectWait) * time.Second),
		nats.MaxReconnects(h.MaxReconnects),
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

	// Apply the configured auth mode. Validate() ensures conflicting modes are rejected.
	if h.NatsCredentials != "" {
		opts = append(opts, nats.UserCredentials(h.NatsCredentials))
	} else if h.NatsToken != "" {
		opts = append(opts, nats.Token(h.NatsToken))
	} else if h.NatsUser != "" && h.NatsPassword != "" {
		opts = append(opts, nats.UserInfo(h.NatsUser, h.NatsPassword))
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

// Cleanup closes the NATS connection when the handler is destroyed.
func (h *Handler) Cleanup() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Clear cached connection state so future requests fail cleanly after shutdown.
	if h.conn != nil {
		h.conn.Close()
		h.conn = nil
	}
	h.js = nil
	return nil
}

// Validate ensures the handler configuration is valid.
func (h *Handler) Validate() error {
	if h.NatsURL == "" {
		return fmt.Errorf("nats_url is required")
	}
	if h.StreamName == "" {
		return fmt.Errorf("stream_name is required for JetStream support")
	}

	// Only one auth strategy is allowed, and user/password must arrive as a pair.
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

	for _, o := range h.AllowedOrigins {
		if o == "*" && authMethods > 0 {
			// Browsers reject Access-Control-Allow-Origin: * with credentials.
			// The handler echoes the real origin so it still works, but the
			// wildcard configuration looks more permissive than intended.
			if h.logger != nil {
				h.logger.Warn("allowed_origins contains '*' alongside NATS auth; " +
					"consider listing explicit origins for credential-aware CORS")
			}
			break
		}
	}

	return nil
}
