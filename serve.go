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

const maxReplayCursor = ^uint64(0)

func isReplayStartSequenceError(err error, hasLastID bool) bool {
	if err == nil || !hasLastID {
		return false
	}
	errMsg := err.Error()
	return strings.Contains(errMsg, "start sequence") || strings.Contains(errMsg, "sequence not found")
}

// ServeHTTP implements the caddyhttp.MiddlewareHandler interface.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	// Health check.
	if r.Method == http.MethodGet && h.matchesHealthPath(r.URL.Path) {
		return h.serveHealthCheck(w)
	}

	// CORS preflight.
	if r.Method == http.MethodOptions {
		h.setCORSHeaders(w, r)
		w.WriteHeader(http.StatusNoContent)
		return nil
	}

	// Method check.
	if r.Method != http.MethodGet {
		if next == nil {
			w.Header().Set("Allow", allowedMethodsHeader(h.AllowedMethods))
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return nil
		}
		return next.ServeHTTP(w, r)
	}

	// Topic extraction.
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

	// Last-ID / replay.
	// Parse the client's cursor before writing any response bytes so that
	// invalid values produce a clean 400 (for explicit ?last-id=) or a
	// fall-through to DeliverNew (for the auto-set Last-Event-ID header).
	lastIDStr := r.URL.Query().Get("last-id")
	queryProvided := lastIDStr != ""
	if lastIDStr == "" {
		lastIDStr = r.Header.Get("Last-Event-ID")
	}
	var nextSequence uint64
	var hasLastID bool
	if lastIDStr != "" {
		parsedID, err := strconv.ParseUint(lastIDStr, 10, 64)
		if err != nil || parsedID == maxReplayCursor {
			if queryProvided {
				http.Error(w, "Invalid last-id value: must be an unsigned integer below the maximum cursor value", http.StatusBadRequest)
				return nil
			}
			// Browser-supplied Last-Event-ID is unparseable. Fall back to
			// DeliverNew so the client does not loop forever on a bad value.
			fields := []zap.Field{zap.String("value", lastIDStr)}
			if err != nil {
				fields = append(fields, zap.Error(err))
			} else {
				fields = append(fields, zap.String("reason", "cursor would overflow"))
			}
			h.logger.Warn("ignoring unparseable Last-Event-ID header; resuming with DeliverNew",
				fields...)
		} else {
			nextSequence = parsedID + 1
			hasLastID = true
			metricsReplayRequests.Inc()
		}
	}

	// JetStream subscription setup.
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return nil
	}

	h.mu.RLock()
	conn := h.conn
	js := h.js
	shutdown := h.shutdown
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
			// msgChan is full — this client is slow. The consumer loop
			// reads slowClient at the top of its select and disconnects
			// as soon as it sees a signal; the client then reconnects and
			// JetStream replays anything we did not forward. We must not
			// silently discard the signal: a lost signal would leave the
			// client connected with a full buffer, seeing no progress and
			// no disconnect. Block (typically microseconds) until either
			// the signal is accepted or the handler tears down and closes
			// done, so every overflow ends in a disconnect, never a
			// silent stall.
			select {
			case slowClient <- msg.Subject:
			case <-done:
			}
		}
	}

	var subscriptions []*nats.Subscription
	var subscribedTopics []string
	var failedTopics []string
	replayFellBack := false
	requestedSubjects := make(map[string]struct{}, len(topics))
	var fullTopics []string
	for _, topic := range topics {
		fullTopic := h.TopicPrefix + topic
		if _, exists := requestedSubjects[fullTopic]; exists {
			continue
		}
		requestedSubjects[fullTopic] = struct{}{}
		fullTopics = append(fullTopics, fullTopic)
		subscribedTopics = append(subscribedTopics, topic)
	}
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
	enqueueRequestedMessage := func(msg *nats.Msg) {
		if _, ok := requestedSubjects[msg.Subject]; !ok {
			return
		}
		enqueueMessage(msg)
	}

	// Pre-check the stream's retained range so we can detect "requested
	// sequence is below the purged frontier" before Subscribe() silently
	// starts us at FirstSeq and replays everything. This is the common
	// replay-storm trigger in modern NATS — StartSequence(N) where N <
	// FirstSeq does not error.
	var streamFirstSeq uint64
	var streamSubjects []string
	if hasLastID || len(fullTopics) > 1 {
		if info, infoErr := js.StreamInfo(h.StreamName); infoErr == nil {
			streamFirstSeq = info.State.FirstSeq
			streamSubjects = info.Config.Subjects
		} else {
			h.logger.Debug("failed to read StreamInfo for request pre-check",
				zap.Error(infoErr))
		}
	}
	if len(fullTopics) > 1 && len(streamSubjects) > 0 {
		for idx, fullTopic := range fullTopics {
			if !subjectAllowedByStream(fullTopic, streamSubjects) {
				failedTopics = append(failedTopics, subscribedTopics[idx])
			}
		}
		if len(failedTopics) > 0 {
			cleanupSubscriptions()
			http.Error(w, fmt.Sprintf("Failed to subscribe to requested topics: %s", strings.Join(failedTopics, ", ")), http.StatusServiceUnavailable)
			return nil
		}
	}

	buildFallbackOpts := func(subjectLabel string, requested uint64, reason string) []nats.SubOpt {
		metricsReplayFallbacks.Inc()
		replayFellBack = true
		opts := []nats.SubOpt{
			nats.BindStream(h.StreamName),
			nats.AckNone(),
		}
		if h.ReplayWindow > 0 {
			start := time.Now().Add(-time.Duration(h.ReplayWindow) * time.Second)
			opts = append(opts, nats.StartTime(start))
			h.logger.Warn("replay fallback: using time-bounded window",
				zap.String("topic", subjectLabel),
				zap.Uint64("requested_sequence", requested),
				zap.String("reason", reason),
				zap.Int("replay_window_seconds", h.ReplayWindow),
			)
		} else {
			opts = append(opts, nats.DeliverAll())
			h.logger.Warn("replay fallback: delivering all retained messages",
				zap.String("topic", subjectLabel),
				zap.Uint64("requested_sequence", requested),
				zap.String("reason", reason),
			)
		}
		return opts
	}
	buildSubscriptionOpts := func(subjectLabel string) ([]nats.SubOpt, bool) {
		var opts []nats.SubOpt
		belowRetention := hasLastID && streamFirstSeq > 0 && nextSequence < streamFirstSeq

		switch {
		case belowRetention:
			opts = buildFallbackOpts(subjectLabel, nextSequence, "sequence below retention")
		case hasLastID:
			opts = []nats.SubOpt{
				nats.BindStream(h.StreamName),
				nats.AckNone(),
				nats.StartSequence(nextSequence),
			}
			h.logger.Debug("subscribing from sequence",
				zap.String("topic", subjectLabel),
				zap.Uint64("start_sequence", nextSequence),
			)
		default:
			opts = []nats.SubOpt{
				nats.BindStream(h.StreamName),
				nats.AckNone(),
				nats.DeliverNew(),
			}
		}
		return opts, belowRetention
	}

	if len(fullTopics) == 1 {
		fullTopic := fullTopics[0]
		opts, belowRetention := buildSubscriptionOpts(fullTopic)
		sub, err := js.Subscribe(fullTopic, enqueueMessage, opts...)

		// Belt-and-suspenders: preserved error-string fallback for the rare
		// case where Subscribe rejects StartSequence instead of silently
		// adjusting. No-op on modern NATS but harmless.
		if err != nil && !belowRetention && isReplayStartSequenceError(err, hasLastID) {
			sub, err = js.Subscribe(fullTopic, enqueueMessage,
				buildFallbackOpts(fullTopic, nextSequence, "subscribe-time start sequence error")...)
		}

		if err != nil {
			metricsSubscriptionErrors.Inc()
			h.logger.Error("failed to subscribe to topic",
				zap.String("topic", fullTopic),
				zap.Error(err),
			)
			failedTopics = append(failedTopics, subscribedTopics[0])
		} else {
			subscriptions = append(subscriptions, sub)
			h.logger.Debug("subscribed to topic", zap.String("topic", fullTopic))
		}
	} else {
		subjectLabel := strings.Join(fullTopics, ",")
		opts, belowRetention := buildSubscriptionOpts(subjectLabel)
		sub, err := h.subscribeToMultipleTopics(js, conn, fullTopics, opts, enqueueRequestedMessage)
		if err != nil && !belowRetention && isReplayStartSequenceError(err, hasLastID) {
			sub, err = h.subscribeToMultipleTopics(js, conn, fullTopics,
				buildFallbackOpts(subjectLabel, nextSequence, "subscribe-time start sequence error"), enqueueRequestedMessage)
		}
		if err != nil {
			metricsSubscriptionErrors.Inc()
			h.logger.Error("failed to subscribe to topics",
				zap.Strings("topics", fullTopics),
				zap.Error(err),
			)
			failedTopics = append(failedTopics, subscribedTopics...)
		} else {
			subscriptions = append(subscriptions, sub)
			h.logger.Debug("subscribed to topics", zap.Strings("topics", fullTopics))
		}
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

	// --- Streaming mode ---
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

	// replayDelivered counts messages delivered on a fallback subscription.
	// It is only consulted when replayFellBack && ReplayMaxMessages > 0.
	replayDelivered := 0

	ctx := r.Context()
	for {
		select {
		case <-shutdown:
			h.logger.Debug("handler shutting down; closing SSE stream")
			return nil

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

			if replayFellBack && h.ReplayMaxMessages > 0 {
				replayDelivered++
				if replayDelivered >= h.ReplayMaxMessages {
					metricsReplayCapReached.Inc()
					h.logger.Warn("closing SSE stream: replay_max_messages reached",
						zap.Int("replay_max_messages", h.ReplayMaxMessages),
					)
					return nil
				}
			}

		case <-heartbeat.C:
			if err := writeSSEChunk(w, flusher, fmt.Sprintf(": heartbeat %s\n\n", time.Now().UTC().Format(time.RFC3339))); err != nil {
				h.logger.Debug("failed to write heartbeat", zap.Error(err))
				return nil
			}
		}
	}
}

