// Request hardening: CORS, connection, topic and event-size limits, probe
// paths, replay caps and windows, and oversized cursors.
package nuts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// ── Configurable CORS headers ─────────────────────────────────────────────

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
	if got := rr.Header().Get("Access-Control-Allow-Methods"); got != "GET, OPTIONS" {
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

// TestHandler_CORS_FiresOnErrorResponses asserts that browser-visible
// error responses (validation 400, method-not-allowed 405) include the
// configured CORS headers. Without this, browsers translate the failure
// into an opaque CORS error and the operator never sees the real status.
func TestHandler_CORS_FiresOnErrorResponses(t *testing.T) {
	h := &Handler{
		AllowedOrigins: []string{"https://app.example.com"},
		StreamName:     "TEST",
		logger:         zap.NewNop(),
	}

	cases := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{"bad request (no topics)", http.MethodGet, "/", http.StatusBadRequest},
		{"method not allowed (POST without next)", http.MethodPost, "/events?topic=x", http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Origin", "https://app.example.com")
			rr := httptest.NewRecorder()
			if err := h.ServeHTTP(rr, req, nil); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rr.Code != tc.want {
				t.Errorf("status = %d, want %d", rr.Code, tc.want)
			}
			if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
				t.Errorf("Access-Control-Allow-Origin: got %q, want %q", got, "https://app.example.com")
			}
			if got := rr.Header().Get("Vary"); !strings.Contains(got, "Origin") {
				t.Errorf("Vary: got %q, want to contain 'Origin'", got)
			}
		})
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

func TestHandler_CORS_CredentialPolicyAppliesToSSE(t *testing.T) {
	tests := []struct {
		name            string
		allowedOrigins  []string
		origin          string
		wantCredentials string
	}{
		{
			name:            "wildcard omits credentials on stream response",
			allowedOrigins:  []string{"*"},
			origin:          "https://app.example.com",
			wantCredentials: "",
		},
		{
			name:            "explicit origin enables credentials on stream response",
			allowedOrigins:  []string{"https://app.example.com"},
			origin:          "https://app.example.com",
			wantCredentials: "true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _, nc := newProvisionedHandler(t)
			defer nc.Close()
			defer h.Cleanup()
			h.AllowedOrigins = tt.allowedOrigins

			ctx, cancel := context.WithCancel(context.Background())
			req := httptest.NewRequest(http.MethodGet, "/events?topic=cors", nil).WithContext(ctx)
			req.Header.Set("Origin", tt.origin)
			rr := newSafeRecorder()
			done := make(chan error, 1)
			go func() { done <- h.ServeHTTP(rr, req, nil) }()

			if !waitForSSEBody(rr, "event: connected", 2*time.Second) {
				cancel()
				<-done
				t.Fatalf("SSE stream did not connect; body=%s", rr.Body())
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("ServeHTTP returned error: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("ServeHTTP did not return after cancel")
			}

			if got := rr.Header().Get("Access-Control-Allow-Origin"); got != tt.origin {
				t.Fatalf("Access-Control-Allow-Origin = %q, want %q", got, tt.origin)
			}
			if got := rr.Header().Get("Access-Control-Allow-Credentials"); got != tt.wantCredentials {
				t.Fatalf("Access-Control-Allow-Credentials = %q, want %q", got, tt.wantCredentials)
			}
			if got := rr.Header().Get("Vary"); !strings.Contains(got, "Origin") {
				t.Fatalf("Vary = %q, want Origin", got)
			}
		})
	}
}

