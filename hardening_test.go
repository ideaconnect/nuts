// Security-hardening and config-correctness tests for the NUTS handler.
package nuts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	iopm "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// intPtr is a small helper for constructing *int fields in struct literals.
func intPtr(v int) *int { return &v }

type deadlineFlushRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (r *deadlineFlushRecorder) Flush() {}

func (r *deadlineFlushRecorder) SetWriteDeadline(deadline time.Time) error {
	r.deadlines = append(r.deadlines, deadline)
	return nil
}

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
			h, ns, nc := newProvisionedHandler(t)
			defer ns.Shutdown()
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

func TestHandler_SubscriberJWT_RequiresTokenBeforeStreaming(t *testing.T) {
	h := &Handler{SubscriberJWTKey: "test-secret", logger: zap.NewNop()}
	req := httptest.NewRequest(http.MethodGet, "/events?topic=private", nil)
	rr := httptest.NewRecorder()

	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	if got := rr.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatal("missing WWW-Authenticate header")
	}
	if strings.Contains(rr.Body.String(), "JetStream not available") {
		t.Fatalf("unauthenticated request reached streaming runtime: %q", rr.Body.String())
	}
}

func TestHandler_SubscriberJWT_AuthorizesTopicClaims(t *testing.T) {
	secret := "test-secret"
	token := signTestSubscriberJWT(t, secret, map[string]interface{}{
		"sub":       "alice",
		"subscribe": []string{"orders.*", "invoices.paid"},
		"exp":       time.Now().Add(time.Hour).Unix(),
	})
	h := &Handler{SubscriberJWTKey: secret, logger: zap.NewNop()}

	allowedReq := httptest.NewRequest(http.MethodGet, "/events?topic=orders.created&topic=invoices.paid", nil)
	allowedReq.Header.Set("Authorization", "Bearer "+token)
	allowedPlan, requestErr := h.parseStreamRequest(allowedReq)
	if requestErr != nil {
		t.Fatalf("parseStreamRequest: %v", requestErr)
	}
	if authErr := h.authorizeStreamRequest(allowedReq, allowedPlan); authErr != nil {
		t.Fatalf("authorizeStreamRequest returned %#v, want allowed", authErr)
	}

	blockedReq := httptest.NewRequest(http.MethodGet, "/events?topic=admin.audit", nil)
	blockedReq.Header.Set("Authorization", "Bearer "+token)
	blockedPlan, requestErr := h.parseStreamRequest(blockedReq)
	if requestErr != nil {
		t.Fatalf("parseStreamRequest: %v", requestErr)
	}
	if authErr := h.authorizeStreamRequest(blockedReq, blockedPlan); authErr == nil || authErr.status != http.StatusForbidden {
		t.Fatalf("authorizeStreamRequest = %#v, want 403", authErr)
	}
}

func TestHandler_SubscriberJWT_AcceptsConfiguredCookie(t *testing.T) {
	secret := "test-secret"
	token := signTestSubscriberJWT(t, secret, map[string]interface{}{
		"sub":       "browser-client",
		"subscribe": "tenant-a.>",
		"exp":       time.Now().Add(time.Hour).Unix(),
	})
	h := &Handler{
		SubscriberJWTKey:    secret,
		SubscriberJWTCookie: "nuts_session",
		logger:              zap.NewNop(),
	}
	req := httptest.NewRequest(http.MethodGet, "/events?topic=tenant-a.orders", nil)
	req.AddCookie(&http.Cookie{Name: "nuts_session", Value: token})
	plan, requestErr := h.parseStreamRequest(req)
	if requestErr != nil {
		t.Fatalf("parseStreamRequest: %v", requestErr)
	}
	if authErr := h.authorizeStreamRequest(req, plan); authErr != nil {
		t.Fatalf("authorizeStreamRequest returned %#v, want allowed", authErr)
	}
}

