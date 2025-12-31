// Package nuts provides a Caddy module that bridges NATS.io Pub/Sub with
// Server-Sent Events (SSE), similar to Mercure.rocks functionality.
package nuts

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(Handler{})
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

	conn   *nats.Conn
	js     nats.JetStreamContext
	logger *zap.Logger
	mu     sync.RWMutex
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.nuts",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision sets up the handler.
func (h *Handler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger(h)

	// Set defaults
	if h.NatsURL == "" {
		h.NatsURL = nats.DefaultURL
	}
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

	// Connect to NATS
	if err := h.connectNATS(); err != nil {
		return fmt.Errorf("failed to connect to NATS: %v", err)
	}

	// Initialize JetStream context
	js, err := h.conn.JetStream()
	if err != nil {
		return fmt.Errorf("failed to create JetStream context: %v", err)
	}
	h.js = js

	// Verify stream exists
	_, err = h.js.StreamInfo(h.StreamName)
	if err != nil {
		return fmt.Errorf("JetStream stream '%s' not found. Please create the stream first. See README for instructions. Error: %v", h.StreamName, err)
	}

	h.logger.Info("nuts handler provisioned",
		zap.String("nats_url", h.NatsURL),
		zap.String("stream_name", h.StreamName),
		zap.String("topic_prefix", h.TopicPrefix),
	)

	return nil
}

// connectNATS establishes a connection to the NATS server.
func (h *Handler) connectNATS() error {
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

	// Add authentication options
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

	if h.conn != nil {
		h.conn.Close()
		h.conn = nil
	}
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
	return nil
}

// ServeHTTP implements the caddyhttp.MiddlewareHandler interface.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	// Handle CORS preflight
	if r.Method == http.MethodOptions {
		h.setCORSHeaders(w, r)
		w.WriteHeader(http.StatusNoContent)
		return nil
	}

	// Only handle GET requests for SSE
	if r.Method != http.MethodGet {
		return next.ServeHTTP(w, r)
	}

	// Get topics from query parameter
	topics := r.URL.Query()["topic"]
	if len(topics) == 0 {
		// Try to get topic from path (e.g., /events/my-topic)
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" {
			topics = []string{path}
		}
	}

	if len(topics) == 0 {
		http.Error(w, "No topics specified. Use ?topic=name or path-based topic", http.StatusBadRequest)
		return nil
	}

	// Parse last-id parameter for message replay (validate early before starting SSE)
	lastIDStr := r.URL.Query().Get("last-id")
	var lastID uint64
	var hasLastID bool
	if lastIDStr != "" {
		parsedID, err := strconv.ParseUint(lastIDStr, 10, 64)
		if err != nil {
			http.Error(w, "Invalid last-id parameter: must be a positive integer", http.StatusBadRequest)
			return nil
		}
		lastID = parsedID
		hasLastID = true
	}

	// Check if the client supports SSE
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return nil
	}

	// Set SSE headers
	h.setCORSHeaders(w, r)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Disable nginx buffering

	// Send initial connection event
	fmt.Fprintf(w, "event: connected\ndata: {\"topics\":%s}\n\n", toJSON(topics))
	flusher.Flush()

	// Create message channel
	msgChan := make(chan *nats.Msg, 64)
	defer close(msgChan)

	// Subscribe to all requested topics using JetStream
	h.mu.RLock()
	js := h.js
	h.mu.RUnlock()

	if js == nil {
		http.Error(w, "JetStream not available", http.StatusServiceUnavailable)
		return nil
	}

	var subscriptions []*nats.Subscription
	for _, topic := range topics {
		fullTopic := h.TopicPrefix + topic

		// Build subscription options for ephemeral consumer
		opts := []nats.SubOpt{
			nats.BindStream(h.StreamName),
			nats.AckNone(), // No ack needed for SSE streaming
		}

		if hasLastID {
			// Start from the sequence after the provided last-id
			opts = append(opts, nats.StartSequence(lastID+1))
			h.logger.Debug("subscribing from sequence",
				zap.String("topic", fullTopic),
				zap.Uint64("start_sequence", lastID+1),
			)
		} else {
			// New subscriber - only deliver new messages
			opts = append(opts, nats.DeliverNew())
		}

		sub, err := js.Subscribe(fullTopic, func(msg *nats.Msg) {
			select {
			case msgChan <- msg:
			default:
				h.logger.Warn("message dropped, channel full", zap.String("topic", msg.Subject))
			}
		}, opts...)

		if err != nil {
			// If sequence not found, fallback to delivering all available messages
			if hasLastID && (err.Error() == "nats: consumer start sequence is not available" ||
				err.Error() == "nats: stream sequence not found") {
				h.logger.Warn("requested sequence not available, falling back to all messages",
					zap.String("topic", fullTopic),
					zap.Uint64("requested_sequence", lastID+1),
				)
				fallbackOpts := []nats.SubOpt{
					nats.BindStream(h.StreamName),
					nats.AckNone(),
					nats.DeliverAll(),
				}
				sub, err = js.Subscribe(fullTopic, func(msg *nats.Msg) {
					select {
					case msgChan <- msg:
					default:
						h.logger.Warn("message dropped, channel full", zap.String("topic", msg.Subject))
					}
				}, fallbackOpts...)
			}
			if err != nil {
				h.logger.Error("failed to subscribe to topic",
					zap.String("topic", fullTopic),
					zap.Error(err),
				)
				continue
			}
		}
		subscriptions = append(subscriptions, sub)
		h.logger.Debug("subscribed to topic", zap.String("topic", fullTopic))
	}

	// Ensure cleanup of subscriptions
	defer func() {
		for _, sub := range subscriptions {
			sub.Unsubscribe()
		}
	}()

	// Create heartbeat ticker
	heartbeat := time.NewTicker(time.Duration(h.HeartbeatInterval) * time.Second)
	defer heartbeat.Stop()

	// Stream messages to client
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			h.logger.Debug("client disconnected")
			return nil

		case msg := <-msgChan:
			if msg == nil {
				continue
			}
			// Remove topic prefix for the event name sent to client
			eventTopic := strings.TrimPrefix(msg.Subject, h.TopicPrefix)

			// Get JetStream metadata for message ID
			var msgID uint64
			if meta, err := msg.Metadata(); err == nil {
				msgID = meta.Sequence.Stream
			}

			// Send SSE event with ID for replay support
			fmt.Fprintf(w, "id: %d\n", msgID)
			fmt.Fprintf(w, "event: message\n")
			fmt.Fprintf(w, "data: %s\n", toJSON(map[string]interface{}{
				"topic":   eventTopic,
				"payload": tryParseJSON(msg.Data),
				"time":    time.Now().UTC().Format(time.RFC3339),
			}))
			fmt.Fprintf(w, "\n")
			flusher.Flush()

		case <-heartbeat.C:
			// Send heartbeat comment to keep connection alive
			fmt.Fprintf(w, ": heartbeat %s\n\n", time.Now().UTC().Format(time.RFC3339))
			flusher.Flush()
		}
	}
}

