// Integration-style handler tests for runtime behavior that depends on
// embedded NATS, JetStream state, metrics, TLS material, or SSE streaming.
package nuts

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	iopm "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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

// newProvisionedHandler starts NATS, creates the EVENTS test stream with
// memory storage, and runs the real Provision so streaming tests see the
// production defaults and lifecycle (idle heartbeat, topic cap, shutdown
// channel). The logger is swapped for a no-op afterwards to keep test output
// quiet; tests that assert logs replace it again. Caller is responsible for
// calling ns.Shutdown() and h.Cleanup().
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
	}
	if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
		ns.Shutdown()
		nc.Close()
		t.Fatalf("Provision: %v", err)
	}
	h.logger = zap.NewNop()
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

	before := counterValue(metricsMessagesDropped, dropReasonRawPayload)

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
		if counterValue(metricsMessagesDropped, dropReasonRawPayload) > before {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	<-done

	got := counterValue(metricsMessagesDropped, dropReasonRawPayload)
	if got <= before {
		t.Errorf("messages_dropped_total{reason=raw_payload} did not increment: before=%v got=%v body=%s", before, got, rr.Body())
	}
}

// ── metrics: slow_client_disconnects_total counter ────────────────────────

func TestMetrics_SlowClientDisconnects_Increments(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
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
	before := counterVal(t, metricsSlowClientDisconnects)
	writeDisconnectsBefore := counterValue(metricsWriteDisconnects, "message")

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
	if got := counterVal(t, metricsSlowClientDisconnects); got != before+1 {
		t.Errorf("slow_client_disconnects_total = %v, want %v", got, before+1)
	}
	if got := counterValue(metricsWriteDisconnects, "message"); got != writeDisconnectsBefore+1 {
		t.Errorf("write_disconnects_total{site=message} = %v, want %v", got, writeDisconnectsBefore+1)
	}
	if !hasLogField(obs, "disconnect_reason", "slow_client") {
		t.Fatalf("expected disconnect_reason=slow_client, logs=%v", obs.All())
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
		NotAfter:              time.Now().Add(24 * time.Hour),
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

// TestHandler_ConnectNATS_TLSConfigErrorPropagates exercises the error
// return on the buildTLSConfig() failure branch inside connectNATS, which
// otherwise sits behind an unreachable code path because the existing
// TLS tests call buildTLSConfig directly.
func TestHandler_ConnectNATS_TLSConfigErrorPropagates(t *testing.T) {
	h := &Handler{
		NatsTLSCA: filepath.Join(t.TempDir(), "does-not-exist.pem"),
		logger:    zap.NewNop(),
	}
	if err := h.connectNATS(); err == nil {
		t.Fatal("expected connectNATS to return the TLS-build error when CA file is missing")
	}
}

// startJetStreamServerWithTLS spins up an embedded NATS server with
// JetStream enabled and TLS required on the client port, using the
// supplied PEM cert/key. The server cert is self-signed by
// generateSelfSignedPEM (CN="nuts-test", no SANs), so connections from
// 127.0.0.1 will fail hostname verification unless the client opts into
// InsecureSkipVerify. That's deliberate: it lets the negative test
// below prove the operator-supplied *tls.Config is actually the one
// nats.go used to dial.
func startJetStreamServerWithTLS(t *testing.T, certPEM, keyPEM []byte) *server.Server {
	t.Helper()
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("tls.X509KeyPair: %v", err)
	}
	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
	}
	ns, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("server.NewServer: %v", err)
	}
	go ns.Start()
	// Register shutdown BEFORE the readiness check so a 5 s flake
	// (e.g. under heavy CI load) doesn't t.Fatal with the server
	// goroutine and its listening socket still alive — repeated under
	// stress runs that leaks ports and breaks sibling tests.
	t.Cleanup(ns.Shutdown)
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("TLS NATS server not ready")
	}
	return ns
}

