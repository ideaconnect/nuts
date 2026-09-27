// ServeHTTP end to end on an embedded JetStream server: streaming, replay
// cursors, write failures, probes, hub discovery, heartbeats, topic prefixes
// and NATS restarts.
package nuts

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestHandler_ServeHTTP_NonGetDelegatesToNext(t *testing.T) {
	h := &Handler{logger: zap.NewNop()}
	req := httptest.NewRequest(http.MethodPost, "/events?topic=test", nil)
	rr := httptest.NewRecorder()

	nextCalled := false
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		nextCalled = true
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("delegated"))
		return nil
	})

	if err := h.ServeHTTP(rr, req, next); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !nextCalled {
		t.Fatal("expected next handler to be called")
	}
	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected delegated status %d, got %d", http.StatusAccepted, rr.Code)
	}
}

func TestHandler_ServeHTTP_StreamingNotSupported(t *testing.T) {
	h := &Handler{logger: zap.NewNop()}
	req := httptest.NewRequest(http.MethodGet, "/events?topic=test", nil)
	rr := newPlainRecorder()

	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rr.statusCode != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d", http.StatusInternalServerError, rr.statusCode)
	}
	if !strings.Contains(rr.body.String(), "Streaming not supported") {
		t.Fatalf("expected streaming not supported message, got %q", rr.body.String())
	}
}

func TestHandler_ServeHTTP_Integration(t *testing.T) {
	ns := startJetStreamServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("failed to connect to NATS: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "TEST_EVENTS", []string{"events.>"})

	// Subtests share the server and the stream, each on subjects of its own,
	// and get a handler each, so they pass in any order and on their own.
	newHandler := func(t *testing.T) *Handler {
		t.Helper()
		h := &Handler{
			NatsURL:           ns.ClientURL(),
			StreamName:        "TEST_EVENTS",
			TopicPrefix:       "events.",
			HeartbeatInterval: 30,
			ReconnectWait:     2,
			MaxReconnects:     intPtr(-1),
			AllowedOrigins:    []string{"*"},
			logger:            zap.NewNop(),
		}
		connectHandler(t, h)
		return h
	}

	t.Run("SSE connection and message delivery", func(t *testing.T) {
		h := newHandler(t)
		rr, cancel, done := startSSE(t, h, "/events?topic=test", "")
		defer stopSSE(t, cancel, done)

		jsCtx, _ := nc.JetStream()
		if _, err := jsCtx.Publish("events.test", []byte(`{"hello":"world"}`)); err != nil {
			t.Fatalf("failed to publish message: %v", err)
		}
		if !waitForSSEBody(rr, `{"hello":"world"}`, 3*time.Second) {
			t.Fatalf("message not delivered; body=%q", rr.Body())
		}

		body := rr.Body()
		if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
			t.Errorf("Content-Type: expected text/event-stream, got %q", ct)
		}
		if !strings.Contains(body, "event: connected") {
			t.Error("response should contain 'event: connected'")
		}
		if !strings.Contains(body, "event: message") {
			t.Error("response should contain 'event: message'")
		}
		// Should contain id field for replay support
		if !strings.Contains(body, "id: ") {
			t.Error("response should contain 'id: ' for replay support")
		}
	})

	t.Run("SSE with last-id parameter", func(t *testing.T) {
		h := newHandler(t)
		jsCtx, _ := nc.JetStream()
		var firstSequence uint64
		for i := 0; i < 3; i++ {
			msg := map[string]interface{}{"count": i}
			data, _ := json.Marshal(msg)
			ack, err := jsCtx.Publish("events.history", data)
			if err != nil {
				t.Fatalf("failed to publish message: %v", err)
			}
			if i == 0 {
				firstSequence = ack.Sequence
			}
		}

		// last-id is the sequence of the last message the client has: the
		// replay starts right after the first message.
		rr, cancel, done := startSSE(t, h, "/events?topic=history&last-id="+strconv.FormatUint(firstSequence, 10), "")
		defer stopSSE(t, cancel, done)
		if !waitForSSEBody(rr, `"count":2`, 3*time.Second) {
			t.Fatalf("replay not delivered; body=%q", rr.Body())
		}
		body := rr.Body()
		if !strings.Contains(body, "event: connected") {
			t.Error("response should contain 'event: connected'")
		}
		if strings.Contains(body, `"count":0`) || !strings.Contains(body, `"count":1`) {
			t.Errorf("replay after last-id=%d should hold counts 1 and 2 only, got: %s", firstSequence, body)
		}
	})

	t.Run("invalid last-id parameter", func(t *testing.T) {
		h := newHandler(t)
		req := httptest.NewRequest(http.MethodGet, "/events?topic=test&last-id=invalid", nil)
		ctx, cancel := context.WithTimeout(req.Context(), 500*time.Millisecond)
		defer cancel()
		req = req.WithContext(ctx)
		rr := httptest.NewRecorder()

		h.ServeHTTP(rr, req, nil)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("expected status %d for invalid last-id, got %d", http.StatusBadRequest, rr.Code)
		}

		if !strings.Contains(rr.Body.String(), "Invalid last-id") {
			t.Errorf("response should mention invalid last-id, got: %s", rr.Body.String())
		}
	})

	t.Run("overflowing last-id parameter", func(t *testing.T) {
		h := newHandler(t)
		req := httptest.NewRequest(http.MethodGet, "/events?topic=test&last-id="+strconv.FormatUint(^uint64(0), 10), nil)
		ctx, cancel := context.WithTimeout(req.Context(), 500*time.Millisecond)
		defer cancel()
		req = req.WithContext(ctx)
		rr := httptest.NewRecorder()

		h.ServeHTTP(rr, req, nil)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("expected status %d for overflowing last-id, got %d", http.StatusBadRequest, rr.Code)
		}

		if !strings.Contains(rr.Body.String(), "Invalid last-id") {
			t.Errorf("response should mention invalid last-id, got: %s", rr.Body.String())
		}
	})

	t.Run("Last-Event-ID header replays messages", func(t *testing.T) {
		h := newHandler(t)
		jsCtx, _ := nc.JetStream()
		var firstSequence uint64
		for i := 0; i < 3; i++ {
			msg := map[string]interface{}{"count": i}
			data, _ := json.Marshal(msg)
			ack, err := jsCtx.Publish("events.header-replay", data)
			if err != nil {
				t.Fatalf("failed to publish message: %v", err)
			}
			if i == 0 {
				firstSequence = ack.Sequence
			}
		}

		rr, cancel, done := startSSE(t, h, "/events?topic=header-replay", strconv.FormatUint(firstSequence, 10))
		defer stopSSE(t, cancel, done)
		if !waitForSSEBody(rr, `"count":2`, 3*time.Second) {
			t.Fatalf("messages after Last-Event-ID not delivered; body=%q", rr.Body())
		}
		body := rr.Body()
		if strings.Contains(body, `"count":0`) {
			t.Errorf("response should not contain replayed message before Last-Event-ID, got: %s", body)
		}
		if !strings.Contains(body, `"count":1`) {
			t.Errorf("response should contain messages after Last-Event-ID, got: %s", body)
		}
	})

	t.Run("invalid Last-Event-ID header falls back to DeliverNew", func(t *testing.T) {
		h := newHandler(t)
		// A bad Last-Event-ID header must NOT 400 — the browser would loop
		// forever reconnecting with the same bad value. The handler should
		// log a warning and resume as a fresh subscriber: startSSE fails
		// unless the connected event arrives.
		_, cancel, done := startSSE(t, h, "/events?topic=test", "invalid")
		stopSSE(t, cancel, done)
	})

	t.Run("non get without next returns method not allowed", func(t *testing.T) {
		h := newHandler(t)
		req := httptest.NewRequest(http.MethodPost, "/events?topic=test", nil)
		rr := httptest.NewRecorder()

		if err := h.ServeHTTP(rr, req, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, rr.Code)
		}
		if allow := rr.Header().Get("Allow"); allow != "GET, OPTIONS" {
			t.Errorf("expected Allow header %q, got %q", "GET, OPTIONS", allow)
		}
	})

	t.Run("jetstream unavailable returns 503 without connected event", func(t *testing.T) {
		unavailable := &Handler{
			NatsURL:        ns.ClientURL(),
			StreamName:     "TEST_EVENTS",
			ReconnectWait:  2,
			MaxReconnects:  intPtr(-1),
			AllowedOrigins: []string{"*"},
			logger:         zap.NewNop(),
		}

		req := httptest.NewRequest(http.MethodGet, "/events?topic=test", nil)
		rr := httptest.NewRecorder()

		if err := unavailable.ServeHTTP(rr, req, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("expected status %d, got %d", http.StatusServiceUnavailable, rr.Code)
		}
		if strings.Contains(rr.Body.String(), "event: connected") {
			t.Error("response should not contain 'event: connected' when JetStream is unavailable")
		}
	})

	t.Run("subscription failure returns 503 without connected event", func(t *testing.T) {
		// Use a stream name that doesn't exist so JetStream.Subscribe fails
		// synchronously with "stream not found". A topic that simply doesn't
		// match an existing stream's subject filter is no longer a sync
		// failure on nats-server >= 2.14 — the server creates a consumer
		// that silently delivers nothing.
		broken := newHandler(t)
		broken.StreamName = "NONEXISTENT_STREAM"

		req := httptest.NewRequest(http.MethodGet, "/events?topic=test", nil)
		rr := httptest.NewRecorder()

		if err := broken.ServeHTTP(rr, req, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("expected status %d, got %d", http.StatusServiceUnavailable, rr.Code)
		}
		if strings.Contains(rr.Body.String(), "event: connected") {
			t.Error("response should not contain 'event: connected' when subscriptions fail")
		}
	})

	t.Run("partial multi-topic subscription failure returns 503", func(t *testing.T) {
		mixed := newHandler(t)
		mixed.TopicPrefix = ""

		req := httptest.NewRequest(http.MethodGet, "/events?topic=events.topic1&topic=missing.topic2", nil)
		rr := httptest.NewRecorder()

		if err := mixed.ServeHTTP(rr, req, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("expected status %d, got %d", http.StatusServiceUnavailable, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "missing.topic2") {
			t.Errorf("response should mention the failed topic, got: %s", rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "event: connected") {
			t.Error("response should not contain 'event: connected' when any requested topic fails")
		}
	})

	t.Run("no topics specified", func(t *testing.T) {
		h := newHandler(t)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		ctx, cancel := context.WithTimeout(req.Context(), 500*time.Millisecond)
		defer cancel()
		req = req.WithContext(ctx)
		rr := httptest.NewRecorder()

		h.ServeHTTP(rr, req, nil)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("expected status %d, got %d", http.StatusBadRequest, rr.Code)
		}
	})

	t.Run("path-based topic", func(t *testing.T) {
		h := newHandler(t)
		rr, cancel, done := startSSE(t, h, "/mytopic", "")
		stopSSE(t, cancel, done)
		if body := rr.Body(); !strings.Contains(body, `"topics":["mytopic"]`) {
			t.Errorf("response should contain topic 'mytopic', got: %s", body)
		}
	})

	t.Run("CORS preflight", func(t *testing.T) {
		h := newHandler(t)
		req := httptest.NewRequest(http.MethodOptions, "/events?topic=test", nil)
		req.Header.Set("Origin", "https://example.com")
		rr := httptest.NewRecorder()

		h.ServeHTTP(rr, req, nil)

		if rr.Code != http.StatusNoContent {
			t.Errorf("expected status %d, got %d", http.StatusNoContent, rr.Code)
		}

		if origin := rr.Header().Get("Access-Control-Allow-Origin"); origin != "https://example.com" {
			t.Errorf("Access-Control-Allow-Origin: expected 'https://example.com', got %q", origin)
		}
	})

	t.Run("multiple topics", func(t *testing.T) {
		h := newHandler(t)
		rr, cancel, done := startSSE(t, h, "/events?topic=topic1&topic=topic2", "")
		stopSSE(t, cancel, done)
		if body := rr.Body(); !strings.Contains(body, `"topics":["topic1","topic2"]`) {
			t.Errorf("response should contain both topics, got: %s", body)
		}
	})
}