func TestHandler_Security_InvalidTopicsRejectedBeforeStreaming(t *testing.T) {
	h := &Handler{logger: zap.NewNop()}
	tests := []struct {
		name  string
		topic string
	}{
		{name: "wildcard star", topic: "orders.*"},
		{name: "wildcard greater-than", topic: "orders.>"},
		{name: "system prefix", topic: "$SYS.accounts"},
		{name: "slash", topic: "tenant/a"},
		{name: "control character", topic: "tenant\x00a"},
		{name: "over max length", topic: strings.Repeat("a", 257)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/events?topic="+url.QueryEscape(tt.topic), nil)
			rr := httptest.NewRecorder()

			if err := h.ServeHTTP(rr, req, nil); err != nil {
				t.Fatalf("ServeHTTP returned error: %v", err)
			}
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
			}
			if !strings.Contains(rr.Body.String(), "Invalid topic name") {
				t.Fatalf("body = %q, want invalid-topic error", rr.Body.String())
			}
			if strings.Contains(rr.Body.String(), "event: connected") {
				t.Fatalf("invalid topic reached streaming path: %q", rr.Body.String())
			}
		})
	}
}

// ── Oversized raw payload dropped before JSON parse ──────────────────────

// TestHandler_MaxEventSize_DropsOversizedRawPayload: a message whose payload
// exceeds max_event_size is dropped before it is parsed or formatted. The
// drop is counted and logged with the message's stream sequence, and the
// messages on either side of it still arrive.
func TestHandler_MaxEventSize_DropsOversizedRawPayload(t *testing.T) {
	core, obs := observer.New(zap.WarnLevel)
	h, nc := provisionOnStream(t, jetstream.StreamConfig{Name: "EVENTS", Subjects: []string{"events.>"}}, func(h *Handler) {
		h.MaxEventSize = 150 // drops the 512-byte payload, keeps a ~100-byte frame
		h.logger = zap.New(core)
	})
	js, _ := nc.JetStream()
	big := strings.Repeat("Z", 512) // not JSON, and far over the limit
	for _, payload := range []string{`{"before":1}`, big, `{"after":1}`} {
		if _, err := js.Publish("events.raw", []byte(payload)); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	droppedBefore := metricValue(t, metricsMessagesDropped.WithLabelValues(dropReasonRawPayload))

	rr, cancel, done := startSSE(t, h, "/events?topic=raw&last-id=0", "")
	if !waitForSSEBody(rr, `{"after":1}`, 3*time.Second) {
		t.Fatalf("message after the oversized one not delivered; body=%q", rr.Body())
	}
	stopSSE(t, cancel, done)

	if ids := parseSSEIDs(t, rr.Body()); !slices.Equal(ids, []uint64{0, 1, 3}) {
		t.Errorf("ids = %v, want the cursor, then 1 and 3 around the dropped 2", ids)
	}
	if strings.Contains(rr.Body(), big) {
		t.Errorf("oversized raw payload leaked into response body")
	}
	if got := metricValue(t, metricsMessagesDropped.WithLabelValues(dropReasonRawPayload)); got != droppedBefore+1 {
		t.Errorf("messages_dropped_total{raw_payload} = %v, want %v", got, droppedBefore+1)
	}
	drops := obs.FilterMessage("dropping oversized NATS payload")
	if drops.Len() != 1 || !hasIntLogField(drops, "stream_sequence", 2) || !hasIntLogField(drops, "payload_size", 512) {
		t.Errorf("want one oversized-payload warning for stream sequence 2, 512 bytes: %+v", obs.All())
	}
}

// ── max_connections cap ───────────────────────────────────────────────────

func TestHandler_MaxConnections_RejectsExcess(t *testing.T) {
	maxConnCore, maxConnLogs := observer.New(zap.WarnLevel)
	ns := startJetStreamServer(t)

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
		AllowedOrigins:    []string{"https://app.example.com"},
		ClientBufferSize:  64,
		logger:            zap.New(maxConnCore),
	}
	connectHandler(t, h)

	before := metricValue(t, metricsConnectionsRejected.WithLabelValues("max_connections"))

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
	req2.Header.Set("Origin", "https://app.example.com")
	rr2 := httptest.NewRecorder()
	if err := h.ServeHTTP(rr2, req2, nil); err != nil {
		t.Fatalf("second ServeHTTP returned err: %v", err)
	}
	if rr2.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 (RFC 6585) for max_connections, got %d", rr2.Code)
	}
	assertRetryAfter(t, rr2.Header())
	// CORS headers precede the cap check (#67): without them a browser sees
	// an opaque CORS failure instead of the 429 and never retries.
	if got := rr2.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("429 Access-Control-Allow-Origin = %q, want the request origin", got)
	}
	if got := rr2.Header().Get("Vary"); got != "Origin" {
		t.Errorf("429 Vary = %q, want Origin", got)
	}
	// The rejection is logged with its reason and the cap (#66).
	if !hasLogField(maxConnLogs, "disconnect_reason", "max_connections") || !hasIntLogField(maxConnLogs, "max_connections", 1) {
		t.Errorf("missing disconnect_reason=max_connections / max_connections=1 in %v", maxConnLogs.All())
	}

	// A native EventSource would give up for good on the 429, so it is told
	// to retry instead (#105).
	req3 := httptest.NewRequest(http.MethodGet, "/events?topic=b", nil)
	req3.Header.Set("Accept", "text/event-stream")
	rr3 := httptest.NewRecorder()
	if err := h.ServeHTTP(rr3, req3, nil); err != nil {
		t.Fatalf("third ServeHTTP returned err: %v", err)
	}
	assertRetryStream(t, rr3, "Too many concurrent connections")

	if got := metricValue(t, metricsConnectionsRejected.WithLabelValues("max_connections")); got != before+2 { // the plain and the EventSource request
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

func TestHandler_MethodNotAllowed_AllowHeaderOnlyAdvertisesServedMethods(t *testing.T) {
	h := &Handler{
		AllowedMethods: []string{"GET", "POST", "OPTIONS"},
		logger:         zap.NewNop(),
	}
	req := httptest.NewRequest(http.MethodPost, "/events?topic=x", nil)
	rr := httptest.NewRecorder()

	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusMethodNotAllowed)
	}
	if got := rr.Header().Get("Allow"); got != "GET, OPTIONS" {
		t.Fatalf("Allow header = %q, want GET, OPTIONS", got)
	}
}

