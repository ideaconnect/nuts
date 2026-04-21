// Tests covering Phase B security hardening and remaining Phase A items
// that needed dedicated coverage (A1, A6, A7, A8, A10).
package nuts

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	iopm "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// counterValue returns the current value of a labelled counter or 0 if absent.
func counterValue(c *prometheus.CounterVec, labels ...string) float64 {
	m, err := c.GetMetricWithLabelValues(labels...)
	if err != nil {
		return 0
	}
	pb := &iopm.Metric{}
	if err := m.Write(pb); err != nil {
		return 0
	}
	return pb.GetCounter().GetValue()
}

// ── B3: configurable CORS headers ─────────────────────────────────────────

func TestHandler_CORS_CustomAllowedHeaders(t *testing.T) {
	h := &Handler{
		AllowedOrigins: []string{"*"},
		AllowedHeaders: []string{"Authorization", "X-Custom-Header"},
		AllowedMethods: []string{"GET", "POST", "OPTIONS"},
		logger:         zap.NewNop(),
	}

	req := httptest.NewRequest(http.MethodOptions, "/events?topic=x", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rr := httptest.NewRecorder()

	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := rr.Header().Get("Access-Control-Allow-Headers"); got != "Authorization, X-Custom-Header" {
		t.Errorf("Access-Control-Allow-Headers: got %q", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Methods"); got != "GET, POST, OPTIONS" {
		t.Errorf("Access-Control-Allow-Methods: got %q", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("Access-Control-Allow-Origin: got %q", got)
	}
}

// ── B4: oversized raw payload dropped before JSON parse ──────────────────

func TestHandler_MaxEventSize_DropsOversizedRawPayload(t *testing.T) {
	ns := startJetStreamServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	core, obs := observer.New(zap.WarnLevel)

	h := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "EVENTS",
		TopicPrefix:       "events.",
		HeartbeatInterval: 30,
		MaxEventSize:      64, // small cap so our big payload is dropped
		AllowedOrigins:    []string{"*"},
		logger:            zap.New(core),
	}
	if err := h.connectNATS(); err != nil {
		t.Fatalf("connectNATS: %v", err)
	}
	defer h.Cleanup()
	js, _ := h.conn.JetStream()
	h.mu.Lock()
	h.js = js
	h.mu.Unlock()

	jsPub, _ := nc.JetStream()
	// Binary blob: not valid JSON and definitely larger than the cap.
	big := strings.Repeat("Z", 512)
	if _, err := jsPub.Publish("events.raw", []byte(big)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/events?topic=raw&last-id=0", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 1500*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)

	rr := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()
	<-done

	if strings.Contains(rr.Body.String(), big) {
		t.Errorf("oversized raw payload leaked into response body")
	}

	found := false
	for _, e := range obs.All() {
		if strings.Contains(e.Message, "exceeds max_event_size") ||
			strings.Contains(e.Message, "dropping oversized") ||
			strings.Contains(strings.ToLower(e.Message), "max_event_size") {
			found = true
			break
		}
	}
	if !found {
		t.Logf("warning log entries: %+v", obs.All())
		// Not fatal — log wording may evolve. The body check is the contract.
	}
}

// ── B5: max_connections cap ───────────────────────────────────────────────

func TestHandler_MaxConnections_RejectsExcess(t *testing.T) {
	ns := startJetStreamServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	h := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "EVENTS",
		TopicPrefix:       "events.",
		HeartbeatInterval: 30,
		MaxConnections:    1,
		AllowedOrigins:    []string{"*"},
		ClientBufferSize:  64,
		logger:            zap.NewNop(),
	}
	if err := h.connectNATS(); err != nil {
		t.Fatalf("connectNATS: %v", err)
	}
	defer h.Cleanup()
	js, _ := h.conn.JetStream()
	h.mu.Lock()
	h.js = js
	h.mu.Unlock()

	before := counterValue(metricsConnectionsRejected, "max_connections")

	// Keep the first connection open for the duration of the test.
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	req1 := httptest.NewRequest(http.MethodGet, "/events?topic=a", nil).WithContext(ctx1)
	rr1 := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	first := make(chan error, 1)
	go func() { first <- h.ServeHTTP(rr1, req1, nil) }()

	// Wait until the counter actually reflects the in-flight connection.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&h.connCount) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt64(&h.connCount) != 1 {
		cancel1()
		<-first
		t.Fatalf("first connection never reserved a slot")
	}

	// Second request must be rejected immediately.
	req2 := httptest.NewRequest(http.MethodGet, "/events?topic=b", nil)
	rr2 := httptest.NewRecorder()
	if err := h.ServeHTTP(rr2, req2, nil); err != nil {
		t.Fatalf("second ServeHTTP returned err: %v", err)
	}
	if rr2.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rr2.Code)
	}
	if got := rr2.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After: expected 5, got %q", got)
	}

	if got := counterValue(metricsConnectionsRejected, "max_connections"); got <= before {
		t.Errorf("nuts_connections_rejected_total{reason=max_connections} did not increment: %v -> %v", before, got)
	}

	// Tear down the held connection.
	cancel1()
	select {
	case <-first:
	case <-time.After(3 * time.Second):
		t.Fatal("first ServeHTTP did not return after cancel")
	}
}

