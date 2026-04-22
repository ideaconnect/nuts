// Tests that close coverage gaps identified against the README Features list:
// per-metric counter/gauge assertions, buildTLSConfig, explicit heartbeat
// framing, topic-prefix translation, NATS reconnect → SSE still works, and
// JetStream persistence across handler lifetimes.
package nuts

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	iopm "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
)

// safeFlushRecorder is an http.ResponseWriter + http.Flusher whose body
// buffer is protected by a mutex so tests can poll it from one goroutine
// while the handler writes from another. The standard
// httptest.ResponseRecorder cannot be read concurrently with writes — the
// race detector flags it immediately.
type safeFlushRecorder struct {
	mu         sync.Mutex
	buf        strings.Builder
	header     http.Header
	statusCode int
}

func newSafeRecorder() *safeFlushRecorder {
	return &safeFlushRecorder{header: make(http.Header)}
}

func (s *safeFlushRecorder) Header() http.Header { return s.header }

func (s *safeFlushRecorder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statusCode == 0 {
		s.statusCode = http.StatusOK
	}
	return s.buf.Write(p)
}

func (s *safeFlushRecorder) WriteHeader(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusCode = code
}

func (s *safeFlushRecorder) Flush() {}

func (s *safeFlushRecorder) Body() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// ── metric helpers ────────────────────────────────────────────────────────

func counterVal(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	pb := &iopm.Metric{}
	if err := c.Write(pb); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return pb.GetCounter().GetValue()
}

func gaugeVal(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	pb := &iopm.Metric{}
	if err := g.Write(pb); err != nil {
		t.Fatalf("read gauge: %v", err)
	}
	return pb.GetGauge().GetValue()
}

// newProvisionedHandler starts NATS, creates the test stream with memory
// storage, and wires a Handler to it. Caller is responsible for calling
// ns.Shutdown() and h.Cleanup().
func newProvisionedHandler(t *testing.T) (*Handler, *server.Server, *nats.Conn) {
	t.Helper()
	ns := startJetStreamServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		ns.Shutdown()
		t.Fatalf("connect: %v", err)
	}
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	h := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "EVENTS",
		TopicPrefix:       "events.",
		HeartbeatInterval: 30,
		MaxEventSize:      -1,
		AllowedOrigins:    []string{"*"},
		logger:            zap.NewNop(),
	}
	if err := h.connectNATS(); err != nil {
		ns.Shutdown()
		nc.Close()
		t.Fatalf("connectNATS: %v", err)
	}
	js, err := h.conn.JetStream()
	if err != nil {
		h.Cleanup()
		ns.Shutdown()
		nc.Close()
		t.Fatalf("JetStream: %v", err)
	}
	h.mu.Lock()
	h.js = js
	h.mu.Unlock()
	return h, ns, nc
}

// waitForSSEBody polls rr until the body contains needle or the deadline is
// reached. Returns whether the needle was observed.
func waitForSSEBody(rr *safeFlushRecorder, needle string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(rr.Body(), needle) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return strings.Contains(rr.Body(), needle)
}

// ── metrics: active_connections gauge ─────────────────────────────────────

func TestMetrics_ActiveConnections_ReflectsLifecycle(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
	defer nc.Close()
	defer h.Cleanup()

	before := gaugeVal(t, metricsActiveConnections)

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

	mid := gaugeVal(t, metricsActiveConnections)
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
	after := gaugeVal(t, metricsActiveConnections)
	if after != before {
		t.Errorf("active_connections did not return to baseline: before=%v after=%v", before, after)
	}
}

// ── metrics: messages_delivered_total counter ─────────────────────────────