func (h *Handler) subscribeToMultipleTopics(js nats.JetStreamContext, conn *nats.Conn, fullTopics []string, opts []nats.SubOpt, cb nats.MsgHandler) (*nats.Subscription, error) {
	if supportsMultiFilterSubjects(conn) {
		filterOpts := append([]nats.SubOpt{}, opts...)
		filterOpts = append(filterOpts, nats.ConsumerFilterSubjects(fullTopics...))
		return js.Subscribe("", cb, filterOpts...)
	}

	wildcardSubject := commonSubjectFilter(fullTopics)
	h.logger.Warn("NATS server does not support multi-filter consumers; using common wildcard subscription",
		zap.Strings("topics", fullTopics),
		zap.String("wildcard_subject", wildcardSubject),
		zap.String("server_version", connectedServerVersion(conn)),
	)
	return js.Subscribe(wildcardSubject, cb, opts...)
}

func supportsMultiFilterSubjects(conn *nats.Conn) bool {
	version := connectedServerVersion(conn)
	major, minor, ok := parseMajorMinorVersion(version)
	if !ok {
		return false
	}
	return major > 2 || (major == 2 && minor >= 10)
}

func connectedServerVersion(conn *nats.Conn) string {
	if conn == nil {
		return ""
	}
	return conn.ConnectedServerVersion()
}

