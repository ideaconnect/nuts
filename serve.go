// serve.go — HTTP/SSE request handling.
package nuts

import (
	"encoding/json"
	"errors"
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

const (
	maxReplayCursor = ^uint64(0)

	// nats.go v1.37 exposes APIError but not this server error-code constant.
	jsErrCodeSequenceNotFound nats.ErrorCode = 10043
)

type replayMode string

const (
	replayModeDeliverNew          replayMode = "deliver_new"
	replayModeStartSequence       replayMode = "start_sequence"
	replayModeFallbackDeliverAll  replayMode = "fallback_deliver_all"
	replayModeFallbackStartTime   replayMode = "fallback_start_time"
	dropReasonRawPayload          string     = "raw_payload"
	dropReasonFormattedSSEMessage string     = "formatted_sse_message"
)

func (m replayMode) isFallback() bool {
	return m == replayModeFallbackDeliverAll || m == replayModeFallbackStartTime
}

type replayPlan struct {
	HasLastID      bool
	Mode           replayMode
	StartSequence  uint64
	StartTime      time.Time
	CapSequence    uint64
	FallbackReason string
}

type streamPlan struct {
	Topics            []string
	FullSubjects      []string
	RequestedSubjects map[string]struct{}
	Replay            replayPlan
	FailedTopics      []string
}

func (p streamPlan) subjectLabel() string {
	return strings.Join(p.FullSubjects, ",")
}

func streamLogFields(plan streamPlan) []zap.Field {
	fields := []zap.Field{
		zap.Strings("topics", plan.Topics),
		zap.Strings("subjects", plan.FullSubjects),
		zap.String("subject_label", plan.subjectLabel()),
		zap.String("replay_mode", string(plan.Replay.Mode)),
		zap.Bool("replay_has_last_id", plan.Replay.HasLastID),
	}
	if plan.Replay.StartSequence > 0 {
		fields = append(fields, zap.Uint64("replay_start_sequence", plan.Replay.StartSequence))
	}
	if plan.Replay.FallbackReason != "" {
		fields = append(fields, zap.String("replay_fallback_reason", plan.Replay.FallbackReason))
	}
	return fields
}

func appendStreamLogFields(plan streamPlan, fields ...zap.Field) []zap.Field {
	return append(streamLogFields(plan), fields...)
}

type streamInfoSnapshot struct {
	FirstSeq             uint64
	LastSeq              uint64
	Subjects             []string
	StartSequenceTime    time.Time
	HasStartSequenceTime bool
}

type streamRuntime struct {
	conn     *nats.Conn
	js       nats.JetStreamContext
	shutdown <-chan struct{}
}

type streamRequestError struct {
	status  int
	message string
}

func (e *streamRequestError) write(w http.ResponseWriter) {
	if e.status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="nuts"`)
	}
	http.Error(w, e.message, e.status)
}

type subscriptionResult struct {
	Subscriptions []*nats.Subscription
	FailedTopics  []string
}

type formattedMessageEvent struct {
	Frame             string
	Subject           string
	StreamSequence    uint64
	HasStreamSequence bool
	MessageTime       time.Time
	HasMessageTime    bool
	Dropped           bool
	DropReason        string
	DropSize          int
	MetadataErr       error
}

func isReplayStartSequenceError(err error, hasLastID bool) bool {
	if err == nil || !hasLastID {
		return false
	}
	var apiErr *nats.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode == jsErrCodeSequenceNotFound {
		return true
	}
	var sequenceMismatch *nats.ErrConsumerSequenceMismatch
	return errors.As(err, &sequenceMismatch)
}

// ServeHTTP implements the caddyhttp.MiddlewareHandler interface.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if handled, err := h.handleControlRequest(w, r, next); handled {
		return err
	}

	plan, requestErr := h.parseStreamRequest(r)
	if requestErr != nil {
		requestErr.write(w)
		return nil
	}
	if authErr := h.authorizeStreamRequest(r, plan); authErr != nil {
		authErr.write(w)
		return nil
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return nil
	}

	runtime := h.currentStreamRuntime()
	if runtime.js == nil {
		if h.logger != nil {
			h.logger.Warn("JetStream not available for SSE stream",
				appendStreamLogFields(plan, zap.String("disconnect_reason", "jetstream_unavailable"))...,
			)
		}
		http.Error(w, "JetStream not available", http.StatusServiceUnavailable)
		return nil
	}

	if h.MaxConnections > 0 {
		if !h.reserveConnSlot() {
			metricsConnectionsRejected.WithLabelValues("max_connections").Inc()
			if h.logger != nil {
				h.logger.Warn("rejecting SSE stream: max_connections reached",
					appendStreamLogFields(plan,
						zap.String("reject_reason", "max_connections"),
						zap.Int("max_connections", h.MaxConnections),
					)...,
				)
			}
			w.Header().Set("Retry-After", "5")
			http.Error(w, "Too many concurrent connections", http.StatusServiceUnavailable)
			return nil
		}
		defer h.releaseConnSlot()
	}

	msgChan, slowClient, done, enqueueMessage := h.newMessageQueue()
	snapshot := h.readStreamSnapshot(runtime.js, plan)
	plan = h.planSubscription(plan, snapshot)
	enqueueRequestedMessage := plan.requestedMessageHandler(enqueueMessage)

	result := h.executeSubscriptionPlan(runtime.js, runtime.conn, plan, enqueueMessage, enqueueRequestedMessage)
	if len(result.FailedTopics) > 0 {
		if h.logger != nil {
			h.logger.Warn("failed to subscribe to requested SSE topics",
				appendStreamLogFields(plan,
					zap.Strings("failed_topics", result.FailedTopics),
					zap.String("disconnect_reason", "subscription_failed"),
				)...,
			)
		}
		h.cleanupStream(done, result.Subscriptions)
		http.Error(w, fmt.Sprintf("Failed to subscribe to requested topics: %s", strings.Join(result.FailedTopics, ", ")), http.StatusServiceUnavailable)
		return nil
	}
	if len(result.Subscriptions) == 0 {
		if h.logger != nil {
			h.logger.Warn("failed to subscribe to any requested SSE topics",
				appendStreamLogFields(plan, zap.String("disconnect_reason", "subscription_empty"))...,
			)
		}
		h.cleanupStream(done, nil)
		http.Error(w, "Failed to subscribe to any requested topics", http.StatusServiceUnavailable)
		return nil
	}
	defer h.cleanupStream(done, result.Subscriptions)

	return h.serveStream(w, flusher, r, plan, msgChan, slowClient, runtime.shutdown)
}

func (h *Handler) handleControlRequest(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) (bool, error) {
	if r.Method == http.MethodGet && h.matchesLivePath(r.URL.Path) {
		return true, h.serveLiveCheck(w)
	}

	if r.Method == http.MethodGet && h.matchesReadyPath(r.URL.Path) {
		return true, h.serveReadinessCheck(w)
	}

	if r.Method == http.MethodGet && h.matchesHealthPath(r.URL.Path) {
		return true, h.serveReadinessCheck(w)
	}

	if r.Method == http.MethodOptions {
		h.setCORSHeaders(w, r)
		w.WriteHeader(http.StatusNoContent)
		return true, nil
	}

	if r.Method != http.MethodGet {
		if next == nil {
			w.Header().Set("Allow", allowedMethodsHeader(h.AllowedMethods))
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return true, nil
		}
		return true, next.ServeHTTP(w, r)
	}
	return false, nil
}

func (h *Handler) parseStreamRequest(r *http.Request) (streamPlan, *streamRequestError) {
	topics := r.URL.Query()["topic"]
	if len(topics) == 0 {
		path := strings.Trim(r.URL.Path, "/")
		if path != "" {
			topics = []string{strings.ReplaceAll(path, "/", ".")}
		}
	}

	// Reject topics that contain illegal characters.
	for _, t := range topics {
		if !isValidTopic(t) {
			return streamPlan{}, &streamRequestError{status: http.StatusBadRequest, message: "Invalid topic name"}
		}
	}

	if len(topics) == 0 {
		return streamPlan{}, &streamRequestError{status: http.StatusBadRequest, message: "No topics specified. Use ?topic=name or path-based topic"}
	}

	plan := streamPlan{
		RequestedSubjects: make(map[string]struct{}, len(topics)),
		Replay:            replayPlan{Mode: replayModeDeliverNew},
	}
	for _, topic := range topics {
		fullSubject := h.TopicPrefix + topic
		if _, exists := plan.RequestedSubjects[fullSubject]; exists {
			continue
		}
		plan.RequestedSubjects[fullSubject] = struct{}{}
		plan.Topics = append(plan.Topics, topic)
		plan.FullSubjects = append(plan.FullSubjects, fullSubject)
	}

	lastIDStr := r.URL.Query().Get("last-id")
	queryProvided := lastIDStr != ""
	if lastIDStr == "" {
		lastIDStr = r.Header.Get("Last-Event-ID")
	}
	if lastIDStr != "" {
		parsedID, err := strconv.ParseUint(lastIDStr, 10, 64)
		if err != nil || parsedID == maxReplayCursor {
			if queryProvided {
				return streamPlan{}, &streamRequestError{status: http.StatusBadRequest, message: "Invalid last-id value: must be an unsigned integer below the maximum cursor value"}
			}
			fields := []zap.Field{zap.String("value", lastIDStr)}
			if err != nil {
				fields = append(fields, zap.Error(err))
			} else {
				fields = append(fields, zap.String("reason", "cursor would overflow"))
			}
			if h.logger != nil {
				h.logger.Warn("ignoring unparseable Last-Event-ID header; resuming with DeliverNew", fields...)
			}
		} else {
			plan.Replay = replayPlan{
				HasLastID:     true,
				Mode:          replayModeStartSequence,
				StartSequence: parsedID + 1,
			}
			metricsReplayRequests.Inc()
		}
	}
	return plan, nil
}

func (h *Handler) currentStreamRuntime() streamRuntime {
	h.mu.RLock()
	runtime := streamRuntime{conn: h.conn, js: h.js, shutdown: h.shutdown}
	h.mu.RUnlock()
	return runtime
}

func (h *Handler) newMessageQueue() (chan *nats.Msg, chan string, chan struct{}, nats.MsgHandler) {
	bufSize := h.ClientBufferSize
	if bufSize <= 0 {
		bufSize = defaultClientBufferSize
	}
	dispatchTimeout := time.Duration(h.DispatchTimeout) * time.Second
	msgChan := make(chan *nats.Msg, bufSize)
	done := make(chan struct{})
	slowClient := make(chan string, 1)

	enqueueMessage := func(msg *nats.Msg) {
		select {
		case <-done:
			return
		case msgChan <- msg:
		default:
			h.signalSlowClient(slowClient, msg.Subject, done, dispatchTimeout)
		}
	}
	return msgChan, slowClient, done, enqueueMessage
}

func (h *Handler) signalSlowClient(slowClient chan<- string, subject string, done <-chan struct{}, timeout time.Duration) {
	if timeout <= 0 {
		select {
		case slowClient <- subject:
		case <-done:
		}
		return
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case slowClient <- subject:
	case <-done:
	case <-timer.C:
		if h.logger != nil {
			h.logger.Warn("timed out signaling slow SSE client",
				zap.String("subject", subject),
				zap.Int("dispatch_timeout_seconds", h.DispatchTimeout),
			)
		}
	}
}

func (p streamPlan) requestedMessageHandler(enqueueMessage nats.MsgHandler) nats.MsgHandler {
	return func(msg *nats.Msg) {
		if _, ok := p.RequestedSubjects[msg.Subject]; !ok {
			return
		}
		enqueueMessage(msg)
	}
}

func (h *Handler) readStreamSnapshot(js nats.JetStreamContext, plan streamPlan) streamInfoSnapshot {
	if plan.Replay.HasLastID || len(plan.FullSubjects) > 1 {
		if info, infoErr := js.StreamInfo(h.StreamName); infoErr == nil {
			snapshot := streamInfoSnapshot{
				FirstSeq: info.State.FirstSeq,
				LastSeq:  info.State.LastSeq,
				Subjects: info.Config.Subjects,
			}
			if plan.Replay.HasLastID && h.ReplayWindow > 0 && plan.Replay.StartSequence >= info.State.FirstSeq {
				if msg, err := js.GetMsg(h.StreamName, plan.Replay.StartSequence); err == nil {
					snapshot.StartSequenceTime = msg.Time
					snapshot.HasStartSequenceTime = true
				} else if h.logger != nil {
					h.logger.Debug("failed to read replay start sequence timestamp",
						appendStreamLogFields(plan, zap.Error(err))...,
					)
				}
			}
			return snapshot
		} else {
			if h.logger != nil {
				h.logger.Debug("failed to read StreamInfo for request pre-check",
					appendStreamLogFields(plan, zap.Error(infoErr))...,
				)
			}
		}
	}
	return streamInfoSnapshot{}
}

func (h *Handler) planSubscription(plan streamPlan, snapshot streamInfoSnapshot) streamPlan {
	if len(plan.FullSubjects) > 1 && len(snapshot.Subjects) > 0 {
		for idx, fullSubject := range plan.FullSubjects {
			if !subjectAllowedByStream(fullSubject, snapshot.Subjects) {
				plan.FailedTopics = append(plan.FailedTopics, plan.Topics[idx])
			}
		}
	}
	if len(plan.FailedTopics) > 0 {
		return plan
	}
	if plan.Replay.HasLastID {
		plan.Replay.CapSequence = snapshot.LastSeq
		if snapshot.FirstSeq > 0 && plan.Replay.StartSequence < snapshot.FirstSeq {
			plan.Replay = h.fallbackReplayPlan(plan.Replay, "sequence below retention")
		} else if h.shouldUseReplayWindow(plan.Replay, snapshot) {
			plan.Replay = h.fallbackReplayPlan(plan.Replay, "sequence outside replay window")
		}
	}
	return plan
}

func (h *Handler) shouldUseReplayWindow(replay replayPlan, snapshot streamInfoSnapshot) bool {
	if h.ReplayWindow <= 0 || !replay.HasLastID || snapshot.LastSeq < replay.StartSequence {
		return false
	}
	if !snapshot.HasStartSequenceTime {
		return true
	}
	windowStart := time.Now().Add(-time.Duration(h.ReplayWindow) * time.Second)
	return snapshot.StartSequenceTime.Before(windowStart)
}

func (h *Handler) fallbackReplayPlan(replay replayPlan, reason string) replayPlan {
	replay.FallbackReason = reason
	if h.ReplayWindow > 0 {
		replay.Mode = replayModeFallbackStartTime
		replay.StartTime = h.replayWindowStart()
	} else {
		replay.Mode = replayModeFallbackDeliverAll
	}
	return replay
}

func (h *Handler) replayWindowStart() time.Time {
	return time.Now().Add(-time.Duration(h.ReplayWindow) * time.Second)
}

func (h *Handler) subscriptionOptions(plan streamPlan) []nats.SubOpt {
	opts := []nats.SubOpt{nats.BindStream(h.StreamName), nats.AckNone()}
	switch plan.Replay.Mode {
	case replayModeStartSequence:
		opts = append(opts, nats.StartSequence(plan.Replay.StartSequence))
		h.logger.Debug("subscribing from sequence",
			appendStreamLogFields(plan, zap.Uint64("start_sequence", plan.Replay.StartSequence))...,
		)
	case replayModeFallbackStartTime:
		metricsReplayFallbacks.Inc()
		start := plan.Replay.StartTime
		if start.IsZero() {
			start = h.replayWindowStart()
		}
		opts = append(opts, nats.StartTime(start))
		h.logger.Warn("replay fallback: using time-bounded window",
			appendStreamLogFields(plan,
				zap.Uint64("requested_sequence", plan.Replay.StartSequence),
				zap.String("reason", plan.Replay.FallbackReason),
				zap.Int("replay_window_seconds", h.ReplayWindow),
				zap.Time("replay_window_start", start),
			)...,
		)
	case replayModeFallbackDeliverAll:
		metricsReplayFallbacks.Inc()
		opts = append(opts, nats.DeliverAll())
		h.logger.Warn("replay fallback: delivering all retained messages",
			appendStreamLogFields(plan,
				zap.Uint64("requested_sequence", plan.Replay.StartSequence),
				zap.String("reason", plan.Replay.FallbackReason),
			)...,
		)
	default:
		opts = append(opts, nats.DeliverNew())
	}
	return opts
}

func (h *Handler) executeSubscriptionPlan(js nats.JetStreamContext, conn *nats.Conn, plan streamPlan, enqueueMessage, enqueueRequestedMessage nats.MsgHandler) subscriptionResult {
	if len(plan.FailedTopics) > 0 {
		return subscriptionResult{FailedTopics: append([]string{}, plan.FailedTopics...)}
	}

	activePlan := plan
	sub, err := h.subscribeWithPlan(js, conn, activePlan, enqueueMessage, enqueueRequestedMessage)
	if err != nil && !activePlan.Replay.Mode.isFallback() && isReplayStartSequenceError(err, activePlan.Replay.HasLastID) {
		activePlan.Replay = h.fallbackReplayPlan(activePlan.Replay, "subscribe-time start sequence error")
		sub, err = h.subscribeWithPlan(js, conn, activePlan, enqueueMessage, enqueueRequestedMessage)
	}
	if err != nil {
		metricsSubscriptionErrors.Inc()
		if len(activePlan.FullSubjects) == 1 {
			h.logger.Error("failed to subscribe to topic",
				appendStreamLogFields(activePlan, zap.Error(err))...,
			)
			return subscriptionResult{FailedTopics: []string{activePlan.Topics[0]}}
		}
		h.logger.Error("failed to subscribe to topics",
			appendStreamLogFields(activePlan, zap.Error(err))...,
		)
		return subscriptionResult{FailedTopics: append([]string{}, activePlan.Topics...)}
	}

	if len(activePlan.FullSubjects) == 1 {
		h.logger.Debug("subscribed to topic", streamLogFields(activePlan)...)
	} else {
		h.logger.Debug("subscribed to topics", streamLogFields(activePlan)...)
	}
	return subscriptionResult{
		Subscriptions: []*nats.Subscription{sub},
	}
}

func (h *Handler) subscribeWithPlan(js nats.JetStreamContext, conn *nats.Conn, plan streamPlan, enqueueMessage, enqueueRequestedMessage nats.MsgHandler) (*nats.Subscription, error) {
	opts := h.subscriptionOptions(plan)
	if len(plan.FullSubjects) == 1 {
		return js.Subscribe(plan.FullSubjects[0], enqueueMessage, opts...)
	}
	return h.subscribeToMultipleTopics(js, plan, opts, enqueueRequestedMessage, supportsMultiFilterSubjects(conn), connectedServerVersion(conn))
}

func (h *Handler) cleanupStream(done chan struct{}, subscriptions []*nats.Subscription) {
	close(done)
	for _, sub := range subscriptions {
		if err := sub.Unsubscribe(); err != nil {
			h.logger.Warn("failed to unsubscribe",
				zap.String("topic", sub.Subject),
				zap.Error(err))
		}
	}
}

func (h *Handler) serveStream(w http.ResponseWriter, flusher http.Flusher, r *http.Request, plan streamPlan, msgChan <-chan *nats.Msg, slowClient <-chan string, shutdown <-chan struct{}) error {
	metricsActiveConnections.Inc()
	defer metricsActiveConnections.Dec()
	writeTimeout := time.Duration(h.WriteTimeout) * time.Second

	h.setCORSHeaders(w, r)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	if h.HubURL != "" {
		w.Header().Set("Link", fmt.Sprintf("<%s>; rel=\"nuts\"", h.HubURL))
	}

	if err := writeSSEChunkWithTimeout(w, flusher, formatConnectedEvent(plan.Topics), writeTimeout); err != nil {
		h.logger.Debug("failed to write connected event",
			appendStreamLogFields(plan,
				zap.String("disconnect_reason", "write_error"),
				zap.Int("write_timeout_seconds", h.WriteTimeout),
				zap.Error(err),
			)...,
		)
		return nil
	}

	heartbeat := time.NewTicker(time.Duration(h.HeartbeatInterval) * time.Second)
	defer heartbeat.Stop()

	replayDelivered := 0

	ctx := r.Context()
	for {
		select {
		case <-shutdown:
			h.logger.Debug("handler shutting down; closing SSE stream",
				appendStreamLogFields(plan, zap.String("disconnect_reason", "handler_shutdown"))...,
			)
			return nil

		case slowTopic := <-slowClient:
			metricsSlowClientDisconnects.Inc()
			h.logger.Warn("disconnecting slow SSE client before dropping messages",
				appendStreamLogFields(plan,
					zap.String("disconnect_reason", "slow_client"),
					zap.String("slow_subject", slowTopic),
					zap.Int("buffer_size", cap(msgChan)),
				)...,
			)
			return nil

		case <-ctx.Done():
			h.logger.Debug("client disconnected",
				appendStreamLogFields(plan,
					zap.String("disconnect_reason", "client_context_done"),
					zap.Error(ctx.Err()),
				)...,
			)
			return nil

		case msg := <-msgChan:
			if msg == nil {
				continue
			}

			formatted := h.formatMessageEvent(msg, time.Now())
			if formatted.MetadataErr != nil {
				h.logger.Warn("failed to read JetStream metadata",
					appendStreamLogFields(plan,
						zap.String("message_subject", formatted.Subject),
						zap.Error(formatted.MetadataErr),
					)...,
				)
			}
			if formatted.Dropped {
				h.recordDroppedMessage(formatted)
				continue
			}
			if shouldSkipReplayWindowMessage(plan, formatted) {
				continue
			}

			if err := writeSSEChunkWithTimeout(w, flusher, formatted.Frame, writeTimeout); err != nil {
				h.logger.Debug("failed to write message event",
					appendStreamLogFields(plan,
						zap.String("disconnect_reason", "write_error"),
						zap.String("message_subject", msg.Subject),
						zap.Int("write_timeout_seconds", h.WriteTimeout),
						zap.Error(err),
					)...,
				)
				return nil
			}
			metricsMessagesDelivered.Inc()

			if h.countsTowardReplayCap(plan, formatted) {
				replayDelivered++
				if replayDelivered >= h.ReplayMaxMessages {
					metricsReplayCapReached.Inc()
					h.logger.Warn("closing SSE stream: replay_max_messages reached",
						appendStreamLogFields(plan,
							zap.String("disconnect_reason", "replay_cap_reached"),
							zap.Int("replay_max_messages", h.ReplayMaxMessages),
							zap.Int("replay_delivered", replayDelivered),
						)...,
					)
					return nil
				}
			}

		case <-heartbeat.C:
			if err := writeSSEChunkWithTimeout(w, flusher, formatHeartbeatEvent(time.Now()), writeTimeout); err != nil {
				h.logger.Debug("failed to write heartbeat",
					appendStreamLogFields(plan,
						zap.String("disconnect_reason", "heartbeat_write_error"),
						zap.Int("write_timeout_seconds", h.WriteTimeout),
						zap.Error(err),
					)...,
				)
				return nil
			}
		}
	}
}

func shouldSkipReplayWindowMessage(plan streamPlan, formatted formattedMessageEvent) bool {
	return plan.Replay.Mode == replayModeFallbackStartTime &&
		!plan.Replay.StartTime.IsZero() &&
		formatted.HasMessageTime &&
		formatted.MessageTime.Before(plan.Replay.StartTime)
}

func (h *Handler) countsTowardReplayCap(plan streamPlan, formatted formattedMessageEvent) bool {
	if h.ReplayMaxMessages <= 0 || !plan.Replay.HasLastID {
		return false
	}
	if plan.Replay.CapSequence == 0 || !formatted.HasStreamSequence {
		return true
	}
	return formatted.StreamSequence <= plan.Replay.CapSequence
}

func formatConnectedEvent(topics []string) string {
	return fmt.Sprintf("event: connected\ndata: {\"topics\":%s}\n\n", toJSON(topics))
}

func formatHeartbeatEvent(now time.Time) string {
	return fmt.Sprintf(": heartbeat %s\n\n", now.UTC().Format(time.RFC3339))
}

func (h *Handler) formatMessageEvent(msg *nats.Msg, now time.Time) formattedMessageEvent {
	formatted := formattedMessageEvent{Subject: msg.Subject}
	if h.MaxEventSize > 0 && len(msg.Data) > h.MaxEventSize {
		formatted.Dropped = true
		formatted.DropReason = dropReasonRawPayload
		formatted.DropSize = len(msg.Data)
		return formatted
	}

	payload := messageEventPayload{
		Topic:   strings.TrimPrefix(msg.Subject, h.TopicPrefix),
		Payload: tryParseJSON(msg.Data),
		Time:    now.UTC().Format(time.RFC3339),
	}
	var eventID uint64
	hasEventID := false
	meta, metaErr := msg.Metadata()
	if metaErr != nil {
		formatted.MetadataErr = metaErr
	} else {
		payload.Time = meta.Timestamp.UTC().Format(time.RFC3339)
		eventID = meta.Sequence.Stream
		hasEventID = true
		formatted.StreamSequence = eventID
		formatted.HasStreamSequence = true
		formatted.MessageTime = meta.Timestamp
		formatted.HasMessageTime = true
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

	if h.MaxEventSize > 0 && event.Len() > h.MaxEventSize {
		formatted.Dropped = true
		formatted.DropReason = dropReasonFormattedSSEMessage
		formatted.DropSize = event.Len()
		return formatted
	}
	formatted.Frame = event.String()
	return formatted
}

func (h *Handler) recordDroppedMessage(formatted formattedMessageEvent) {
	metricsMessagesDropped.Inc()
	switch formatted.DropReason {
	case dropReasonRawPayload:
		h.logger.Warn("dropping oversized NATS payload",
			zap.String("topic", formatted.Subject),
			zap.Int("payload_size", formatted.DropSize),
			zap.Int("max_event_size", h.MaxEventSize),
		)
	case dropReasonFormattedSSEMessage:
		h.logger.Warn("dropping oversized SSE event",
			zap.String("topic", formatted.Subject),
			zap.Int("event_size", formatted.DropSize),
			zap.Int("max_event_size", h.MaxEventSize),
		)
	}
}

func (h *Handler) subscribeToMultipleTopics(js nats.JetStreamContext, plan streamPlan, opts []nats.SubOpt, cb nats.MsgHandler, useMultiFilter bool, serverVersion string) (*nats.Subscription, error) {
	if useMultiFilter {
		filterOpts := append([]nats.SubOpt{}, opts...)
		filterOpts = append(filterOpts, nats.ConsumerFilterSubjects(plan.FullSubjects...))
		return js.Subscribe("", cb, filterOpts...)
	}

	wildcardSubject := commonSubjectFilter(plan.FullSubjects)
	h.logger.Warn("NATS server does not support multi-filter consumers; using common wildcard subscription",
		appendStreamLogFields(plan,
			zap.String("wildcard_subject", wildcardSubject),
			zap.String("server_version", serverVersion),
		)...,
	)
	return js.Subscribe(wildcardSubject, cb, opts...)
}

func supportsMultiFilterSubjects(conn *nats.Conn) bool {
	return supportsMultiFilterSubjectsVersion(connectedServerVersion(conn))
}

func supportsMultiFilterSubjectsVersion(version string) bool {
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
	return matchesConfiguredPath(reqPath, h.HealthPath, defaultHealthPath)
}

func (h *Handler) matchesLivePath(reqPath string) bool {
	return matchesConfiguredPath(reqPath, h.LivePath, defaultLivePath)
}

func (h *Handler) matchesReadyPath(reqPath string) bool {
	return matchesConfiguredPath(reqPath, h.ReadyPath, defaultReadyPath)
}

func matchesConfiguredPath(reqPath, configuredPath, defaultPath string) bool {
	path := configuredPath
	if path == "" {
		path = defaultPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if reqPath == path {
		return true
	}
	return strings.HasSuffix(reqPath, path)
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

// serveLiveCheck responds with process liveness only. It intentionally does
// not check NATS or JetStream so orchestrators can avoid killing a healthy
// Caddy process during a backend outage.
func (h *Handler) serveLiveCheck(w http.ResponseWriter) error {
	resp := struct {
		Status string `json:"status"`
	}{Status: "ok"}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil && h.logger != nil {
		h.logger.Debug("failed to encode liveness response", zap.Error(err))
	}
	return nil
}

// serveReadinessCheck responds with NATS / stream readiness status.
func (h *Handler) serveReadinessCheck(w http.ResponseWriter) error {
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
