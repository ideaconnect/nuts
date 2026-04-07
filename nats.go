// Package nuts provides a Caddy module that bridges NATS.io Pub/Sub with
// Server-Sent Events (SSE), similar to Mercure.rocks functionality.
package nuts

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

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

// ServeHTTP implements the caddyhttp.MiddlewareHandler interface.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	// Browsers issue OPTIONS preflight requests before cross-origin EventSource connections.
	if r.Method == http.MethodOptions {
		h.setCORSHeaders(w, r)
		w.WriteHeader(http.StatusNoContent)
		return nil
	}

	// SSE is GET-only; everything else either falls through or returns a normal HTTP error.
	if r.Method != http.MethodGet {
		if next == nil {
			w.Header().Set("Allow", "GET, OPTIONS")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return nil
		}
		return next.ServeHTTP(w, r)
	}

	// Query parameters support multiple topics; the path form is a shorthand for one topic.
	topics := r.URL.Query()["topic"]
	if len(topics) == 0 {
		// Try to get topic from path (e.g., /events/my-topic)
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" {
			topics = []string{path}
		}
	}

	// Reject topics that contain control characters or empty segments.
	for _, t := range topics {
		if !isValidTopic(t) {
			http.Error(w, "Invalid topic name", http.StatusBadRequest)
			return nil
		}
	}

	if len(topics) == 0 {
		http.Error(w, "No topics specified. Use ?topic=name or path-based topic", http.StatusBadRequest)
		return nil
	}

	// Validate replay state before committing the response so bad cursors return plain HTTP errors.
	lastIDStr := r.URL.Query().Get("last-id")
	lastIDSource := "last-id"
	if lastIDStr == "" {
		lastIDStr = r.Header.Get("Last-Event-ID")
		lastIDSource = "Last-Event-ID"
	}
	var lastID uint64
	var hasLastID bool
	if lastIDStr != "" {
		parsedID, err := strconv.ParseUint(lastIDStr, 10, 64)
		if err != nil {
			http.Error(w, fmt.Sprintf("Invalid %s value: must be a positive integer", lastIDSource), http.StatusBadRequest)
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

	// Build subscriptions before writing any SSE bytes so setup failures stay normal HTTP failures.
	h.mu.RLock()
	js := h.js
	h.mu.RUnlock()

	if js == nil {
		http.Error(w, "JetStream not available", http.StatusServiceUnavailable)
		return nil
	}

	// Subscription callbacks can outlive the request briefly, so done gates enqueue during teardown.
	msgChan := make(chan *nats.Msg, 64)
	done := make(chan struct{})
	slowClient := make(chan string, 1)
	enqueueMessage := func(msg *nats.Msg) {
		select {
		case <-done:
			return
		case msgChan <- msg:
		default:
			select {
			case slowClient <- msg.Subject:
			default:
			}
		}
	}

	var subscriptions []*nats.Subscription
	var subscribedTopics []string
	var failedTopics []string
	cleanupSubscriptions := func() {
		// Signal callbacks to stop enqueuing before tearing down subscriptions.
		// Closing done first is intentional: Unsubscribe may race with a callback
		// already in flight, and the <-done check in enqueueMessage ensures
		// such callbacks exit immediately. Any messages already buffered in
		// msgChan are orphaned but will be garbage-collected with the channel.
		close(done)
		for _, sub := range subscriptions {
			if err := sub.Unsubscribe(); err != nil {
				h.logger.Warn("failed to unsubscribe",
					zap.String("topic", sub.Subject),
					zap.Error(err))
			}
		}
	}

	for _, topic := range topics {
		fullTopic := h.TopicPrefix + topic

		// Build subscription options for ephemeral consumer
		opts := []nats.SubOpt{
			nats.BindStream(h.StreamName),
			nats.AckNone(), // No ack needed for SSE streaming
		}

		// Live subscribers only need new messages; replay subscribers resume from a known sequence.
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

		sub, err := js.Subscribe(fullTopic, enqueueMessage, opts...)

		if err != nil {
			// When the requested sequence has been purged, replay all retained messages instead.
			// Use substring match rather than exact equality so the fallback survives
			// error-message changes across NATS server versions.
			errMsg := err.Error()
			if hasLastID && (strings.Contains(errMsg, "start sequence") ||
				strings.Contains(errMsg, "sequence not found")) {
				h.logger.Warn("requested sequence not available, falling back to all messages",
					zap.String("topic", fullTopic),
					zap.Uint64("requested_sequence", lastID+1),
				)
				fallbackOpts := []nats.SubOpt{
					nats.BindStream(h.StreamName),
					nats.AckNone(),
					nats.DeliverAll(),
				}
				sub, err = js.Subscribe(fullTopic, enqueueMessage, fallbackOpts...)
			}
			if err != nil {
				h.logger.Error("failed to subscribe to topic",
					zap.String("topic", fullTopic),
					zap.Error(err),
				)
				failedTopics = append(failedTopics, topic)
				continue
			}
		}
		subscriptions = append(subscriptions, sub)
		subscribedTopics = append(subscribedTopics, topic)
		h.logger.Debug("subscribed to topic", zap.String("topic", fullTopic))
	}

	if len(failedTopics) > 0 {
		cleanupSubscriptions()
		http.Error(w, fmt.Sprintf("Failed to subscribe to requested topics: %s", strings.Join(failedTopics, ", ")), http.StatusServiceUnavailable)
		return nil
	}

	if len(subscriptions) == 0 {
		cleanupSubscriptions()
		http.Error(w, "Failed to subscribe to any requested topics", http.StatusServiceUnavailable)
		return nil
	}

	defer cleanupSubscriptions()

	// Only now is it safe to switch into streaming mode and emit the connected event.
	h.setCORSHeaders(w, r)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Disable nginx buffering

	// Send initial connection event.
	if err := writeSSEChunk(w, flusher, fmt.Sprintf("event: connected\ndata: {\"topics\":%s}\n\n", toJSON(subscribedTopics))); err != nil {
		h.logger.Debug("failed to write connected event", zap.Error(err))
		return nil
	}

	// Create heartbeat ticker
	heartbeat := time.NewTicker(time.Duration(h.HeartbeatInterval) * time.Second)
	defer heartbeat.Stop()

	// Stream messages until the client disconnects; heartbeats keep idle connections alive.
	ctx := r.Context()
	for {
		select {
		case slowTopic := <-slowClient:
			h.logger.Warn("disconnecting slow SSE client before dropping messages",
				zap.String("topic", slowTopic),
				zap.Int("buffer_size", cap(msgChan)),
			)
			return nil

		case <-ctx.Done():
			h.logger.Debug("client disconnected")
			return nil

		case msg := <-msgChan:
			if msg == nil {
				continue
			}
			// Remove topic prefix for the event name sent to client
			eventTopic := strings.TrimPrefix(msg.Subject, h.TopicPrefix)
			eventTime := time.Now().UTC().Format(time.RFC3339)
			payload := messageEventPayload{
				Topic:   eventTopic,
				Payload: tryParseJSON(msg.Data),
				Time:    eventTime,
			}
			var eventID uint64
			hasEventID := false

			// Prefer JetStream metadata so replayed messages keep their original publish timestamp.
			meta, metaErr := msg.Metadata()
			if metaErr != nil {
				h.logger.Warn("failed to read JetStream metadata", zap.String("topic", msg.Subject), zap.Error(metaErr))
			} else {
				eventTime = meta.Timestamp.UTC().Format(time.RFC3339)
				payload.Time = eventTime
				eventID = meta.Sequence.Stream
				hasEventID = true
			}

			var event strings.Builder
			// Send SSE event with ID for replay support when JetStream metadata is available.
			if hasEventID {
				event.WriteString("id: ")
				event.WriteString(strconv.FormatUint(eventID, 10))
				event.WriteString("\n")
			}
			event.WriteString("event: message\n")
			event.WriteString("data: ")
			event.WriteString(toJSON(payload))
			event.WriteString("\n\n")

			// Guard against oversized events that could exhaust client memory.
			if h.MaxEventSize > 0 && event.Len() > h.MaxEventSize {
				h.logger.Warn("dropping oversized SSE event",
					zap.String("topic", msg.Subject),
					zap.Int("event_size", event.Len()),
					zap.Int("max_event_size", h.MaxEventSize),
				)
				continue
			}

			if err := writeSSEChunk(w, flusher, event.String()); err != nil {
				h.logger.Debug("failed to write message event", zap.String("topic", msg.Subject), zap.Error(err))
				return nil
			}

		case <-heartbeat.C:
			// Send heartbeat comment to keep connection alive
			if err := writeSSEChunk(w, flusher, fmt.Sprintf(": heartbeat %s\n\n", time.Now().UTC().Format(time.RFC3339))); err != nil {
				h.logger.Debug("failed to write heartbeat", zap.Error(err))
				return nil
			}
		}
	}
}

// setCORSHeaders sets the appropriate CORS headers.
func (h *Handler) setCORSHeaders(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}

	// Echo the matched origin rather than '*' so credentialed SSE requests remain valid.
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

// writeSSEChunk writes one complete SSE chunk and flushes it to the client.
func writeSSEChunk(w io.Writer, flusher http.Flusher, chunk string) error {
	if _, err := io.WriteString(w, chunk); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for d.NextBlock(0) {
			// Each directive maps directly onto a handler field or tunable.
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

			case "max_event_size":
				if !d.NextArg() {
					return d.ArgErr()
				}
				var size int
				if _, err := fmt.Sscanf(d.Val(), "%d", &size); err != nil {
					return d.Errf("invalid max_event_size: %v", err)
				}
				h.MaxEventSize = size

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

// isValidTopic rejects topic names that contain control characters, empty
// segments, NATS wildcards, or NATS internal subjects.
func isValidTopic(topic string) bool {
	const maxTopicLen = 256
	if topic == "" || len(topic) > maxTopicLen {
		return false
	}
	// Reject NATS wildcards — server-side consumers should use TopicPrefix for broad subscriptions.
	if strings.ContainsAny(topic, "*>") {
		return false
	}
	// Reject NATS internal/system subjects (e.g. $JS, $SYS).
	if strings.HasPrefix(topic, "$") {
		return false
	}
	for _, r := range topic {
		if unicode.IsControl(r) {
			return false
		}
	}
	if strings.Contains(topic, "..") {
		return false
	}
	return true
}

// redactURL strips userinfo (embedded credentials / tokens) from a URL before logging.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("REDACTED")
	return u.String()
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
