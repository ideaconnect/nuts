// Tests that each metric in metrics.go moves with the event it counts, driven
// through real streams on an embedded NATS server, and that the collectors
// register on the registry Caddy serves.
package nuts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// ── metrics: active_connections gauge ─────────────────────────────────────

func TestMetrics_ActiveConnections_ReflectsLifecycle(t *testing.T) {
	h, _, nc := newProvisionedHandler(t)
	defer nc.Close()
	defer h.Cleanup()

	before := metricValue(t, metricsActiveConnections)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/events?topic=gauge", nil).WithContext(ctx)
	rr := newSafeRecorder()
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()

	if !waitForSSEBody(rr, "event: connected", 2*time.Second) {
		cancel()
		<-done
		t.Fatal("handler never entered streaming loop")
	}

	mid := metricValue(t, metricsActiveConnections)
	if mid < before+1 {
		cancel()
		<-done
		t.Errorf("active_connections did not rise while a client was connected: before=%v mid=%v", before, mid)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after cancel")
	}

	// The gauge is decremented with a defer inside ServeHTTP, so it must be
	// back to the baseline once ServeHTTP has returned.
	after := metricValue(t, metricsActiveConnections)
	if after != before {
		t.Errorf("active_connections did not return to baseline: before=%v after=%v", before, after)
	}
}

// ── metrics: messages_delivered_total counter ─────────────────────────────

func TestMetrics_MessagesDelivered_IncrementsPerEvent(t *testing.T) {
	h, _, nc := newProvisionedHandler(t)
	defer nc.Close()
	defer h.Cleanup()

	jsPub, _ := nc.JetStream()
	for i := 0; i < 3; i++ {
		if _, err := jsPub.Publish("events.delivered", []byte(`{"i":`+strconv.Itoa(i)+`}`)); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	before := metricValue(t, metricsMessagesDelivered)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// last-id=0 replays the three messages from the start of the stream.
	req := httptest.NewRequest(http.MethodGet, "/events?topic=delivered&last-id=0", nil).WithContext(ctx)
	rr := newSafeRecorder()
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()

	// Wait until all three events have been framed.
	if !waitForSSEBody(rr, `"i":2`, 2*time.Second) {
		cancel()
		<-done
		t.Fatalf("did not observe all three messages; body=%s", rr.Body())
	}
	cancel()
	<-done

	if got := metricValue(t, metricsMessagesDelivered); got != before+3 {
		t.Errorf("messages_delivered_total = %v, want %v (exactly the 3 messages)", got, before+3)
	}
}

// ── metrics: slow_client_disconnects_total counter ────────────────────────

func TestMetrics_SlowClientDisconnects_Increments(t *testing.T) {
	h, _, nc := newProvisionedHandler(t)
	defer nc.Close()
	defer h.Cleanup()
	h.WriteTimeout = 1
	core, obs := observer.New(zap.WarnLevel)
	h.logger = zap.New(core)

	jsPub, _ := nc.JetStream()
	for i := 0; i < 5; i++ {
		if _, err := jsPub.Publish("events.slow", []byte(`{"i":`+strconv.Itoa(i)+`}`)); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	before := metricValue(t, metricsSlowClientDisconnects)
	writeDisconnectsBefore := metricValue(t, metricsWriteDisconnects.WithLabelValues("message"))

	req := httptest.NewRequest(http.MethodGet, "/events?topic=slow", nil)
	req.Header.Set("Last-Event-ID", "0")
	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	// The connected event and the first message get through; the next
	// write stalls until write_timeout expires.
	w := newStalledDeadlineWriter(2)
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- h.ServeHTTP(w, req, nil) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeHTTP: %v", err)
		}
	case <-time.After(4 * time.Second):
		cancel()
		<-done
		t.Fatal("a client that stopped reading was not disconnected by write_timeout")
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("disconnected after %v, before write_timeout expired", elapsed)
	}
	if got := metricValue(t, metricsSlowClientDisconnects); got != before+1 {
		t.Errorf("slow_client_disconnects_total = %v, want %v", got, before+1)
	}
	if got := metricValue(t, metricsWriteDisconnects.WithLabelValues("message")); got != writeDisconnectsBefore+1 {
		t.Errorf("write_disconnects_total{site=message} = %v, want %v", got, writeDisconnectsBefore+1)
	}
	if !hasLogField(obs, "disconnect_reason", "slow_client") {
		t.Fatalf("expected disconnect_reason=slow_client, logs=%v", obs.All())
	}
}

// ── metrics: replay_requests_total counter ────────────────────────────────

func TestMetrics_ReplayRequests_Increments(t *testing.T) {
	h, _, nc := newProvisionedHandler(t)
	defer nc.Close()
	defer h.Cleanup()

	before := metricValue(t, metricsReplayRequests)

	// Client with an explicit last-id is a replay request, regardless of
	// whether any messages exist yet.
	_, cancel, done := startSSE(t, h, "/events?topic=replay&last-id=0", "")
	stopSSE(t, cancel, done)

	if got := metricValue(t, metricsReplayRequests); got != before+1 {
		t.Errorf("replay_requests_total = %v, want %v", got, before+1)
	}
}