// TestHandler_ConnectNATS_TLS_LiveHandshake proves that the *tls.Config
// produced by buildTLSConfig is actually used to dial NATS — that
// nats.Secure(tlsCfg) is wired correctly in connectNATS. The existing
// TestBuildTLSConfig_* tests only inspect the returned struct; a
// regression that dropped the nats.Secure() option (e.g. replacing it
// with nats.RootCAs(...) which discards client certs and TLS-Required
// negotiation) would pass every prior TLS test but break this one.
func TestHandler_ConnectNATS_TLS_LiveHandshake(t *testing.T) {
	certPEM, keyPEM := generateSelfSignedPEM(t)
	ns := startJetStreamServerWithTLS(t, certPEM, keyPEM)

	t.Run("InsecureSkipVerify connects against self-signed CN-only cert", func(t *testing.T) {
		h := &Handler{
			NatsURL:                   ns.ClientURL(),
			StreamName:                "EVENTS",
			NatsTLSInsecureSkipVerify: true,
			MaxReconnects:             intPtr(0),
			logger:                    zap.NewNop(),
		}
		if err := h.connectNATS(); err != nil {
			t.Fatalf("connectNATS against TLS NATS with InsecureSkipVerify: %v", err)
		}
		defer h.Cleanup()
		if !h.conn.TLSRequired() {
			t.Error("TLSRequired = false; the connection negotiated plaintext, not TLS — nats.Secure(tlsCfg) may not be wired")
		}
	})

	t.Run("strict verify against same CA fails on 127.0.0.1 hostname mismatch", func(t *testing.T) {
		// Same self-signed cert configured as a trust root; no
		// InsecureSkipVerify. The cert has CN="nuts-test" and no
		// IPAddresses SAN, so Go's verifier must reject the 127.0.0.1
		// dial. A pass here would mean either the CA bundle is being
		// ignored OR InsecureSkipVerify was silently flipped on.
		dir := t.TempDir()
		caPath := writeTempFile(t, dir, "ca.pem", certPEM)
		h := &Handler{
			NatsURL:       ns.ClientURL(),
			StreamName:    "EVENTS",
			NatsTLSCA:     caPath,
			MaxReconnects: intPtr(0),
			logger:        zap.NewNop(),
		}
		err := h.connectNATS()
		if err == nil {
			t.Fatal("expected handshake failure for hostname-mismatched self-signed CA")
		}
		// nats.go wraps the verifier error; the underlying x509 error for
		// our CN-only cert dialled by IP is:
		//   "x509: cannot validate certificate for 127.0.0.1 because it
		//    doesn't contain any IP SANs"
		// We pin to the IP-SAN phrase: it is emitted only by the x509
		// verifier during the handshake, so a pass here cannot be faked
		// by a setup-time error from buildTLSConfig (which uses phrases
		// like "no certificates found in" or "read nats_tls_ca").
		msg := err.Error()
		if !strings.Contains(msg, "doesn't contain any IP SANs") {
			t.Fatalf("error %q doesn't look like an x509 hostname-mismatch failure; nats.Secure(tlsCfg) may not be wiring buildTLSConfig's output", msg)
		}
	})
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
	js, _ := jetstream.New(h.conn)
	h.mu.Lock()
	h.js = js
	h.mu.Unlock()
	defer h.Cleanup()

	beforeDisconnect := counterValue(metricsNATSConnectionEvents, "disconnect")
	beforeReconnect := counterValue(metricsNATSConnectionEvents, "reconnect")

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

	// The disconnect+reconnect cycle must have bumped the flap-detection
	// counter so an SRE alert can fire on broker instability. nats.go
	// dispatches DisconnectErrHandler/ReconnectHandler on its async callback
	// goroutine, which is not happens-before with the IsConnected() state
	// flip observed above — poll until the counters reflect the lifecycle
	// events instead of reading once and racing the dispatcher.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if counterValue(metricsNATSConnectionEvents, "disconnect") > beforeDisconnect &&
			counterValue(metricsNATSConnectionEvents, "reconnect") > beforeReconnect {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if got := counterValue(metricsNATSConnectionEvents, "disconnect"); got <= beforeDisconnect {
		t.Errorf("nuts_nats_connection_events_total{event=disconnect} did not increment: %v -> %v", beforeDisconnect, got)
	}
	if got := counterValue(metricsNATSConnectionEvents, "reconnect"); got <= beforeReconnect {
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
	js, _ := jetstream.New(h.conn)
	h.mu.Lock()
	h.js = js
	h.mu.Unlock()
	defer h.Cleanup()

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

	ns2 := startServer()
	defer ns2.Shutdown()
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
	js1, _ := jetstream.New(h1.conn)
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
	js2, _ := jetstream.New(h2.conn)
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

// startSSE runs ServeHTTP for target on a goroutine and waits for the
// connected event. The returned cancel ends the request; done yields
// ServeHTTP's return value.
func startSSE(t *testing.T, h *Handler, target, lastEventID string) (*safeFlushRecorder, context.CancelFunc, <-chan error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	ctx, cancel := context.WithTimeout(req.Context(), 15*time.Second)
	req = req.WithContext(ctx)
	rr := newSafeRecorder()
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()
	if !waitForSSEBody(rr, "event: connected", 3*time.Second) {
		cancel()
		t.Fatalf("no connected event for %s; body=%q", target, rr.Body())
	}
	return rr, cancel, done
}

// stopSSE cancels a request started with startSSE and waits for ServeHTTP.
func stopSSE(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ServeHTTP returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServeHTTP did not return within 3s of cancellation")
	}
}

// waitForFirstConsumer polls the stream until a consumer exists and returns
// its info, or nil when the timeout elapses first.
func waitForFirstConsumer(t *testing.T, js jetstream.JetStream, stream string, timeout time.Duration) *jetstream.ConsumerInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		var found *jetstream.ConsumerInfo
		if s, err := js.Stream(ctx, stream); err == nil {
			lister := s.ListConsumers(ctx)
			for ci := range lister.Info() {
				if found == nil {
					found = ci
				}
			}
		}
		cancel()
		if found != nil {
			return found
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

func TestHandler_MultiTopicStreamUsesServerSideFilterSubjects(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
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

// TestHandler_ConsumerDeletedMidStream_RecreatesAndResumes covers what M9
// Batch B was meant to fix: a consumer lost on the server (reaped, deleted,
// dropped with a leafnode route) used to leave the SSE stream open and silent.
// The ordered consumer detects the missing heartbeats, recreates itself from
// the last delivered sequence, and the stream continues without a gap.
func TestHandler_ConsumerDeletedMidStream_RecreatesAndResumes(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
	defer nc.Close()
	defer h.Cleanup()
	h.NatsIdleHeartbeat = 1
	h.HeartbeatInterval = 60
	core, obs := observer.New(zap.InfoLevel)
	h.logger = zap.New(core)
	js, _ := nc.JetStream()
	admin, _ := jetstream.New(nc)

	rr, cancel, done := startSSE(t, h, "/events?topic=alpha", "")
	if _, err := js.Publish("events.alpha", []byte(`{"n":1}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !waitForSSEBody(rr, `{"n":1}`, 3*time.Second) {
		t.Fatalf("first message not delivered; body=%q", rr.Body())
	}
	info := waitForFirstConsumer(t, admin, "EVENTS", time.Second)
	if info == nil {
		t.Fatal("no consumer to delete")
	}
	recreatedBefore := counterValue(metricsConsumerInvalidated, "recreated")
	if err := admin.DeleteConsumer(context.Background(), "EVENTS", info.Name); err != nil {
		t.Fatalf("DeleteConsumer: %v", err)
	}
	for _, n := range []string{"2", "3"} {
		if _, err := js.Publish("events.alpha", []byte(`{"n":`+n+`}`)); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	if !waitForSSEBody(rr, `{"n":3}`, 10*time.Second) {
		t.Fatalf("stream did not recover after the consumer was deleted; body=%q", rr.Body())
	}
	select {
	case err := <-done:
		t.Fatalf("SSE handler exited (err=%v) instead of recovering", err)
	default:
	}
	if got := parseSSEIDs(t, rr.Body()); !reflect.DeepEqual(got, []uint64{0, 1, 2, 3}) {
		t.Fatalf("ids = %v, want contiguous [0 1 2 3]", got)
	}
	if got := counterValue(metricsConsumerInvalidated, "recreated"); got <= recreatedBefore {
		t.Fatalf("nuts_consumer_invalidated_total{reason=recreated} = %v, want > %v", got, recreatedBefore)
	}
	if !hasLogField(obs, "previous_consumer", info.Name) {
		t.Fatalf("no recreation log naming the deleted consumer %q; logs=%v", info.Name, obs.All())
	}
	stopSSE(t, cancel, done)
}

// TestHandler_Provision_IdleHeartbeatReachesPullOptions pins the
// nats_idle_heartbeat contract end to end: the value Provision settles on is
// the heartbeat every pull request asks JetStream for.
func TestHandler_Provision_IdleHeartbeatReachesPullOptions(t *testing.T) {
	ns := startJetStreamServer(t)
	defer ns.Shutdown()
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	for _, c := range []struct {
		name       string
		configured int
		want       time.Duration
	}{
		{name: "omitted uses the 10s default", configured: 0, want: 10 * time.Second},
		{name: "explicit value is kept", configured: 5, want: 5 * time.Second},
		{name: "disable sentinel leaves the library default", configured: natsIdleHeartbeatDisabledSentinel, want: 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := &Handler{NatsURL: ns.ClientURL(), StreamName: "EVENTS", NatsIdleHeartbeat: c.configured}
			if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
				t.Fatalf("Provision: %v", err)
			}
			defer h.Cleanup()
			var got time.Duration
			for _, opt := range h.pullOptions() {
				if hb, ok := opt.(jetstream.PullHeartbeat); ok {
					got = time.Duration(hb)
				}
			}
			if got != c.want {
				t.Fatalf("PullHeartbeat = %v, want %v", got, c.want)
			}
		})
	}
}

// TestHandler_NoCursorRequestStartsAfterLastSeq checks the explicit start
// position of a request without a cursor: retained history is not replayed,
// the connected event names the last retained sequence, and the next
// published message arrives with the following id.
func TestHandler_NoCursorRequestStartsAfterLastSeq(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
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
	if !strings.HasPrefix(rr.Body(), "id: 3\nevent: connected\n") {
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
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
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
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
	defer nc.Close()
	defer h.Cleanup()
	h.TopicPrefix = ""
	core, obs := observer.New(zap.WarnLevel)
	h.logger = zap.New(core)
	before := counterVal(t, metricsSubscriptionErrors)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/events?topic=orders", nil)
	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "Failed to subscribe to requested topics: orders") {
		t.Fatalf("response = %d %q, want 503 naming the topic", rr.Code, rr.Body.String())
	}
	if got := counterVal(t, metricsSubscriptionErrors); got != before+1 {
		t.Fatalf("subscription_errors_total = %v, want %v", got, before+1)
	}
	if !hasLogField(obs, "disconnect_reason", "subscription_failed") {
		t.Fatalf("missing disconnect_reason=subscription_failed: %v", obs.All())
	}
	if got := consumerCount(mustJetStream(t, nc), "EVENTS"); got != 0 {
		t.Fatalf("consumers after rejection = %d, want 0", got)
	}
}

func TestHandler_Provision_WriteTimeoutDefaults(t *testing.T) {
	ns := startJetStreamServer(t)
	defer ns.Shutdown()
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	for _, c := range []struct {
		name       string
		configured int
		want       int
	}{
		{name: "omitted uses the 30s default", configured: 0, want: defaultWriteTimeoutSeconds},
		{name: "explicit value is kept", configured: 5, want: 5},
		{name: "-1 keeps deadlines disabled", configured: writeTimeoutDisabledSentinel, want: writeTimeoutDisabledSentinel},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := &Handler{NatsURL: ns.ClientURL(), StreamName: "EVENTS", WriteTimeout: c.configured}
			if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
				t.Fatalf("Provision: %v", err)
			}
			defer h.Cleanup()
			if h.WriteTimeout != c.want {
				t.Fatalf("WriteTimeout = %d, want %d", h.WriteTimeout, c.want)
			}
		})
	}
}
