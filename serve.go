// serve.go — HTTP/SSE request handling.
//
// This file contains the per-request hot path: it accepts an HTTP GET from a
// browser EventSource, parses the requested topics and replay cursor, opens an
// ordered JetStream consumer (consumer.go), and writes messages back as
// Server-Sent Events until the client disconnects, the handler shuts down, a
// write misses its deadline, or the consumer cannot be recreated.
//
// The request lifecycle is broken into small, testable steps invoked from
// ServeHTTP:
//
//  1. handleControlRequest   — short-circuits health/liveness/readiness/CORS.
//  2. parseStreamRequest     — extracts and validates topics and the cursor.
//  3. authorizeStreamRequest — enforces optional subscriber JWT auth.
//  4. readStreamSnapshot     — reads JetStream state to inform planning.
//  5. planSubscription       — picks the start position and detects bad topics.
//  6. openConsumerStream     — creates the ordered consumer and its feed.
//  7. serveStream            — runs the SSE select-loop until disconnect.
package nuts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
)

// varyContains reports whether token (case-insensitive, trimmed) is
// present as one of the comma-separated entries across the Vary header
// values. Used to make setCORSHeaders idempotent against both
// repeated Add("Vary", "Origin") (separate header values) and an
// upstream that wrote a single combined value like
// "Origin, Accept-Encoding". Token comparison is the contract the
// HTTP spec defines for Vary; treating Header().Values() as opaque
// strings would miss the combined case and emit a duplicate header.
//
// Note: Vary entries are bare HTTP field-name tokens (RFC 7231 §7.1.4),
// which forbid commas and quoted-strings, so the simple comma split is
// sufficient — no need to honour RFC 7230 quoted-string parsing here.
func varyContains(values []string, token string) bool {
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

const (
	// maxReplayCursor is reserved as an "invalid" sentinel: a Last-Event-ID
	// equal to math.MaxUint64 cannot be incremented for StartSequence without
	// wrapping, so we reject it up-front and fall back to DeliverNew.
	maxReplayCursor = ^uint64(0)
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
	// dropReasonReplayWindow tags a replayed message that JetStream delivered
	// from a time-bounded fallback but that predates the replay window.
	dropReasonReplayWindow string = "replay_window"
	// dropReasonControlMessage tags a message the server stored for its own
	// bookkeeping (a subject delete marker or a schedule definition), which
	// is not an application event.
	dropReasonControlMessage string = "control_message"
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
	// HasSnapshot mirrors streamInfoSnapshot.HasSnapshot at plan time.
	// Required for the replay-cap accounting in countsTowardReplayCap to
	// distinguish "no snapshot observed" from "snapshot says LastSeq=0";
	// see that function's comment for the failure mode the flag prevents.
	HasSnapshot bool
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
	// RequestedSubjects is the set of FullSubjects, used to drop duplicate
	// topics while parsing the request.
	RequestedSubjects map[string]struct{}
	// Replay is the resolved replay plan for this request.
	Replay replayPlan
	// FailedTopics lists topic names that were rejected during planning
	// (e.g. not allowed by the configured stream's subject filters). When
	// non-empty, the request short-circuits with 503.
	FailedTopics []string
	// ConsumerInactiveLimit caps the consumer's InactiveThreshold; see
	// streamInfoSnapshot.
	ConsumerInactiveLimit time.Duration
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

// streamInfoSnapshot is a frozen view of relevant JetStream stream state at
// the moment the request was planned. Reading once and reusing avoids racing
// against background JetStream activity during planning decisions.
type streamInfoSnapshot struct {
	// HasSnapshot is true when StreamInfo succeeded and the FirstSeq /
	// LastSeq fields below carry an observed value. False means
	// "snapshot unavailable" — distinct from "snapshot says LastSeq=0"
	// (an empty stream). The replay-cap accounting in
	// countsTowardReplayCap relies on this distinction so a transient
	// StreamInfo failure doesn't make live messages count toward
	// replay_max_messages.
	HasSnapshot bool
	// FirstSeq is the lowest sequence currently retained in the stream.
	// Used to detect requests for purged sequences.
	FirstSeq uint64
	// LastSeq is the highest sequence at snapshot time. Used as the replay
	// cap so live messages don't count against replay_max_messages.
	LastSeq uint64
	// Subjects are the configured stream subject filters; a multi-topic
	// request whose subjects are not allowed by these filters is rejected.
	Subjects []string
	// ConsumerInactiveLimit is the stream's consumer_limits.inactive_threshold
	// (0 when unset). The server refuses consumers that ask for more.
	ConsumerInactiveLimit time.Duration
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
	js       jetstream.JetStream
	shutdown <-chan struct{}
	// disconnected is true while the NATS connection is down and
	// reconnecting, when every JetStream call could only time out.
	disconnected bool
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
	// ConsumerName is the server-side consumer that delivered the message. A
	// change between messages means the ordered consumer recreated itself.
	ConsumerName string
	// NumPending is how many matching messages were still waiting in the
	// stream when this one was delivered.
	NumPending uint64
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

// ServeHTTP implements caddyhttp.MiddlewareHandler. It dispatches health and
// CORS-preflight requests, validates and authorizes the SSE subscription
// request, opens an ordered JetStream consumer, and runs the SSE streaming
// loop until the client or the server closes the connection. Non-stream
// requests are passed to the next handler in the Caddy chain when one is
// configured.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	// Set CORS headers before any short-circuit response path. Otherwise a
	// browser hitting a 401 (JWT failure), 400 (validation), 405 (method),
	// 503 (jetstream unavailable / subscription failed / readiness degraded),
	// or 429 (max_connections) would see an opaque CORS failure instead of
	// the real status code. setCORSHeaders is a
	// no-op when the request has no Origin or the Origin is not allow-
	// listed, so this is safe to call on every request including probes.
	h.setCORSHeaders(w, r)

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

	if !supportsFlush(w) {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return nil
	}

	// While NATS is reconnecting, answer at once instead of letting the
	// stream-info read and the consumer create each run into their timeout.
	runtime := h.currentStreamRuntime()
	if runtime.js == nil || runtime.disconnected {
		h.log().Warn("JetStream not available for SSE stream",
			appendStreamLogFields(plan, zap.String("disconnect_reason", "jetstream_unavailable"))...,
		)
		h.rejectTransient(w, r, http.StatusServiceUnavailable, "JetStream not available")
		return nil
	}

	// Reserve before creating a JetStream consumer so we never pay the
	// consumer cost for a request we'd just reject anyway.
	if h.MaxConnections > 0 {
		if !h.reserveConnSlot() {
			metricsConnectionsRejected.WithLabelValues("max_connections").Inc()
			h.log().Warn("rejecting SSE stream: max_connections reached",
				appendStreamLogFields(plan,
					zap.String("disconnect_reason", "max_connections"),
					zap.Int("max_connections", h.MaxConnections),
				)...,
			)
			// 429 (RFC 6585) is the precise status for a per-client/server
			// concurrency cap: the server is healthy, the caller should back
			// off. Using 503 here would collide with the genuine-503 paths
			// (jetstream_unavailable / subscription_failed / readiness probe
			// degraded) and trip circuit breakers into opening the circuit
			// when the right reaction is to keep retrying with Retry-After.
			h.rejectTransient(w, r, http.StatusTooManyRequests, "Too many concurrent connections")
			return nil
		}
		defer h.releaseConnSlot()
	}

	snapshot := h.readStreamSnapshot(r.Context(), runtime.js, plan)
	plan = h.planSubscription(plan, snapshot)
	if len(plan.FailedTopics) > 0 {
		metricsSubscriptionErrors.Inc()
		h.log().Warn("failed to subscribe to requested SSE topics",
			appendStreamLogFields(plan,
				zap.Strings("failed_topics", plan.FailedTopics),
				zap.String("disconnect_reason", "subscription_failed"),
			)...,
		)
		http.Error(w, fmt.Sprintf("Failed to subscribe to requested topics: %s", strings.Join(plan.FailedTopics, ", ")), http.StatusServiceUnavailable)
		return nil
	}

	feed, err := h.openFeed(r.Context(), runtime.js, plan)
	if err != nil {
		h.rejectConsumerFailure(w, r, plan, err)
		return nil
	}
	defer feed.stop()

	return h.serveStream(w, r, plan, feed, runtime.shutdown)
}

// transientRetryBase is the average delay a client is asked to wait before
// retrying a stream request that failed for a transient reason. The delay
// actually sent is jittered by ±50%, so clients rejected together do not all
// come back together.
const transientRetryBase = 5 * time.Second

// retryDelay returns a delay in [transientRetryBase/2, 3*transientRetryBase/2).
func retryDelay() time.Duration {
	return transientRetryBase/2 + rand.N(transientRetryBase)
}

// acceptsEventStream reports whether the client asked for an event stream.
// Native EventSource always sends Accept: text/event-stream.
func acceptsEventStream(r *http.Request) bool {
	for _, accept := range r.Header.Values("Accept") {
		if strings.Contains(strings.ToLower(accept), "text/event-stream") {
			return true
		}
	}
	return false
}

// rejectTransient answers a stream request that failed for a reason expected
// to clear up on its own: JetStream unavailable, max_connections, the
// stream's consumer limit, or a failed consumer create. A native EventSource
// treats any answer other than a 200 event stream as fatal and stops
// reconnecting for good (#105). A client that accepts text/event-stream
// therefore gets an empty 200 stream that carries only a retry: delay; it
// reconnects after that delay and keeps its Last-Event-ID. Other clients get
// the status code with a Retry-After header.
func (h *Handler) rejectTransient(w http.ResponseWriter, r *http.Request, status int, message string) {
	delay := retryDelay()
	if acceptsEventStream(r) {
		h.setSSEHeaders(w)
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, ": %s\nretry: %d\n\n", message, delay.Milliseconds())
		return
	}
	w.Header().Set("Retry-After", strconv.Itoa(int((delay+time.Second-1)/time.Second)))
	http.Error(w, message, status)
}

// rejectConsumerFailure answers a request whose JetStream consumer could not
// be created. Every cause is treated as transient: the stream may be
// recreated, and consumer limits free up as other clients leave.
func (h *Handler) rejectConsumerFailure(w http.ResponseWriter, r *http.Request, plan streamPlan, err error) {
	switch {
	case errors.Is(err, errHandlerClosing):
		h.log().Debug("rejecting SSE stream: handler shutting down",
			appendStreamLogFields(plan, zap.String("disconnect_reason", "handler_shutdown"))...,
		)
		h.rejectTransient(w, r, http.StatusServiceUnavailable, "JetStream not available")
	case errors.Is(err, jetstream.ErrMaximumConsumersLimit):
		// nats-server 2.15 caps every stream at 1000 consumers unless
		// max_consumers says otherwise, and NUTS needs one per connection.
		metricsConnectionsRejected.WithLabelValues("stream_consumer_limit").Inc()
		h.log().Warn("rejecting SSE stream: the stream's consumer limit is reached",
			appendStreamLogFields(plan,
				zap.String("disconnect_reason", "stream_consumer_limit"),
				zap.Error(err),
			)...,
		)
		h.rejectTransient(w, r, http.StatusServiceUnavailable, "Stream consumer limit reached")
	default:
		metricsSubscriptionErrors.Inc()
		fields := []zap.Field{zap.String("disconnect_reason", "subscription_failed"), zap.Error(err)}
		h.log().Error("failed to create JetStream consumer",
			appendStreamLogFields(plan, append(fields, jetStreamErrorFields(err)...)...)...,
		)
		h.rejectTransient(w, r, http.StatusServiceUnavailable, "Failed to subscribe to requested topics: "+strings.Join(plan.Topics, ", "))
	}
}

// jetStreamErrorFields names the JetStream API error code of a failed call,
// so server-side refusals can be told apart in the logs: for example 10153,
// an inactive threshold above the stream's consumer limit, or 10059, a
// missing stream.
func jetStreamErrorFields(err error) []zap.Field {
	var apiErr *jetstream.APIError
	if errors.As(err, &apiErr) {
		return []zap.Field{zap.Int("jetstream_error_code", int(apiErr.ErrorCode))}
	}
	return nil
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
		// setCORSHeaders already ran at the top of ServeHTTP before
		// handleControlRequest was called. No need to call it again.
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

	// Both cursor sources are parsed. A malformed ?last-id= is the client's
	// explicit mistake (400); a malformed Last-Event-ID came from a browser
	// auto-resume, so it is logged and ignored rather than breaking
	// reconnect. When both are valid the header wins: EventSource keeps its
	// URL for its whole lifetime, so every auto-reconnect resends the
	// original ?last-id= next to the fresher Last-Event-ID.
	cursor, queryProblem, _ := parseReplayCursor(r.URL.Query().Get("last-id"))
	switch queryProblem {
	case cursorTooLong:
		return streamPlan{}, &streamRequestError{status: http.StatusBadRequest, message: "Invalid last-id value: too long"}
	case cursorInvalid:
		return streamPlan{}, &streamRequestError{status: http.StatusBadRequest, message: "Invalid last-id value: must be an unsigned integer below the maximum cursor value"}
	}
	hasCursor := queryProblem == cursorValid

	headerValue := r.Header.Get("Last-Event-ID")
	headerCursor, headerProblem, headerErr := parseReplayCursor(headerValue)
	switch headerProblem {
	case cursorValid:
		cursor, hasCursor = headerCursor, true
	case cursorTooLong:
		h.log().Warn("ignoring oversized Last-Event-ID header", zap.Int("length", len(headerValue)))
	case cursorInvalid:
		fields := []zap.Field{zap.String("value", headerValue)}
		if headerErr != nil {
			fields = append(fields, zap.Error(headerErr))
		} else {
			fields = append(fields, zap.String("reason", "cursor would overflow"))
		}
		h.log().Warn("ignoring unparseable Last-Event-ID header", fields...)
	}

	if hasCursor {
		plan.Replay = replayPlan{
			HasLastID:     true,
			Mode:          replayModeStartSequence,
			StartSequence: cursor + 1,
		}
		metricsReplayRequests.Inc()
	}
	return plan, nil
}

// cursorProblem classifies a replay cursor value.
type cursorProblem int

const (
	cursorAbsent cursorProblem = iota
	cursorValid
	cursorTooLong
	cursorInvalid
)

// parseReplayCursor parses a last-id / Last-Event-ID value. The returned
// error, if any, is strconv's reason for an invalid value.
func parseReplayCursor(value string) (uint64, cursorProblem, error) {
	if value == "" {
		return 0, cursorAbsent, nil
	}
	// Cap the input length before strconv.ParseUint so a multi-MB numeric
	// string can't cause large allocations. uint64 max is 20 digits.
	if len(value) > 20 {
		return 0, cursorTooLong, nil
	}
	id, err := strconv.ParseUint(value, 10, 64)
	// id+1 is the JetStream start sequence. When id == maxReplayCursor-1
	// that addition lands on maxReplayCursor itself, the reserved "invalid"
	// sentinel, and JetStream would park the consumer at a sequence that
	// never arrives, so the off-by-one is rejected along with the
	// == maxReplayCursor case.
	if err != nil || id >= maxReplayCursor-1 {
		return 0, cursorInvalid, err
	}
	return id, cursorValid, nil
}

// currentStreamRuntime takes a single locked snapshot of the handler's
// NATS-level state. Cleanup() can swap these fields under the same mutex,
// so a per-request snapshot lets the streaming loop run without holding
// the lock for the duration of the connection.
func (h *Handler) currentStreamRuntime() streamRuntime {
	h.mu.RLock()
	runtime := streamRuntime{
		js:           h.js,
		shutdown:     h.shutdown,
		disconnected: h.conn != nil && !h.conn.IsConnected(),
	}
	h.mu.RUnlock()
	return runtime
}

// readStreamSnapshot reads the JetStream stream metadata that planning needs:
// first/last sequence for the start position and the replay cap, the
// configured subjects to validate the requested topics, and, for replay
// requests under a replay_window, the publish time of the resume message.
// Every request reads it, since requests without a cursor start at
// LastSeq+1. A failed read is logged and returns a zero-value snapshot, which
// planning treats as "no snapshot information available".
//
// Both reads share one context bounded by defaultMetadataReadTimeout, so a
// partially degraded JetStream cluster cannot stall a new SSE handshake past
// the per-request budget while holding a Caddy handler goroutine and a
// MaxConnections slot. The readiness probe (serveReadinessCheck) applies the
// same pattern with the tighter defaultReadinessProbeTimeout.
func (h *Handler) readStreamSnapshot(ctx context.Context, js streamLookup, plan streamPlan) streamInfoSnapshot {
	readCtx, cancel := context.WithTimeout(ctx, defaultMetadataReadTimeout)
	defer cancel()
	stream, err := js.Stream(readCtx, h.StreamName)
	if err != nil {
		h.log().Warn("failed to read JetStream stream info for request planning",
			appendStreamLogFields(plan, zap.Error(err))...,
		)
		return streamInfoSnapshot{}
	}
	info := stream.CachedInfo()
	snapshot := streamInfoSnapshot{
		HasSnapshot:           true,
		FirstSeq:              info.State.FirstSeq,
		LastSeq:               info.State.LastSeq,
		Subjects:              info.Config.Subjects,
		ConsumerInactiveLimit: info.Config.ConsumerLimits.InactiveThreshold,
	}
	if plan.Replay.HasLastID && h.ReplayWindow > 0 && plan.Replay.StartSequence >= info.State.FirstSeq {
		msg, err := stream.GetMsg(readCtx, plan.Replay.StartSequence)
		if errors.Is(err, jetstream.ErrMsgNotFound) {
			// The resume message itself was deleted (TTL, rollup,
			// MaxMsgsPerSubject); the next retained message dates the
			// resume point instead of forcing a window fallback that would
			// replay messages the client already has.
			msg, err = stream.GetMsg(readCtx, plan.Replay.StartSequence, jetstream.WithGetMsgSubject(">"))
		}
		if err == nil {
			snapshot.StartSequenceTime = msg.Time
			snapshot.HasStartSequenceTime = true
		} else {
			h.log().Debug("failed to read replay start sequence timestamp",
				appendStreamLogFields(plan, zap.Error(err))...,
			)
		}
	}
	return snapshot
}

// planSubscription finalises the streamPlan in the light of the JetStream
// snapshot. It rejects topics that the configured stream's subjects do not
// cover (current nats-servers would otherwise create a consumer that silently
// delivers nothing), gives requests without a cursor an explicit start
// sequence, and rewrites the replay plan to a fallback when the requested
// sequence is below retention or outside the configured replay_window.
// Idempotent: calling it twice with the same snapshot yields the same result.
func (h *Handler) planSubscription(plan streamPlan, snapshot streamInfoSnapshot) streamPlan {
	plan.ConsumerInactiveLimit = snapshot.ConsumerInactiveLimit
	if len(snapshot.Subjects) > 0 {
		for idx, fullSubject := range plan.FullSubjects {
			if !subjectAllowedByStream(fullSubject, snapshot.Subjects) {
				plan.FailedTopics = append(plan.FailedTopics, plan.Topics[idx])
			}
		}
	}
	if len(plan.FailedTopics) > 0 {
		return plan
	}
	if !plan.Replay.HasLastID {
		// Start right after the snapshot's last message. The connected event
		// then carries that position as its id, so a client that disconnects
		// before its first message still resumes without a gap.
		if snapshot.HasSnapshot {
			plan.Replay.StartSequence = snapshot.LastSeq + 1
		}
		return plan
	}
	plan.Replay.CapSequence = snapshot.LastSeq
	plan.Replay.HasSnapshot = snapshot.HasSnapshot
	switch {
	case !snapshot.HasSnapshot:
		// The resume point cannot be dated, so replay_window falls back to
		// the window itself instead of replaying history the operator
		// restricted. The replay cap still applies: see replayHistory.
		if h.ReplayWindow > 0 {
			plan.Replay = h.fallbackReplayPlan(plan.Replay, "stream info unavailable")
		}
	case plan.Replay.StartSequence > snapshot.LastSeq+1:
		// A cursor from a recreated or restored stream: the server would
		// park the consumer until the stream reached it, silently skipping
		// everything before.
		plan.Replay = h.fallbackReplayPlan(plan.Replay, "cursor ahead of stream")
	case plan.Replay.StartSequence < snapshot.FirstSeq:
		// The server itself clamps a start below retention to FirstSeq and
		// returns no error, so this branch exists to apply replay_window and
		// to log and count the fallback, not to avoid a failed subscribe.
		plan.Replay = h.fallbackReplayPlan(plan.Replay, "sequence below retention")
	case h.shouldUseReplayWindow(plan.Replay, snapshot):
		plan.Replay = h.fallbackReplayPlan(plan.Replay, "sequence outside replay window")
	}
	return plan
}

// shouldUseReplayWindow reports whether the configured replay window forces
// the request into the time-bounded fallback even though the requested
// sequence is still retained. We force the fallback when the original
// publish time is older than now-ReplayWindow, or when we couldn't read
// the start-sequence timestamp at all (conservative: assume out-of-window
// rather than risk emitting forbidden history).
//
// Condition order is load-bearing: the `snapshot.LastSeq < replay.StartSequence`
// short-circuit in the first conditional below MUST run before the
// `!HasStartSequenceTime` conservative branch. A fully caught-up client
// passes `last-id == LastSeq`, which
// computes `StartSequence = LastSeq+1` — a sequence that doesn't yet
// exist, so readStreamSnapshot's GetMsg call fails and leaves
// HasStartSequenceTime=false. The "caught up" short-circuit returns
// false (no fallback needed); reordering would re-introduce a bogus
// time-windowed fallback for clients that are simply up to date.
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

// setSSEHeaders sets the headers of an event-stream response.
func (h *Handler) setSSEHeaders(w http.ResponseWriter) {
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
}

// serveStream is the SSE writer loop. It writes the response headers and the
// initial "connected" event, then multiplexes between these sources until one
// of them terminates the connection:
//
//   - shutdown     — Cleanup() closing the handler-wide channel.
//   - ctx.Done()   — the HTTP client closed or timed out.
//   - feed.errs    — the ordered consumer could not be recreated.
//   - feed.frames  — a formatted JetStream message to write and flush.
//   - heartbeat.C  — periodic SSE comment to keep proxies from closing idle
//     connections.
//
// A slow client is one whose writes miss the write_timeout deadline; the
// feed never overflows, because it stops pulling while the writer is busy.
//
// Returns nil on every termination path: SSE has no notion of an HTTP error
// after streaming has begun, so all exits are observable as a normal
// connection close.
func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request, plan streamPlan, feed *streamFeed, shutdown <-chan struct{}) error {
	metricsActiveConnections.Inc()
	defer metricsActiveConnections.Dec()
	writeTimeout := time.Duration(h.WriteTimeout) * time.Second
	// All flushing and write deadlines go through the ResponseController so
	// they reach the connection through Caddy's wrapping writers.
	rc := http.NewResponseController(w)

	// CORS headers were already applied at the top of ServeHTTP — repeating
	// the call here is harmless (idempotent) but unnecessary.
	h.setSSEHeaders(w)

	if err := writeSSEChunkWithTimeout(w, rc, formatConnectedEvent(plan), writeTimeout); err != nil {
		h.recordWriteDisconnect(plan, "connected", err)
		return nil
	}

	heartbeat := time.NewTicker(time.Duration(h.HeartbeatInterval) * time.Second)
	defer heartbeat.Stop()

	replayDelivered := 0
	history := newReplayHistory(plan)
	batch := make([]string, 0, maxBatchFrames)

	ctx := r.Context()
	for {
		select {
		case <-shutdown:
			h.log().Debug("handler shutting down; closing SSE stream",
				appendStreamLogFields(plan, zap.String("disconnect_reason", "handler_shutdown"))...,
			)
			return nil

		case <-ctx.Done():
			h.log().Debug("client disconnected",
				appendStreamLogFields(plan,
					zap.String("disconnect_reason", "client_context_done"),
					zap.Error(ctx.Err()),
				)...,
			)
			return nil

		case err := <-feed.errs:
			metricsConsumerInvalidated.WithLabelValues("unrecoverable").Inc()
			h.log().Warn("closing SSE stream: JetStream consumer could not be recreated",
				appendStreamLogFields(plan,
					zap.String("disconnect_reason", "consumer_unrecoverable"),
					zap.Error(err),
				)...,
			)
			return nil

		case formatted := <-feed.frames:
			var capReached bool
			batch, capReached = h.collectBatch(batch[:0], plan, feed.frames, formatted, history, &replayDelivered)
			if len(batch) > 0 {
				if err := writeSSEChunksWithTimeout(w, rc, writeTimeout, batch...); err != nil {
					h.recordWriteDisconnect(plan, "message", err, zap.String("message_subject", formatted.Subject))
					return nil
				}
				metricsMessagesDelivered.Add(float64(len(batch)))
			}
			if capReached {
				metricsReplayCapReached.Inc()
				h.log().Warn("closing SSE stream: replay_max_messages reached",
					appendStreamLogFields(plan,
						zap.String("disconnect_reason", "replay_cap_reached"),
						zap.Int("replay_max_messages", h.ReplayMaxMessages),
						zap.Int("replay_delivered", replayDelivered),
					)...,
				)
				return nil
			}

		case <-heartbeat.C:
			if err := writeSSEChunkWithTimeout(w, rc, formatHeartbeatEvent(time.Now()), writeTimeout); err != nil {
				h.recordWriteDisconnect(plan, "heartbeat", err)
				return nil
			}
		}
	}
}