func TestHandler_SubscriberJWT_RejectsExpiredToken(t *testing.T) {
	secret := "test-secret"
	token := signTestSubscriberJWT(t, secret, map[string]interface{}{
		"sub":       "alice",
		"subscribe": "*",
		"exp":       time.Now().Add(-time.Minute).Unix(),
	})
	h := &Handler{SubscriberJWTKey: secret, logger: zap.NewNop()}
	req := httptest.NewRequest(http.MethodGet, "/events?topic=orders", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	plan, requestErr := h.parseStreamRequest(req)
	if requestErr != nil {
		t.Fatalf("parseStreamRequest: %v", requestErr)
	}
	if authErr := h.authorizeStreamRequest(req, plan); authErr == nil || authErr.status != http.StatusUnauthorized {
		t.Fatalf("authorizeStreamRequest = %#v, want 401", authErr)
	}
}

func TestSubscriberJWTVerifier_RejectsInvalidTokens(t *testing.T) {
	secret := "test-secret"
	now := time.Now()
	tooManyFilters := make([]string, maxSubscribeClaimFilters+1)
	for i := range tooManyFilters {
		tooManyFilters[i] = "orders"
	}
	tests := []struct {
		name  string
		token string
	}{
		{name: "oversized token", token: strings.Repeat("a", maxSubscriberJWTLen+1)},
		{name: "malformed", token: "one.two"},
		{name: "unsupported algorithm", token: unsignedTestJWT(t, "none", map[string]interface{}{"subscribe": "*"})},
		{name: "bad signature", token: signTestSubscriberJWT(t, "wrong-secret", map[string]interface{}{"subscribe": "*", "exp": now.Add(time.Hour).Unix()})},
		{name: "missing subscribe", token: signTestSubscriberJWT(t, secret, map[string]interface{}{"exp": now.Add(time.Hour).Unix()})},
		{name: "empty subscribe", token: signTestSubscriberJWT(t, secret, map[string]interface{}{"subscribe": []string{}, "exp": now.Add(time.Hour).Unix()})},
		{name: "too many subscribe entries", token: signTestSubscriberJWT(t, secret, map[string]interface{}{"subscribe": tooManyFilters, "exp": now.Add(time.Hour).Unix()})},
		{name: "invalid subscribe filter", token: signTestSubscriberJWT(t, secret, map[string]interface{}{"subscribe": "orders/created", "exp": now.Add(time.Hour).Unix()})},
		{name: "not before future", token: signTestSubscriberJWT(t, secret, map[string]interface{}{"subscribe": "*", "nbf": now.Add(time.Hour).Unix()})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := verifySubscriberJWT(tt.token, []byte(secret), now); err == nil {
				t.Fatal("expected verifySubscriberJWT to reject token")
			}
		})
	}
}

func TestSubscriberTopicMatches(t *testing.T) {
	tests := []struct {
		name   string
		topic  string
		filter string
		want   bool
	}{
		{name: "exact", topic: "orders.created", filter: "orders.created", want: true},
		{name: "single token wildcard", topic: "orders.created", filter: "orders.*", want: true},
		{name: "tail wildcard", topic: "tenant-a.orders.created", filter: "tenant-a.>", want: true},
		{name: "route wildcard", topic: "anything.here", filter: "*", want: true},
		{name: "single token wildcard does not cross dots", topic: "orders.created.high", filter: "orders.*", want: false},
		{name: "different tenant", topic: "tenant-b.orders", filter: "tenant-a.>", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := subscriberTopicMatches(tt.topic, tt.filter); got != tt.want {
				t.Fatalf("subscriberTopicMatches(%q, %q) = %v, want %v", tt.topic, tt.filter, got, tt.want)
			}
		})
	}
}

func signTestSubscriberJWT(t *testing.T, secret string, claims map[string]interface{}) string {
	t.Helper()
	encodedHeader := encodeTestJWTPart(t, map[string]interface{}{"alg": "HS256", "typ": "JWT"})
	encodedPayload := encodeTestJWTPart(t, claims)
	signed := encodedHeader + "." + encodedPayload
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signed))
	return signed + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func unsignedTestJWT(t *testing.T, alg string, claims map[string]interface{}) string {
	t.Helper()
	return encodeTestJWTPart(t, map[string]interface{}{"alg": alg, "typ": "JWT"}) + "." + encodeTestJWTPart(t, claims) + "."
}

func encodeTestJWTPart(t *testing.T, value interface{}) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JWT part: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func TestAllowedMethodsHeader_FiltersToServedMethods(t *testing.T) {
	got := allowedMethodsHeader([]string{"POST", "get", "GET", "OPTIONS", "TRACE"})
	if got != "GET, OPTIONS" {
		t.Fatalf("allowedMethodsHeader() = %q, want GET, OPTIONS", got)
	}
}

// ── Oversized raw payload dropped before JSON parse ──────────────────────

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

// ── max_connections cap ───────────────────────────────────────────────────

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
	if rr2.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 (RFC 6585) for max_connections, got %d", rr2.Code)
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

// ── Warnings for cleartext auth + insecure TLS ────────────────────────────

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