func TestMetrics_MessagesDelivered_IncrementsPerEvent(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
	defer nc.Close()
	defer h.Cleanup()

	jsPub, _ := nc.JetStream()
	for i := 0; i < 3; i++ {
		if _, err := jsPub.Publish("events.delivered", []byte(`{"i":`+strconv.Itoa(i)+`}`)); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	before := counterVal(t, metricsMessagesDelivered)

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

	got := counterVal(t, metricsMessagesDelivered)
	if got < before+3 {
		t.Errorf("messages_delivered_total did not advance by 3: before=%v got=%v", before, got)
	}
}

// ── metrics: messages_dropped_total counter ───────────────────────────────

func TestMetrics_MessagesDropped_IncrementsOnOversized(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
	defer nc.Close()
	defer h.Cleanup()

	h.MaxEventSize = 50 // forces the oversized branch

	jsPub, _ := nc.JetStream()
	if _, err := jsPub.Publish("events.drop", []byte(strings.Repeat("A", 200))); err != nil {
		t.Fatalf("publish oversized: %v", err)
	}

	before := counterVal(t, metricsMessagesDropped)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/events?topic=drop&last-id=0", nil).WithContext(ctx)
	rr := newSafeRecorder()
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()

	// Give the handler time to observe the message, decide to drop, and
	// increment the counter. We don't need to wait for ctx to expire.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if counterVal(t, metricsMessagesDropped) > before {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	<-done

	got := counterVal(t, metricsMessagesDropped)
	if got <= before {
		t.Errorf("messages_dropped_total did not increment: before=%v got=%v body=%s", before, got, rr.Body())
	}
}

// ── metrics: slow_client_disconnects_total counter ────────────────────────

func TestMetrics_SlowClientDisconnects_Increments(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
	defer nc.Close()
	defer h.Cleanup()

	h.ClientBufferSize = 4 // make the buffer easy to overflow

	jsPub, _ := nc.JetStream()
	var firstSeq uint64
	for i := 0; i < 200; i++ {
		ack, err := jsPub.Publish("events.slow", []byte(`{"i":`+strconv.Itoa(i)+`}`))
		if err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
		if i == 0 {
			firstSeq = ack.Sequence
		}
	}

	before := counterVal(t, metricsSlowClientDisconnects)

	req := httptest.NewRequest(http.MethodGet, "/events?topic=slow", nil)
	req.Header.Set("Last-Event-ID", strconv.FormatUint(firstSeq-1, 10))
	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	rr := &slowFlushRecorder{ResponseRecorder: httptest.NewRecorder(), writeDelay: 15 * time.Millisecond}
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeHTTP: %v", err)
		}
	case <-time.After(4 * time.Second):
		cancel()
		<-done
		t.Fatal("handler did not disconnect slow client in time")
	}

	if got := counterVal(t, metricsSlowClientDisconnects); got <= before {
		t.Errorf("slow_client_disconnects_total did not increment: before=%v got=%v", before, got)
	}
}

// ── metrics: replay_requests_total counter ────────────────────────────────

func TestMetrics_ReplayRequests_Increments(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
	defer nc.Close()
	defer h.Cleanup()

	before := counterVal(t, metricsReplayRequests)

	// Client with an explicit last-id is a replay request, regardless of
	// whether any messages exist yet.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/events?topic=replay&last-id=0", nil).WithContext(ctx)
	rr := newSafeRecorder()
	_ = h.ServeHTTP(rr, req, nil)

	if got := counterVal(t, metricsReplayRequests); got <= before {
		t.Errorf("replay_requests_total did not increment: before=%v got=%v", before, got)
	}
}

// ── metrics: replay_fallbacks_total counter ───────────────────────────────

func TestMetrics_ReplayFallbacks_Increments(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
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

	before := counterVal(t, metricsReplayFallbacks)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/events?topic=fallback&last-id=1", nil).WithContext(ctx)
	rr := newSafeRecorder()
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()
	<-done

	if got := counterVal(t, metricsReplayFallbacks); got <= before {
		t.Errorf("replay_fallbacks_total did not increment: before=%v got=%v", before, got)
	}
}

// ── metrics: replay_cap_reached_total counter ─────────────────────────────