// ── health_path custom + suffix match ─────────────────────────────────────

func TestHandler_HealthPath_CustomSuffix(t *testing.T) {
	ns := startJetStreamServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	h := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "EVENTS",
		HealthPath:        "/status",
		HeartbeatInterval: 30,
		logger:            zap.NewNop(),
	}
	connectHandler(t, h)

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
	// it falls through to SSE topic handling, as topic "healthz", which the
	// stream (events.>) does not carry.
	rr2 := httptest.NewRecorder()
	if err := h.ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/healthz", nil), nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rr2.Code != http.StatusServiceUnavailable || !strings.Contains(rr2.Body.String(), "Failed to subscribe to requested topics: healthz") {
		t.Errorf("/healthz with HealthPath=/status should be handled as topic healthz, got %d %q", rr2.Code, rr2.Body.String())
	}
}

func TestHandler_LiveAndReadyPaths_AreDistinctProbes(t *testing.T) {
	ns := startJetStreamServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	h := &Handler{
		NatsURL:    ns.ClientURL(),
		StreamName: "EVENTS",
		logger:     zap.NewNop(),
	}
	if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	assertProbe := func(path string, wantStatus int, wantBody string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		if err := h.ServeHTTP(rr, req, nil); err != nil {
			t.Fatalf("ServeHTTP(%s): %v", path, err)
		}
		if rr.Code != wantStatus {
			t.Fatalf("%s status = %d, want %d; body=%s", path, rr.Code, wantStatus, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), wantBody) {
			t.Fatalf("%s body = %q, want to contain %q", path, rr.Body.String(), wantBody)
		}
	}

	assertProbe("/livez", http.StatusOK, `"status":"ok"`)
	assertProbe("/readyz", http.StatusOK, `"stream":"available"`)
	assertProbe("/events/healthz", http.StatusOK, `"nats":"connected"`)

	if err := h.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	assertProbe("/livez", http.StatusOK, `"status":"ok"`)
	assertProbe("/readyz", http.StatusServiceUnavailable, `"nats":"disconnected"`)
}

