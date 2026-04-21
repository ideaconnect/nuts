// serve.go — HTTP/SSE request handling.
package nuts

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// ServeHTTP implements the caddyhttp.MiddlewareHandler interface.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	// Phase 0: Health check.
	if r.Method == http.MethodGet && h.matchesHealthPath(r.URL.Path) {
		return h.serveHealthCheck(w)
	}

	// Phase 1: CORS preflight.
	if r.Method == http.MethodOptions {
		h.setCORSHeaders(w, r)
		w.WriteHeader(http.StatusNoContent)
		return nil
	}

	// Phase 2: Method check.
	if r.Method != http.MethodGet {
		if next == nil {
			allow := "GET, OPTIONS"
			if len(h.AllowedMethods) > 0 {
				allow = strings.Join(h.AllowedMethods, ", ")
			}
			w.Header().Set("Allow", allow)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return nil
		}
		return next.ServeHTTP(w, r)
	}

	// Phase 3: Topic extraction.
	topics := r.URL.Query()["topic"]
	if len(topics) == 0 {
		// Path shorthand: convert remaining path into a dotted topic.
		// "/orders/new" → "orders.new". Operators behind a `route /events*`
		// matcher should put `uri strip_prefix /events` before `nuts` so the
		// matched prefix is removed from r.URL.Path.
		path := strings.Trim(r.URL.Path, "/")
		if path != "" {
			topics = []string{strings.ReplaceAll(path, "/", ".")}
		}
	}

	// Reject topics that contain illegal characters.
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
	// invalid values produce a clean 400 (for explicit ?last-id=) or a
	// fall-through to DeliverNew (for the auto-set Last-Event-ID header).
	lastIDStr := r.URL.Query().Get("last-id")
	queryProvided := lastIDStr != ""
	if lastIDStr == "" {
		lastIDStr = r.Header.Get("Last-Event-ID")
	}
	var lastID uint64
	var hasLastID bool
	if lastIDStr != "" {
		parsedID, err := strconv.ParseUint(lastIDStr, 10, 64)
		if err != nil {
			if queryProvided {
				http.Error(w, "Invalid last-id value: must be a positive integer", http.StatusBadRequest)
				return nil
			}
			// Browser-supplied Last-Event-ID is unparseable. Fall back to
			// DeliverNew so the client does not loop forever on a bad value.
			h.logger.Warn("ignoring unparseable Last-Event-ID header; resuming with DeliverNew",
				zap.String("value", lastIDStr),
				zap.Error(err))
		} else {
			lastID = parsedID
			hasLastID = true
			metricsReplayRequests.Inc()
		}
	}

	// Phase 5: JetStream subscription setup.
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return nil
	}

	h.mu.RLock()
	js := h.js
	h.mu.RUnlock()

	if js == nil {
		http.Error(w, "JetStream not available", http.StatusServiceUnavailable)
		return nil
	}

	// Connection cap. Reserve the slot before opening subscriptions so we
	// don't churn JetStream consumers for rejected requests.
	if h.MaxConnections > 0 {
		if !h.reserveConnSlot() {
			metricsConnectionsRejected.WithLabelValues("max_connections").Inc()
			w.Header().Set("Retry-After", "5")
			http.Error(w, "Too many concurrent connections", http.StatusServiceUnavailable)
			return nil
		}
		defer h.releaseConnSlot()
	}

	bufSize := h.ClientBufferSize
	if bufSize <= 0 {
		bufSize = defaultClientBufferSize
	}
	msgChan := make(chan *nats.Msg, bufSize)
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

		opts := []nats.SubOpt{
			nats.BindStream(h.StreamName),
			nats.AckNone(),
		}
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
			errMsg := err.Error()
			if hasLastID && (strings.Contains(errMsg, "start sequence") ||
				strings.Contains(errMsg, "sequence not found")) {
				metricsReplayFallbacks.Inc()
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
				metricsSubscriptionErrors.Inc()
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
	metricsActiveConnections.Inc()
	defer metricsActiveConnections.Dec()

	h.setCORSHeaders(w, r)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	if h.HubURL != "" {
		w.Header().Set("Link", fmt.Sprintf("<%s>; rel=\"nuts\"", h.HubURL))
	}

	if err := writeSSEChunk(w, flusher, fmt.Sprintf("event: connected\ndata: {\"topics\":%s}\n\n", toJSON(subscribedTopics))); err != nil {
		h.logger.Debug("failed to write connected event", zap.Error(err))
		return nil
	}

	heartbeat := time.NewTicker(time.Duration(h.HeartbeatInterval) * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case slowTopic := <-slowClient:
			metricsSlowClientDisconnects.Inc()
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

			// Cheap pre-check: drop oversized messages BEFORE parsing JSON
			// so a hostile producer cannot force unbounded allocations.
			// MaxEventSize < 0 means "unlimited".
			if h.MaxEventSize > 0 && len(msg.Data) > h.MaxEventSize {
				metricsMessagesDropped.Inc()
				h.logger.Warn("dropping oversized NATS payload",
					zap.String("topic", msg.Subject),
					zap.Int("payload_size", len(msg.Data)),
					zap.Int("max_event_size", h.MaxEventSize),
				)
				continue
			}

			eventTopic := strings.TrimPrefix(msg.Subject, h.TopicPrefix)
			payload := messageEventPayload{
				Topic:   eventTopic,
				Payload: tryParseJSON(msg.Data),
				Time:    time.Now().UTC().Format(time.RFC3339),
			}
			var eventID uint64
			hasEventID := false

			meta, metaErr := msg.Metadata()
			if metaErr != nil {
				h.logger.Warn("failed to read JetStream metadata", zap.String("topic", msg.Subject), zap.Error(metaErr))
			} else {
				payload.Time = meta.Timestamp.UTC().Format(time.RFC3339)
				eventID = meta.Sequence.Stream
				hasEventID = true
			}

			var event strings.Builder
			event.Grow(len(msg.Data) + 128)
			if hasEventID {
				event.WriteString("id: ")
				event.WriteString(strconv.FormatUint(eventID, 10))
				event.WriteString("\n")
			}
			event.WriteString("event: message\n")
			event.WriteString("data: ")
			event.WriteString(toJSON(payload))
			event.WriteString("\n\n")

			// Final guard against oversized formatted events (JSON inflation).
			if h.MaxEventSize > 0 && event.Len() > h.MaxEventSize {
				metricsMessagesDropped.Inc()
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
			metricsMessagesDelivered.Inc()

		case <-heartbeat.C:
			if err := writeSSEChunk(w, flusher, fmt.Sprintf(": heartbeat %s\n\n", time.Now().UTC().Format(time.RFC3339))); err != nil {
				h.logger.Debug("failed to write heartbeat", zap.Error(err))
				return nil
			}
		}
	}
}

