// Security-hardening and config-correctness tests for the NUTS handler.
package nuts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
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
		MaxEventSize:      150, // drops the 512-byte payload, keeps a ~100-byte frame
		AllowedOrigins:    []string{"*"},
		logger:            zap.New(core),
	}
	if err := h.connectNATS(); err != nil {
		t.Fatalf("connectNATS: %v", err)
	}
	defer h.Cleanup()
	js, _ := jetstream.New(h.conn)
	h.mu.Lock()
	h.js = js
	h.mu.Unlock()

	jsPub, _ := nc.JetStream()
	// Binary blob: not valid JSON and definitely larger than the cap.
	big := strings.Repeat("Z", 512)
	if _, err := jsPub.Publish("events.raw", []byte(big)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// A small message after it proves the stream kept going past the drop.
	if _, err := jsPub.Publish("events.raw", []byte(`{"after":1}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	droppedBefore := counterValue(metricsMessagesDropped, dropReasonRawPayload)

	rr, cancel, done := startSSE(t, h, "/events?topic=raw&last-id=0", "")
	if !waitForSSEBody(rr, `{"after":1}`, 3*time.Second) {
		t.Fatalf("message after the oversized one not delivered; body=%q", rr.Body())
	}
	stopSSE(t, cancel, done)

	if strings.Contains(rr.Body(), big) {
		t.Errorf("oversized raw payload leaked into response body")
	}
	if got := counterValue(metricsMessagesDropped, dropReasonRawPayload); got != droppedBefore+1 {
		t.Errorf("messages_dropped_total{raw_payload} = %v, want %v", got, droppedBefore+1)
	}
	if obs.FilterMessage("dropping oversized NATS payload").Len() != 1 {
		t.Errorf("missing the oversized-payload warning: %+v", obs.All())
	}
}

// ── max_connections cap ───────────────────────────────────────────────────

func TestHandler_MaxConnections_RejectsExcess(t *testing.T) {
	maxConnCore, maxConnLogs := observer.New(zap.WarnLevel)
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
		AllowedOrigins:    []string{"https://app.example.com"},
		ClientBufferSize:  64,
		logger:            zap.New(maxConnCore),
	}
	if err := h.connectNATS(); err != nil {
		t.Fatalf("connectNATS: %v", err)
	}
	defer h.Cleanup()
	js, _ := jetstream.New(h.conn)
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

	if got := counterValue(metricsConnectionsRejected, "max_connections"); got != before+2 { // the plain and the EventSource request
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

// TestHandler_WarnAboutTransportSecurity covers which settings count as
// sending credentials unencrypted (#77): every plaintext scheme, servers
// without a scheme, credentials embedded in the URL, and the nats_tls_*
// directives that make nats.go use TLS on nats:// and ws:// too.
func TestHandler_WarnAboutTransportSecurity(t *testing.T) {
	cases := []struct {
		name         string
		h            *Handler
		wantCleartxt bool
		wantInsecure bool
	}{
		{name: "token over nats://", h: &Handler{NatsURL: "nats://nats.example.com:4222", NatsToken: "secret-token"}, wantCleartxt: true},
		{name: "creds file over nats://", h: &Handler{NatsURL: "nats://nats.example.com:4222", NatsCredentials: "/etc/nats/user.creds"}, wantCleartxt: true},
		{name: "user and password over ws://", h: &Handler{NatsURL: "ws://nats.example.com:8080", NatsUser: "u", NatsPassword: "p"}, wantCleartxt: true},
		{name: "token to a server without a scheme", h: &Handler{NatsURL: "nats.example.com:4222", NatsToken: "t"}, wantCleartxt: true},
		{name: "credentials embedded in the URL", h: &Handler{NatsURL: "nats://user:secret@nats.example.com:4222"}, wantCleartxt: true},
		{name: "one plaintext server in a list", h: &Handler{NatsURL: "tls://a:4222,nats://b:4222", NatsToken: "t"}, wantCleartxt: true},
		{name: "token over tls://", h: &Handler{NatsURL: "tls://nats.example.com:4222", NatsToken: "t"}},
		{name: "token over wss://", h: &Handler{NatsURL: "wss://nats.example.com:443", NatsToken: "t"}},
		{name: "token over nats:// with a CA bundle", h: &Handler{NatsURL: "nats://nats.example.com:4222", NatsToken: "t", NatsTLSCA: "/etc/ca.pem"}},
		{name: "token over ws:// with a client certificate", h: &Handler{NatsURL: "ws://nats.example.com:8080", NatsToken: "t", NatsTLSCert: "/c.pem", NatsTLSKey: "/k.pem"}},
		{name: "no credentials over nats://", h: &Handler{NatsURL: "nats://nats.example.com:4222"}},
		{name: "insecure skip verify", h: &Handler{NatsURL: "tls://nats.example.com:4222", NatsTLSInsecureSkipVerify: true}, wantInsecure: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			core, obs := observer.New(zap.WarnLevel)
			h := c.h
			h.logger = zap.New(core)
			h.warnAboutTransportSecurity()
			if got := hasLogContaining(obs, "sent unencrypted"); got != c.wantCleartxt {
				t.Fatalf("cleartext warning = %v, want %v: %v", got, c.wantCleartxt, obs.All())
			}
			if got := hasLogContaining(obs, "insecure_skip_verify"); got != c.wantInsecure {
				t.Fatalf("insecure warning = %v, want %v: %v", got, c.wantInsecure, obs.All())
			}
			if strings.Contains(fmt.Sprint(obs.All()), "secret") {
				t.Fatalf("a warning leaked a credential: %v", obs.All())
			}
		})
	}
}

// TestHandler_Provision_WarnsBeforeDialling covers #82: the transport
// warnings used to come from Validate, which Caddy runs after Provision has
// already sent the credentials and the first JetStream requests.
func TestHandler_Provision_WarnsBeforeDialling(t *testing.T) {
	core, obs := observer.New(zap.WarnLevel)
	h := &Handler{
		NatsURL:                   "tls://127.0.0.1:1",
		StreamName:                "EVENTS",
		NatsToken:                 "t",
		NatsTLSInsecureSkipVerify: true,
		logger:                    zap.New(core),
	}
	if err := h.Provision(caddy.Context{Context: context.Background()}); err == nil {
		t.Fatal("Provision against a closed port succeeded")
	}
	if !hasLogContaining(obs, "insecure_skip_verify") {
		t.Fatalf("no insecure_skip_verify warning although the dial failed: %v", obs.All())
	}
	if err := h.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if n := obs.FilterMessageSnippet("insecure_skip_verify").Len(); n != 1 {
		t.Fatalf("insecure_skip_verify warned %d times, want once (Validate must not repeat it)", n)
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

	// The explicit 0 must survive Provision and reach the connection.
	ns := startJetStreamServer(t)
	defer ns.Shutdown()
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})
	h.NatsURL = ns.ClientURL()
	h.logger = zap.NewNop()
	if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()
	if got := h.conn.Opts.MaxReconnect; got != 0 {
		t.Fatalf("connection MaxReconnects = %d after Provision, want 0", got)
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
			name:    "write timeout below the -1 disable sentinel",
			line:    "write_timeout -2",
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
		{
			name:    "negative heartbeat interval at parse time",
			line:    "heartbeat_interval -30",
			wantErr: "heartbeat_interval must be >= 0",
		},
		{
			name:    "negative reconnect wait at parse time",
			line:    "reconnect_wait -1",
			wantErr: "reconnect_wait must be >= 0",
		},
		{
			name:    "topic cap below the -1 sentinel",
			line:    "max_topics_per_subscription -2",
			wantErr: "max_topics_per_subscription",
		},
		{
			name:        "CA bundle with verification disabled",
			line:        "nats_tls_ca /etc/nats/ca.pem\n    nats_tls_insecure_skip_verify",
			wantErr:     "nats_tls_ca cannot be combined with nats_tls_insecure_skip_verify",
			validateErr: true,
		},
		{
			name:        "unsupported nats_url scheme",
			line:        "nats_url http://localhost:4222",
			wantErr:     `nats_url scheme "http" is not supported`,
			validateErr: true,
		},
		{
			name:        "comma-joined allowed origins",
			line:        "allowed_origins https://a.example.com,https://b.example.com",
			wantErr:     "allowed_origins",
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
			name:    "write timeout below the -1 disable sentinel",
			mutate:  func(h *Handler) { h.WriteTimeout = -2 },
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
		{
			name:    "nats_idle_heartbeat negative typo just below sentinel",
			mutate:  func(h *Handler) { h.NatsIdleHeartbeat = -2 },
			wantErr: "nats_idle_heartbeat",
		},
		{
			name:    "nats_idle_heartbeat large-magnitude negative typo",
			mutate:  func(h *Handler) { h.NatsIdleHeartbeat = -100 },
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
			name:     "write timeout below the -1 disable sentinel",
			fragment: `"write_timeout": -2`,
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
		{
			name:     "nats_idle_heartbeat negative typo just below sentinel",
			fragment: `"nats_idle_heartbeat": -2`,
			wantErr:  "nats_idle_heartbeat",
		},
		{
			name:     "nats_idle_heartbeat large-magnitude negative typo",
			fragment: `"nats_idle_heartbeat": -100`,
			wantErr:  "nats_idle_heartbeat",
		},
		{
			name:     "topic cap below the -1 sentinel",
			fragment: `"max_topics_per_subscription": -2`,
			wantErr:  "max_topics_per_subscription",
		},
		{
			name:     "CA bundle with verification disabled",
			fragment: `"nats_tls_ca": "/etc/nats/ca.pem", "nats_tls_insecure_skip_verify": true`,
			wantErr:  "nats_tls_ca",
		},
		{
			name:     "mistyped nats_url scheme",
			fragment: `"nats_url": "tsl://127.0.0.1:1"`,
			wantErr:  "nats_url",
		},
		{
			name:     "allowed origin with a path",
			fragment: `"allowed_origins": ["https://app.example.com/"]`,
			wantErr:  "allowed_origins",
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

// TestWriteSSEChunkWithTimeout_DisabledSetsNoDeadline pins write_timeout -1:
// a negative timeout writes without touching the connection deadline.
func TestWriteSSEChunkWithTimeout_DisabledSetsNoDeadline(t *testing.T) {
	rr := &deadlineFlushRecorder{ResponseRecorder: httptest.NewRecorder()}
	timeout := time.Duration(writeTimeoutDisabledSentinel) * time.Second
	if err := writeSSEChunkWithTimeout(rr, http.NewResponseController(rr), "event: ping\n\n", timeout); err != nil {
		t.Fatalf("writeSSEChunkWithTimeout: %v", err)
	}
	if len(rr.deadlines) != 0 {
		t.Fatalf("deadline calls = %d, want 0 with write_timeout disabled", len(rr.deadlines))
	}
	if got := rr.Body.String(); got != "event: ping\n\n" {
		t.Fatalf("body = %q", got)
	}
}

func TestWriteSSEChunkWithTimeout_SetsAndClearsDeadline(t *testing.T) {
	rr := &deadlineFlushRecorder{ResponseRecorder: httptest.NewRecorder()}
	if err := writeSSEChunkWithTimeout(rr, http.NewResponseController(rr), "event: ping\n\n", time.Second); err != nil {
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
	t.Run("Validate rejects non-sentinel negatives as typo guard", func(t *testing.T) {
		// Sibling fields (heartbeat_interval, reconnect_wait) reject
		// negatives unconditionally; nats_idle_heartbeat accepts exactly
		// -1 (the explicit operator-disable sentinel) and rejects every
		// other negative as a likely typo. Pin both ends of the -1
		// boundary to catch a future regression that loosens the check
		// (e.g. "any negative disables") and re-enables silent typo
		// configurations.
		for _, v := range []int{-2, -10, -100} {
			h := Handler{NatsURL: "nats://localhost:4222", StreamName: "EVENTS", NatsIdleHeartbeat: v}
			err := h.Validate()
			if err == nil {
				t.Fatalf("Validate accepted nats_idle_heartbeat=%d, want rejection (only -1 is the disable sentinel)", v)
			}
			if !strings.Contains(err.Error(), "nats_idle_heartbeat") {
				t.Fatalf("Validate error for nats_idle_heartbeat=%d does not mention the field: %v", v, err)
			}
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
	js, _ := jetstream.New(h.conn)
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

// TestHandler_ProbesAcceptATrailingSlash covers #83: "/livez/" used to fall
// through to topic parsing and answer 503 "Failed to subscribe" for topic
// "livez", so a kubelet probe with a trailing slash restarted the pod.
func TestHandler_ProbesAcceptATrailingSlash(t *testing.T) {
	ns := startJetStreamServer(t)
	defer ns.Shutdown()

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
	js, _ := jetstream.New(h.conn)
	h.mu.Lock()
	h.js = js
	h.mu.Unlock()

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
	js, _ := jetstream.New(h.conn)
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
	js, _ := jetstream.New(h.conn)
	h.mu.Lock()
	h.js = js
	h.mu.Unlock()

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
	defer ns.Shutdown()

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
	if err := h.connectNATS(); err != nil {
		t.Fatalf("connectNATS: %v", err)
	}
	defer h.Cleanup()
	js, _ := jetstream.New(h.conn)
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
	defer ns.Shutdown()

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
	if err := h.connectNATS(); err != nil {
		t.Fatalf("connectNATS: %v", err)
	}
	defer h.Cleanup()
	js, _ := jetstream.New(h.conn)
	h.mu.Lock()
	h.js = js
	h.mu.Unlock()

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

// ── Provision() fails BEFORE opening NATS when required fields absent ────

func TestHandler_Provision_RejectsBeforeDialing(t *testing.T) {
	// Point at a guaranteed-closed port so any actual dial attempt would fail
	// with a connection error — but we expect the missing-field error instead.
	for _, c := range []struct {
		name    string
		h       *Handler
		wantErr string
	}{
		{name: "missing nats_url", h: &Handler{StreamName: "EVENTS"}, wantErr: "nats_url is required"},
		{name: "missing stream_name", h: &Handler{NatsURL: "nats://127.0.0.1:1"}, wantErr: "stream_name is required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.h.logger = zap.NewNop()
			err := c.h.Provision(caddy.Context{Context: context.Background()})
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("Provision error = %v, want %q before any dial", err, c.wantErr)
			}
			if c.h.conn != nil {
				t.Fatal("Provision opened a connection despite invalid config")
			}
		})
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
	}
	// The real Provision must create the shutdown channel; hand-building it
	// here would hide a Provision regression.
	if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	h.logger = zap.NewNop()

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
	js, _ := jetstream.New(h.conn)
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
		t.Fatalf("post-rejection: consumer count = %d, want 0 (auth must run before the consumer is created)", got)
	}
}

// TestHandler_Validate_WarnsAboutIneffectiveSettings covers the warnings for
// settings the pull consumer made meaningless: dispatch_timeout, and
// nats_idle_heartbeat -1 (heartbeats can no longer be turned off).
func TestHandler_Validate_WarnsAboutIneffectiveSettings(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*Handler)
		wantWarn string
	}{
		{name: "dispatch_timeout set", mutate: func(h *Handler) { h.DispatchTimeout = 5 }, wantWarn: "dispatch_timeout is deprecated"},
		{name: "nats_idle_heartbeat disabled", mutate: func(h *Handler) { h.NatsIdleHeartbeat = natsIdleHeartbeatDisabledSentinel }, wantWarn: "nats_idle_heartbeat -1 no longer disables"},
		{name: "defaults stay quiet", mutate: func(*Handler) {}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			core, obs := observer.New(zap.WarnLevel)
			h := &Handler{NatsURL: "tls://127.0.0.1:4222", StreamName: "EVENTS", NatsIdleHeartbeat: 10, AllowedOrigins: []string{"https://app.example"}, logger: zap.New(core)}
			c.mutate(h)
			if err := h.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			var messages []string
			for _, entry := range obs.All() {
				messages = append(messages, entry.Message)
			}
			joined := strings.Join(messages, "\n")
			if c.wantWarn == "" {
				if len(messages) != 0 {
					t.Fatalf("unexpected warnings: %v", messages)
				}
				return
			}
			if !strings.Contains(joined, c.wantWarn) {
				t.Fatalf("warnings %v do not mention %q", messages, c.wantWarn)
			}
		})
	}
}

func TestServerVersionBefore(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"2.10.29", true},
		{"2.12.15", true},
		{"2.14.6", true},
		{"2.14.7", false},
		{"2.14.8", false},
		{"2.15.0", false},
		{"2.15.0-beta.1", false},
		{"2.14.6-RC.2", true},
		{"3.0.0", false},
		{"1.99.99", true},
		{"", false},
		{"2.14", false},
		{"2.x.7", false},
	}
	for _, c := range cases {
		if got := serverVersionBefore(c.version, multiFilterPurgeFixed); got != c.want {
			t.Errorf("serverVersionBefore(%q) = %v, want %v", c.version, got, c.want)
		}
	}
}

func TestHandler_WarnAboutServerVersion(t *testing.T) {
	for version, wantWarn := range map[string]bool{"2.12.15": true, "2.14.7": false, "garbage": false} {
		core, obs := observer.New(zap.WarnLevel)
		h := &Handler{logger: zap.New(core)}
		h.warnAboutServerVersion(version)
		if got := hasLogField(obs, "server_version", version); got != wantWarn {
			t.Errorf("server %q: warned=%v, want %v (%v)", version, got, wantWarn, obs.All())
		}
	}
}

func TestNatsServerSchemes(t *testing.T) {
	cases := []struct {
		raw     string
		want    []string
		wantErr string
	}{
		{raw: "nats://a:4222", want: []string{"nats"}},
		{raw: "tls://a:4222", want: []string{"tls"}},
		{raw: "ws://a:8080", want: []string{"ws"}},
		{raw: "wss://a:443", want: []string{"wss"}},
		{raw: "NATS://a:4222", want: []string{"nats"}},
		{raw: "a:4222", want: []string{"nats"}},
		{raw: "nats://user:pass@a:4222", want: []string{"nats"}},
		{raw: "tls://a:4222, nats://b:4222,c:4222", want: []string{"tls", "nats", "nats"}},
		{raw: "http://a:4222", wantErr: `scheme "http" is not supported`},
		{raw: "tsl://a:4222", wantErr: `scheme "tsl" is not supported`},
		{raw: "nats://", wantErr: "has no host"},
		{raw: "nats://a:4222,,nats://b:4222", wantErr: "empty server entry"},
		{raw: "nats://user:pa ss@a:4222", wantErr: "is not a valid URL"},
	}
	for _, c := range cases {
		got, err := natsServerSchemes(c.raw)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("natsServerSchemes(%q) error = %v, want %q", c.raw, err, c.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "pa ss") {
				t.Errorf("natsServerSchemes(%q) error leaks the password: %v", c.raw, err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("natsServerSchemes(%q) = %v, %v; want %v", c.raw, got, err, c.want)
		}
	}
}

func TestValidateAllowedOrigins(t *testing.T) {
	valid := [][]string{
		nil,
		{"*"},
		{"https://app.example.com"},
		{"http://localhost:3000", "https://app.example.com", "*"},
		{"https://[::1]:8443"},
	}
	for _, origins := range valid {
		if err := validateAllowedOrigins(origins); err != nil {
			t.Errorf("validateAllowedOrigins(%q) = %v, want nil", origins, err)
		}
	}
	invalid := map[string]string{
		"": "empty entry",
		"https://a.example.com,https://b.example.com": "comma or whitespace",
		"https://a.example.com ":                      "comma or whitespace",
		"https://a.example.com\r\n":                   "comma or whitespace",
		"app.example.com":                             "not an origin",
		"https://app.example.com/":                    "not an origin",
		"https://app.example.com/path":                "not an origin",
		"https://app.example.com?x=1":                 "not an origin",
		"https://app.example.com?":                    "not an origin",
		"https://app.example.com#top":                 "not an origin",
		"https://user@app.example.com":                "not an origin",
		"null":                                        "not an origin",
		"mailto:someone@example.com":                  "not an origin",
		"https://App.Example.com":                     "must be lowercase",
	}
	for origin, wantErr := range invalid {
		err := validateAllowedOrigins([]string{"https://ok.example.com", origin})
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("validateAllowedOrigins(%q) = %v, want an error containing %q", origin, err, wantErr)
		}
	}
}

// TestHandler_Provision_LogsDisabledTopicCap covers #81: switching the topic
// cap off is visible in the logs.
func TestHandler_Provision_LogsDisabledTopicCap(t *testing.T) {
	for _, c := range []struct {
		cap     int
		wantLog bool
	}{{-1, true}, {0, false}, {5, false}} {
		core, obs := observer.New(zap.InfoLevel)
		h := &Handler{NatsURL: "nats://127.0.0.1:1", StreamName: "EVENTS", MaxTopicsPerSubscription: c.cap, logger: zap.New(core)}
		_ = h.Provision(caddy.Context{Context: context.Background()}) // the dial fails; the log comes first
		if got := hasLogContaining(obs, "max_topics_per_subscription is -1"); got != c.wantLog {
			t.Errorf("cap %d: logged=%v, want %v", c.cap, got, c.wantLog)
		}
	}
}

func TestHandler_UnmarshalCaddyfile_SharedSubscriptions(t *testing.T) {
	for _, c := range []struct {
		line    string
		want    bool
		wantErr bool
	}{
		{line: "shared_subscriptions", want: true},
		{line: "shared_subscriptions true", want: true},
		{line: "shared_subscriptions false", want: false},
		{line: "shared_subscriptions maybe", wantErr: true},
	} {
		d := caddyfile.NewTestDispenser("nuts {\n    nats_url nats://localhost:4222\n    stream_name EVENTS\n    " + c.line + "\n}")
		h := Handler{}
		err := h.UnmarshalCaddyfile(d)
		if c.wantErr {
			if err == nil || !strings.Contains(err.Error(), "shared_subscriptions") {
				t.Errorf("%q: error = %v, want an invalid shared_subscriptions error", c.line, err)
			}
			continue
		}
		if err != nil || h.SharedSubscriptions != c.want {
			t.Errorf("%q: SharedSubscriptions = %v (err %v), want %v", c.line, h.SharedSubscriptions, err, c.want)
		}
	}
}

// TestHandler_ReadinessProbe_StreamInfoErrorIsCounted covers #65: with NATS
// connected but the stream gone, the probe reports stream_info_error, once.
func TestHandler_ReadinessProbe_StreamInfoErrorIsCounted(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
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
		before[cause] = counterValue(metricsReadinessFailures, cause)
	}

	rr := httptest.NewRecorder()
	if err := h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil), nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), `"stream":"unavailable"`) || !strings.Contains(rr.Body.String(), `"nats":"connected"`) {
		t.Fatalf("probe = %d %s, want 503 with the stream unavailable and NATS connected", rr.Code, rr.Body.String())
	}
	for cause, want := range map[string]float64{"stream_info_error": 1, "nats_disconnected": 0, "jetstream_missing": 0} {
		if got := counterValue(metricsReadinessFailures, cause) - before[cause]; got != want {
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

// TestHandler_SubscriberJWT_CookieConfiguredButMissing covers #87: with the
// cookie transport configured, a request without the cookie, or with an
// empty one, and no Authorization header is refused.
func TestHandler_SubscriberJWT_CookieConfiguredButMissing(t *testing.T) {
	h := &Handler{SubscriberJWTKey: "test-secret", SubscriberJWTCookie: "nuts_session", logger: zap.NewNop()}
	for name, cookie := range map[string]*http.Cookie{
		"no cookie":    nil,
		"empty cookie": {Name: "nuts_session", Value: ""},
		"other cookie": {Name: "unrelated", Value: "x"},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/events?topic=a", nil)
			if cookie != nil {
				req.AddCookie(cookie)
			}
			rr := httptest.NewRecorder()
			if err := h.ServeHTTP(rr, req, nil); err != nil {
				t.Fatalf("ServeHTTP: %v", err)
			}
			if rr.Code != http.StatusUnauthorized || rr.Header().Get("WWW-Authenticate") != `Bearer realm="nuts"` {
				t.Fatalf("response = %d (WWW-Authenticate %q), want 401 with a Bearer challenge", rr.Code, rr.Header().Get("WWW-Authenticate"))
			}
		})
	}
}