// TestHandler_ProbesAcceptATrailingSlash covers #83: "/livez/" used to fall
// through to topic parsing and answer 503 "Failed to subscribe" for topic
// "livez", so a kubelet probe with a trailing slash restarted the pod.
func TestHandler_ProbesAcceptATrailingSlash(t *testing.T) {
	ns := startJetStreamServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	h := &Handler{
		NatsURL:     ns.ClientURL(),
		StreamName:  "EVENTS",
		TopicPrefix: "events.",
		logger:      zap.NewNop(),
	}
	if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()

	for _, c := range []struct{ path, wantBody string }{
		{"/livez/", `"status":"ok"`},
		{"/readyz/", `"stream":"available"`},
		{"/healthz/", `"nats":"connected"`},
		{"/events/readyz/", `"stream":"available"`},
	} {
		t.Run(c.path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			if err := h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, c.path, nil), nil); err != nil {
				t.Fatalf("ServeHTTP: %v", err)
			}
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
			}
			if got := rr.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
			if !strings.Contains(rr.Body.String(), c.wantBody) {
				t.Fatalf("body = %q, want to contain %q", rr.Body.String(), c.wantBody)
			}
		})
	}
}

// ── health_path segment-boundary match ──────────────────────────────────

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
		{"trailing slash default", "", "/healthz/", true},
		{"trailing slash under a route prefix", "", "/events/healthz/", true},
		{"configured with a trailing slash", "/status/", "/status", true},
		{"configured with a trailing slash under a prefix", "/status/", "/api/v1/status", true},
		{"glued suffix with a trailing slash", "", "/eventshealthz/", false},
		{"only one trailing slash is ignored", "", "/healthz//", false},
		{"root request", "", "/", false},
		{"root configured path matches root", "/", "/", true},
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

// ── MaxEventSize < 0 disables the limit ──────────────────────────────────

func TestHandler_MaxEventSize_NegativeDisablesLimit(t *testing.T) {
	ns := startJetStreamServer(t)

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
	connectHandler(t, h)

	jsPub, _ := nc.JetStream()
	payload := []byte(`{"x":"` + strings.Repeat("Y", 4000) + `"}`)
	if _, err := jsPub.Publish("events.big", payload); err != nil {
		t.Fatalf("publish: %v", err)
	}

	rr, cancel, done := startSSE(t, h, "/events?topic=big&last-id=0", "")
	if !waitForSSEBody(rr, strings.Repeat("Y", 4000), 3*time.Second) {
		t.Errorf("expected large payload delivered when MaxEventSize<0")
	}
	stopSSE(t, cancel, done)
}

// ── #10: replay cap ──────────────────────────────────────────────────────

// TestHandler_ReplayMaxMessages_CapsFallback verifies that when the client
// reconnects with a last-id below the stream's retained range (replay storm
// scenario), ReplayMaxMessages closes the SSE stream after N events.
func TestHandler_ReplayMaxMessages_CapsFallback(t *testing.T) {
	ns := startJetStreamServer(t)

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

	core, obs := observer.New(zap.WarnLevel)
	h := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "EVENTS",
		TopicPrefix:       "events.",
		HeartbeatInterval: 30,
		MaxEventSize:      -1,
		AllowedOrigins:    []string{"*"},
		ReplayMaxMessages: 2,
		logger:            zap.New(core),
	}
	connectHandler(t, h)

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
	if !hasLogField(obs, "reason", "sequence below retention") {
		t.Errorf("expected below-retention replay fallback log, entries=%+v", obs.All())
	}
	if !hasLogField(obs, "disconnect_reason", "replay_cap_reached") {
		t.Errorf("expected replay cap disconnect reason log, entries=%+v", obs.All())
	}
	if !hasLogField(obs, "subject_label", "events.cap") {
		t.Errorf("expected subject_label log field, entries=%+v", obs.All())
	}
	if !hasLogField(obs, "replay_mode", string(replayModeFallbackDeliverAll)) {
		t.Errorf("expected replay_mode log field, entries=%+v", obs.All())
	}
}