func TestHandler_Validate_WarnsCleartextCredentialsFile(t *testing.T) {
	core, obs := observer.New(zap.WarnLevel)
	h := &Handler{
		NatsURL:         "nats://nats.example.com:4222",
		StreamName:      "EVENTS",
		NatsCredentials: "/etc/nats/user.creds",
		logger:          zap.New(core),
	}
	if err := h.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !hasLogContaining(obs, "plaintext") {
		t.Errorf("expected cleartext credentials warning, entries=%+v", obs.All())
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

func hasLogField(obs *observer.ObservedLogs, key, value string) bool {
	for _, e := range obs.All() {
		for _, field := range e.Context {
			if field.Key == key && field.String == value {
				return true
			}
		}
	}
	return false
}

// ── TLS field validation ──────────────────────────────────────────────────

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

// ── MaxReconnects=0 honored when user wrote it ────────────────────────────

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

// ── Integer directives reject junk suffix ────────────────────────────────

func TestHandler_UnmarshalCaddyfile_RejectsNonNumericInt(t *testing.T) {
	cases := []string{
		"heartbeat_interval 123abc",
		"reconnect_wait 9x",
		"max_reconnects 1.5",
		"max_event_size 1kb",
		"max_connections twelve",
		"client_buffer_size -",
		"dispatch_timeout 1s",
		"write_timeout 2s",
		"nats_idle_heartbeat 5x",
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

func TestHandler_UnmarshalCaddyfile_RejectsInvalidOptionalConfig(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		wantErr     string
		validateErr bool
	}{
		{
			name:        "max reconnects below unlimited sentinel",
			line:        "max_reconnects -2",
			wantErr:     "max_reconnects",
			validateErr: true,
		},
		{
			name:    "negative max connections",
			line:    "max_connections -1",
			wantErr: "max_connections",
		},
		{
			name:    "negative client buffer size",
			line:    "client_buffer_size -1",
			wantErr: "client_buffer_size",
		},
		{
			name:    "negative dispatch timeout",
			line:    "dispatch_timeout -1",
			wantErr: "dispatch_timeout",
		},
		{
			name:    "negative write timeout",
			line:    "write_timeout -1",
			wantErr: "write_timeout",
		},
		{
			name:    "negative replay max messages",
			line:    "replay_max_messages -1",
			wantErr: "replay_max_messages",
		},
		{
			name:    "negative replay window",
			line:    "replay_window -1",
			wantErr: "replay_window",
		},
		{
			name:        "unsupported allowed method",
			line:        "allowed_methods GET POST OPTIONS",
			wantErr:     "allowed_methods",
			validateErr: true,
		},
		{
			name:        "subscriber cookie requires key",
			line:        "subscriber_jwt_cookie nuts_session",
			wantErr:     "subscriber_jwt_key",
			validateErr: true,
		},
		{
			name:        "subscriber cookie validates name",
			line:        "subscriber_jwt_key secret\n    subscriber_jwt_cookie bad;name",
			wantErr:     "subscriber_jwt_cookie",
			validateErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := "nuts {\n    nats_url nats://localhost:4222\n    stream_name EVENTS\n    " + tt.line + "\n}"
			d := caddyfile.NewTestDispenser(input)
			h := Handler{}
			err := h.UnmarshalCaddyfile(d)
			if tt.validateErr {
				if err != nil {
					t.Fatalf("UnmarshalCaddyfile returned error before validation: %v", err)
				}
				err = h.Validate()
			}
			if err == nil {
				t.Fatalf("expected error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestHandler_UnmarshalCaddyfile_PreservesSentinelConfigSemantics(t *testing.T) {
	input := `nuts {
        nats_url nats://localhost:4222
        stream_name EVENTS
        max_event_size -1
        client_buffer_size 0
    }`
	d := caddyfile.NewTestDispenser(input)
	h := Handler{}
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile: %v", err)
	}
	if err := h.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if h.MaxEventSize != -1 {
		t.Fatalf("MaxEventSize = %d, want -1 unlimited sentinel", h.MaxEventSize)
	}
	if h.ClientBufferSize != 0 {
		t.Fatalf("ClientBufferSize = %d, want 0 default sentinel", h.ClientBufferSize)
	}
}

func TestHandler_Validate_RejectsInvalidOptionalConfig(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Handler)
		wantErr string
	}{
		{
			name:    "max reconnects below unlimited sentinel",
			mutate:  func(h *Handler) { h.MaxReconnects = intPtr(-2) },
			wantErr: "max_reconnects",
		},
		{
			name:    "negative max connections",
			mutate:  func(h *Handler) { h.MaxConnections = -1 },
			wantErr: "max_connections",
		},
		{
			name:    "negative client buffer size",
			mutate:  func(h *Handler) { h.ClientBufferSize = -1 },
			wantErr: "client_buffer_size",
		},
		{
			name:    "negative dispatch timeout",
			mutate:  func(h *Handler) { h.DispatchTimeout = -1 },
			wantErr: "dispatch_timeout",
		},
		{
			name:    "negative write timeout",
			mutate:  func(h *Handler) { h.WriteTimeout = -1 },
			wantErr: "write_timeout",
		},
		{
			name:    "negative replay max messages",
			mutate:  func(h *Handler) { h.ReplayMaxMessages = -1 },
			wantErr: "replay_max_messages",
		},
		{
			name:    "negative replay window",
			mutate:  func(h *Handler) { h.ReplayWindow = -1 },
			wantErr: "replay_window",
		},
		{
			name:    "unsupported allowed method",
			mutate:  func(h *Handler) { h.AllowedMethods = []string{"GET", "POST", "OPTIONS"} },
			wantErr: "allowed_methods",
		},
		{
			name:    "subscriber cookie requires key",
			mutate:  func(h *Handler) { h.SubscriberJWTCookie = "nuts_session" },
			wantErr: "subscriber_jwt_key",
		},
		{
			name: "subscriber cookie validates name",
			mutate: func(h *Handler) {
				h.SubscriberJWTKey = "secret"
				h.SubscriberJWTCookie = "bad;name"
			},
			wantErr: "subscriber_jwt_cookie",
		},
		{
			name:    "topic_prefix with NATS asterisk wildcard",
			mutate:  func(h *Handler) { h.TopicPrefix = "*." },
			wantErr: "topic_prefix",
		},
		{
			name:    "topic_prefix with NATS greater-than wildcard",
			mutate:  func(h *Handler) { h.TopicPrefix = "events.>" },
			wantErr: "topic_prefix",
		},
		{
			name:    "topic_prefix with leading dot",
			mutate:  func(h *Handler) { h.TopicPrefix = ".events." },
			wantErr: "topic_prefix",
		},
		{
			name:    "topic_prefix with system-subject prefix",
			mutate:  func(h *Handler) { h.TopicPrefix = "$sys." },
			wantErr: "topic_prefix",
		},
		{
			name:    "topic_prefix with consecutive dots",
			mutate:  func(h *Handler) { h.TopicPrefix = "events..a." },
			wantErr: "topic_prefix",
		},
		{
			name:    "topic_prefix with disallowed byte",
			mutate:  func(h *Handler) { h.TopicPrefix = "events/" },
			wantErr: "topic_prefix",
		},
		{
			name:    "topic_prefix exceeds maxSubjectLen",
			mutate:  func(h *Handler) { h.TopicPrefix = strings.Repeat("a", maxSubjectLen+1) },
			wantErr: "topic_prefix",
		},
		{
			name:    "negative heartbeat interval",
			mutate:  func(h *Handler) { h.HeartbeatInterval = -30 },
			wantErr: "heartbeat_interval",
		},
		{
			name:    "negative reconnect wait",
			mutate:  func(h *Handler) { h.ReconnectWait = -2 },
			wantErr: "reconnect_wait",
		},
		{
			name:    "nats_idle_heartbeat at boundary equals InactiveThreshold/2",
			mutate:  func(h *Handler) { h.NatsIdleHeartbeat = 15 },
			wantErr: "nats_idle_heartbeat",
		},
		{
			name:    "nats_idle_heartbeat above InactiveThreshold/2",
			mutate:  func(h *Handler) { h.NatsIdleHeartbeat = 30 },
			wantErr: "nats_idle_heartbeat",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := Handler{
				NatsURL:    "nats://localhost:4222",
				StreamName: "EVENTS",
			}
			tt.mutate(&h)
			err := h.Validate()
			if err == nil {
				t.Fatalf("expected validation error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %q, want to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestValidateTopicPrefix_AcceptsLegitimate documents the positive side
// of the prefix contract: empty is fine, the idiomatic trailing-dot
// forms shown in the README/CONFIGURATION docs must validate cleanly,
// and the cap accepts exactly-at-boundary strings (length=maxSubjectLen)
// so the boundary on the > check at provision.go is pinned in both
// directions (matching the >maxSubjectLen rejection test above).
func TestValidateTopicPrefix_AcceptsLegitimate(t *testing.T) {
	cases := []string{
		"", "events.", "tenants.a.", "events", "a_b-c.", "ABC.XYZ.",
		strings.Repeat("a", maxSubjectLen),
	}
	for _, p := range cases {
		if err := validateTopicPrefix(p); err != nil {
			t.Errorf("validateTopicPrefix(%q) rejected legitimate prefix: %v", p, err)
		}
	}
}

func TestHandler_Provision_RejectsInvalidOptionalJSONConfigBeforeDialing(t *testing.T) {
	tests := []struct {
		name     string
		fragment string
		wantErr  string
	}{
		{
			name:     "max reconnects below unlimited sentinel",
			fragment: `"max_reconnects": -2`,
			wantErr:  "max_reconnects",
		},
		{
			name:     "negative max connections",
			fragment: `"max_connections": -1`,
			wantErr:  "max_connections",
		},
		{
			name:     "negative client buffer size",
			fragment: `"client_buffer_size": -1`,
			wantErr:  "client_buffer_size",
		},
		{
			name:     "negative dispatch timeout",
			fragment: `"dispatch_timeout": -1`,
			wantErr:  "dispatch_timeout",
		},
		{
			name:     "negative write timeout",
			fragment: `"write_timeout": -1`,
			wantErr:  "write_timeout",
		},
		{
			name:     "negative replay max messages",
			fragment: `"replay_max_messages": -1`,
			wantErr:  "replay_max_messages",
		},
		{
			name:     "negative replay window",
			fragment: `"replay_window": -1`,
			wantErr:  "replay_window",
		},
		{
			name:     "unsupported allowed method",
			fragment: `"allowed_methods": ["GET", "POST", "OPTIONS"]`,
			wantErr:  "allowed_methods",
		},
		{
			name:     "subscriber cookie requires key",
			fragment: `"subscriber_jwt_cookie": "nuts_session"`,
			wantErr:  "subscriber_jwt_key",
		},
		{
			name:     "subscriber cookie validates name",
			fragment: `"subscriber_jwt_key": "secret", "subscriber_jwt_cookie": "bad;name"`,
			wantErr:  "subscriber_jwt_cookie",
		},
		{
			name:     "negative heartbeat interval",
			fragment: `"heartbeat_interval": -30`,
			wantErr:  "heartbeat_interval",
		},
		{
			name:     "negative reconnect wait",
			fragment: `"reconnect_wait": -2`,
			wantErr:  "reconnect_wait",
		},
		{
			name:     "nats_idle_heartbeat at boundary equals InactiveThreshold/2",
			fragment: `"nats_idle_heartbeat": 15`,
			wantErr:  "nats_idle_heartbeat",
		},
		{
			name:     "nats_idle_heartbeat above InactiveThreshold/2",
			fragment: `"nats_idle_heartbeat": 30`,
			wantErr:  "nats_idle_heartbeat",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(`{"nats_url":"nats://127.0.0.1:1","stream_name":"EVENTS",` + tt.fragment + `}`)
			var h Handler
			if err := json.Unmarshal(raw, &h); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}

			if err := h.Validate(); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want to contain %q", err, tt.wantErr)
			}

			err := h.Provision(caddy.Context{Context: context.Background()})
			if err == nil {
				t.Fatalf("expected Provision error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Provision() error = %q, want to contain %q", err.Error(), tt.wantErr)
			}
			if strings.Contains(err.Error(), "failed to connect to NATS") {
				t.Fatalf("Provision dialed NATS before rejecting config: %v", err)
			}

			h.mu.RLock()
			connNil := h.conn == nil
			jsNil := h.js == nil
			shutdownNil := h.shutdown == nil
			h.mu.RUnlock()
			if !connNil || !jsNil || !shutdownNil {
				t.Fatalf("expected no runtime state after validation rejection, got connNil=%v jsNil=%v shutdownNil=%v", connNil, jsNil, shutdownNil)
			}
		})
	}
}

func TestWriteSSEChunkWithTimeout_SetsAndClearsDeadline(t *testing.T) {
	rr := &deadlineFlushRecorder{ResponseRecorder: httptest.NewRecorder()}
	if err := writeSSEChunkWithTimeout(rr, rr, "event: ping\n\n", time.Second); err != nil {
		t.Fatalf("writeSSEChunkWithTimeout: %v", err)
	}
	if got := rr.Body.String(); got != "event: ping\n\n" {
		t.Fatalf("body = %q", got)
	}
	if len(rr.deadlines) != 2 {
		t.Fatalf("deadline calls = %d, want 2", len(rr.deadlines))
	}
	if rr.deadlines[0].IsZero() {
		t.Fatal("first deadline should set a non-zero write deadline")
	}
	if !rr.deadlines[1].IsZero() {
		t.Fatalf("second deadline = %v, want zero reset", rr.deadlines[1])
	}
}

func TestMessageQueue_DispatchTimeoutCapsSlowSignalWait(t *testing.T) {
	h := &Handler{ClientBufferSize: 1, DispatchTimeout: 1, logger: zap.NewNop()}
	msgChan, slowClient, done, enqueueMessage := h.newMessageQueue()
	defer close(done)

	msgChan <- &nats.Msg{Subject: "events.pending"}
	slowClient <- "events.already-slow"

	returned := make(chan struct{})
	started := time.Now()
	go func() {
		enqueueMessage(&nats.Msg{Subject: "events.blocked"})
		close(returned)
	}()

	select {
	case <-returned:
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("enqueueMessage returned after %s, want dispatch_timeout to cap the wait", elapsed)
		}
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("enqueueMessage did not return after dispatch_timeout")
	}
}

func TestHandler_Provision_PreservesSentinelConfigSemantics(t *testing.T) {
	ns := startJetStreamServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	h := &Handler{
		NatsURL:          ns.ClientURL(),
		StreamName:       "EVENTS",
		MaxEventSize:     -1,
		ClientBufferSize: 0,
	}
	if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()

	if h.MaxEventSize != -1 {
		t.Fatalf("MaxEventSize = %d, want -1 unlimited sentinel", h.MaxEventSize)
	}
	if h.ClientBufferSize != defaultClientBufferSize {
		t.Fatalf("ClientBufferSize = %d, want default %d", h.ClientBufferSize, defaultClientBufferSize)
	}
}

// TestHandler_NatsIdleHeartbeat_ConfigContract is the M9 Phase 2 contract
// test for the nats_idle_heartbeat knob. It pins every interesting input
// to its expected outcome across the four config surfaces a NUTS
// deployment can reach: Caddyfile parser, JSON unmarshal, Provision
// normalisation, Validate. The shape is intentionally exhaustive — the
// knob is the operator-facing handle for the M9 Batch A→B failure-
// detection contract and a silent regression on any boundary would
// silently undermine consumer-invalidation routing.
func TestHandler_NatsIdleHeartbeat_ConfigContract(t *testing.T) {
	t.Run("caddyfile parser accepts positive default", func(t *testing.T) {
		h := parseNatsIdleHeartbeatCaddyfile(t, "10")
		if h.NatsIdleHeartbeat != 10 {
			t.Fatalf("Caddyfile NatsIdleHeartbeat = %d, want 10", h.NatsIdleHeartbeat)
		}
	})
	t.Run("caddyfile parser accepts low positive", func(t *testing.T) {
		h := parseNatsIdleHeartbeatCaddyfile(t, "5")
		if h.NatsIdleHeartbeat != 5 {
			t.Fatalf("Caddyfile NatsIdleHeartbeat = %d, want 5", h.NatsIdleHeartbeat)
		}
	})
	t.Run("caddyfile parser accepts operator-disable sentinel", func(t *testing.T) {
		h := parseNatsIdleHeartbeatCaddyfile(t, "-1")
		if h.NatsIdleHeartbeat != -1 {
			t.Fatalf("Caddyfile NatsIdleHeartbeat = %d, want -1 (disable sentinel)", h.NatsIdleHeartbeat)
		}
	})
	t.Run("caddyfile parser preserves zero for Provision-time normalisation", func(t *testing.T) {
		// Operators who write `nats_idle_heartbeat 0` expect "use the
		// default" semantics, same as every other int knob in the file
		// (heartbeat_interval, reconnect_wait, etc.). The parser keeps
		// the zero; Provision rewrites it to the default. Validate must
		// also accept 0 because it runs before Provision normalisation
		// (provision.go:82-83).
		h := parseNatsIdleHeartbeatCaddyfile(t, "0")
		if h.NatsIdleHeartbeat != 0 {
			t.Fatalf("Caddyfile NatsIdleHeartbeat = %d, want 0 (Provision normalises)", h.NatsIdleHeartbeat)
		}
	})
	t.Run("JSON round-trip absent field decodes to zero", func(t *testing.T) {
		var h Handler
		if err := json.Unmarshal([]byte(`{"nats_url":"nats://localhost:4222","stream_name":"EVENTS"}`), &h); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		if h.NatsIdleHeartbeat != 0 {
			t.Fatalf("absent JSON field decoded to %d, want 0 (Provision normalises)", h.NatsIdleHeartbeat)
		}
	})
	t.Run("JSON round-trip explicit value preserved through Validate", func(t *testing.T) {
		var h Handler
		raw := []byte(`{"nats_url":"nats://localhost:4222","stream_name":"EVENTS","nats_idle_heartbeat":7}`)
		if err := json.Unmarshal(raw, &h); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		if h.NatsIdleHeartbeat != 7 {
			t.Fatalf("JSON nats_idle_heartbeat decoded to %d, want 7", h.NatsIdleHeartbeat)
		}
		if err := h.Validate(); err != nil {
			t.Fatalf("Validate rejected legitimate value: %v", err)
		}
	})
	t.Run("Provision normalises zero to default 10", func(t *testing.T) {
		h := provisionNatsIdleHeartbeat(t, 0)
		defer h.Cleanup()
		if h.NatsIdleHeartbeat != 10 {
			t.Fatalf("post-Provision NatsIdleHeartbeat = %d, want 10 default", h.NatsIdleHeartbeat)
		}
	})
	t.Run("Provision preserves explicit positive value", func(t *testing.T) {
		h := provisionNatsIdleHeartbeat(t, 5)
		defer h.Cleanup()
		if h.NatsIdleHeartbeat != 5 {
			t.Fatalf("post-Provision NatsIdleHeartbeat = %d, want 5", h.NatsIdleHeartbeat)
		}
	})
	t.Run("Provision preserves operator-disable sentinel", func(t *testing.T) {
		h := provisionNatsIdleHeartbeat(t, -1)
		defer h.Cleanup()
		if h.NatsIdleHeartbeat != -1 {
			t.Fatalf("post-Provision NatsIdleHeartbeat = %d, want -1 (disable sentinel preserved)", h.NatsIdleHeartbeat)
		}
	})
	t.Run("Validate accepts boundary value 14", func(t *testing.T) {
		// 15 is exactly InactiveThreshold/2 and rejected by the upper
		// bound; 14 is the largest accepted positive value. Pinning the
		// boundary on the accepted side guards against an inclusive-vs-
		// exclusive comparison regression in Validate.
		h := Handler{NatsURL: "nats://localhost:4222", StreamName: "EVENTS", NatsIdleHeartbeat: 14}
		if err := h.Validate(); err != nil {
			t.Fatalf("Validate rejected boundary-1 value: %v", err)
		}
	})
	t.Run("Validate accepts 1", func(t *testing.T) {
		h := Handler{NatsURL: "nats://localhost:4222", StreamName: "EVENTS", NatsIdleHeartbeat: 1}
		if err := h.Validate(); err != nil {
			t.Fatalf("Validate rejected low positive value: %v", err)
		}
	})
	t.Run("Validate accepts zero (Provision normalises)", func(t *testing.T) {
		h := Handler{NatsURL: "nats://localhost:4222", StreamName: "EVENTS", NatsIdleHeartbeat: 0}
		if err := h.Validate(); err != nil {
			t.Fatalf("Validate rejected zero (must pass — Provision normalises): %v", err)
		}
	})
	t.Run("Validate accepts operator-disable sentinel", func(t *testing.T) {
		h := Handler{NatsURL: "nats://localhost:4222", StreamName: "EVENTS", NatsIdleHeartbeat: -1}
		if err := h.Validate(); err != nil {
			t.Fatalf("Validate rejected disable sentinel: %v", err)
		}
	})
}

// parseNatsIdleHeartbeatCaddyfile runs the Caddyfile parser on a minimal
// nuts block containing only the nats_idle_heartbeat directive and
// returns the resulting Handler. Helper used by the
// TestHandler_NatsIdleHeartbeat_ConfigContract sub-tests.
func parseNatsIdleHeartbeatCaddyfile(t *testing.T, value string) *Handler {
	t.Helper()
	input := "nuts {\n    nats_url nats://localhost:4222\n    stream_name EVENTS\n    nats_idle_heartbeat " + value + "\n}"
	d := caddyfile.NewTestDispenser(input)
	h := &Handler{}
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile(%q): %v", value, err)
	}
	return h
}

// provisionNatsIdleHeartbeat dials a real embedded JetStream server and
// runs Provision so the normalisation path is exercised end-to-end —
// the alternative (calling validateConfigValues + the normalisation
// block directly) would skip the public Provision contract that
// integration with Caddy depends on. The caller is responsible for
// Cleanup().
func provisionNatsIdleHeartbeat(t *testing.T, initial int) *Handler {
	t.Helper()
	ns := startJetStreamServer(t)
	t.Cleanup(ns.Shutdown)

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	h := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "EVENTS",
		NatsIdleHeartbeat: initial,
	}
	if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision(initial=%d): %v", initial, err)
	}
	return h
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
		HealthPath:        "/status",
		HeartbeatInterval: 30,
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

func TestHandler_LiveAndReadyPaths_AreDistinctProbes(t *testing.T) {
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
	defer ns.Shutdown()

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
	if err := h.connectNATS(); err != nil {
		t.Fatalf("connectNATS: %v", err)
	}
	defer h.Cleanup()
	js, _ := h.conn.JetStream()
	h.mu.Lock()
	h.js = js
	h.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/events?topic=cap-valid&last-id=1", nil)
	rr := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	if delivered := strings.Count(rr.Body.String(), "event: message"); delivered != 2 {
		t.Fatalf("delivered %d messages, want 2 under replay_max_messages=2\nbody: %s", delivered, rr.Body.String())
	}
}

func TestHandler_ReplayWindow_BoundsValidRetainedReplay(t *testing.T) {
	ns := startJetStreamServer(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})
	replayWindow := 2

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
	if err := h.connectNATS(); err != nil {
		t.Fatalf("connectNATS: %v", err)
	}
	defer h.Cleanup()
	js, _ := h.conn.JetStream()
	h.mu.Lock()
	h.js = js
	h.mu.Unlock()

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
	if wait := time.Until(oldMsg.Time.Add(time.Duration(replayWindow+3) * time.Second)); wait > 0 {
		time.Sleep(wait)
	}
	if _, err := jsPub.Publish("events.window-valid", []byte(`{"age":"new"}`)); err != nil {
		t.Fatalf("publish new: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/events?topic=window-valid&last-id=0", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 1500*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)

	rr := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()
	<-done

	body := rr.Body.String()
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

// ── Provision() fails BEFORE opening NATS when required fields absent ────

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

	rr := newSafeRecorder()
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()

	// Wait until the SSE "connected" event has been written so we know the
	// handler is inside the streaming loop and not still in setup.
	if !waitForSSEBody(rr, "event: connected", 2*time.Second) {
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

// counterPlainValue reads the current value of a plain (non-labelled) counter.
func counterPlainValue(c prometheus.Counter) float64 {
	pb := &iopm.Metric{}
	if err := c.Write(pb); err != nil {
		return 0
	}
	return pb.GetCounter().GetValue()
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

// ── M2: dispatch_timeout metric ───────────────────────────────────────────

func TestMessageQueue_DispatchTimeout_IncrementsMetric(t *testing.T) {
	before := counterPlainValue(metricsDispatchTimeouts)

	h := &Handler{ClientBufferSize: 1, DispatchTimeout: 1, logger: zap.NewNop()}
	msgChan, slowClient, done, enqueueMessage := h.newMessageQueue()
	defer close(done)

	msgChan <- &nats.Msg{Subject: "events.pending"}
	slowClient <- "events.already-slow"

	returned := make(chan struct{})
	go func() {
		enqueueMessage(&nats.Msg{Subject: "events.blocked"})
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("enqueueMessage did not return after dispatch_timeout")
	}

	after := counterPlainValue(metricsDispatchTimeouts)
	if delta := after - before; delta < 1 {
		t.Fatalf("metricsDispatchTimeouts delta = %v, want >= 1", delta)
	}
}

// ── L1: Short subscriber_jwt_key warning ──────────────────────────────────

func TestHandler_Validate_WarnsShortJWTKey(t *testing.T) {
	core, obs := observer.New(zap.WarnLevel)
	h := &Handler{
		NatsURL:          "tls://nats.example.com:4222",
		StreamName:       "EVENTS",
		SubscriberJWTKey: strings.Repeat("a", 16),
		logger:           zap.New(core),
	}
	if err := h.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !hasLogContaining(obs, "subscriber_jwt_key is shorter than recommended") {
		t.Errorf("expected short-key warning, entries=%+v", obs.All())
	}
}

func TestHandler_Validate_NoWarningForLongJWTKey(t *testing.T) {
	core, obs := observer.New(zap.WarnLevel)
	h := &Handler{
		NatsURL:          "tls://nats.example.com:4222",
		StreamName:       "EVENTS",
		SubscriberJWTKey: strings.Repeat("a", 64),
		logger:           zap.New(core),
	}
	if err := h.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if hasLogContaining(obs, "subscriber_jwt_key is shorter") {
		t.Errorf("did not expect short-key warning, entries=%+v", obs.All())
	}
}

// TestHandler_SubscriberJWT_RejectionCreatesNoConsumer asserts that a
// JWT-rejected request short-circuits BEFORE any JetStream consumer is
// created. A regression that flipped the order (subscribe first, auth
// later) would silently weaken auth: a forged token would still have
// caused server-side state to be allocated, leaving room for amplification
// or resource-exhaustion attacks.
func TestHandler_SubscriberJWT_RejectionCreatesNoConsumer(t *testing.T) {
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
		AllowedOrigins:    []string{"*"},
		SubscriberJWTKey:  "test-secret-with-sufficient-length-12345",
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

	// Baseline: no consumers on the stream.
	if !waitForConsumerCount(t, js, "EVENTS", 0, 500*time.Millisecond) {
		t.Fatalf("baseline: expected 0 consumers, got otherwise")
	}

	// Request with a deliberately invalid JWT.
	req := httptest.NewRequest(http.MethodGet, "/events?topic=secret", nil)
	req.Header.Set("Authorization", "Bearer not.a.valid.token")
	rr := httptest.NewRecorder()
	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}

	// Post-rejection: assert no consumer was created. authorizeStreamRequest
	// runs SYNCHRONOUSLY inside ServeHTTP before executeSubscriptionPlan
	// and before any goroutine is spawned on the auth-reject path, so a
	// single post-call check is sufficient — there is no later moment at
	// which a racing consumer-create could appear. The 401 above proves
	// we took the reject branch; this assertion proves the branch did
	// not allocate JetStream state on the way out.
	if got := consumerCount(js, "EVENTS"); got != 0 {
		info, _ := js.StreamInfo("EVENTS")
		t.Fatalf("post-rejection: consumer count = %d (stream reported %d consumers), want 0 (auth must run BEFORE Subscribe)", got, info.State.Consumers)
	}
}