// setCORSHeaders sets the appropriate CORS headers.
func (h *Handler) setCORSHeaders(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}

	for _, allowed := range h.AllowedOrigins {
		if allowed == "*" || allowed == origin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Cache-Control, Last-Event-ID")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			break
		}
	}
}

// toJSON converts a value to JSON string.
func toJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// tryParseJSON attempts to parse data as JSON, returns string if not valid JSON.
func tryParseJSON(data []byte) interface{} {
	var v interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		return string(data)
	}
	return v
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for d.NextBlock(0) {
			switch d.Val() {
			case "nats_url":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.NatsURL = d.Val()

			case "nats_credentials":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.NatsCredentials = d.Val()

			case "nats_token":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.NatsToken = d.Val()

			case "nats_user":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.NatsUser = d.Val()

			case "nats_password":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.NatsPassword = d.Val()

			case "topic_prefix":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.TopicPrefix = d.Val()

			case "allowed_origins":
				h.AllowedOrigins = d.RemainingArgs()
				if len(h.AllowedOrigins) == 0 {
					return d.ArgErr()
				}

			case "heartbeat_interval":
				if !d.NextArg() {
					return d.ArgErr()
				}
				var interval int
				if _, err := fmt.Sscanf(d.Val(), "%d", &interval); err != nil {
					return d.Errf("invalid heartbeat_interval: %v", err)
				}
				h.HeartbeatInterval = interval

			case "reconnect_wait":
				if !d.NextArg() {
					return d.ArgErr()
				}
				var wait int
				if _, err := fmt.Sscanf(d.Val(), "%d", &wait); err != nil {
					return d.Errf("invalid reconnect_wait: %v", err)
				}
				h.ReconnectWait = wait

			case "max_reconnects":
				if !d.NextArg() {
					return d.ArgErr()
				}
				var max int
				if _, err := fmt.Sscanf(d.Val(), "%d", &max); err != nil {
					return d.Errf("invalid max_reconnects: %v", err)
				}
				h.MaxReconnects = max

			case "stream_name":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.StreamName = d.Val()

			default:
				return d.Errf("unrecognized option: %s", d.Val())
			}
		}
	}
	return nil
}

// parseCaddyfile parses the Caddyfile directive.
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var handler Handler
	err := handler.UnmarshalCaddyfile(h.Dispenser)
	return &handler, err
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