func TestHandler_ReplayMaxMessages_CapsValidRetainedReplay(t *testing.T) {
	ns := startJetStreamServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	jsPub, _ := nc.JetStream()
	for i := 0; i < 10; i++ {
		if _, err := jsPub.Publish("events.cap-valid", []byte(`{"i":`+strconv.Itoa(i)+`}`)); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
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
	connectHandler(t, h)

	// The cap ends the stream by itself; the deadline only turns a broken
	// cap into a failure instead of a hung test package.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/events?topic=cap-valid&last-id=1", nil).WithContext(ctx)
	rr := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("replay_max_messages did not end the stream; the request ran into its deadline")
	}
	if delivered := strings.Count(rr.Body.String(), "event: message"); delivered != 2 {
		t.Fatalf("delivered %d messages, want 2 under replay_max_messages=2\nbody: %s", delivered, rr.Body.String())
	}
}

func TestHandler_ReplayWindow_BoundsValidRetainedReplay(t *testing.T) {
	ns := startJetStreamServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})
	replayWindow := 1

	h := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "EVENTS",
		TopicPrefix:       "events.",
		HeartbeatInterval: 30,
		MaxEventSize:      -1,
		AllowedOrigins:    []string{"*"},
		ReplayWindow:      replayWindow,
		logger:            zap.NewNop(),
	}
	connectHandler(t, h)

	jsPub, _ := nc.JetStream()
	oldAck, err := jsPub.Publish("events.window-valid", []byte(`{"age":"old"}`))
	if err != nil {
		t.Fatalf("publish old: %v", err)
	}
	oldMsg, err := jsPub.GetMsg("EVENTS", oldAck.Sequence)
	if err != nil {
		t.Fatalf("get old message: %v", err)
	}
	// Sleep is the assertion here, not synchronisation: the test
	// verifies that NUTS treats a message older than replay_window as
	// out-of-window. We wait until the old message's publish time has
	// aged past replay_window (+ a small buffer) so the next-published
	// "new" message is strictly inside the window.
	if wait := time.Until(oldMsg.Time.Add(time.Duration(replayWindow)*time.Second + 200*time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	if _, err := jsPub.Publish("events.window-valid", []byte(`{"age":"new"}`)); err != nil {
		t.Fatalf("publish new: %v", err)
	}

	rr, cancel, done := startSSE(t, h, "/events?topic=window-valid&last-id=0", "")
	waitForSSEBody(rr, `"new"`, 3*time.Second)
	stopSSE(t, cancel, done)

	body := rr.Body()
	if strings.Contains(body, `"old"`) {
		t.Fatalf("old message outside replay_window was delivered:\n%s", body)
	}
	if !strings.Contains(body, `"new"`) {
		t.Fatalf("new message inside replay_window was not delivered:\n%s", body)
	}
}

// TestHandler_ReplayWindow_UsesStartTime verifies that ReplayWindow replaces
// DeliverAll with a time-bounded StartTime subscription when the requested
// last-id is below the stream's retention frontier, so events older than
// the window are not replayed.
func TestHandler_ReplayWindow_UsesStartTime(t *testing.T) {
	ns := startJetStreamServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	jsPub, _ := nc.JetStream()
	// An old message, purged below, and a recent one inside the window.
	if _, err := jsPub.Publish("events.win", []byte(`{"age":"old"}`)); err != nil {
		t.Fatalf("publish old: %v", err)
	}
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
	connectHandler(t, h)

	rr, cancel, done := startSSE(t, h, "/events?topic=win&last-id=0", "")
	waitForSSEBody(rr, `"new"`, 3*time.Second)
	stopSSE(t, cancel, done)

	body := rr.Body()
	if strings.Contains(body, `"old"`) {
		t.Errorf("message older than replay_window leaked into body:\n%s", body)
	}
	if !strings.Contains(body, `"new"`) {
		t.Errorf("recent message should be replayed under replay_window:\n%s", body)
	}
}

// ── M1: MaxTopicsPerSubscription cap ──────────────────────────────────────

func TestHandler_MaxTopicsPerSubscription_RejectsExcess(t *testing.T) {
	h := &Handler{MaxTopicsPerSubscription: 4, logger: zap.NewNop()}

	url := "/events?topic=a&topic=b&topic=c&topic=d&topic=e"
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rr := httptest.NewRecorder()
	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rr.Body.String(), "Too many topics") {
		t.Fatalf("body = %q, want too-many-topics error", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "event: connected") {
		t.Fatalf("excess-topic request reached streaming path: %q", rr.Body.String())
	}
}

func TestHandler_MaxTopicsPerSubscription_DefaultIs32(t *testing.T) {
	ns := startJetStreamServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	h := &Handler{
		NatsURL:    ns.ClientURL(),
		StreamName: "EVENTS",
	}
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()

	if h.MaxTopicsPerSubscription != 32 {
		t.Fatalf("MaxTopicsPerSubscription = %d, want 32", h.MaxTopicsPerSubscription)
	}
}

func TestHandler_MaxTopicsPerSubscription_NegativeDisablesCap(t *testing.T) {
	h := &Handler{MaxTopicsPerSubscription: -1, logger: zap.NewNop()}

	parts := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		parts = append(parts, "topic=t"+strconv.Itoa(i))
	}
	req := httptest.NewRequest(http.MethodGet, "/events?"+strings.Join(parts, "&"), nil)
	plan, requestErr := h.parseStreamRequest(req)
	if requestErr != nil {
		t.Fatalf("parseStreamRequest returned %#v with cap disabled", requestErr)
	}
	if len(plan.Topics) != 100 {
		t.Fatalf("plan.Topics len = %d, want 100", len(plan.Topics))
	}
}