// maxBatchFrames and maxBatchBytes bound a batch: frames already waiting when
// the writer takes one are written with it and flushed together, which saves
// a flush, a syscall and a deadline set/clear per frame during bursts and
// replays (#121). A batch only ever holds frames that are already queued, so
// it adds no latency.
const (
	maxBatchFrames = 32
	maxBatchBytes  = 64 << 10
)

// collectBatch appends the frame just received, and whatever frames are
// queued behind it, to batch. Each frame goes through the replay_window
// filter and counts towards replay_max_messages; when the cap is reached the
// batch ends with that frame and the stream is to be closed.
func (h *Handler) collectBatch(batch []string, plan streamPlan, frames <-chan formattedMessageEvent, frame formattedMessageEvent, history *replayHistory, replayDelivered *int) ([]string, bool) {
	size := 0
	for {
		historical := history.isHistory(frame)
		if historical && shouldSkipReplayWindowMessage(plan, frame) {
			metricsMessagesDropped.WithLabelValues(dropReasonReplayWindow).Inc()
			h.log().Debug("skipping replayed message older than replay_window",
				appendStreamLogFields(plan, zap.Uint64("stream_sequence", frame.StreamSequence))...,
			)
		} else {
			batch = append(batch, frame.Frame)
			size += len(frame.Frame)
			if historical && h.ReplayMaxMessages > 0 {
				*replayDelivered++
				if *replayDelivered >= h.ReplayMaxMessages {
					return batch, true
				}
			}
		}
		if len(batch) >= maxBatchFrames || size >= maxBatchBytes {
			return batch, false
		}
		select {
		case frame = <-frames:
		default:
			return batch, false
		}
	}
}