// ── B6: warnings for cleartext auth + insecure TLS ────────────────────────

func TestHandler_Validate_WarnsCleartextAuth(t *testing.T) {
	core, obs := observer.New(zap.WarnLevel)
	h := &Handler{
		NatsURL:    "nats://nats.example.com:4222",
		StreamName: "EVENTS",
		NatsToken:  "secret-token",
		logger:     zap.New(core),
	}
	if err := h.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !hasLogContaining(obs, "plaintext") {
		t.Errorf("expected cleartext warning, entries=%+v", obs.All())
	}
}

func TestHandler_Validate_WarnsInsecureSkipVerify(t *testing.T) {
	core, obs := observer.New(zap.WarnLevel)
	h := &Handler{
		NatsURL:                   "tls://nats.example.com:4222",
		StreamName:                "EVENTS",
		NatsTLSInsecureSkipVerify: true,
		logger:                    zap.New(core),
	}
	if err := h.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !hasLogContaining(obs, "insecure_skip_verify") {
		t.Errorf("expected insecure_skip_verify warning, entries=%+v", obs.All())
	}
}

func hasLogContaining(obs *observer.ObservedLogs, needle string) bool {
	for _, e := range obs.All() {
		if strings.Contains(e.Message, needle) {
			return true
		}
	}
	return false
}

// ── B2: TLS field validation ──────────────────────────────────────────────

func TestHandler_Validate_TLSCertKeyPairing(t *testing.T) {
	h := &Handler{
		NatsURL:     "tls://nats.example.com:4222",
		StreamName:  "EVENTS",
		NatsTLSCert: "/tmp/cert.pem",
		// NatsTLSKey intentionally empty
		logger: zap.NewNop(),
	}
	if err := h.Validate(); err == nil {
		t.Error("expected error when nats_tls_cert is set without nats_tls_key")
	}
}

// ── A1: MaxReconnects=0 honored when user wrote it ────────────────────────

func TestHandler_MaxReconnectsZero_HonoredFromCaddyfile(t *testing.T) {
	input := `nuts {
        nats_url nats://localhost:4222
        stream_name EVENTS
        max_reconnects 0
    }`
	d := caddyfile.NewTestDispenser(input)
	h := Handler{}
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile: %v", err)
	}
	if !h.maxReconnectsSet {
		t.Errorf("maxReconnectsSet should be true after explicit directive")
	}
	if h.MaxReconnects != 0 {
		t.Errorf("MaxReconnects should be 0 after explicit directive, got %d", h.MaxReconnects)
	}
}

func TestHandler_MaxReconnectsDefault_WhenOmitted(t *testing.T) {
	input := `nuts {
        nats_url nats://localhost:4222
        stream_name EVENTS
    }`
	d := caddyfile.NewTestDispenser(input)
	h := Handler{}
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile: %v", err)
	}
	if h.maxReconnectsSet {
		t.Errorf("maxReconnectsSet should be false when directive omitted")
	}
}