func TestHandler_MaxTopicsPerSubscription_DedupBeforeCap(t *testing.T) {
	// Cap of 2 with three duplicate topics that collapse to one — must pass.
	h := &Handler{MaxTopicsPerSubscription: 2, logger: zap.NewNop()}
	req := httptest.NewRequest(http.MethodGet, "/events?topic=a&topic=a&topic=a", nil)
	plan, requestErr := h.parseStreamRequest(req)
	if requestErr != nil {
		t.Fatalf("parseStreamRequest: %#v (dedup should bring topics under cap)", requestErr)
	}
	if len(plan.Topics) != 1 {
		t.Fatalf("plan.Topics len = %d, want 1 (deduped)", len(plan.Topics))
	}
}

// ── L3: Oversized last-id ─────────────────────────────────────────────────

func TestHandler_LastEventID_RejectsOversizedQuery(t *testing.T) {
	h := &Handler{logger: zap.NewNop()}
	overlong := strings.Repeat("1", 30)
	req := httptest.NewRequest(http.MethodGet, "/events?topic=x&last-id="+overlong, nil)
	rr := httptest.NewRecorder()
	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rr.Body.String(), "too long") {
		t.Fatalf("body = %q, want too-long error", rr.Body.String())
	}
}

func TestHandler_LastEventID_OversizedHeaderFallsBackToDeliverNew(t *testing.T) {
	core, obs := observer.New(zap.WarnLevel)
	h := &Handler{logger: zap.New(core)}
	overlong := strings.Repeat("9", 30)
	req := httptest.NewRequest(http.MethodGet, "/events?topic=x", nil)
	req.Header.Set("Last-Event-ID", overlong)
	plan, requestErr := h.parseStreamRequest(req)
	if requestErr != nil {
		t.Fatalf("parseStreamRequest returned %#v; want fall-back to DeliverNew", requestErr)
	}
	if plan.Replay.HasLastID {
		t.Fatal("plan.Replay.HasLastID = true; want false (header should be ignored)")
	}
	if !hasLogContaining(obs, "oversized Last-Event-ID") {
		t.Errorf("expected oversized-header warning, entries=%+v", obs.All())
	}
}