// matchesHealthPath returns true when the request path equals HealthPath or
// ends with HealthPath as a full path segment.
func (h *Handler) matchesHealthPath(reqPath string) bool {
	hp := h.HealthPath
	if hp == "" {
		hp = defaultHealthPath
	}
	if !strings.HasPrefix(hp, "/") {
		hp = "/" + hp
	}
	if reqPath == hp {
		return true
	}
	return strings.HasSuffix(reqPath, hp)
}

// reserveConnSlot atomically tries to reserve a connection slot.
func (h *Handler) reserveConnSlot() bool {
	for {
		cur := atomic.LoadInt64(&h.connCount)
		if int(cur) >= h.MaxConnections {
			return false
		}
		if atomic.CompareAndSwapInt64(&h.connCount, cur, cur+1) {
			return true
		}
	}
}

func (h *Handler) releaseConnSlot() {
	atomic.AddInt64(&h.connCount, -1)
}

// setCORSHeaders sets CORS response headers when the request includes an
// Origin header.
func (h *Handler) setCORSHeaders(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	methods := "GET, OPTIONS"
	if len(h.AllowedMethods) > 0 {
		methods = strings.Join(h.AllowedMethods, ", ")
	}
	headers := "Cache-Control, Last-Event-ID"
	if len(h.AllowedHeaders) > 0 {
		headers = strings.Join(h.AllowedHeaders, ", ")
	}
	for _, allowed := range h.AllowedOrigins {
		if allowed == "*" || allowed == origin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", methods)
			w.Header().Set("Access-Control-Allow-Headers", headers)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			break
		}
	}
}

// serveHealthCheck responds with a JSON health status.
func (h *Handler) serveHealthCheck(w http.ResponseWriter) error {
	type healthResponse struct {
		Status string `json:"status"`
		NATS   string `json:"nats"`
		Stream string `json:"stream"`
	}

	resp := healthResponse{
		Status: "ok",
		NATS:   "connected",
		Stream: "available",
	}
	statusCode := http.StatusOK

	h.mu.RLock()
	conn := h.conn
	js := h.js
	h.mu.RUnlock()

	if conn == nil || !conn.IsConnected() {
		resp.Status = "degraded"
		resp.NATS = "disconnected"
		statusCode = http.StatusServiceUnavailable
	}

	if js == nil {
		resp.Status = "degraded"
		resp.Stream = "unavailable"
		statusCode = http.StatusServiceUnavailable
	} else {
		_, err := js.StreamInfo(h.StreamName)
		if err != nil {
			resp.Status = "degraded"
			resp.Stream = "unavailable"
			statusCode = http.StatusServiceUnavailable
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.Debug("failed to encode health response", zap.Error(err))
	}
	return nil
}