func TestHandler_ServeHTTP_PreservesJSONNumberLexemes(t *testing.T) {
	h, _, nc := newProvisionedHandler(t)
	defer nc.Close()
	defer h.Cleanup()

	jsPub, _ := nc.JetStream()
	largeJSON := `{"id":900719925474099312345,"nested":{"n":12345678901234567890}}`
	if _, err := jsPub.Publish("events.json", []byte(largeJSON)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/events?topic=json&last-id=0", nil).WithContext(ctx)
	rr := newSafeRecorder()
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()

	if !waitForSSEBody(rr, `900719925474099312345`, 1500*time.Millisecond) {
		cancel()
		<-done
		t.Fatalf("large JSON number was not delivered; body=%s", rr.Body())
	}
	cancel()
	<-done

	body := rr.Body()
	if !strings.Contains(body, `900719925474099312345`) || !strings.Contains(body, `12345678901234567890`) {
		t.Fatalf("JSON number lexemes were not preserved; body=%s", body)
	}
	if strings.Contains(body, `9.007199254740993e+20`) || strings.Contains(body, `12345678901234567000`) {
		t.Fatalf("JSON numbers appear to have been coerced through float64; body=%s", body)
	}
}

func TestHandler_ServeHTTP_MultiTopicReplayEmitsIncreasingIDs(t *testing.T) {
	h, _, nc := newProvisionedHandler(t)
	defer nc.Close()
	defer h.Cleanup()

	jsPub, _ := nc.JetStream()
	for i := 0; i < 40; i++ {
		subject := "events.alpha"
		if i%2 == 1 {
			subject = "events.beta"
		}
		if _, err := jsPub.Publish(subject, []byte(`{"i":`+strconv.Itoa(i)+`}`)); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/events?topic=alpha&topic=beta&last-id=0", nil).WithContext(ctx)
	rr := newSafeRecorder()
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()

	if !waitForSSEBody(rr, `"i":39`, 2*time.Second) {
		cancel()
		<-done
		t.Fatalf("did not receive full replay; body=%s", rr.Body())
	}
	cancel()
	<-done

	ids := parseSSEIDs(t, rr.Body())
	// The connected event carries the replay cursor (last-id=0) first.
	if len(ids) != 41 || ids[0] != 0 {
		t.Fatalf("expected the connected id 0 then 40 message ids, got %d; ids=%v body=%s", len(ids), ids, rr.Body())
	}
	ids = ids[1:]
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("SSE ids must be strictly increasing for a single Last-Event-ID cursor; ids=%v body=%s", ids, rr.Body())
		}
	}
}