// TestHandler_ReadinessProbe_StreamInfoErrorIsCounted covers #65: with NATS
// connected but the stream gone, the probe reports stream_info_error, once.
func TestHandler_ReadinessProbe_StreamInfoErrorIsCounted(t *testing.T) {
	h, _, nc := newProvisionedHandler(t)
	defer nc.Close()
	defer h.Cleanup()
	core, obs := observer.New(zap.WarnLevel)
	h.logger = zap.New(core)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := mustJetStream(t, nc).DeleteStream(ctx, "EVENTS"); err != nil {
		t.Fatalf("DeleteStream: %v", err)
	}
	before := map[string]float64{}
	for _, cause := range []string{"stream_info_error", "nats_disconnected", "jetstream_missing"} {
		before[cause] = metricValue(t, metricsReadinessFailures.WithLabelValues(cause))
	}

	rr := httptest.NewRecorder()
	if err := h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil), nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), `"stream":"unavailable"`) || !strings.Contains(rr.Body.String(), `"nats":"connected"`) {
		t.Fatalf("probe = %d %s, want 503 with the stream unavailable and NATS connected", rr.Code, rr.Body.String())
	}
	for cause, want := range map[string]float64{"stream_info_error": 1, "nats_disconnected": 0, "jetstream_missing": 0} {
		if got := metricValue(t, metricsReadinessFailures.WithLabelValues(cause)) - before[cause]; got != want {
			t.Errorf("readiness_failures_total{cause=%s} moved by %v, want %v", cause, got, want)
		}
	}
	if !hasLogField(obs, "cause", "stream_info_error") {
		t.Errorf("missing cause=stream_info_error warning: %v", obs.All())
	}
}

// TestHandler_RejectionsCarryCORSHeaders covers #67 for the other refusals:
// a browser can only read a 400, 401, 403 or 503 when it carries the CORS
// headers.
func TestHandler_RejectionsCarryCORSHeaders(t *testing.T) {
	secret := "test-secret"
	forbidden := signTestSubscriberJWT(t, secret, map[string]interface{}{"sub": "a", "subscribe": "other", "exp": time.Now().Add(time.Hour).Unix()})
	for _, c := range []struct {
		name   string
		h      *Handler
		target string
		auth   string
		want   int
	}{
		{name: "invalid topic", h: &Handler{}, target: "/events?topic=a*b", want: http.StatusBadRequest},
		{name: "missing token", h: &Handler{SubscriberJWTKey: secret}, target: "/events?topic=a", want: http.StatusUnauthorized},
		{name: "forbidden topic", h: &Handler{SubscriberJWTKey: secret}, target: "/events?topic=a", auth: "Bearer " + forbidden, want: http.StatusForbidden},
		{name: "JetStream unavailable", h: &Handler{}, target: "/events?topic=a", want: http.StatusServiceUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.h.AllowedOrigins = []string{"https://app.example.com"}
			c.h.logger = zap.NewNop()
			req := httptest.NewRequest(http.MethodGet, c.target, nil)
			req.Header.Set("Origin", "https://app.example.com")
			if c.auth != "" {
				req.Header.Set("Authorization", c.auth)
			}
			rr := httptest.NewRecorder()
			if err := c.h.ServeHTTP(rr, req, nil); err != nil {
				t.Fatalf("ServeHTTP: %v", err)
			}
			if rr.Code != c.want {
				t.Fatalf("status = %d, want %d", rr.Code, c.want)
			}
			if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
				t.Fatalf("Access-Control-Allow-Origin = %q, want the request origin", got)
			}
		})
	}
}