// recordWriteDisconnect logs and counts a stream that ended on a failed write.
// A write that missed its write_timeout deadline means the client stopped
// reading: that is the slow-client case now that backpressure keeps the feed
// from ever overflowing.
func (h *Handler) recordWriteDisconnect(plan streamPlan, site string, err error, fields ...zap.Field) {
	metricsWriteDisconnects.WithLabelValues(site).Inc()
	reason := "write_error"
	message := "failed to write " + site + " event"
	if site == "heartbeat" {
		reason = "heartbeat_write_error"
		message = "failed to write heartbeat"
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		metricsSlowClientDisconnects.Inc()
		reason = "slow_client"
	}
	h.log().Warn(message,
		appendStreamLogFields(plan, append([]zap.Field{
			zap.String("disconnect_reason", reason),
			zap.String("write_site", site),
			zap.Int("write_timeout_seconds", h.WriteTimeout),
			zap.Error(err),
		}, fields...)...)...,
	)
}

// shouldSkipReplayWindowMessage filters out replayed messages that JetStream
// delivered but that predate the configured replay window. Necessary because
// the start-time resolution is server-side and can include messages published
// an instant before the requested cutoff. Callers apply it to the replayed
// backlog only: live messages can carry older timestamps (a mirror catching
// up, clock skew) and must never be filtered.
func shouldSkipReplayWindowMessage(plan streamPlan, formatted formattedMessageEvent) bool {
	return plan.Replay.Mode == replayModeFallbackStartTime &&
		!plan.Replay.StartTime.IsZero() &&
		formatted.HasMessageTime &&
		formatted.MessageTime.Before(plan.Replay.StartTime)
}