func TestMetrics_ReplayCapReached_Increments(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
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

	before := counterVal(t, metricsReplayCapReached)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/events?topic=cap&last-id=1", nil).WithContext(ctx)
	rr := newSafeRecorder()
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()
	<-done

	if got := counterVal(t, metricsReplayCapReached); got <= before {
		t.Errorf("replay_cap_reached_total did not increment: before=%v got=%v", before, got)
	}
}

// ── metrics: subscription_errors_total counter ────────────────────────────

func TestMetrics_SubscriptionErrors_Increments(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
	defer nc.Close()
	defer h.Cleanup()

	// Point the handler at a stream that does not exist so js.Subscribe
	// fails with "stream not found" and the error branch in serve.go fires.
	h.StreamName = "NOPE_DOES_NOT_EXIST"

	before := counterVal(t, metricsSubscriptionErrors)

	req := httptest.NewRequest(http.MethodGet, "/events?topic=err", nil)
	rr := httptest.NewRecorder()
	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 on subscription failure, got %d", rr.Code)
	}

	if got := counterVal(t, metricsSubscriptionErrors); got <= before {
		t.Errorf("subscription_errors_total did not increment: before=%v got=%v", before, got)
	}
}

// ── TLS: buildTLSConfig ───────────────────────────────────────────────────

// generateSelfSignedPEM returns a PEM-encoded cert + PKCS#8 key pair suitable
// for dropping into buildTLSConfig without starting a TLS server. The cert is
// a self-signed CA so the same blob can be used as both ca and client cert.
func generateSelfSignedPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "nuts-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	return certPEM, keyPEM
}

func writeTempFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func TestBuildTLSConfig_CAOnlyPopulatesRootCAs(t *testing.T) {
	certPEM, _ := generateSelfSignedPEM(t)
	dir := t.TempDir()
	caPath := writeTempFile(t, dir, "ca.pem", certPEM)

	h := &Handler{NatsTLSCA: caPath}
	cfg, err := h.buildTLSConfig()
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if cfg.RootCAs == nil {
		t.Fatal("expected RootCAs to be populated when nats_tls_ca is set")
	}
	if cfg.MinVersion != 0x0303 { // TLS 1.2 sentinel
		t.Errorf("expected TLS 1.2 minimum, got 0x%x", cfg.MinVersion)
	}
	if len(cfg.Certificates) != 0 {
		t.Errorf("expected no client certs when only CA is configured, got %d", len(cfg.Certificates))
	}
	if cfg.InsecureSkipVerify {
		t.Error("expected InsecureSkipVerify to be false")
	}
}

func TestBuildTLSConfig_MTLSLoadsCertPair(t *testing.T) {
	certPEM, keyPEM := generateSelfSignedPEM(t)
	dir := t.TempDir()
	certPath := writeTempFile(t, dir, "client.crt", certPEM)
	keyPath := writeTempFile(t, dir, "client.key", keyPEM)

	h := &Handler{NatsTLSCert: certPath, NatsTLSKey: keyPath}
	cfg, err := h.buildTLSConfig()
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("expected one client certificate, got %d", len(cfg.Certificates))
	}
	if cfg.RootCAs != nil {
		t.Error("expected RootCAs to be nil when no CA is configured")
	}
}

func TestBuildTLSConfig_InsecureSkipVerifyHonored(t *testing.T) {
	h := &Handler{NatsTLSInsecureSkipVerify: true}
	cfg, err := h.buildTLSConfig()
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if !cfg.InsecureSkipVerify {
		t.Error("expected InsecureSkipVerify=true")
	}
}

func TestBuildTLSConfig_MalformedCAReturnsError(t *testing.T) {
	dir := t.TempDir()
	caPath := writeTempFile(t, dir, "bogus.pem", []byte("not a certificate"))

	h := &Handler{NatsTLSCA: caPath}
	if _, err := h.buildTLSConfig(); err == nil {
		t.Fatal("expected error when CA bundle contains no valid PEM blocks")
	}
}

