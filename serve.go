// serve.go — HTTP/SSE request handling.
//
// This file contains the per-request hot path: it accepts an HTTP GET from a
// browser EventSource, parses the requested topics and replay cursor, opens a
// JetStream subscription, and pumps messages back as Server-Sent Events until
// the client disconnects, the handler shuts down, or the slow-client guard
// fires.
//
// The request lifecycle is broken into small, testable steps invoked from
// ServeHTTP:
//
//  1. handleControlRequest    — short-circuits health/liveness/readiness/CORS.
//  2. parseStreamRequest      — extracts and validates topics and Last-Event-ID.
//  3. authorizeStreamRequest  — enforces optional subscriber JWT auth.
//  4. readStreamSnapshot      — reads JetStream state to inform planning.
//  5. planSubscription        — picks the replay mode and detects bad topics.
//  6. executeSubscriptionPlan — opens the JetStream subscription (with fallback).
//  7. serveStream             — runs the SSE select-loop until disconnect.
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
	// maxReplayCursor is reserved as an "invalid" sentinel: a Last-Event-ID
	// equal to math.MaxUint64 cannot be incremented for StartSequence without
	// wrapping, so we reject it up-front and fall back to DeliverNew.
	maxReplayCursor = ^uint64(0)

	// jsErrCodeSequenceNotFound is the JetStream API error returned when a
	// requested StartSequence has been purged from retention. nats.go v1.37
	// exposes APIError but not this server error-code constant, so we redeclare
	// it locally and match against APIError.ErrorCode.
	jsErrCodeSequenceNotFound nats.ErrorCode = 10043
)

// replayMode describes how a JetStream subscription should position itself
// when a client connects: starting at "now", at a specific sequence, or via
// one of the fallback strategies when the requested sequence is unreachable.
type replayMode string

const (
	// replayModeDeliverNew skips all retained messages and only delivers
	// future events. Used when no Last-Event-ID is provided.
	replayModeDeliverNew replayMode = "deliver_new"
	// replayModeStartSequence resumes from a specific JetStream sequence
	// (Last-Event-ID + 1). Used on browser EventSource reconnects.
	replayModeStartSequence replayMode = "start_sequence"
	// replayModeFallbackDeliverAll is chosen when the requested sequence has
	// been purged and no replay window is configured: replay everything still
	// retained.
	replayModeFallbackDeliverAll replayMode = "fallback_deliver_all"
	// replayModeFallbackStartTime is chosen when a replay window is configured
	// and either the requested sequence was purged or it predates the window.
	// The subscription starts at now-ReplayWindow instead of at the sequence.
	replayModeFallbackStartTime replayMode = "fallback_start_time"

	// dropReasonRawPayload tags a drop that fired because the inbound NATS
	// payload itself exceeded MaxEventSize (checked before JSON parsing so we
	// never allocate the formatted frame for an oversized message).
	dropReasonRawPayload string = "raw_payload"
	// dropReasonFormattedSSEMessage tags a drop where the raw payload fit but
	// the JSON-wrapped SSE frame (id/event/data envelope plus encoded payload)
	// exceeded MaxEventSize.
	dropReasonFormattedSSEMessage string = "formatted_sse_message"
)

// isFallback reports whether the mode was selected by the fallback path
// rather than by an explicit client request. Used to avoid double-fallback
// when a fallback subscription itself fails.
func (m replayMode) isFallback() bool {
	return m == replayModeFallbackDeliverAll || m == replayModeFallbackStartTime
}

// replayPlan captures the resolved replay strategy for a single SSE request.
// It is built during planSubscription and consumed by subscriptionOptions.
type replayPlan struct {
	// HasLastID is true when the request supplied either a Last-Event-ID
	// header or a last-id query parameter.
	HasLastID bool
	// Mode is the strategy chosen for this request (see replayMode constants).
	Mode replayMode
	// StartSequence is the JetStream sequence to resume from (last-id + 1)
	// when Mode is replayModeStartSequence, or the originally requested
	// sequence preserved for log/diagnostic context when a fallback fires.
	StartSequence uint64
	// StartTime is set when Mode is replayModeFallbackStartTime; messages
	// before this instant are filtered out client-side as well to defend
	// against server clock skew.
	StartTime time.Time
	// CapSequence is the highest sequence considered "historical replay"
	// at the moment the subscription opens. Anything above it is live
	// traffic and does not count toward replay_max_messages.
	CapSequence uint64
	// FallbackReason is a short human-readable string explaining why a
	// fallback was chosen; surfaced in logs and metrics.
	FallbackReason string
}