type failingFlushRecorder struct {
	header        http.Header
	statusCode    int
	allowedWrites int
	writes        int
	body          strings.Builder
}

func newFailingFlushRecorder(allowedWrites int) *failingFlushRecorder {
	return &failingFlushRecorder{
		header:        make(http.Header),
		allowedWrites: allowedWrites,
	}
}

func (f *failingFlushRecorder) Header() http.Header {
	return f.header
}

func (f *failingFlushRecorder) WriteHeader(statusCode int) {
	f.statusCode = statusCode
}

func (f *failingFlushRecorder) Write(p []byte) (int, error) {
	if f.statusCode == 0 {
		f.statusCode = http.StatusOK
	}
	if f.writes >= f.allowedWrites {
		return 0, errors.New("forced write failure")
	}
	f.writes++
	return f.body.Write(p)
}

func (f *failingFlushRecorder) Flush() {
	// No-op for testing, actual flushing happens in real HTTP response.
}

func TestHandler_ServeHTTP_InvalidTopic(t *testing.T) {
	ns := startJetStreamServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer nc.Close()

	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	js, _ := jetstream.New(nc)
	h := &Handler{
		StreamName:        "EVENTS",
		TopicPrefix:       "events.",
		AllowedOrigins:    []string{"*"},
		HeartbeatInterval: 30,
		conn:              nc,
		js:                js,
		logger:            zap.NewNop(),
	}

	req := httptest.NewRequest(http.MethodGet, "/events?topic=a%00b", nil)
	w := &flushRecorder{httptest.NewRecorder()}
	if err := h.ServeHTTP(w, req, nil); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Invalid topic") {
		t.Errorf("expected 'Invalid topic' in body, got %q", w.Body.String())
	}
}

func TestHandler_ServeHTTP_ConnectedWriteFailure(t *testing.T) {
	ns := startJetStreamServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("failed to connect to NATS: %v", err)
	}
	defer nc.Close()

	createTestStream(t, nc, "TEST_EVENTS", []string{"events.>"})

	h := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "TEST_EVENTS",
		TopicPrefix:       "events.",
		HeartbeatInterval: 30,
		ReconnectWait:     2,
		MaxReconnects:     intPtr(-1),
		AllowedOrigins:    []string{"*"},
		MaxConnections:    1,
		logger:            zap.NewNop(),
	}

	js := connectHandler(t, h)

	connectedBefore := metricValue(t, metricsWriteDisconnects.WithLabelValues("connected"))
	req := httptest.NewRequest(http.MethodGet, "/events?topic=test", nil)
	w := newFailingFlushRecorder(0)

	done := make(chan error, 1)
	go func() {
		done <- h.ServeHTTP(w, req, nil)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not return after initial write failure")
	}
	if got := atomic.LoadInt64(&h.connCount); got != 0 {
		t.Fatalf("connCount = %d, want 0 after connected write failure", got)
	}
	if got := metricValue(t, metricsWriteDisconnects.WithLabelValues("connected")); got != connectedBefore+1 {
		t.Fatalf("nuts_write_disconnects_total{site=connected} = %v, want %v", got, connectedBefore+1)
	}
	if !waitForConsumerCount(t, js, "TEST_EVENTS", 0, 2*time.Second) {
		t.Fatalf("ephemeral consumer leaked after connected write failure")
	}
}

func TestHandler_ServeHTTP_SubscribeFailureReleasesConnectionSlot(t *testing.T) {
	h, _, nc := newProvisionedHandler(t)
	defer nc.Close()
	defer h.Cleanup()

	h.MaxConnections = 1
	h.StreamName = "NOPE_DOES_NOT_EXIST"

	req := httptest.NewRequest(http.MethodGet, "/events?topic=err", nil)
	w := &flushRecorder{httptest.NewRecorder()}
	if err := h.ServeHTTP(w, req, nil); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if got := atomic.LoadInt64(&h.connCount); got != 0 {
		t.Fatalf("connCount = %d, want 0 after subscription failure", got)
	}
}

func TestHandler_ServeHTTP_SubjectPrecheckReleasesConnectionSlot(t *testing.T) {
	ns := startJetStreamServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("failed to connect to NATS: %v", err)
	}
	defer nc.Close()

	createTestStream(t, nc, "TEST_EVENTS", []string{"events.allowed"})

	h := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "TEST_EVENTS",
		TopicPrefix:       "events.",
		HeartbeatInterval: 30,
		MaxConnections:    1,
		AllowedOrigins:    []string{"*"},
		logger:            zap.NewNop(),
	}
	js := connectHandler(t, h)

	req := httptest.NewRequest(http.MethodGet, "/events?topic=allowed&topic=blocked", nil)
	w := &flushRecorder{httptest.NewRecorder()}
	if err := h.ServeHTTP(w, req, nil); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if got := atomic.LoadInt64(&h.connCount); got != 0 {
		t.Fatalf("connCount = %d, want 0 after subject pre-check failure", got)
	}
	if !waitForConsumerCount(t, js, "TEST_EVENTS", 0, 2*time.Second) {
		t.Fatalf("unexpected consumer after subject pre-check failure")
	}
}

func TestHandler_ServeHTTP_MessageWriteFailure(t *testing.T) {
	ns := startJetStreamServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("failed to connect to NATS: %v", err)
	}
	defer nc.Close()

	createTestStream(t, nc, "TEST_EVENTS", []string{"events.>"})

	h := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "TEST_EVENTS",
		TopicPrefix:       "events.",
		HeartbeatInterval: 30,
		ReconnectWait:     2,
		MaxReconnects:     intPtr(-1),
		AllowedOrigins:    []string{"*"},
		MaxConnections:    1,
		logger:            zap.NewNop(),
	}

	js := connectHandler(t, h)

	req := httptest.NewRequest(http.MethodGet, "/events?topic=test", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	messageBefore := metricValue(t, metricsWriteDisconnects.WithLabelValues("message"))
	w := newFailingFlushRecorder(1)
	done := make(chan error, 1)
	go func() {
		done <- h.ServeHTTP(w, req, nil)
	}()

	// Publish only once the consumer exists; published earlier, the message
	// would precede the stream's start position.
	if waitForFirstConsumer(t, mustJetStream(t, nc), "TEST_EVENTS", 3*time.Second) == nil {
		t.Fatal("stream never created its consumer")
	}
	jsCtx, _ := nc.JetStream()
	if _, err := jsCtx.Publish("events.test", []byte(`{"hello":"world"}`)); err != nil {
		t.Fatalf("failed to publish message: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not return after message write failure")
	}
	if got := atomic.LoadInt64(&h.connCount); got != 0 {
		t.Fatalf("connCount = %d, want 0 after message write failure", got)
	}
	if got := metricValue(t, metricsWriteDisconnects.WithLabelValues("message")); got != messageBefore+1 {
		t.Fatalf("nuts_write_disconnects_total{site=message} = %v, want %v", got, messageBefore+1)
	}
	if !waitForConsumerCount(t, js, "TEST_EVENTS", 0, 2*time.Second) {
		t.Fatalf("ephemeral consumer leaked after message write failure")
	}
}