func TestBuildTLSConfig_MissingCAFileReturnsError(t *testing.T) {
	h := &Handler{NatsTLSCA: filepath.Join(t.TempDir(), "does-not-exist.pem")}
	if _, err := h.buildTLSConfig(); err == nil {
		t.Fatal("expected error when CA file is missing")
	}
}

func TestBuildTLSConfig_InvalidCertKeyPairReturnsError(t *testing.T) {
	dir := t.TempDir()
	certPath := writeTempFile(t, dir, "cert.pem", []byte("not a cert"))
	keyPath := writeTempFile(t, dir, "key.pem", []byte("not a key"))

	h := &Handler{NatsTLSCert: certPath, NatsTLSKey: keyPath}
	if _, err := h.buildTLSConfig(); err == nil {
		t.Fatal("expected error when cert/key files are not valid PEM")
	}
}

// ── Heartbeat: the SSE stream emits the keep-alive comment ────────────────

func TestHandler_Heartbeat_EmitsFrame(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
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

// ── topic_prefix: subject translation and inbound filtering ───────────────

func TestHandler_TopicPrefix_PrependsSubjectAndStripsFromPayload(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
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

// ── NATS reconnect: subsequent SSE requests still succeed ─────────────────

// TestHandler_NATSReconnect_AllowsSubsequentSSE verifies that after the NATS
// server restarts (with the same port + StoreDir so the stream survives),
// the handler's long-lived connection recovers and a fresh SSE request
// succeeds. This goes beyond TestHandler_connectNATS_ReconnectLifecycle,
// which only asserts the NATS client reconnects — here we exercise the
// full SSE path after recovery.
func TestHandler_NATSReconnect_AllowsSubsequentSSE(t *testing.T) {
	// Reserve a port we control across server restarts.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	storeDir := t.TempDir()

	startServer := func() *server.Server {
		opts := &server.Options{
			Host:      "127.0.0.1",
			Port:      port,
			JetStream: true,
			StoreDir:  storeDir,
		}
		ns, err := server.NewServer(opts)
		if err != nil {
			t.Fatalf("new server: %v", err)
		}
		go ns.Start()
		if !ns.ReadyForConnections(5 * time.Second) {
			t.Fatal("server not ready")
		}
		return ns
	}

	ns := startServer()
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
	if err := h.connectNATS(); err != nil {
		ns.Shutdown()
		t.Fatalf("connectNATS: %v", err)
	}
	js, _ := h.conn.JetStream()
	h.mu.Lock()
	h.js = js
	h.mu.Unlock()
	defer h.Cleanup()

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

	ns2 := startServer()
	defer ns2.Shutdown()

	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !h.conn.IsConnected() {
		time.Sleep(100 * time.Millisecond)
	}
	if !h.conn.IsConnected() {
		t.Fatal("handler's NATS connection did not reconnect")
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

// ── JetStream persistence: messages survive handler cleanup ───────────────

func TestHandler_JetStreamPersistence_MessagesSurviveHandlerLifetime(t *testing.T) {
	ns := startJetStreamServer(t)
	defer ns.Shutdown()

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

	// First handler lifetime: provision, do nothing, tear down.
	h1 := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "EVENTS",
		TopicPrefix:       "events.",
		HeartbeatInterval: 30,
		MaxEventSize:      -1,
		AllowedOrigins:    []string{"*"},
		logger:            zap.NewNop(),
	}
	if err := h1.connectNATS(); err != nil {
		t.Fatalf("h1 connectNATS: %v", err)
	}
	js1, _ := h1.conn.JetStream()
	h1.mu.Lock()
	h1.js = js1
	h1.mu.Unlock()
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
	if err := h2.connectNATS(); err != nil {
		t.Fatalf("h2 connectNATS: %v", err)
	}
	defer h2.Cleanup()
	js2, _ := h2.conn.JetStream()
	h2.mu.Lock()
	h2.js = js2
	h2.mu.Unlock()

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