// ── metrics: replay_fallbacks_total counter ───────────────────────────────

func TestMetrics_ReplayFallbacks_Increments(t *testing.T) {
	h, _, nc := newProvisionedHandler(t)
	defer nc.Close()
	defer h.Cleanup()

	jsPub, _ := nc.JetStream()
	for i := 0; i < 5; i++ {
		if _, err := jsPub.Publish("events.fallback", []byte(`{"i":`+strconv.Itoa(i)+`}`)); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	// Purge the first three so FirstSeq > 1 and a last-id of 1 forces the
	// below-retention fallback branch.
	if err := jsPub.PurgeStream("EVENTS", &nats.StreamPurgeRequest{Sequence: 4}); err != nil {
		t.Fatalf("purge: %v", err)
	}

	before := metricValue(t, metricsReplayFallbacks)

	rr, cancel, done := startSSE(t, h, "/events?topic=fallback&last-id=1", "")
	if !waitForSSEBody(rr, `{"i":4}`, 3*time.Second) {
		t.Fatalf("fallback replay not delivered; body=%q", rr.Body())
	}
	stopSSE(t, cancel, done)

	if got := metricValue(t, metricsReplayFallbacks); got != before+1 {
		t.Errorf("replay_fallbacks_total = %v, want %v", got, before+1)
	}
}

// ── metrics: replay_cap_reached_total counter ─────────────────────────────

func TestMetrics_ReplayCapReached_Increments(t *testing.T) {
	h, _, nc := newProvisionedHandler(t)
	defer nc.Close()
	defer h.Cleanup()

	h.ReplayMaxMessages = 2

	jsPub, _ := nc.JetStream()
	for i := 0; i < 10; i++ {
		if _, err := jsPub.Publish("events.cap", []byte(`{"i":`+strconv.Itoa(i)+`}`)); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	if err := jsPub.PurgeStream("EVENTS", &nats.StreamPurgeRequest{Sequence: 6}); err != nil {
		t.Fatalf("purge: %v", err)
	}

	before := metricValue(t, metricsReplayCapReached)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/events?topic=cap&last-id=1", nil).WithContext(ctx)
	rr := newSafeRecorder()
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()
	<-done

	if got := metricValue(t, metricsReplayCapReached); got != before+1 {
		t.Errorf("replay_cap_reached_total = %v, want %v", got, before+1)
	}
}

// ── metrics: subscription_errors_total counter ────────────────────────────

func TestMetrics_SubscriptionErrors_Increments(t *testing.T) {
	h, _, nc := newProvisionedHandler(t)
	defer nc.Close()
	defer h.Cleanup()

	// Point the handler at a stream that does not exist so js.Subscribe
	// fails with "stream not found" and the error branch in serve.go fires.
	h.StreamName = "NOPE_DOES_NOT_EXIST"

	before := metricValue(t, metricsSubscriptionErrors)

	req := httptest.NewRequest(http.MethodGet, "/events?topic=err", nil)
	rr := httptest.NewRecorder()
	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 on subscription failure, got %d", rr.Code)
	}

	if got := metricValue(t, metricsSubscriptionErrors); got != before+1 {
		t.Errorf("subscription_errors_total = %v, want %v", got, before+1)
	}
}

// TestMetrics_ConsumerInvalidated_RegisteredWithExpectedLabels locks the
// metric's label set: dashboards and alerts reference reason="recreated" and
// reason="unrecoverable". Their increments are asserted, as deltas, by the
// consumer recovery and unrecoverable-consumer tests.
func TestMetrics_ConsumerInvalidated_RegisteredWithExpectedLabels(t *testing.T) {
	for _, reason := range []string{"recreated", "unrecoverable"} {
		if _, err := metricsConsumerInvalidated.GetMetricWithLabelValues(reason); err != nil {
			t.Fatalf("consumer_invalidated_total{reason=%q}: %v", reason, err)
		}
	}
	if _, err := metricsConsumerInvalidated.GetMetricWithLabelValues("recreated", "extra"); err == nil {
		t.Fatal("consumer_invalidated_total accepted two labels, want exactly reason")
	}
}

func TestRegisterMetrics(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	for i := 0; i < 2; i++ { // a second nuts handler in the same config
		if err := registerMetrics(registry); err != nil {
			t.Fatalf("registerMetrics (call %d): %v", i+1, err)
		}
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	found := false
	for _, family := range families {
		if family.GetName() == "nuts_active_connections" {
			found = true
		}
	}
	if !found {
		t.Fatal("nuts_active_connections missing from the registry")
	}
	if err := registerMetrics(nil); err != nil {
		t.Fatalf("registerMetrics(nil) = %v, want no-op", err)
	}
}