// TestHandler_ServeHTTP_HeartbeatWriteFailure covers the heartbeat-write
// branch in serve.go (disconnect_reason=heartbeat_write_error) that was
// previously the only write site without an end-to-end test. The
// connected event is allowed (1 permitted write); no JetStream messages
// are published; the next write site to fire is the heartbeat tick.
func TestHandler_ServeHTTP_HeartbeatWriteFailure(t *testing.T) {
	ns := startJetStreamServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("failed to connect to NATS: %v", err)
	}
	defer nc.Close()

	createTestStream(t, nc, "TEST_EVENTS", []string{"events.>"})

	h := &Handler{
		NatsURL:     ns.ClientURL(),
		StreamName:  "TEST_EVENTS",
		TopicPrefix: "events.",
		// HeartbeatInterval is in SECONDS (see provision.go default 30).
		// 1 = the minimum supported value; the heartbeat ticker fires
		// after ~1 s, well before the 4 s ctx deadline below. Don't
		// reduce this unit without updating the test timeout windows.
		HeartbeatInterval: 1,
		ReconnectWait:     2,
		MaxReconnects:     intPtr(-1),
		AllowedOrigins:    []string{"*"},
		logger:            zap.NewNop(),
	}

	connectHandler(t, h)

	before := metricValue(t, metricsWriteDisconnects.WithLabelValues("heartbeat"))

	req := httptest.NewRequest(http.MethodGet, "/events?topic=hb", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 4*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	w := newFailingFlushRecorder(1) // 1 = allow `event: connected`, fail heartbeat
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(w, req, nil) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServeHTTP did not return after heartbeat write failure")
	}

	if got := metricValue(t, metricsWriteDisconnects.WithLabelValues("heartbeat")); got != before+1 {
		t.Fatalf("nuts_write_disconnects_total{site=heartbeat} = %v, want %v", got, before+1)
	}
	if got := atomic.LoadInt64(&h.connCount); got != 0 {
		t.Fatalf("connCount = %d, want 0 after heartbeat write failure", got)
	}
}

// TestHandler_ServeHTTP_SlowReaderGetsBackpressureNotDisconnect: a reader
// slower than JetStream receives the whole backlog, in order, on one
// connection. The pull consumer waits for the writer instead of a queue
// overflowing into a slow-client disconnect (#100).
func TestHandler_ServeHTTP_SlowReaderGetsBackpressureNotDisconnect(t *testing.T) {
	h, _, nc := newProvisionedHandler(t)
	defer nc.Close()
	defer h.Cleanup()
	h.ClientBufferSize = 4

	jsCtx, _ := nc.JetStream()
	const total = 100
	for i := 1; i <= total; i++ {
		if _, err := jsCtx.Publish("events.burst", []byte(`{"count":`+strconv.Itoa(i)+`}`)); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	slowBefore := metricValue(t, metricsSlowClientDisconnects)

	req := httptest.NewRequest(http.MethodGet, "/events?topic=burst", nil)
	req.Header.Set("Last-Event-ID", "0")
	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	rr := &slowSafeRecorder{safeFlushRecorder: newSafeRecorder(), delay: 5 * time.Millisecond}
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()

	if !waitForSSEBody(rr.safeFlushRecorder, `{"count":100}`, 8*time.Second) {
		t.Fatalf("slow reader did not receive the whole backlog; body tail=%q", tail(rr.Body(), 200))
	}
	select {
	case err := <-done:
		t.Fatalf("slow reader was disconnected (err=%v) instead of throttled", err)
	default:
	}
	ids := parseSSEIDs(t, rr.Body())
	if len(ids) != total+1 || ids[0] != 0 {
		t.Fatalf("got %d ids starting %v, want the connected id 0 then %d messages", len(ids), ids[:min(3, len(ids))], total)
	}
	for i, id := range ids[1:] {
		if id != uint64(i+1) {
			t.Fatalf("id[%d] = %d, want %d (ids must be contiguous)", i+1, id, i+1)
		}
	}
	if got := metricValue(t, metricsSlowClientDisconnects); got != slowBefore {
		t.Fatalf("slow_client_disconnects_total moved from %v to %v for a reader that kept up", slowBefore, got)
	}
	cancel()
	<-done
}

// tail returns the last n bytes of s.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// ── Health check and hub discovery ──────────────────────────────────────

func TestHandler_HealthCheck(t *testing.T) {
	ns := startJetStreamServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("failed to connect to NATS: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "HEALTH_EVENTS", []string{"events.>"})

	// A connected handler per subtest: the subtests share only the server.
	newHandler := func(t *testing.T) *Handler {
		h := &Handler{
			NatsURL:    ns.ClientURL(),
			StreamName: "HEALTH_EVENTS",
			logger:     zap.NewNop(),
		}
		connectHandler(t, h)
		return h
	}

	t.Run("healthy returns 200 with JSON status", func(t *testing.T) {
		h := newHandler(t)
		req := httptest.NewRequest(http.MethodGet, "/events/healthz", nil)
		rr := httptest.NewRecorder()

		if err := h.ServeHTTP(rr, req, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if rr.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rr.Code)
		}
		if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected application/json, got %q", ct)
		}

		var resp map[string]string
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to parse response: %v", err)
		}
		if resp["status"] != "ok" {
			t.Errorf("expected status ok, got %q", resp["status"])
		}
		if resp["nats"] != "connected" {
			t.Errorf("expected nats connected, got %q", resp["nats"])
		}
		if resp["stream"] != "available" {
			t.Errorf("expected stream available, got %q", resp["stream"])
		}
	})

	t.Run("degraded when NATS disconnected returns 503", func(t *testing.T) {
		// Create handler with nil conn (simulating disconnected state)
		degraded := &Handler{
			StreamName: "HEALTH_EVENTS",
			logger:     zap.NewNop(),
		}

		req := httptest.NewRequest(http.MethodGet, "/events/healthz", nil)
		rr := httptest.NewRecorder()

		if err := degraded.ServeHTTP(rr, req, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("expected 503, got %d", rr.Code)
		}

		var resp map[string]string
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to parse response: %v", err)
		}
		if resp["status"] != "degraded" {
			t.Errorf("expected status degraded, got %q", resp["status"])
		}
	})

	t.Run("healthz path suffix works with different base paths", func(t *testing.T) {
		h := newHandler(t)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/nuts/healthz", nil)
		rr := httptest.NewRecorder()

		if err := h.ServeHTTP(rr, req, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if rr.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rr.Code)
		}
	})
}