// replayHistory tells replayed backlog apart from live traffic on one stream,
// so replay_max_messages and the replay_window filter apply to the backlog
// only. With a snapshot the boundary is the stream's LastSeq when the request
// was planned. Without one (StreamInfo failed) the first delivered message's
// pending count gives the size of the backlog instead, so both protections
// still hold rather than switching off.
type replayHistory struct {
	replay     bool
	bySequence bool
	lastSeq    uint64
	counting   bool
	remaining  uint64
}

func newReplayHistory(plan streamPlan) *replayHistory {
	return &replayHistory{
		replay:     plan.Replay.HasLastID,
		bySequence: plan.Replay.HasSnapshot,
		lastSeq:    plan.Replay.CapSequence,
	}
}

// isHistory reports whether a delivered message belongs to the replayed
// backlog. Call it once per message, in delivery order.
func (r *replayHistory) isHistory(formatted formattedMessageEvent) bool {
	if !r.replay {
		return false
	}
	if !formatted.HasStreamSequence {
		// Nothing to place the message by: count it, erring on the side of
		// the operator's replay budget.
		return true
	}
	if r.bySequence {
		return formatted.StreamSequence <= r.lastSeq
	}
	if !r.counting {
		r.counting = true
		r.remaining = formatted.NumPending + 1
	}
	if r.remaining == 0 {
		return false
	}
	r.remaining--
	return true
}