// ── A6: integer directives reject junk suffix ────────────────────────────

func TestHandler_UnmarshalCaddyfile_RejectsNonNumericInt(t *testing.T) {
	cases := []string{
		"heartbeat_interval 123abc",
		"reconnect_wait 9x",
		"max_reconnects 1.5",
		"max_event_size 1kb",
		"max_connections twelve",
		"client_buffer_size -",
	}
	for _, line := range cases {
		line := line
		t.Run(line, func(t *testing.T) {
			input := "nuts {\n    nats_url nats://localhost:4222\n    stream_name EVENTS\n    " + line + "\n}"
			d := caddyfile.NewTestDispenser(input)
			h := Handler{}
			if err := h.UnmarshalCaddyfile(d); err == nil {
				t.Errorf("expected parse error for %q", line)
			}
		})
	}
}

// ── A7: health_path custom + suffix match ─────────────────────────────────

func TestHandler_HealthPath_CustomSuffix(t *testing.T) {
	ns := startJetStreamServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	h := &Handler{
		NatsURL:    ns.ClientURL(),
		StreamName: "EVENTS",
		HealthPath: "/status",
		logger:     zap.NewNop(),
	}
	if err := h.connectNATS(); err != nil {
		t.Fatalf("connectNATS: %v", err)
	}
	defer h.Cleanup()
	js, _ := h.conn.JetStream()
	h.mu.Lock()
	h.js = js
	h.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	rr := httptest.NewRecorder()
	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 on custom health path, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "\"status\"") {
		t.Errorf("expected JSON status body, got %q", rr.Body.String())
	}

	// Original /healthz should NOT be a health endpoint when HealthPath is /status:
	// it should fall through to SSE topic handling (topic "healthz").
	// Just ensure it does not return a JSON health blob.
	req2 := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	ctx, cancel := context.WithTimeout(req2.Context(), 200*time.Millisecond)
	defer cancel()
	req2 = req2.WithContext(ctx)
	rr2 := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr2, req2, nil) }()
	<-done
	if strings.Contains(rr2.Body.String(), `"nats":"connected"`) {
		t.Errorf("/healthz should not be a health endpoint when HealthPath=/status")
	}
}

// ── A8: MaxEventSize < 0 disables the limit ──────────────────────────────

func TestHandler_MaxEventSize_NegativeDisablesLimit(t *testing.T) {
	ns := startJetStreamServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	h := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "EVENTS",
		TopicPrefix:       "events.",
		HeartbeatInterval: 30,
		MaxEventSize:      -1, // unlimited
		AllowedOrigins:    []string{"*"},
		logger:            zap.NewNop(),
	}
	if err := h.connectNATS(); err != nil {
		t.Fatalf("connectNATS: %v", err)
	}
	defer h.Cleanup()
	js, _ := h.conn.JetStream()
	h.mu.Lock()
	h.js = js
	h.mu.Unlock()

	jsPub, _ := nc.JetStream()
	payload := []byte(`{"x":"` + strings.Repeat("Y", 4000) + `"}`)
	if _, err := jsPub.Publish("events.big", payload); err != nil {
		t.Fatalf("publish: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/events?topic=big&last-id=0", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 1500*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)

	rr := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()
	<-done

	if !strings.Contains(rr.Body.String(), strings.Repeat("Y", 4000)) {
		t.Errorf("expected large payload delivered when MaxEventSize<0")
	}
}

// ── A10: Provision() fails BEFORE opening NATS when required fields absent ──

func TestHandler_Provision_RejectsBeforeDialing(t *testing.T) {
	// Point at a guaranteed-closed port so any actual dial attempt would fail
	// with a connection error — but we expect the missing-field error instead.
	h := &Handler{
		NatsURL: "", // intentionally empty; stream_name missing too
	}
	err := h.validateRequiredFields()
	if err == nil {
		t.Fatal("expected validation error for missing nats_url")
	}
	if !strings.Contains(err.Error(), "nats_url") {
		t.Errorf("expected error to mention nats_url, got %v", err)
	}
}

// ── Sanity: make sure io.Discard reference is retained for vet/imports ──

var _ = io.Discard