func TestHandler_HubDiscovery(t *testing.T) {
	ns := startJetStreamServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("failed to connect to NATS: %v", err)
	}
	defer nc.Close()

	createTestStream(t, nc, "HUB_EVENTS", []string{"events.>"})

	t.Run("Link header present when hub_url is set", func(t *testing.T) {
		h := &Handler{
			NatsURL:           ns.ClientURL(),
			StreamName:        "HUB_EVENTS",
			TopicPrefix:       "events.",
			HeartbeatInterval: 30,
			ReconnectWait:     2,
			MaxReconnects:     intPtr(-1),
			AllowedOrigins:    []string{"*"},
			HubURL:            "https://example.com/events",
			logger:            zap.NewNop(),
		}
		connectHandler(t, h)

		rr, cancel, done := startSSE(t, h, "/events?topic=test", "")
		stopSSE(t, cancel, done)

		link := rr.Header().Get("Link")
		expected := `<https://example.com/events>; rel="nuts"`
		if link != expected {
			t.Errorf("Link header: expected %q, got %q", expected, link)
		}
	})

	t.Run("no Link header when hub_url is empty", func(t *testing.T) {
		h := &Handler{
			NatsURL:           ns.ClientURL(),
			StreamName:        "HUB_EVENTS",
			TopicPrefix:       "events.",
			HeartbeatInterval: 30,
			ReconnectWait:     2,
			MaxReconnects:     intPtr(-1),
			AllowedOrigins:    []string{"*"},
			logger:            zap.NewNop(),
		}
		connectHandler(t, h)

		rr, cancel, done := startSSE(t, h, "/events?topic=test", "")
		stopSSE(t, cancel, done)

		if link := rr.Header().Get("Link"); link != "" {
			t.Errorf("expected no Link header, got %q", link)
		}
	})
}

// ── Heartbeat: the SSE stream emits the keep-alive comment ────────────────

func TestHandler_Heartbeat_EmitsFrame(t *testing.T) {
	t.Parallel() // asserts no process-wide metric
	h, _, nc := newProvisionedHandler(t)
	defer nc.Close()
	defer h.Cleanup()

	h.HeartbeatInterval = 1 // seconds — smallest practical value

	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/events?topic=hb", nil).WithContext(ctx)
	rr := newSafeRecorder()
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()

	if !waitForSSEBody(rr, ": heartbeat ", 2*time.Second) {
		cancel()
		<-done
		t.Fatalf("no ': heartbeat' comment in body after two seconds; got:\n%s", rr.Body())
	}
	cancel()
	<-done
}

func updateStreamSubjects(nc *nats.Conn, stream string, subjects []string) error {
	js, err := nc.JetStream()
	if err != nil {
		return err
	}
	info, err := js.StreamInfo(stream)
	if err != nil {
		return err
	}
	cfg := info.Config
	cfg.Subjects = subjects
	_, err = js.UpdateStream(&cfg)
	return err
}

// ── topic_prefix: subject translation and inbound filtering ───────────────

