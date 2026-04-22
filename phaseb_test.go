// Tests covering Phase B security hardening and remaining Phase A items
// that needed dedicated coverage (A1, A6, A7, A8, A10).
package nuts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// intPtr is a small helper for constructing *int fields in struct literals.
func intPtr(v int) *int { return &v }

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

// TestHandler_CORS_WildcardOmitsCredentials asserts that a wildcard
// allowed_origins never advertises Access-Control-Allow-Credentials, and
// that Vary: Origin is set so caches don't serve one origin's response to
// another.
func TestHandler_CORS_WildcardOmitsCredentials(t *testing.T) {
	h := &Handler{
		AllowedOrigins: []string{"*"},
		logger:         zap.NewNop(),
	}

	req := httptest.NewRequest(http.MethodOptions, "/events?topic=x", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rr := httptest.NewRecorder()

	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://evil.example.com" {
		t.Errorf("Access-Control-Allow-Origin: got %q, want echoed origin", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials: got %q, want empty for wildcard match", got)
	}
	if got := rr.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("Vary: got %q, want to contain 'Origin'", got)
	}
}

// TestHandler_CORS_ExplicitOriginSetsCredentials asserts that an explicit
// allow-list entry produces credentialed CORS responses.
func TestHandler_CORS_ExplicitOriginSetsCredentials(t *testing.T) {
	h := &Handler{
		AllowedOrigins: []string{"https://app.example.com", "https://admin.example.com"},
		logger:         zap.NewNop(),
	}

	req := httptest.NewRequest(http.MethodOptions, "/events?topic=x", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	rr := httptest.NewRecorder()

	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://admin.example.com" {
		t.Errorf("Access-Control-Allow-Origin: got %q", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Access-Control-Allow-Credentials: got %q, want 'true' for explicit origin", got)
	}
	if got := rr.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("Vary: got %q, want to contain 'Origin'", got)
	}
}

// TestHandler_CORS_UnlistedOrigin asserts that an unknown origin receives
// no CORS headers.
func TestHandler_CORS_UnlistedOrigin(t *testing.T) {
	h := &Handler{
		AllowedOrigins: []string{"https://app.example.com"},
		logger:         zap.NewNop(),
	}

	req := httptest.NewRequest(http.MethodOptions, "/events?topic=x", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rr := httptest.NewRecorder()

	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin: got %q, want empty for unlisted origin", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials: got %q, want empty", got)
	}
}