// formatConnectedEvent renders the SSE handshake event sent immediately
// after headers. Useful for clients that want to confirm the subscription
// landed on the topics they expected (after path-shorthand expansion or
// authorization-driven topic filtering). It carries the stream position the
// consumer starts after as its id, so a client that reconnects before its
// first message resumes from there instead of from "now".
func formatConnectedEvent(plan streamPlan) string {
	var event strings.Builder
	if id, ok := connectedEventID(plan); ok {
		event.WriteString("id: ")
		event.WriteString(strconv.FormatUint(id, 10))
		event.WriteString("\n")
	}
	event.WriteString("event: connected\ndata: {\"topics\":")
	event.WriteString(toJSON(plan.Topics))
	event.WriteString("}\n\n")
	return event.String()
}

// connectedEventID is the last sequence before the consumer's explicit start
// position. Fallback plans have none: the client keeps its previous cursor,
// which replans to the same fallback on reconnect.
func connectedEventID(plan streamPlan) (uint64, bool) {
	if plan.Replay.Mode.isFallback() || plan.Replay.StartSequence == 0 {
		return 0, false
	}
	return plan.Replay.StartSequence - 1, true
}

// formatHeartbeatEvent renders an SSE comment line ("colon-prefixed"
// per the SSE spec) carrying the current time. Comments are ignored by
// EventSource clients but keep proxies from declaring the connection idle.
func formatHeartbeatEvent(now time.Time) string {
	return fmt.Sprintf(": heartbeat %s\n\n", now.UTC().Format(time.RFC3339))
}