// streamPlan is the full per-request plan: which topics/subjects to bind to,
// the chosen replay strategy, and any topics that pre-flight rejected.
type streamPlan struct {
	// Topics are the un-prefixed topic names from the request, in the order
	// they were supplied. Used for client-facing log fields.
	Topics []string
	// FullSubjects are the topics with TopicPrefix applied, in the same order.
	// These are what JetStream actually sees.
	FullSubjects []string
	// RequestedSubjects is a set of FullSubjects, used as a fast filter for
	// the multi-topic wildcard fallback path (see subscribeToMultipleTopics).
	RequestedSubjects map[string]struct{}
	// Replay is the resolved replay plan for this request.
	Replay replayPlan
	// FailedTopics lists topic names that were rejected during planning
	// (e.g. not allowed by the configured stream's subject filters). When
	// non-empty, the request short-circuits with 503.
	FailedTopics []string
}

// subjectLabel produces a single comma-joined subject string suitable for log
// fields where one entry per stream is expected.
func (p streamPlan) subjectLabel() string {
	return strings.Join(p.FullSubjects, ",")
}

// streamLogFields builds the standard set of structured log fields for a
// stream request. Always include these on stream-related log lines so log
// aggregators can correlate events from the same SSE connection.
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

// appendStreamLogFields returns the standard stream log fields with extra
// per-call fields appended. Prefer this over manual append() calls so the
// base set stays consistent across log lines.
func appendStreamLogFields(plan streamPlan, fields ...zap.Field) []zap.Field {
	return append(streamLogFields(plan), fields...)
}

// streamMetadataReader is the subset of nats.JetStreamContext that
// readStreamSnapshot depends on. Extracted so tests can stub the metadata
// reads independently of a live JetStream connection — `*nats.js` (the
// real implementation) satisfies it automatically.
type streamMetadataReader interface {
	StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error)
	GetMsg(name string, seq uint64, opts ...nats.JSOpt) (*nats.RawStreamMsg, error)
}

// streamInfoSnapshot is a frozen view of relevant JetStream stream state at
// the moment the request was planned. Reading once and reusing avoids racing
// against background JetStream activity during planning decisions.
type streamInfoSnapshot struct {
	// FirstSeq is the lowest sequence currently retained in the stream.
	// Used to detect requests for purged sequences.
	FirstSeq uint64
	// LastSeq is the highest sequence at snapshot time. Used as the replay
	// cap so live messages don't count against replay_max_messages.
	LastSeq uint64
	// Subjects are the configured stream subject filters; a multi-topic
	// request whose subjects are not allowed by these filters is rejected.
	Subjects []string
	// StartSequenceTime is the publish time of the requested replay start
	// sequence, when the message is still retained. Used for replay_window
	// comparisons.
	StartSequenceTime time.Time
	// HasStartSequenceTime distinguishes a missing timestamp from a zero one.
	HasStartSequenceTime bool
}

// streamRuntime is a snapshot of the handler's NATS-level state under the
// handler mutex. Captured once per request so the streaming loop can run
// without re-locking on every message.
type streamRuntime struct {
	conn     *nats.Conn
	js       nats.JetStreamContext
	shutdown <-chan struct{}
}

// streamRequestError is a structured error returned by the request-parsing
// and authorization steps. Exists so handlers can defer the actual HTTP
// write (and any auth-specific headers) to a single .write() call.
type streamRequestError struct {
	status  int
	message string
}