func TestHandler_TopicPrefix_PrependsSubjectAndStripsFromPayload(t *testing.T) {
	h, _, nc := newProvisionedHandler(t)
	defer nc.Close()
	defer h.Cleanup()

	jsPub, _ := nc.JetStream()
	// Message on the prefixed subject: should be delivered.
	if _, err := jsPub.Publish("events.prefixed", []byte(`{"kind":"matches"}`)); err != nil {
		t.Fatalf("publish prefixed: %v", err)
	}
	// Message on a non-prefixed subject filtered out by the stream. Add it
	// to the stream's subject list first so the server accepts it; our
	// subscription should not receive it because the handler subscribes to
	// "events.prefixed", not "prefixed".
	if err := updateStreamSubjects(nc, "EVENTS", []string{"events.>", "other.>"}); err != nil {
		t.Fatalf("update subjects: %v", err)
	}
	if _, err := jsPub.Publish("other.prefixed", []byte(`{"kind":"unrelated"}`)); err != nil {
		t.Fatalf("publish unrelated: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/events?topic=prefixed&last-id=0", nil).WithContext(ctx)
	rr := newSafeRecorder()
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()

	if !waitForSSEBody(rr, `"kind":"matches"`, 2*time.Second) {
		cancel()
		<-done
		t.Fatalf("prefixed message was not delivered; body=%s", rr.Body())
	}
	cancel()
	<-done

	body := rr.Body()
	// Payload should carry the topic WITHOUT the prefix.
	if !strings.Contains(body, `"topic":"prefixed"`) {
		t.Errorf("expected payload to contain unprefixed topic, got body=%s", body)
	}
	if strings.Contains(body, `"topic":"events.prefixed"`) {
		t.Errorf("payload leaked the topic_prefix into the topic field: %s", body)
	}
	// The unrelated subject was published but the subscription is scoped to
	// events.prefixed, so it must not appear.
	if strings.Contains(body, `"kind":"unrelated"`) {
		t.Errorf("message from non-prefixed subject leaked into the stream: %s", body)
	}
}

// ── NATS reconnect: subsequent SSE requests still succeed ─────────────────

// TestHandler_NATSReconnect_AllowsSubsequentSSE verifies that after the NATS
// server restarts (with the same port + StoreDir so the stream survives),
// the handler's long-lived connection recovers and a fresh SSE request
// succeeds. This goes beyond TestHandler_connectNATS_ReconnectLifecycle,
// which only asserts the NATS client reconnects — here we exercise the
// full SSE path after recovery.
func TestHandler_NATSReconnect_AllowsSubsequentSSE(t *testing.T) {
	// A restart keeps the port and store directory: the handler reconnects to
	// the same URL and the file-backed stream survives.
	ns, restart := startRestartableJetStreamServer(t)
	// File-backed stream so it survives restart.
	admin, err := nats.Connect(ns.ClientURL())
	if err != nil {
		ns.Shutdown()
		t.Fatalf("admin connect: %v", err)
	}
	adminJS, _ := admin.JetStream()
	if _, err := adminJS.AddStream(&nats.StreamConfig{
		Name:     "EVENTS",
		Subjects: []string{"events.>"},
		Storage:  nats.FileStorage,
	}); err != nil {
		admin.Close()
		ns.Shutdown()
		t.Fatalf("add stream: %v", err)
	}
	admin.Close()

	h := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "EVENTS",
		TopicPrefix:       "events.",
		HeartbeatInterval: 30,
		ReconnectWait:     1,
		MaxReconnects:     intPtr(-1),
		MaxEventSize:      -1,
		AllowedOrigins:    []string{"*"},
		logger:            zap.NewNop(),
	}
	connectHandler(t, h)

	beforeDisconnect := metricValue(t, metricsNATSConnectionEvents.WithLabelValues("disconnect"))
	beforeReconnect := metricValue(t, metricsNATSConnectionEvents.WithLabelValues("reconnect"))

	// Kill the server, wait for the client to observe the disconnect, then
	// bring the server back up so the client auto-reconnects.
	ns.Shutdown()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && h.conn.IsConnected() {
		time.Sleep(50 * time.Millisecond)
	}
	if h.conn.IsConnected() {
		t.Fatal("handler's NATS connection did not observe the shutdown")
	}

	ns2 := restart()

	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !h.conn.IsConnected() {
		time.Sleep(100 * time.Millisecond)
	}
	if !h.conn.IsConnected() {
		t.Fatal("handler's NATS connection did not reconnect")
	}

	// The disconnect+reconnect cycle must have bumped the flap-detection
	// counter so an SRE alert can fire on broker instability. nats.go
	// dispatches DisconnectErrHandler/ReconnectHandler on its async callback
	// goroutine, which is not happens-before with the IsConnected() state
	// flip observed above — poll until the counters reflect the lifecycle
	// events instead of reading once and racing the dispatcher.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if metricValue(t, metricsNATSConnectionEvents.WithLabelValues("disconnect")) > beforeDisconnect &&
			metricValue(t, metricsNATSConnectionEvents.WithLabelValues("reconnect")) > beforeReconnect {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if got := metricValue(t, metricsNATSConnectionEvents.WithLabelValues("disconnect")); got <= beforeDisconnect {
		t.Errorf("nuts_nats_connection_events_total{event=disconnect} did not increment: %v -> %v", beforeDisconnect, got)
	}
	if got := metricValue(t, metricsNATSConnectionEvents.WithLabelValues("reconnect")); got <= beforeReconnect {
		t.Errorf("nuts_nats_connection_events_total{event=reconnect} did not increment: %v -> %v", beforeReconnect, got)
	}

	// Publish a fresh message via a new admin connection and confirm the
	// handler's JetStream context can subscribe and deliver it.
	pub, err := nats.Connect(ns2.ClientURL())
	if err != nil {
		t.Fatalf("post-reconnect admin connect: %v", err)
	}
	defer pub.Close()
	pubJS, _ := pub.JetStream()
	if _, err := pubJS.Publish("events.recovered", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("publish after reconnect: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/events?topic=recovered&last-id=0", nil).WithContext(ctx)
	rr := newSafeRecorder()
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()

	if !waitForSSEBody(rr, `"ok":true`, 3*time.Second) {
		cancel()
		<-done
		t.Fatalf("post-reconnect SSE did not receive the published message; body=%s", rr.Body())
	}
	cancel()
	<-done
}

func TestHandler_NATSReconnect_ConnectedSSEReceivesPostReconnectMessage(t *testing.T) {
	t.Parallel() // asserts no process-wide metric
	// A restart keeps the port and store directory: the handler reconnects to
	// the same URL and the file-backed stream survives.
	ns, restart := startRestartableJetStreamServer(t)
	admin, err := nats.Connect(ns.ClientURL())
	if err != nil {
		ns.Shutdown()
		t.Fatalf("admin connect: %v", err)
	}
	adminJS, _ := admin.JetStream()
	if _, err := adminJS.AddStream(&nats.StreamConfig{
		Name:     "EVENTS",
		Subjects: []string{"events.>"},
		Storage:  nats.FileStorage,
	}); err != nil {
		admin.Close()
		ns.Shutdown()
		t.Fatalf("add stream: %v", err)
	}
	admin.Close()

	h := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "EVENTS",
		TopicPrefix:       "events.",
		HeartbeatInterval: 30,
		ReconnectWait:     1,
		MaxReconnects:     intPtr(-1),
		MaxEventSize:      -1,
		AllowedOrigins:    []string{"*"},
		logger:            zap.NewNop(),
	}
	connectHandler(t, h)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/events?topic=live-reconnect", nil).WithContext(ctx)
	rr := newSafeRecorder()
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()

	if !waitForSSEBody(rr, "event: connected", 2*time.Second) {
		cancel()
		<-done
		ns.Shutdown()
		t.Fatalf("SSE stream did not connect before restart; body=%s", rr.Body())
	}

	ns.Shutdown()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && h.conn.IsConnected() {
		time.Sleep(50 * time.Millisecond)
	}
	if h.conn.IsConnected() {
		cancel()
		<-done
		t.Fatal("handler's NATS connection did not observe the shutdown")
	}
	select {
	case err := <-done:
		t.Fatalf("connected SSE stream ended during NATS shutdown: %v body=%s", err, rr.Body())
	default:
	}

	ns2 := restart()
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !h.conn.IsConnected() {
		time.Sleep(100 * time.Millisecond)
	}
	if !h.conn.IsConnected() {
		cancel()
		<-done
		t.Fatal("handler's NATS connection did not reconnect")
	}

	pub, err := nats.Connect(ns2.ClientURL())
	if err != nil {
		cancel()
		<-done
		t.Fatalf("post-reconnect admin connect: %v", err)
	}
	defer pub.Close()
	pubJS, _ := pub.JetStream()
	if _, err := pubJS.Publish("events.live-reconnect", []byte(`{"after":true}`)); err != nil {
		cancel()
		<-done
		t.Fatalf("publish after reconnect: %v", err)
	}

	if !waitForSSEBody(rr, `"after":true`, 5*time.Second) {
		cancel()
		<-done
		t.Fatalf("connected SSE stream did not receive post-reconnect message; body=%s", rr.Body())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not return after context cancel")
	}
}

// ── JetStream persistence: messages survive handler cleanup ───────────────

func TestHandler_JetStreamPersistence_MessagesSurviveHandlerLifetime(t *testing.T) {
	ns := startJetStreamServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	// Publish messages with NO SSE client connected. JetStream must retain
	// them regardless of whether a subscriber exists.
	jsPub, _ := nc.JetStream()
	for i := 0; i < 3; i++ {
		if _, err := jsPub.Publish("events.persist", []byte(`{"seq":`+strconv.Itoa(i)+`}`)); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	// First handler lifetime: deliver the messages to a client, then tear
	// down. Delivering them must not consume them from the stream.
	h1 := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "EVENTS",
		TopicPrefix:       "events.",
		HeartbeatInterval: 30,
		MaxEventSize:      -1,
		AllowedOrigins:    []string{"*"},
		logger:            zap.NewNop(),
	}
	connectHandler(t, h1)
	first, cancelFirst, doneFirst := startSSE(t, h1, "/events?topic=persist&last-id=0", "")
	if !waitForSSEBody(first, `"seq":2`, 3*time.Second) {
		t.Fatalf("first handler did not deliver the messages; body=%s", first.Body())
	}
	stopSSE(t, cancelFirst, doneFirst)
	if err := h1.Cleanup(); err != nil {
		t.Fatalf("h1 Cleanup: %v", err)
	}

	// Second handler lifetime: fresh Handler reads the same stream and must
	// observe all three previously-published messages by replaying from
	// last-id=0.
	h2 := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "EVENTS",
		TopicPrefix:       "events.",
		HeartbeatInterval: 30,
		MaxEventSize:      -1,
		AllowedOrigins:    []string{"*"},
		logger:            zap.NewNop(),
	}
	connectHandler(t, h2)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/events?topic=persist&last-id=0", nil).WithContext(ctx)
	rr := newSafeRecorder()
	done := make(chan error, 1)
	go func() { done <- h2.ServeHTTP(rr, req, nil) }()

	if !waitForSSEBody(rr, `"seq":2`, 3*time.Second) {
		cancel()
		<-done
		t.Fatalf("messages did not survive handler cleanup; body=%s", rr.Body())
	}
	cancel()
	<-done

	body := rr.Body()
	for i := 0; i < 3; i++ {
		needle := `"seq":` + strconv.Itoa(i)
		if !strings.Contains(body, needle) {
			t.Errorf("expected body to contain %s, got:\n%s", needle, body)
		}
	}
}