// formatMessageEvent turns a JetStream message into a fully rendered SSE frame
// (or a Dropped record). Drops are decided in two places:
//
//  1. Before any work: if the raw NATS payload exceeds MaxEventSize, we
//     bail before parsing JSON or building the envelope.
//  2. After rendering: if the JSON-wrapped frame exceeds MaxEventSize.
//
// A message without JetStream metadata is still rendered, with `now` as the
// timestamp and no `id:` field, so a metadata blip doesn't take the stream
// down; the error is surfaced to the caller so it can be logged once.
func (h *Handler) formatMessageEvent(msg streamMessage, now time.Time) formattedMessageEvent {
	formatted := formattedMessageEvent{Subject: msg.Subject, MetadataErr: msg.MetadataErr}
	if msg.HasMetadata {
		formatted.StreamSequence = msg.StreamSequence
		formatted.HasStreamSequence = true
		formatted.MessageTime = msg.Timestamp
		formatted.HasMessageTime = true
		formatted.ConsumerName = msg.ConsumerName
		formatted.NumPending = msg.NumPending
	}
	if isControlMessage(msg.Header) {
		formatted.Dropped = true
		formatted.DropReason = dropReasonControlMessage
		return formatted
	}
	if h.MaxEventSize > 0 && len(msg.Data) > h.MaxEventSize {
		formatted.Dropped = true
		formatted.DropReason = dropReasonRawPayload
		formatted.DropSize = len(msg.Data)
		return formatted
	}

	// Build the frame in one pass. The data line is the JSON encoding of
	// messageEventPayload, written by hand: the payload is validated once
	// and copied once, compacted and HTML-escaped exactly as json.Marshal
	// embeds a json.RawMessage (#122). Pre-size for the payload plus the
	// envelope (id/event/data lines and the JSON wrapper).
	var event strings.Builder
	event.Grow(len(msg.Data) + 128)
	var scratch [64]byte
	if msg.HasMetadata {
		event.WriteString("id: ")
		event.Write(strconv.AppendUint(scratch[:0], msg.StreamSequence, 10))
		event.WriteByte('\n')
	}
	event.WriteString("event: message\ndata: {\"topic\":")
	writeJSONString(&event, strings.TrimPrefix(msg.Subject, h.TopicPrefix))
	event.WriteString(`,"payload":`)
	writeJSONPayload(&event, msg.Data)
	event.WriteString(`,"time":"`)
	timestamp := now
	if msg.HasMetadata {
		timestamp = msg.Timestamp
	}
	event.Write(timestamp.UTC().AppendFormat(scratch[:0], time.RFC3339))
	event.WriteString("\"}\n\n")

	if h.MaxEventSize > 0 && event.Len() > h.MaxEventSize {
		formatted.Dropped = true
		formatted.DropReason = dropReasonFormattedSSEMessage
		formatted.DropSize = event.Len()
		return formatted
	}
	formatted.Frame = event.String()
	return formatted
}