// write sends the error to the client. For 401 responses it also advertises
// the bearer scheme so browsers and clients know which credential to send.
func (e *streamRequestError) write(w http.ResponseWriter) {
	if e.status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="nuts"`)
	}
	http.Error(w, e.message, e.status)
}

// subscriptionResult is the outcome of executeSubscriptionPlan: either a set
// of opened subscriptions to be cleaned up on disconnect, or a list of
// topics that could not be subscribed.
type subscriptionResult struct {
	Subscriptions []*nats.Subscription
	FailedTopics  []string
}

// formattedMessageEvent is the output of formatMessageEvent: either a fully
// rendered SSE frame, or a drop record explaining why nothing was sent.
// Splitting "format" from "write" keeps the streaming select-loop free of
// allocation/encoding logic and makes the formatter unit-testable.
type formattedMessageEvent struct {
	// Frame is the rendered SSE bytes ready to flush. Empty when Dropped.
	Frame string
	// Subject is the NATS subject the message arrived on (logged on drops).
	Subject string
	// StreamSequence is the JetStream sequence; used as the SSE id: field and
	// to bound replay_max_messages.
	StreamSequence    uint64
	HasStreamSequence bool
	// MessageTime is the original publish time from JetStream metadata; used
	// to filter out messages older than the replay window.
	MessageTime    time.Time
	HasMessageTime bool
	// Dropped is set when the formatter chose not to emit (oversize).
	Dropped bool
	// DropReason names the drop bucket for metrics/logging.
	DropReason string
	// DropSize is the size that triggered the drop, used in operator logs.
	DropSize int
	// MetadataErr captures a JetStream metadata read failure. The frame is
	// still emitted in this case (with fallback timestamp/no id) so a
	// metadata blip doesn't drop messages, but the error is logged.
	MetadataErr error
}

// isReplayStartSequenceError reports whether the JetStream subscribe error
// signals "the requested StartSequence is unreachable". Two server signals
// can mean this: an APIError with code 10043, or a consumer sequence
// mismatch. Both are recoverable via the replay-fallback path.
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

// ServeHTTP implements caddyhttp.MiddlewareHandler. It dispatches health and
// CORS-preflight requests, validates and authorizes the SSE subscription
// request, opens a JetStream subscription, and runs the SSE streaming loop
// until the client or the server closes the connection. Non-stream requests
// are passed to the next handler in the Caddy chain when one is configured.
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

	// Reserve before opening a JetStream subscription so we never pay the
	// subscription cost for a request we'd just reject anyway.
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

// handleControlRequest short-circuits requests that aren't SSE subscriptions:
// liveness/readiness/health probes, CORS preflight, and any non-GET method.
// The first return value reports whether the request was handled (and
// ServeHTTP should stop); the second is the error to bubble up.
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

// parseStreamRequest extracts the topic list and replay cursor from the
// incoming request and returns the resulting streamPlan. Topics may come
// from repeated ?topic= query parameters or from a path shorthand
// (/a/b → a.b). Returns a streamRequestError for any client-fixable issue
// so ServeHTTP can map it to a 4xx response.
func (h *Handler) parseStreamRequest(r *http.Request) (streamPlan, *streamRequestError) {
	topics := r.URL.Query()["topic"]
	if len(topics) == 0 {
		// Path shorthand: GET /orders/new is equivalent to ?topic=orders.new.
		// Lets simple consumers subscribe without query-string handling.
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
		// De-duplicate so a client requesting ?topic=a&topic=a doesn't
		// double-subscribe and double-deliver every message.
		if _, exists := plan.RequestedSubjects[fullSubject]; exists {
			continue
		}
		plan.RequestedSubjects[fullSubject] = struct{}{}
		plan.Topics = append(plan.Topics, topic)
		plan.FullSubjects = append(plan.FullSubjects, fullSubject)
	}

	// Cap the topic count after dedup so a client cannot pin large NATS-side
	// state by sending thousands of distinct ?topic= parameters. A negative
	// MaxTopicsPerSubscription disables the cap (operator opt-out).
	if h.MaxTopicsPerSubscription > 0 && len(plan.Topics) > h.MaxTopicsPerSubscription {
		return streamPlan{}, &streamRequestError{status: http.StatusBadRequest, message: "Too many topics requested"}
	}

	// Query parameter takes precedence over the header so a client can
	// override what the browser auto-attaches on EventSource reconnect.
	lastIDStr := r.URL.Query().Get("last-id")
	queryProvided := lastIDStr != ""
	if lastIDStr == "" {
		lastIDStr = r.Header.Get("Last-Event-ID")
	}
	if lastIDStr != "" {
		// Cap the input length before strconv.ParseUint so a multi-MB numeric
		// string can't cause large allocations. uint64 max is 20 digits.
		if len(lastIDStr) > 20 {
			if queryProvided {
				return streamPlan{}, &streamRequestError{status: http.StatusBadRequest, message: "Invalid last-id value: too long"}
			}
			if h.logger != nil {
				h.logger.Warn("ignoring oversized Last-Event-ID header; resuming with DeliverNew",
					zap.Int("length", len(lastIDStr)))
			}
			return plan, nil
		}
		parsedID, err := strconv.ParseUint(lastIDStr, 10, 64)
		if err != nil || parsedID == maxReplayCursor {
			// Explicit ?last-id= is a client error (bad request); a header
			// value usually came from a browser auto-resume and we'd rather
			// downgrade to DeliverNew than break reconnect entirely.
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

// currentStreamRuntime takes a single locked snapshot of the handler's
// NATS-level state. Cleanup() can swap these fields under the same mutex,
// so a per-request snapshot lets the streaming loop run without holding
// the lock for the duration of the connection.
func (h *Handler) currentStreamRuntime() streamRuntime {
	h.mu.RLock()
	runtime := streamRuntime{conn: h.conn, js: h.js, shutdown: h.shutdown}
	h.mu.RUnlock()
	return runtime
}

// newMessageQueue creates the per-request fan-in channels used to ferry
// JetStream messages from the NATS callback goroutine to the SSE writer.
// Returns:
//   - msgChan: the bounded buffer the SSE loop reads from.
//   - slowClient: 1-slot signal channel; receives the offending subject when
//     the buffer fills, telling the loop to disconnect rather than drop.
//   - done: closed by cleanupStream to release the NATS callback if it is
//     blocked trying to publish to a dead client.
//   - enqueueMessage: the nats.MsgHandler the subscription is bound to.
//
// Buffer size comes from ClientBufferSize (defaults to defaultClientBufferSize).
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
			// Connection already torn down; drop silently rather than
			// blocking the NATS callback indefinitely.
			return
		case msgChan <- msg:
		default:
			// Buffer is full: the client is consuming slower than the stream
			// is producing. Signal the SSE loop to disconnect (the only safe
			// option — accumulating would OOM, dropping would silently lose).
			h.signalSlowClient(slowClient, msg.Subject, done, dispatchTimeout)
		}
	}
	return msgChan, slowClient, done, enqueueMessage
}

// signalSlowClient sends the subject of the message that overflowed the
// client buffer to the SSE loop, so it can disconnect with a meaningful
// log line. If timeout > 0 the send is bounded so a wedged loop cannot
// deadlock the NATS callback goroutine; instead a warning is logged and
// the message is effectively dropped at this point. The done channel
// short-circuits the wait when the connection is being torn down.
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
		metricsDispatchTimeouts.Inc()
		if h.logger != nil {
			h.logger.Warn("timed out signaling slow SSE client",
				zap.String("subject", subject),
				zap.Int("dispatch_timeout_seconds", h.DispatchTimeout),
			)
		}
	}
}

// requestedMessageHandler wraps enqueueMessage with a subject filter. Used
// for the multi-topic wildcard fallback (servers older than NATS 2.10 do
// not support multi-filter consumers, so we subscribe to a common parent
// subject and filter client-side).
func (p streamPlan) requestedMessageHandler(enqueueMessage nats.MsgHandler) nats.MsgHandler {
	return func(msg *nats.Msg) {
		if _, ok := p.RequestedSubjects[msg.Subject]; !ok {
			return
		}
		enqueueMessage(msg)
	}
}

// readStreamSnapshot fetches the JetStream stream metadata needed for
// planning, but only when planning actually depends on it: replay requests
// (need first/last sequence and the start-sequence timestamp for window
// checks) or multi-topic requests (need configured subjects to validate
// each topic against the stream filter). Single-topic, no-replay requests
// skip the round trip entirely. Read failures are logged at debug and
// return a zero-value snapshot, which downstream code treats as "no
// snapshot info available".
func (h *Handler) readStreamSnapshot(js streamMetadataReader, plan streamPlan) streamInfoSnapshot {
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
		} else if h.logger != nil {
			h.logger.Debug("failed to read StreamInfo for request pre-check",
				appendStreamLogFields(plan, zap.Error(infoErr))...,
			)
		}
	}
	return streamInfoSnapshot{}
}

// planSubscription finalises the streamPlan in the light of the JetStream
// snapshot. It detects topics that the configured stream does not allow
// (multi-topic only — single-topic subscribe failures surface at subscribe
// time as a clearer error), and rewrites the replay plan to a fallback
// when the requested sequence is below retention or outside the configured
// replay_window. Idempotent: calling it twice with the same snapshot yields
// the same result.
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

// shouldUseReplayWindow reports whether the configured replay window forces
// the request into the time-bounded fallback even though the requested
// sequence is still retained. We force the fallback when the original
// publish time is older than now-ReplayWindow, or when we couldn't read
// the start-sequence timestamp at all (conservative: assume out-of-window
// rather than risk emitting forbidden history).
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

// fallbackReplayPlan rewrites a replay plan to one of the two fallback
// modes: time-bounded when ReplayWindow is configured, otherwise deliver-all.
// The reason string is preserved on the plan and surfaced in logs/metrics so
// operators can tell why a fallback fired.
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

// replayWindowStart returns the wall-clock instant at which a time-bounded
// fallback subscription should begin (now - ReplayWindow seconds).
func (h *Handler) replayWindowStart() time.Time {
	return time.Now().Add(-time.Duration(h.ReplayWindow) * time.Second)
}

// subscriptionOptions translates the resolved replay plan into the SubOpts
// that JetStream expects. Always pins the consumer to the configured stream
// (BindStream) and disables acks (AckNone) — NUTS is read-only and replay is
// driven entirely by start position, not by ack state.
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

// executeSubscriptionPlan opens the JetStream subscription that backs the
// SSE stream. If the initial subscribe fails specifically because the
// requested StartSequence is unreachable, it retries once via the
// replay-fallback path so a transient race between StreamInfo (used for
// planning) and Subscribe (which actually positions the consumer) does not
// surface as a 503 to the client.
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

// subscribeWithPlan opens one JetStream subscription that satisfies the
// plan. Single-topic uses Subscribe directly; multi-topic dispatches to
// subscribeToMultipleTopics, which picks between native multi-filter
// (NATS 2.10+) and the wildcard-with-client-filter fallback.
func (h *Handler) subscribeWithPlan(js nats.JetStreamContext, conn *nats.Conn, plan streamPlan, enqueueMessage, enqueueRequestedMessage nats.MsgHandler) (*nats.Subscription, error) {
	opts := h.subscriptionOptions(plan)
	if len(plan.FullSubjects) == 1 {
		return js.Subscribe(plan.FullSubjects[0], enqueueMessage, opts...)
	}
	return h.subscribeToMultipleTopics(js, plan, opts, enqueueRequestedMessage, supportsMultiFilterSubjects(conn), connectedServerVersion(conn))
}

// cleanupStream tears down a request's NATS subscriptions and signals the
// message-queue side via the done channel. Closing done first releases any
// NATS callback that may still be blocked on a slow-client signal so it
// doesn't outlive the request. Unsubscribe failures are logged but not
// propagated — there's nothing the caller can do about them.
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

// serveStream is the SSE writer loop. It writes the response headers and the
// initial "connected" event, then multiplexes between four sources until any
// of them terminates the connection:
//
//   - shutdown    — Cleanup() closing the handler-wide channel.
//   - slowClient  — the message queue overflowed; disconnect to protect us.
//   - ctx.Done()  — the HTTP client closed or timed out.
//   - msgChan     — a JetStream message to format and flush.
//   - heartbeat.C — periodic SSE comment to keep proxies from closing idle
//     connections.
//
// Returns nil on every termination path: SSE has no notion of an HTTP error
// after streaming has begun, so all exits are observable as a normal
// connection close.
func (h *Handler) serveStream(w http.ResponseWriter, flusher http.Flusher, r *http.Request, plan streamPlan, msgChan <-chan *nats.Msg, slowClient <-chan string, shutdown <-chan struct{}) error {
	metricsActiveConnections.Inc()
	defer metricsActiveConnections.Dec()
	writeTimeout := time.Duration(h.WriteTimeout) * time.Second

	h.setCORSHeaders(w, r)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// X-Accel-Buffering: no tells nginx (and other proxies that respect it)
	// not to buffer the response, which would otherwise hold events until
	// flush thresholds are met and break SSE's near-real-time guarantee.
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
			if !h.finalizeStreamedMessage(plan, formatted) {
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

// finalizeStreamedMessage handles the side effects of a formatted message
// just received from the JetStream subscription, then reports whether it
// should be written to the SSE response. Returns false when the message
// was Dropped (and recorded to metrics) or when it falls outside the
// active replay window.
func (h *Handler) finalizeStreamedMessage(plan streamPlan, formatted formattedMessageEvent) bool {
	if formatted.Dropped {
		h.recordDroppedMessage(formatted)
		return false
	}
	if shouldSkipReplayWindowMessage(plan, formatted) {
		return false
	}
	return true
}

// shouldSkipReplayWindowMessage filters out messages that JetStream
// delivered but predate the configured replay window. Necessary because
// nats.StartTime() resolution is server-side and can include messages
// published an instant before the requested cutoff; this is the
// belt-and-braces client-side check.
func shouldSkipReplayWindowMessage(plan streamPlan, formatted formattedMessageEvent) bool {
	return plan.Replay.Mode == replayModeFallbackStartTime &&
		!plan.Replay.StartTime.IsZero() &&
		formatted.HasMessageTime &&
		formatted.MessageTime.Before(plan.Replay.StartTime)
}

// countsTowardReplayCap reports whether a delivered message should count
// against ReplayMaxMessages. Only true when (a) the cap is configured,
// (b) the request asked for replay, and (c) the message's sequence is at
// or below the cap recorded when the subscription opened (i.e. it is
// historical replay, not new live traffic produced after we connected).
// Without sequence metadata we conservatively count it.
func (h *Handler) countsTowardReplayCap(plan streamPlan, formatted formattedMessageEvent) bool {
	if h.ReplayMaxMessages <= 0 || !plan.Replay.HasLastID {
		return false
	}
	if plan.Replay.CapSequence == 0 || !formatted.HasStreamSequence {
		return true
	}
	return formatted.StreamSequence <= plan.Replay.CapSequence
}

// formatConnectedEvent renders the SSE handshake event sent immediately
// after headers. Useful for clients that want to confirm the subscription
// landed on the topics they expected (after path-shorthand expansion or
// authorization-driven topic filtering).
func formatConnectedEvent(topics []string) string {
	return fmt.Sprintf("event: connected\ndata: {\"topics\":%s}\n\n", toJSON(topics))
}

// formatHeartbeatEvent renders an SSE comment line ("colon-prefixed"
// per the SSE spec) carrying the current time. Comments are ignored by
// EventSource clients but keep proxies from declaring the connection idle.
func formatHeartbeatEvent(now time.Time) string {
	return fmt.Sprintf(": heartbeat %s\n\n", now.UTC().Format(time.RFC3339))
}

// formatMessageEvent turns a NATS message into a fully rendered SSE frame
// (or a Dropped record). Drops are decided in two places:
//
//  1. Before any work: if the raw NATS payload exceeds MaxEventSize, we
//     bail before parsing JSON or building the envelope.
//  2. After rendering: if the JSON-wrapped frame exceeds MaxEventSize.
//
// JetStream metadata read failures do not drop the message — we render
// with `now` as the timestamp and no `id:` field so a metadata blip
// doesn't take the stream down — but the error is surfaced to the caller
// so it can be logged once.
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

	// Pre-size the SSE frame builder to avoid reallocations: payload + a
	// rough envelope budget (id/event/data lines plus JSON wrapper).
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

// recordDroppedMessage emits the metric and structured log line for a
// message the formatter decided not to send. Operators rely on these logs
// to size MaxEventSize correctly without grepping the message payloads.
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

// subscribeToMultipleTopics opens one consumer that delivers every
// requested subject. Two paths:
//
//   - useMultiFilter (NATS 2.10+): use ConsumerFilterSubjects, the
//     server-side multi-filter API. Server delivers only the requested
//     subjects, no client-side filtering needed.
//   - fallback (older servers): subscribe to the smallest common
//     wildcard parent subject and rely on requestedMessageHandler to
//     drop subjects the request didn't ask for. This wastes some bandwidth
//     when sibling subjects are busy, but keeps the feature usable on
//     older servers; we log a warning so operators notice.
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

// supportsMultiFilterSubjects reports whether the connected NATS server is
// new enough to support ConsumerFilterSubjects (multi-filter consumers,
// added in NATS 2.10).
func supportsMultiFilterSubjects(conn *nats.Conn) bool {
	return supportsMultiFilterSubjectsVersion(connectedServerVersion(conn))
}

// supportsMultiFilterSubjectsVersion is the version-string-only half of
// supportsMultiFilterSubjects, exposed separately so it can be unit-tested
// without standing up a NATS server.
func supportsMultiFilterSubjectsVersion(version string) bool {
	major, minor, ok := parseMajorMinorVersion(version)
	if !ok {
		// Unknown version → assume unsupported and use the wildcard fallback.
		return false
	}
	return major > 2 || (major == 2 && minor >= 10)
}

// connectedServerVersion returns the version reported by the currently
// connected NATS server, or "" if the connection isn't established.
func connectedServerVersion(conn *nats.Conn) string {
	if conn == nil {
		return ""
	}
	return conn.ConnectedServerVersion()
}

// parseMajorMinorVersion extracts the MAJOR and MINOR components from a
// semver-ish string (with optional leading "v" and "-pre"/"+build" suffix).
// Returns ok=false if the string can't be parsed; callers should treat that
// as an unknown / unsupported version.
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

// commonSubjectFilter returns the narrowest single wildcard NATS subject
// that matches every input subject. Used by the older-server fallback in
// subscribeToMultipleTopics.
//
// Examples:
//
//	["a.b.c", "a.b.d"]   → "a.b.>"
//	["a.b", "a.c"]       → "a.>"
//	["a.b", "x.y"]       → ">"
//	["a.b.c", "a.b"]     → "a.>"   (the second is a parent of the first)
//
// The trim-by-one step at the end handles the parent/child case: if any
// input subject is exactly the length of the common prefix, the prefix
// itself can't be the wildcard subject (it would be too narrow), so we
// drop one token before appending ">".
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

// subjectAllowedByStream reports whether the configured stream's subject
// filters cover the given subject. Used during planning to reject topics
// that would never produce messages, with a clearer error than the
// JetStream subscribe-time failure.
func subjectAllowedByStream(subject string, streamSubjects []string) bool {
	for _, streamSubject := range streamSubjects {
		if subjectMatchesFilter(subject, streamSubject) {
			return true
		}
	}
	return false
}

// subjectMatchesFilter reports whether subject matches a NATS subject
// filter. Recognises the two NATS wildcards:
//
//   - "*" matches exactly one token.
//   - ">" matches one or more trailing tokens (must be the final token).
//
// A bare ">" matches any non-empty subject; an empty subject matches no
// filter.
func subjectMatchesFilter(subject, filter string) bool {
	if subject == "" || filter == "" {
		return false
	}
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

// matchesLivePath returns true when the request path matches LivePath
// (exact-or-suffix), with the same segment-boundary guarantee as
// matchesHealthPath.
func (h *Handler) matchesLivePath(reqPath string) bool {
	return matchesConfiguredPath(reqPath, h.LivePath, defaultLivePath)
}

// matchesReadyPath returns true when the request path matches ReadyPath
// (exact-or-suffix), with the same segment-boundary guarantee as
// matchesHealthPath.
func (h *Handler) matchesReadyPath(reqPath string) bool {
	return matchesConfiguredPath(reqPath, h.ReadyPath, defaultReadyPath)
}

// matchesConfiguredPath compares the request path against an operator-
// configured probe path. Falls back to defaultPath when configuredPath is
// empty, normalises a leading slash, and accepts either an exact match or
// a HasSuffix match — the leading slash is what makes the suffix check
// safe (e.g. "/eventslivez" cannot accidentally match "/livez").
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

// reserveConnSlot atomically tries to reserve a connection slot, returning
// true on success and false if MaxConnections has already been reached.
// Implemented as a CAS loop so concurrent ServeHTTP goroutines can race
// without locking. Every successful reserveConnSlot must be paired with
// exactly one releaseConnSlot.
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

// releaseConnSlot decrements the connection counter. Always defer this
// after a successful reserveConnSlot so an early return or panic doesn't
// leak a slot.
func (h *Handler) releaseConnSlot() {
	atomic.AddInt64(&h.connCount, -1)
}

// allowedMethodsHeader builds a deduplicated, canonical "Allow"/CORS
// methods header value from the configured AllowedMethods list. Only GET
// and OPTIONS are advertised — NUTS is read-only, and exposing other
// methods would mislead clients into expecting features that don't exist.
// Falls back to "GET, OPTIONS" when nothing valid is configured.
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