func TestHandler_MultiTopicStreamUsesServerSideFilterSubjects(t *testing.T) {
	h, _, nc := newProvisionedHandler(t)
	defer nc.Close()
	defer h.Cleanup()
	js, _ := nc.JetStream()
	admin, _ := jetstream.New(nc)

	rr, cancel, done := startSSE(t, h, "/events?topic=alpha&topic=beta", "")
	defer stopSSE(t, cancel, done)

	info := waitForFirstConsumer(t, admin, "EVENTS", 3*time.Second)
	if info == nil {
		t.Fatal("no consumer created for the multi-topic request")
	}
	if !reflect.DeepEqual(info.Config.FilterSubjects, []string{"events.alpha", "events.beta"}) {
		t.Fatalf("FilterSubjects = %v, want [events.alpha events.beta]", info.Config.FilterSubjects)
	}
	if info.Config.AckPolicy != jetstream.AckNonePolicy {
		t.Fatalf("AckPolicy = %v, want none", info.Config.AckPolicy)
	}
	if info.Config.DeliverPolicy != jetstream.DeliverByStartSequencePolicy || info.Config.OptStartSeq != 1 {
		t.Fatalf("start = %v/%d, want an explicit start at sequence 1 on an empty stream", info.Config.DeliverPolicy, info.Config.OptStartSeq)
	}
	if !strings.HasPrefix(info.Name, consumerNamePrefix) {
		t.Fatalf("consumer name %q lacks the %q prefix", info.Name, consumerNamePrefix)
	}

	for _, subj := range []string{"events.alpha", "events.gamma", "events.beta"} {
		if _, err := js.Publish(subj, []byte(`{"subject":"`+subj+`"}`)); err != nil {
			t.Fatalf("publish %s: %v", subj, err)
		}
	}
	if !waitForSSEBody(rr, `"subject":"events.beta"`, 3*time.Second) {
		t.Fatalf("multi-topic stream missed events.beta; body=%q", rr.Body())
	}
	if strings.Contains(rr.Body(), "events.gamma") {
		t.Fatalf("multi-topic stream delivered an unrequested subject; body=%q", rr.Body())
	}
	if got := parseSSEIDs(t, rr.Body()); !reflect.DeepEqual(got, []uint64{0, 1, 3}) {
		t.Fatalf("ids = %v, want connected id 0 then stream sequences 1 and 3", got)
	}
}

