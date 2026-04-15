// serve.go — HTTP/SSE request handling.
//
// This file contains the core of NUTS: the ServeHTTP method that turns
// an incoming HTTP GET into a long-lived SSE stream backed by NATS
// JetStream subscriptions, plus a CORS helper.
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
//
// High-level flow (each phase is annotated inline below):
//
//	1. OPTIONS — handle CORS preflight and return immediately.
//	2. Method check — only GET is allowed for SSE; other methods fall through.
//	3. Topic extraction — from ?topic= query params or the URL path.
//	4. Last-ID / replay — parse the client’s cursor for message replay.
//	5. JetStream subscription — subscribe to each topic; fall back on purge.
//	6. Streaming select loop — multiplex messages, heartbeats, slow-client
//	   detection, and client disconnect.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	// Phase 1: CORS preflight.
	// Browsers issue OPTIONS preflight requests before cross-origin EventSource connections.
	if r.Method == http.MethodOptions {
		h.setCORSHeaders(w, r)
		w.WriteHeader(http.StatusNoContent)
		return nil
	}

	// Phase 2: Method check.
	// SSE is GET-only; everything else either falls through or returns a normal HTTP error.
	if r.Method != http.MethodGet {
		if next == nil {
			w.Header().Set("Allow", "GET, OPTIONS")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return nil
		}
		return next.ServeHTTP(w, r)
	}

	// Phase 3: Topic extraction.
	// Query parameters support multiple topics (?topic=a&topic=b);
	// the path form is a shorthand for a single topic (/events/my-topic).
	topics := r.URL.Query()["topic"]
	if len(topics) == 0 {
		// Fall back to the URL path as a single topic name.
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

	// Phase 4: Last-ID / replay.
	// Parse the client's cursor before writing any response bytes so that
	// invalid values produce a clean 400 Bad Request instead of a broken stream.
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

	// Phase 5: JetStream subscription setup.
	// Before switching to streaming mode we need two things:
	// a) a Flusher — required to push partial writes to the client;
	// b) active NATS subscriptions — so setup errors produce normal HTTP errors.
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return nil
	}

	// Grab the JetStream context under read-lock; Provision/Cleanup write to it.
	h.mu.RLock()
	js := h.js
	h.mu.RUnlock()

	if js == nil {
		http.Error(w, "JetStream not available", http.StatusServiceUnavailable)
		return nil
	}

	// --- Channel and callback setup ---
	//
	// msgChan:     buffered channel delivering NATS messages to the select loop.
	// done:        closed during cleanup to tell callbacks to stop enqueuing.
	// slowClient:  signals when msgChan is full and a message would be dropped;
	//              the select loop disconnects the client to prevent silent loss.
	msgChan := make(chan *nats.Msg, 64)
	done := make(chan struct{})
	slowClient := make(chan string, 1)

	// enqueueMessage is the callback passed to js.Subscribe(). NATS calls it
	// on a separate goroutine for every incoming message. It tries to push the
	// message into msgChan; if the buffer is full the client is "slow" and we
	// signal disconnection instead of silently dropping events.
	enqueueMessage := func(msg *nats.Msg) {
		select {
		case <-done:
			// Subscription is being torn down — discard the message.
			return
		case msgChan <- msg:
			// Successfully queued.
		default:
			// Buffer full — notify the select loop to disconnect this client.
			select {
			case slowClient <- msg.Subject:
			default:
				// Signal already pending; no need to send again.
			}
		}
	}

	var subscriptions []*nats.Subscription
	var subscribedTopics []string
	var failedTopics []string
	cleanupSubscriptions := func() {
		// Close done FIRST so that any callback currently running in
		// enqueueMessage sees the signal and returns instead of writing
		// to a channel that nobody will read from. Then Unsubscribe each
		// subscription. Buffered messages in msgChan become garbage-collected
		// when the channel goes out of scope.
		close(done)
		for _, sub := range subscriptions {
			if err := sub.Unsubscribe(); err != nil {
				h.logger.Warn("failed to unsubscribe",
					zap.String("topic", sub.Subject),
					zap.Error(err))
			}
		}
	}

	// Subscribe to each requested topic via JetStream.
	for _, topic := range topics {
		fullTopic := h.TopicPrefix + topic

		// Build options for an *ephemeral* consumer (no durable name).
		// BindStream ties the subscription to our pre-existing stream.
		// AckNone means we never acknowledge messages — we are read-only
		// viewers, not a processing queue.
		opts := []nats.SubOpt{
			nats.BindStream(h.StreamName),
			nats.AckNone(),
		}

		// Decide where in the stream to start reading:
		// • If the client sent a last-id, resume from the NEXT sequence.
		// • Otherwise, only deliver messages published from now on.
		if hasLastID {
			opts = append(opts, nats.StartSequence(lastID+1))
			h.logger.Debug("subscribing from sequence",
				zap.String("topic", fullTopic),
				zap.Uint64("start_sequence", lastID+1),
			)
		} else {
			opts = append(opts, nats.DeliverNew())
		}

		sub, err := js.Subscribe(fullTopic, enqueueMessage, opts...)

		if err != nil {
			// The requested sequence may have been purged from the stream
			// (e.g., by a retention policy or manual purge). In that case,
			// fall back to replaying ALL retained messages so the client
			// does not miss anything that is still available.
			//
			// We use substring matching on the error message because NATS
			// does not expose a typed error for this, and the wording may
			// change between server versions.
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

	// --- Phase 6: Streaming mode ---
	// All setup succeeded. Switch to SSE by writing the proper headers.
	// From this point on we can only communicate via the SSE wire format.
	h.setCORSHeaders(w, r)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Tell nginx/similar not to buffer

	// Send an initial "connected" event so the client knows which topics
	// were successfully subscribed and can render a ready state.
	if err := writeSSEChunk(w, flusher, fmt.Sprintf("event: connected\ndata: {\"topics\":%s}\n\n", toJSON(subscribedTopics))); err != nil {
		h.logger.Debug("failed to write connected event", zap.Error(err))
		return nil
	}

	// Heartbeat ticker — fires periodically to send a keep-alive comment.
	// Without this, reverse proxies or load balancers may close the
	// connection after their idle timeout.
	heartbeat := time.NewTicker(time.Duration(h.HeartbeatInterval) * time.Second)
	defer heartbeat.Stop()

	// Main select loop — multiplexes four event sources:
	//   slowClient:  msgChan overflowed — disconnect before dropping messages.
	//   ctx.Done():  the browser closed the connection.
	//   msgChan:     a new NATS message is ready to forward as an SSE event.
	//   heartbeat.C: time to send a keep-alive comment.
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
			// Strip the TopicPrefix so the client sees the short topic name.
			eventTopic := strings.TrimPrefix(msg.Subject, h.TopicPrefix)
			eventTime := time.Now().UTC().Format(time.RFC3339)
			payload := messageEventPayload{
				Topic:   eventTopic,
				Payload: tryParseJSON(msg.Data),
				Time:    eventTime,
			}
			var eventID uint64
			hasEventID := false

			// When available, JetStream metadata gives us two things:
			// 1) The stream sequence number — used as the SSE event ID for replay.
			// 2) The original publish timestamp — more accurate than time.Now().
			meta, metaErr := msg.Metadata()
			if metaErr != nil {
				h.logger.Warn("failed to read JetStream metadata", zap.String("topic", msg.Subject), zap.Error(metaErr))
			} else {
				eventTime = meta.Timestamp.UTC().Format(time.RFC3339)
				payload.Time = eventTime
				eventID = meta.Sequence.Stream
				hasEventID = true
			}

			// Build the SSE frame. The format is:
			//   id: <sequence>\n      (optional, enables replay)
			//   event: message\n
			//   data: <json>\n\n
			var event strings.Builder
			if hasEventID {
				event.WriteString("id: ")
				event.WriteString(strconv.FormatUint(eventID, 10))
				event.WriteString("\n")
			}
			event.WriteString("event: message\n")
			event.WriteString("data: ")
			event.WriteString(toJSON(payload))
			event.WriteString("\n\n")

			// Guard against oversized events. A malicious or misconfigured
			// producer could publish huge messages; dropping them here
			// protects browser clients from excessive memory usage.
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
			// SSE comments (lines starting with ":") are ignored by the
			// browser's EventSource API but keep the TCP connection alive.
			if err := writeSSEChunk(w, flusher, fmt.Sprintf(": heartbeat %s\n\n", time.Now().UTC().Format(time.RFC3339))); err != nil {
				h.logger.Debug("failed to write heartbeat", zap.Error(err))
				return nil
			}
		}
	}
}

// setCORSHeaders sets CORS response headers when the request includes an
// Origin header. Instead of blindly sending "Access-Control-Allow-Origin: *",
// we echo back the specific origin that matched our AllowedOrigins list.
// This is required because browsers reject wildcard origins on requests
// that carry credentials (cookies, authorization headers, etc.).
func (h *Handler) setCORSHeaders(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return // Same-origin request; CORS headers not needed.
	}

	// Walk the allow-list and echo the first match.
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