func parseMajorMinorVersion(version string) (int, int, bool) {
	version = strings.TrimPrefix(version, "v")
	if cut := strings.IndexAny(version, "-+"); cut >= 0 {
		version = version[:cut]
	}
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

func commonSubjectFilter(subjects []string) string {
	if len(subjects) == 0 {
		return ">"
	}
	common := strings.Split(subjects[0], ".")
	for _, subject := range subjects[1:] {
		parts := strings.Split(subject, ".")
		limit := len(common)
		if len(parts) < limit {
			limit = len(parts)
		}
		idx := 0
		for idx < limit && common[idx] == parts[idx] {
			idx++
		}
		common = common[:idx]
		if len(common) == 0 {
			return ">"
		}
	}
	for _, subject := range subjects {
		if len(strings.Split(subject, ".")) == len(common) {
			common = common[:len(common)-1]
			break
		}
	}
	if len(common) == 0 {
		return ">"
	}
	return strings.Join(common, ".") + ".>"
}

func subjectAllowedByStream(subject string, streamSubjects []string) bool {
	for _, streamSubject := range streamSubjects {
		if subjectMatchesFilter(subject, streamSubject) {
			return true
		}
	}
	return false
}

func subjectMatchesFilter(subject, filter string) bool {
	subjectTokens := strings.Split(subject, ".")
	filterTokens := strings.Split(filter, ".")
	for idx, filterToken := range filterTokens {
		if filterToken == ">" {
			return idx < len(subjectTokens)
		}
		if idx >= len(subjectTokens) {
			return false
		}
		if filterToken != "*" && filterToken != subjectTokens[idx] {
			return false
		}
	}
	return len(subjectTokens) == len(filterTokens)
}

// matchesHealthPath returns true when the request path equals HealthPath or
// ends with HealthPath as a full path segment. Because HealthPath is
// normalised to start with '/', a plain HasSuffix check already enforces
// the segment boundary — "/eventshealthz" does not HasSuffix "/healthz".
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

func allowedMethodsHeader(methods []string) string {
	if len(methods) == 0 {
		return "GET, OPTIONS"
	}
	seen := make(map[string]struct{}, len(methods))
	allowed := make([]string, 0, 2)
	for _, method := range methods {
		method = strings.ToUpper(method)
		switch method {
		case http.MethodGet, http.MethodOptions:
			if _, exists := seen[method]; exists {
				continue
			}
			seen[method] = struct{}{}
			allowed = append(allowed, method)
		}
	}
	if len(allowed) == 0 {
		return "GET, OPTIONS"
	}
	return strings.Join(allowed, ", ")
}

// setCORSHeaders sets CORS response headers when the request includes an
// Origin header.
//
// Access-Control-Allow-Credentials is only advertised when the request Origin
// is explicitly allow-listed. Setting it alongside a wildcard match would let
// any browser-visited origin attach cookies or Authorization headers to an
// SSE subscription, defeating the point of opt-in CORS. Operators who need
// credentialed flows must list explicit origins in allowed_origins.
//
// Vary: Origin is added whenever the response reflects the request origin so
// that shared caches don't serve headers keyed to one origin to a different
// origin.
func (h *Handler) setCORSHeaders(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	methods := allowedMethodsHeader(h.AllowedMethods)
	headers := "Cache-Control, Last-Event-ID"
	if len(h.AllowedHeaders) > 0 {
		headers = strings.Join(h.AllowedHeaders, ", ")
	}
	var wildcard, explicit bool
	for _, allowed := range h.AllowedOrigins {
		if allowed == origin {
			explicit = true
			break
		}
		if allowed == "*" {
			wildcard = true
		}
	}
	if !explicit && !wildcard {
		return
	}
	w.Header().Add("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", methods)
	w.Header().Set("Access-Control-Allow-Headers", headers)
	if explicit {
		w.Header().Set("Access-Control-Allow-Credentials", "true")
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