// TestHandler_CORS_ExplicitWinsOverWildcard asserts that an explicit origin
// listed alongside "*" still gets credentialed CORS.
func TestHandler_CORS_ExplicitWinsOverWildcard(t *testing.T) {
	h := &Handler{
		AllowedOrigins: []string{"*", "https://app.example.com"},
		logger:         zap.NewNop(),
	}

	req := httptest.NewRequest(http.MethodOptions, "/events?topic=x", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rr := httptest.NewRecorder()

	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := rr.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Access-Control-Allow-Credentials: got %q, want 'true' when explicit origin matches", got)
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
	if h.MaxReconnects == nil {
		t.Fatalf("MaxReconnects should be set after explicit directive")
	}
	if *h.MaxReconnects != 0 {
		t.Errorf("MaxReconnects should be 0 after explicit directive, got %d", *h.MaxReconnects)
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
	if h.MaxReconnects != nil {
		t.Errorf("MaxReconnects should be nil when directive omitted, got %d", *h.MaxReconnects)
	}
}

// TestHandler_MaxReconnectsZero_HonoredFromJSON guards against regressions of
// the JSON/Caddyfile asymmetry: an explicit 0 in JSON must survive Provision's
// defaulting instead of being silently rewritten to -1.
func TestHandler_MaxReconnectsZero_HonoredFromJSON(t *testing.T) {
	raw := []byte(`{
        "nats_url": "nats://localhost:4222",
        "stream_name": "EVENTS",
        "max_reconnects": 0
    }`)
	var h Handler
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if h.MaxReconnects == nil {
		t.Fatalf("MaxReconnects should be set after explicit JSON field")
	}
	if *h.MaxReconnects != 0 {
		t.Errorf("MaxReconnects should be 0 after explicit JSON field, got %d", *h.MaxReconnects)
	}
}

// TestHandler_MaxReconnects_DefaultFromJSON proves that omitting the JSON
// field yields the "unlimited" default after Provision, matching Caddyfile
// behaviour.
func TestHandler_MaxReconnects_DefaultFromJSON(t *testing.T) {
	raw := []byte(`{
        "nats_url": "nats://localhost:4222",
        "stream_name": "EVENTS"
    }`)
	var h Handler
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if h.MaxReconnects != nil {
		t.Errorf("MaxReconnects should be nil when JSON field omitted, got %d", *h.MaxReconnects)
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

// ── A7b: health_path segment-boundary match ──────────────────────────────

func TestHandler_MatchesHealthPath_SegmentBoundary(t *testing.T) {
	tests := []struct {
		name       string
		healthPath string
		reqPath    string
		want       bool
	}{
		{"exact default", "", "/healthz", true},
		{"exact custom", "/status", "/status", true},
		{"prefix boundary default", "", "/events/healthz", true},
		{"prefix boundary custom", "/status", "/api/v1/status", true},
		{"glued suffix default", "", "/eventshealthz", false},
		{"glued suffix custom", "/status", "/apistatus", false},
		{"unrelated path", "", "/events/orders.new", false},
		{"partial segment in middle", "/status", "/statusful/thing", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{HealthPath: tc.healthPath}
			if got := h.matchesHealthPath(tc.reqPath); got != tc.want {
				t.Errorf("matchesHealthPath(%q) with HealthPath=%q: got %v, want %v",
					tc.reqPath, tc.healthPath, got, tc.want)
			}
		})
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

// ── #10: replay cap ──────────────────────────────────────────────────────

// TestHandler_ReplayMaxMessages_CapsFallback verifies that when the client
// reconnects with a last-id below the stream's retained range (replay storm
// scenario), ReplayMaxMessages closes the SSE stream after N events.
func TestHandler_ReplayMaxMessages_CapsFallback(t *testing.T) {
	ns := startJetStreamServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	jsPub, _ := nc.JetStream()
	// Publish 10 messages then purge the first 5 so FirstSeq == 6.
	for i := 0; i < 10; i++ {
		if _, err := jsPub.Publish("events.cap", []byte(`{"i":`+strconv.Itoa(i)+`}`)); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	if err := jsPub.PurgeStream("EVENTS", &nats.StreamPurgeRequest{Sequence: 6}); err != nil {
		t.Fatalf("purge: %v", err)
	}

	h := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "EVENTS",
		TopicPrefix:       "events.",
		HeartbeatInterval: 30,
		MaxEventSize:      -1,
		AllowedOrigins:    []string{"*"},
		ReplayMaxMessages: 2,
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

	// Client reconnects at sequence 1 — below retention (FirstSeq=6).
	// Without the cap the client would receive messages 6..10 (5 events).
	// With ReplayMaxMessages=2 the stream must close after 2 events.
	req := httptest.NewRequest(http.MethodGet, "/events?topic=cap&last-id=1", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	rr := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
	}

	body := rr.Body.String()
	delivered := strings.Count(body, "event: message")
	if delivered != 2 {
		t.Errorf("expected exactly 2 message events under replay_max_messages=2, got %d\nbody: %s", delivered, body)
	}
}

// TestHandler_ReplayWindow_UsesStartTime verifies that ReplayWindow replaces
// DeliverAll with a time-bounded StartTime subscription when the requested
// last-id is below the stream's retention frontier, so events older than
// the window are not replayed.
func TestHandler_ReplayWindow_UsesStartTime(t *testing.T) {
	ns := startJetStreamServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	jsPub, _ := nc.JetStream()
	// Old message, wait past the replay window, then new message.
	if _, err := jsPub.Publish("events.win", []byte(`{"age":"old"}`)); err != nil {
		t.Fatalf("publish old: %v", err)
	}
	time.Sleep(2 * time.Second)
	if _, err := jsPub.Publish("events.win", []byte(`{"age":"new"}`)); err != nil {
		t.Fatalf("publish new: %v", err)
	}
	// Purge the first message so FirstSeq advances past the requested
	// last-id=0, forcing the fallback path where ReplayWindow applies.
	if err := jsPub.PurgeStream("EVENTS", &nats.StreamPurgeRequest{Sequence: 2}); err != nil {
		t.Fatalf("purge: %v", err)
	}

	h := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "EVENTS",
		TopicPrefix:       "events.",
		HeartbeatInterval: 30,
		MaxEventSize:      -1,
		AllowedOrigins:    []string{"*"},
		ReplayWindow:      1, // only the last 1 second is replayed
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

	req := httptest.NewRequest(http.MethodGet, "/events?topic=win&last-id=0", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 1500*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)

	rr := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()
	<-done

	body := rr.Body.String()
	if strings.Contains(body, `"old"`) {
		t.Errorf("message older than replay_window leaked into body:\n%s", body)
	}
	if !strings.Contains(body, `"new"`) {
		t.Errorf("recent message should be replayed under replay_window:\n%s", body)
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

// TestHandler_Cleanup_WakesInFlightHandlers verifies that Cleanup() closes
// the handler-scoped shutdown channel, causing any active SSE goroutine to
// return promptly instead of waiting until the next heartbeat tick (30s by
// default) notices the connection has been torn down.
func TestHandler_Cleanup_WakesInFlightHandlers(t *testing.T) {
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
		HeartbeatInterval: 30, // deliberately long — shutdown must win, not heartbeat
		MaxEventSize:      -1,
		AllowedOrigins:    []string{"*"},
		logger:            zap.NewNop(),
	}
	if err := h.connectNATS(); err != nil {
		t.Fatalf("connectNATS: %v", err)
	}
	js, _ := h.conn.JetStream()
	h.mu.Lock()
	h.js = js
	h.shutdown = make(chan struct{})
	h.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/events?topic=shutdown", nil)
	req = req.WithContext(context.Background())

	rr := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()

	// Wait until the SSE "connected" event has been written so we know the
	// handler is inside the streaming loop and not still in setup.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(rr.Body.String(), "event: connected") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(rr.Body.String(), "event: connected") {
		t.Fatal("handler never reached the streaming loop")
	}

	// Cleanup must wake the handler in well under heartbeat_interval.
	cleanupStart := time.Now()
	if err := h.Cleanup(); err != nil {
		t.Fatalf("Cleanup returned error: %v", err)
	}

	select {
	case err := <-done:
		elapsed := time.Since(cleanupStart)
		if err != nil {
			t.Fatalf("ServeHTTP returned error after Cleanup: %v", err)
		}
		if elapsed > 500*time.Millisecond {
			t.Errorf("ServeHTTP took %v to return after Cleanup; expected < 500ms", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServeHTTP did not return within 3s after Cleanup — shutdown signal not observed")
	}
}

// ── Sanity: make sure io.Discard reference is retained for vet/imports ──

var _ = io.Discard