// TestHandler_NoCursorRequestStartsAfterLastSeq checks the explicit start
// position of a request without a cursor: retained history is not replayed,
// the connected event names the last retained sequence, and the next
// published message arrives with the following id.
func TestHandler_NoCursorRequestStartsAfterLastSeq(t *testing.T) {
	h, _, nc := newProvisionedHandler(t)
	defer nc.Close()
	defer h.Cleanup()
	js, _ := nc.JetStream()
	for i := 1; i <= 3; i++ {
		if _, err := js.Publish("events.alpha", []byte(`{"old":`+strconv.Itoa(i)+`}`)); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	rr, cancel, done := startSSE(t, h, "/events?topic=alpha", "")
	defer stopSSE(t, cancel, done)
	if !strings.HasPrefix(rr.Body(), "event: connected\ndata: {\"topics\":[\"alpha\"]}\nid: 3\n\n") {
		t.Fatalf("connected event = %q, want it to carry id 3", rr.Body())
	}
	if _, err := js.Publish("events.alpha", []byte(`{"new":4}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !waitForSSEBody(rr, `{"new":4}`, 3*time.Second) {
		t.Fatalf("new message not delivered; body=%q", rr.Body())
	}
	if strings.Contains(rr.Body(), `"old"`) {
		t.Fatalf("retained history was replayed to a request without a cursor; body=%q", rr.Body())
	}
	if got := parseSSEIDs(t, rr.Body()); !reflect.DeepEqual(got, []uint64{3, 4}) {
		t.Fatalf("ids = %v, want [3 4]", got)
	}
}

// TestHandler_ReconnectBeforeFirstMessageLosesNothing is the regression test
// for #101: the connected event used to carry no id, so a client whose stream
// ended before its first message reconnected without Last-Event-ID and lost
// everything published in the gap.
func TestHandler_ReconnectBeforeFirstMessageLosesNothing(t *testing.T) {
	h, _, nc := newProvisionedHandler(t)
	defer nc.Close()
	defer h.Cleanup()
	js, _ := nc.JetStream()

	first, cancel, done := startSSE(t, h, "/events?topic=alpha", "")
	ids := parseSSEIDs(t, first.Body())
	stopSSE(t, cancel, done)
	if len(ids) != 1 {
		t.Fatalf("connected event ids = %v, want exactly one", ids)
	}

	for i := 1; i <= 3; i++ {
		if _, err := js.Publish("events.alpha", []byte(`{"gap":`+strconv.Itoa(i)+`}`)); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	second, cancel2, done2 := startSSE(t, h, "/events?topic=alpha", strconv.FormatUint(ids[0], 10))
	defer stopSSE(t, cancel2, done2)
	if !waitForSSEBody(second, `{"gap":3}`, 3*time.Second) {
		t.Fatalf("messages published during the reconnect gap were not delivered; body=%q", second.Body())
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(second.Body(), `{"gap":`+strconv.Itoa(i)+`}`) {
			t.Fatalf("gap message %d missing; body=%q", i, second.Body())
		}
	}
}

// TestHandler_ServeHTTP_TopicOutsideStreamIsRejected covers the planning-time
// rejection for a single topic the stream does not carry: a 503 naming the
// topic, one nuts_subscription_errors_total tick, the
// disconnect_reason=subscription_failed log field, and no consumer left
// behind.
func TestHandler_ServeHTTP_TopicOutsideStreamIsRejected(t *testing.T) {
	h, _, nc := newProvisionedHandler(t)
	defer nc.Close()
	defer h.Cleanup()
	h.TopicPrefix = ""
	core, obs := observer.New(zap.WarnLevel)
	h.logger = zap.New(core)
	before := metricValue(t, metricsSubscriptionErrors)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/events?topic=orders", nil)
	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "Failed to subscribe to requested topics: orders") {
		t.Fatalf("response = %d %q, want 503 naming the topic", rr.Code, rr.Body.String())
	}
	if got := metricValue(t, metricsSubscriptionErrors); got != before+1 {
		t.Fatalf("subscription_errors_total = %v, want %v", got, before+1)
	}
	if !hasLogField(obs, "disconnect_reason", "subscription_failed") {
		t.Fatalf("missing disconnect_reason=subscription_failed: %v", obs.All())
	}
	if got := consumerCount(mustJetStream(t, nc), "EVENTS"); got != 0 {
		t.Fatalf("consumers after rejection = %d, want 0", got)
	}
}

// TestHandler_EventTypeFromTheTopicOrAHeader: with event_type, each
// message's SSE event name comes from its topic or from a header of the
// published message, and falls back to "message" without one (#142).
func TestHandler_EventTypeFromTheTopicOrAHeader(t *testing.T) {
	for _, c := range []struct {
		name      string
		configure func(*Handler)
		want      []string
	}{
		{"topic", func(h *Handler) { h.EventType = eventTypeTopic }, []string{"orders", "orders"}},
		{"header", func(h *Handler) { h.EventType, h.EventTypeHeader = eventTypeHeader, "Event-Type" }, []string{"order_created", "message"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ns := startJetStreamServer(t)
			nc, err := nats.Connect(ns.ClientURL())
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(nc.Close)
			createTestStream(t, nc, "EVENTS", []string{"events.>"})
			_, srv := newContractServer(t, ns.ClientURL(), c.configure)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events?topic=orders", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			lines := bufio.NewScanner(resp.Body)
			nextEvent := func() string {
				t.Helper()
				for lines.Scan() {
					if name, ok := strings.CutPrefix(lines.Text(), "event: "); ok {
						return name
					}
				}
				t.Fatalf("stream ended: %v", lines.Err())
				return ""
			}
			if first := nextEvent(); first != "connected" {
				t.Fatalf("first event = %q, want connected", first)
			}
			js, _ := nc.JetStream()
			if _, err := js.PublishMsg(&nats.Msg{Subject: "events.orders", Header: nats.Header{"Event-Type": []string{"order_created"}}, Data: []byte(`{"n":1}`)}); err != nil {
				t.Fatalf("publish: %v", err)
			}
			if _, err := js.Publish("events.orders", []byte(`{"n":2}`)); err != nil {
				t.Fatalf("publish: %v", err)
			}
			for i, want := range c.want {
				if got := nextEvent(); got != want {
					t.Fatalf("message %d: event %q, want %q", i+1, got, want)
				}
			}
		})
	}
}

// TestHandler_RawPayloadFormat: with payload_format raw, an event's data is
// the published payload, a data line per line of it; a payload SSE cannot
// carry is dropped, and the stream goes on (#143).
func TestHandler_RawPayloadFormat(t *testing.T) {
	ns := startJetStreamServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	createTestStream(t, nc, "EVENTS", []string{"events.>"})
	_, srv := newContractServer(t, ns.ClientURL(), func(h *Handler) { h.PayloadFormat = payloadFormatRaw })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events?topic=text", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	lines := bufio.NewScanner(resp.Body)
	nextData := func() string {
		t.Helper()
		var data []string
		event := ""
		for lines.Scan() {
			line := lines.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			case line == "" && event == "message":
				return strings.Join(data, "\n")
			case line == "":
				event, data = "", nil
			}
		}
		t.Fatalf("stream ended: %v", lines.Err())
		return ""
	}
	js, _ := nc.JetStream()
	for _, payload := range []string{"first line\nsecond line", "carriage\rreturn", `{"after":"the drop"}`} {
		if _, err := js.Publish("events.text", []byte(payload)); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	if got := nextData(); got != "first line\nsecond line" {
		t.Fatalf("first event data = %q", got)
	}
	if got := nextData(); got != `{"after":"the drop"}` {
		t.Fatalf("the event after the unsendable payload = %q", got)
	}
}

// TestHandler_HealthDetails covers #144: with health_details the readiness
// probes add the NATS server and the stream as the probe read it. Without
// it they stay as they were, a stream that cannot be read has no details,
// and a disconnected handler names no server.
func TestHandler_HealthDetails(t *testing.T) {
	ns := startJetStreamServer(t, func(o *natsserver.Options) {
		o.JetStreamDomain = "hub"
		o.Cluster.Name = "east" // named, without a cluster listener
	})
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	if _, err := mustJetStream(t, nc).CreateStream(context.Background(), jetstream.StreamConfig{
		Name: "DETAILS", Subjects: []string{"events.>"}, Storage: jetstream.MemoryStorage,
		MaxMsgs: 1000, MaxAge: time.Hour, MaxConsumers: 50,
	}); err != nil {
		t.Fatalf("CreateStream: %v", err)
	}
	js, _ := nc.JetStream()
	publishRange(t, js, "events.a", 1, 3)
	if _, err := mustJetStream(t, nc).CreateOrUpdateConsumer(context.Background(), "DETAILS", jetstream.ConsumerConfig{Durable: "reader"}); err != nil {
		t.Fatalf("CreateOrUpdateConsumer: %v", err)
	}

	probe := func(t *testing.T, h *Handler) (int, map[string]json.RawMessage) {
		t.Helper()
		rr := httptest.NewRecorder()
		if err := h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/events/readyz", nil), nil); err != nil {
			t.Fatalf("ServeHTTP: %v", err)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("body %q: %v", rr.Body.String(), err)
		}
		return rr.Code, body
	}
	handler := func(t *testing.T, stream string, details bool) *Handler {
		h := &Handler{NatsURL: ns.ClientURL(), StreamName: stream, HealthDetails: details, logger: zap.NewNop()}
		connectHandler(t, h)
		return h
	}

	code, body := probe(t, handler(t, "DETAILS", true))
	var server natsServerDetails
	var stream streamDetails
	if err := json.Unmarshal(body["nats_server"], &server); err != nil || code != http.StatusOK {
		t.Fatalf("status %d, nats_server %s: %v", code, body["nats_server"], err)
	}
	if server.Version != natsserver.VERSION || server.Name != ns.Name() || server.Cluster != "east" || server.Domain != "hub" {
		t.Fatalf("nats_server = %+v, want version %s, name %s, cluster east, domain hub", server, natsserver.VERSION, ns.Name())
	}
	if err := json.Unmarshal(body["stream_info"], &stream); err != nil {
		t.Fatalf("stream_info %s: %v", body["stream_info"], err)
	}
	if stream.Name != "DETAILS" || !slices.Equal(stream.Subjects, []string{"events.>"}) || stream.Storage != jetstream.MemoryStorage ||
		stream.Messages != 3 || stream.Bytes == 0 || stream.Consumers != 1 || stream.FirstSeq != 1 || stream.LastSeq != 3 || stream.MaxMsgs != 1000 ||
		stream.MaxBytes != -1 || stream.MaxAgeSeconds != 3600 || stream.MaxConsumers != 50 || stream.Replicas != 1 || stream.Created.IsZero() {
		t.Fatalf("stream_info = %+v", stream)
	}
	if !strings.Contains(string(body["stream_info"]), `"storage":"memory"`) {
		t.Fatalf("storage is not written as a name: %s", body["stream_info"])
	}

	if _, body := probe(t, handler(t, "DETAILS", false)); body["nats_server"] != nil || body["stream_info"] != nil {
		t.Fatalf("details without health_details: %v", body)
	}
	if code, body := probe(t, handler(t, "MISSING", true)); code != http.StatusServiceUnavailable || body["stream_info"] != nil || body["nats_server"] == nil {
		t.Fatalf("a stream that cannot be read: status %d, body %v; want 503 with the server and no stream", code, body)
	}

	h := handler(t, "DETAILS", true)
	ns.Shutdown()
	for h.conn.IsConnected() {
		time.Sleep(10 * time.Millisecond)
	}
	if code, body := probe(t, h); code != http.StatusServiceUnavailable || body["nats_server"] != nil {
		t.Fatalf("disconnected: status %d, body %v; want 503 without a server", code, body)
	}
	if newStreamDetails(nil) != nil {
		t.Fatal("details of no stream info")
	}
}