// isControlMessage reports whether a stored message was written by the server
// or holds a schedule rather than being an application event: subject delete
// markers (ADR-43) and message schedule definitions (ADR-51). The messages a
// schedule produces carry Nats-Scheduler instead of Nats-Schedule and are
// delivered as usual.
func isControlMessage(header nats.Header) bool {
	if _, ok := header[jetstream.MarkerReasonHeader]; ok {
		return true
	}
	_, ok := header[jetstream.ScheduleHeader]
	return ok
}

// recordDroppedMessage emits the metric and structured log line for a
// message the formatter decided not to send. Operators rely on these logs
// to size MaxEventSize correctly without grepping the message payloads.
//
// Callers must only invoke this with a formattedMessageEvent where Dropped
// is true; formatMessageEvent always sets DropReason to one of the declared
// constants (dropReasonControlMessage / dropReasonRawPayload /
// dropReasonFormattedSSEMessage) at every Dropped=true assignment site, so
// the metric label set stays bounded to the values documented in README
// "nuts_messages_dropped_total".
func (h *Handler) recordDroppedMessage(formatted formattedMessageEvent) {
	metricsMessagesDropped.WithLabelValues(formatted.DropReason).Inc()
	switch formatted.DropReason {
	case dropReasonControlMessage:
		// Routine with per-message TTLs and schedules, so not a warning.
		h.log().Debug("skipping server control message",
			zap.String("topic", formatted.Subject),
			zap.Uint64("stream_sequence", formatted.StreamSequence),
		)
	case dropReasonRawPayload:
		h.log().Warn("dropping oversized NATS payload",
			zap.String("topic", formatted.Subject),
			zap.Uint64("stream_sequence", formatted.StreamSequence),
			zap.Int("payload_size", formatted.DropSize),
			zap.Int("max_event_size", h.MaxEventSize),
		)
	case dropReasonFormattedSSEMessage:
		h.log().Warn("dropping oversized SSE event",
			zap.String("topic", formatted.Subject),
			zap.Uint64("stream_sequence", formatted.StreamSequence),
			zap.Int("event_size", formatted.DropSize),
			zap.Int("max_event_size", h.MaxEventSize),
		)
	}
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
	// Walk both token by token without allocating (#124). A subject token
	// is always left to compare: the loop only continues while both have
	// more, and a subject ends with at least one (possibly empty) token.
	for {
		filterToken, filterRest, filterMore := strings.Cut(filter, ".")
		if filterToken == ">" {
			return true
		}
		subjectToken, subjectRest, subjectMore := strings.Cut(subject, ".")
		if filterToken != "*" && filterToken != subjectToken {
			return false
		}
		if !filterMore || !subjectMore {
			return filterMore == subjectMore
		}
		filter, subject = filterRest, subjectRest
	}
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
// safe (e.g. "/eventslivez" cannot accidentally match "/livez"). One
// trailing slash is ignored on both sides: kubelet manifests, load
// balancers and browser address bars add one, and "/livez/" must not fall
// through to topic parsing as topic "livez".
func matchesConfiguredPath(reqPath, configuredPath, defaultPath string) bool {
	path := configuredPath
	if path == "" {
		path = defaultPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	path = trimTrailingSlash(path)
	reqPath = trimTrailingSlash(reqPath)
	if reqPath == path {
		return true
	}
	return strings.HasSuffix(reqPath, path)
}

// trimTrailingSlash drops one trailing slash, keeping the root path "/".
func trimTrailingSlash(p string) string {
	if len(p) > 1 && strings.HasSuffix(p, "/") {
		return p[:len(p)-1]
	}
	return p
}

// reserveConnSlot atomically tries to reserve a connection slot, returning
// true on success and false if MaxConnections has already been reached.
// Implemented as a CAS loop so concurrent ServeHTTP goroutines can race
// without locking. Every successful reserveConnSlot must be paired with
// exactly one releaseConnSlot.
func (h *Handler) reserveConnSlot() bool {
	for {
		cur := atomic.LoadInt64(&h.connCount)
		if cur >= int64(h.MaxConnections) {
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
	// Idempotent: serveStream (and any other code path) may also call
	// setCORSHeaders. Token-membership check (not element equality) so
	// that an upstream middleware which already wrote a combined Vary
	// like "Origin, Accept-Encoding" doesn't make us append a duplicate.
	if !varyContains(w.Header().Values("Vary"), "Origin") {
		w.Header().Add("Vary", "Origin")
	}
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
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.log().Debug("failed to encode liveness response", zap.Error(err))
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

	// recordFailure is a one-shot guard: a single 503 response must increment
	// exactly one cause label (the first matched), so that the documented
	// 1:1 contract between probe-failure count and sum-over-cause series
	// holds. The body-field population below stays as multiple independent
	// `if` blocks so a missing-runtime probe still reports both
	// `nats=disconnected` and `stream=unavailable` in the JSON body.
	failureRecorded := false
	recordFailure := func(cause string, fields ...zap.Field) {
		if failureRecorded {
			return
		}
		failureRecorded = true
		metricsReadinessFailures.WithLabelValues(cause).Inc()
		h.log().Warn("readiness probe degraded", append([]zap.Field{zap.String("cause", cause)}, fields...)...)
	}

	if conn == nil || !conn.IsConnected() {
		resp.Status = "degraded"
		resp.NATS = "disconnected"
		statusCode = http.StatusServiceUnavailable
		recordFailure("nats_disconnected")
	}

	if js == nil {
		resp.Status = "degraded"
		resp.Stream = "unavailable"
		statusCode = http.StatusServiceUnavailable
		recordFailure("jetstream_missing")
	} else {
		// Bound the JetStream call so a partially-degraded server can't stall
		// the probe past the orchestrator's readiness budget. See
		// defaultReadinessProbeTimeout for rationale.
		ctx, cancel := context.WithTimeout(context.Background(), defaultReadinessProbeTimeout)
		_, err := js.Stream(ctx, h.StreamName)
		cancel()
		if err != nil {
			resp.Status = "degraded"
			resp.Stream = "unavailable"
			statusCode = http.StatusServiceUnavailable
			recordFailure("stream_info_error", zap.Error(err))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.log().Debug("failed to encode health response", zap.Error(err))
	}
	return nil
}
