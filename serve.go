package nuts

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

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
