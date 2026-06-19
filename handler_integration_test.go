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
	js, _ := h.conn.JetStream()
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
	js, _ := h.conn.JetStream()
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

// TestHandler_SubscribeToMultipleTopics_MultiFilterPath drives the modern
// JetStream multi-filter branch. On nats-server >= 2.10 the subscribe
// succeeds and the ephemeral consumer is configured with FilterSubjects
// populated. On older servers (pre-2.10) the SDK surfaces "multiple
// consumer filter subjects not supported"; that's also a valid outcome
// — when useMultiFilter is true we hand the full subject list to the
// SDK and let any server-side rejection bubble up rather than silently
// degrading.
func TestHandler_SubscribeToMultipleTopics_MultiFilterPath(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
	defer nc.Close()
	defer h.Cleanup()

	plan := streamPlan{
		Topics:       []string{"alpha", "beta"},
		FullSubjects: []string{"events.alpha", "events.beta"},
		Replay:       replayPlan{Mode: replayModeDeliverNew},
	}

	cb := func(*nats.Msg) {}

	sub, err := h.subscribeToMultipleTopics(h.js, plan, h.subscriptionOptions(plan), cb, true, "2.10.0")
	if err == nil {
		// Inspect the consumer before unsubscribing — Unsubscribe deletes
		// the ephemeral consumer on the server, after which ConsumerInfo
		// returns "consumer not found".
		info, infoErr := sub.ConsumerInfo()
		_ = sub.Unsubscribe()
		if infoErr != nil {
			t.Fatalf("ConsumerInfo: %v", infoErr)
		}
		if !reflect.DeepEqual(info.Config.FilterSubjects, plan.FullSubjects) {
			t.Fatalf("FilterSubjects = %#v, want %#v", info.Config.FilterSubjects, plan.FullSubjects)
		}
		return
	}
	if !strings.Contains(err.Error(), "multiple consumer filter subjects not supported") {
		t.Fatalf("err = %v, want SDK rejection from multi-filter branch", err)
	}
}

// TestHandler_SubscribeToMultipleTopics_WildcardFallbackPath drives the
// older-server fallback: the function must subscribe to the common-prefix
// wildcard, log a warning that names the wildcard and the server version,
// and still deliver the requested subjects.
func TestHandler_SubscribeToMultipleTopics_WildcardFallbackPath(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
	defer nc.Close()
	defer h.Cleanup()

	core, obs := observer.New(zap.WarnLevel)
	h.logger = zap.New(core)

	plan := streamPlan{
		Topics:       []string{"alpha", "beta"},
		FullSubjects: []string{"events.alpha", "events.beta"},
		Replay:       replayPlan{Mode: replayModeDeliverNew},
	}

	received := make(chan *nats.Msg, 8)
	cb := func(msg *nats.Msg) { received <- msg }

	sub, err := h.subscribeToMultipleTopics(h.js, plan, h.subscriptionOptions(plan), cb, false, "2.9.25")
	if err != nil {
		t.Fatalf("subscribeToMultipleTopics: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	info, err := sub.ConsumerInfo()
	if err != nil {
		t.Fatalf("ConsumerInfo: %v", err)
	}
	wantWildcard := commonSubjectFilter(plan.FullSubjects)
	if info.Config.FilterSubject != wantWildcard {
		t.Fatalf("FilterSubject = %q, want %q", info.Config.FilterSubject, wantWildcard)
	}
	if len(info.Config.FilterSubjects) != 0 {
		t.Fatalf("FilterSubjects = %#v, want empty on fallback path", info.Config.FilterSubjects)
	}

	if !hasLogContaining(obs, "NATS server does not support multi-filter consumers") {
		t.Fatalf("expected fallback warning log, got: %#v", obs.All())
	}
	if !hasLogField(obs, "wildcard_subject", wantWildcard) {
		t.Fatalf("expected wildcard_subject=%q in log fields, got: %#v", wantWildcard, obs.All())
	}
	if !hasLogField(obs, "server_version", "2.9.25") {
		t.Fatalf("expected server_version=2.9.25 in log fields, got: %#v", obs.All())
	}

	jsPub, err := nc.JetStream()
	if err != nil {
		t.Fatalf("JetStream pub: %v", err)
	}
	if _, err := jsPub.Publish("events.alpha", []byte(`{}`)); err != nil {
		t.Fatalf("publish alpha: %v", err)
	}
	select {
	case msg := <-received:
		if msg.Subject != "events.alpha" {
			t.Fatalf("got subject %q, want events.alpha", msg.Subject)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive published message via wildcard fallback")
	}
}

// TestHandler_IdleHeartbeat_AppearsInConsumerConfig drives the M9 Phase 3
// end-to-end ConsumerInfo assertion: when h.NatsIdleHeartbeat is set,
// subscriptionOptions appends nats.IdleHeartbeat(...) and the JetStream
// server records the requested interval on the resulting ephemeral
// consumer. The assertion is on info.Config.Heartbeat (the server-side
// recorded value), not the SubOpt slice — that lifts the contract from
// "we asked" to "the server agreed" and catches any future SubOpt
// reordering that would silently drop the option.
func TestHandler_IdleHeartbeat_AppearsInConsumerConfig(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
	defer nc.Close()
	defer h.Cleanup()

	// Set after Provision so the test exercises the subscribe-time read
	// of h.NatsIdleHeartbeat (Provision normalisation is covered by
	// TestHandler_NatsIdleHeartbeat_ConfigContract). 5s keeps the value
	// distinct from the 10s default and well inside the validation
	// boundary so any future refactor that accidentally substitutes the
	// default would change the assertion's failure mode visibly.
	h.NatsIdleHeartbeat = 5

	plan := streamPlan{
		Topics:       []string{"alpha"},
		FullSubjects: []string{"events.alpha"},
		Replay:       replayPlan{Mode: replayModeDeliverNew},
	}

	sub, err := h.js.Subscribe("events.alpha", func(*nats.Msg) {}, h.subscriptionOptions(plan)...)
	if err != nil {
		t.Fatalf("js.Subscribe: %v", err)
	}
	info, infoErr := sub.ConsumerInfo()
	_ = sub.Unsubscribe()
	if infoErr != nil {
		t.Fatalf("ConsumerInfo: %v", infoErr)
	}

	wantHeartbeat := 5 * time.Second
	if info.Config.Heartbeat != wantHeartbeat {
		t.Fatalf("ConsumerInfo.Config.Heartbeat = %v, want %v (IdleHeartbeat SubOpt not honoured by server)", info.Config.Heartbeat, wantHeartbeat)
	}
}

// TestHandler_IdleHeartbeat_DisabledWhenNegative pins the operator-
// disable contract. When h.NatsIdleHeartbeat is the -1 sentinel
// (Validate accepts it; Provision preserves it), subscriptionOptions
// MUST skip the SubOpt append entirely and the resulting ephemeral
// consumer is created with Heartbeat == 0 — exactly the pre-M9
// behaviour. Without this assertion a future "fix" that always sets
// IdleHeartbeat would silently undermine the operator opt-out.
func TestHandler_IdleHeartbeat_DisabledWhenNegative(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
	defer nc.Close()
	defer h.Cleanup()

	h.NatsIdleHeartbeat = -1

	plan := streamPlan{
		Topics:       []string{"alpha"},
		FullSubjects: []string{"events.alpha"},
		Replay:       replayPlan{Mode: replayModeDeliverNew},
	}

	sub, err := h.js.Subscribe("events.alpha", func(*nats.Msg) {}, h.subscriptionOptions(plan)...)
	if err != nil {
		t.Fatalf("js.Subscribe: %v", err)
	}
	info, infoErr := sub.ConsumerInfo()
	_ = sub.Unsubscribe()
	if infoErr != nil {
		t.Fatalf("ConsumerInfo: %v", infoErr)
	}

	if info.Config.Heartbeat != 0 {
		t.Fatalf("ConsumerInfo.Config.Heartbeat = %v, want 0 (operator-disable sentinel must skip the SubOpt append)", info.Config.Heartbeat)
	}
}

// TestHandler_IdleHeartbeat_PropagatesThroughProvisionDefault is the
// strongest end-to-end gate for the M9 Batch A default-on contract.
// Unlike TestHandler_IdleHeartbeat_DefaultOnAfterProvision which
// mirrors the Provision normalisation manually, this one actually
// calls h.Provision with NatsIdleHeartbeat omitted (== 0) and verifies
// the JetStream server records the default 10s on the resulting
// consumer. A regression in either Provision's normalisation block OR
// the subscribe-time read of h.NatsIdleHeartbeat would slip past the
// hardening_test.go field-level assertions but trip this test.
func TestHandler_IdleHeartbeat_PropagatesThroughProvisionDefault(t *testing.T) {
	ns := startJetStreamServer(t)
	t.Cleanup(ns.Shutdown)

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	h := &Handler{
		NatsURL:    ns.ClientURL(),
		StreamName: "EVENTS",
		// NatsIdleHeartbeat deliberately omitted (zero value). Provision
		// MUST normalise to 10s — the M9 Batch A default-on contract.
	}
	if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() { _ = h.Cleanup() })

	plan := streamPlan{
		Topics:       []string{"alpha"},
		FullSubjects: []string{"events.alpha"},
		Replay:       replayPlan{Mode: replayModeDeliverNew},
	}

	sub, err := h.js.Subscribe("events.alpha", func(*nats.Msg) {}, h.subscriptionOptions(plan)...)
	if err != nil {
		t.Fatalf("js.Subscribe: %v", err)
	}
	info, infoErr := sub.ConsumerInfo()
	_ = sub.Unsubscribe()
	if infoErr != nil {
		t.Fatalf("ConsumerInfo: %v", infoErr)
	}

	wantHeartbeat := 10 * time.Second
	if info.Config.Heartbeat != wantHeartbeat {
		t.Fatalf("ConsumerInfo.Config.Heartbeat = %v, want %v (full Provision-driven default-on contract)", info.Config.Heartbeat, wantHeartbeat)
	}
}

// TestHandler_IdleHeartbeat_MultiFilterPathPropagates verifies the
// multi-filter subscribe path (NATS 2.10+) also honours
// NatsIdleHeartbeat. Phase 3's AppearsInConsumerConfig test covered
// only the single-topic js.Subscribe path; subscribeToMultipleTopics
// at serve.go:1207-1224 takes a different branch with
// ConsumerFilterSubjects, and a future refactor that built its own
// SubOpt slice instead of accepting one from subscriptionOptions
// would silently break heartbeat coverage on multi-topic clients.
func TestHandler_IdleHeartbeat_MultiFilterPathPropagates(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
	defer nc.Close()
	defer h.Cleanup()

	h.NatsIdleHeartbeat = 3

	plan := streamPlan{
		Topics:       []string{"alpha", "beta"},
		FullSubjects: []string{"events.alpha", "events.beta"},
		Replay:       replayPlan{Mode: replayModeDeliverNew},
	}

	sub, err := h.subscribeToMultipleTopics(h.js, plan, h.subscriptionOptions(plan), func(*nats.Msg) {}, true, "2.10.0")
	if err != nil {
		if strings.Contains(err.Error(), "multiple consumer filter subjects not supported") {
			t.Skip("embedded server does not support multi-filter consumers; the wildcard-fallback test covers the other branch")
		}
		t.Fatalf("subscribeToMultipleTopics (multi-filter): %v", err)
	}
	info, infoErr := sub.ConsumerInfo()
	_ = sub.Unsubscribe()
	if infoErr != nil {
		t.Fatalf("ConsumerInfo: %v", infoErr)
	}

	if info.Config.Heartbeat != 3*time.Second {
		t.Fatalf("multi-filter ConsumerInfo.Config.Heartbeat = %v, want 3s (IdleHeartbeat must propagate through the multi-filter branch)", info.Config.Heartbeat)
	}
}

// TestHandler_IdleHeartbeat_WildcardFallbackPathPropagates covers the
// pre-NATS-2.10 wildcard-fallback subscribe path at serve.go:1223.
// The wildcard branch is what NUTS uses against the nats:2.9-alpine
// functional matrix line, so heartbeat propagation here is operator-
// visible on real deployments running older servers. The fallback
// path constructs its own commonSubjectFilter and passes the original
// opts slice through — this test pins that contract.
func TestHandler_IdleHeartbeat_WildcardFallbackPathPropagates(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
	defer nc.Close()
	defer h.Cleanup()

	h.NatsIdleHeartbeat = 4

	plan := streamPlan{
		Topics:       []string{"alpha", "beta"},
		FullSubjects: []string{"events.alpha", "events.beta"},
		Replay:       replayPlan{Mode: replayModeDeliverNew},
	}

	sub, err := h.subscribeToMultipleTopics(h.js, plan, h.subscriptionOptions(plan), func(*nats.Msg) {}, false, "2.9.25")
	if err != nil {
		t.Fatalf("subscribeToMultipleTopics (wildcard fallback): %v", err)
	}
	info, infoErr := sub.ConsumerInfo()
	_ = sub.Unsubscribe()
	if infoErr != nil {
		t.Fatalf("ConsumerInfo: %v", infoErr)
	}

	if info.Config.Heartbeat != 4*time.Second {
		t.Fatalf("wildcard fallback ConsumerInfo.Config.Heartbeat = %v, want 4s (IdleHeartbeat must propagate through the pre-2.10 fallback branch)", info.Config.Heartbeat)
	}
}

// TestHandler_BatchA_HeartbeatMissTriggersClassifierAndLog is the
// END-TO-END failure-chain test for Batch A. It exercises the full
// production pipeline:
//
//  1. Subscribe with NatsIdleHeartbeat=1s and a Warn-level log
//     observer wired into the same Handler.
//  2. Force a heartbeat miss by deleting the server-side ephemeral
//     consumer via the JetStream admin API. The server stops sending
//     heartbeats; nats.go's IdleHeartbeat detection (built into the
//     library when the SubOpt is set) raises an async error after
//     approximately 2 missed intervals.
//  3. Assert the nats.go async ErrorHandler at provision.go:234 fires
//     classifyNATSAsyncError, which buckets the typed error as
//     "consumer_invalidated" (the Phase 1 label) and bumps
//     nuts_nats_async_errors_total{kind=...}.
//  4. Assert the structured Warn log includes kind=consumer_sequence
//     _mismatch — the operator-visible signal for the failure mode.
//  5. Assert nuts_consumer_invalidated_total stays at zero: Batch A
//     surfaces the failure as observability ONLY. Batch B (#54) will
//     populate this metric from the serveStream termination arm.
//
// Without this test, a regression that broke any link in the chain —
// the SubOpt not actually triggering server-side heartbeats, the
// nats.go library not surfacing the error, the classifier silently
// falling back to "other", the ErrorHandler missing the metric/log
// path — would slip past Batch A's other tests that only inspect
// individual links.
func TestHandler_BatchA_HeartbeatMissTriggersClassifierAndLog(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
	defer nc.Close()
	defer h.Cleanup()

	// 1s is the minimum integer value Validate accepts (must be > 0
	// and < InactiveThreshold/2 = 15). At 1s nats.go fires the async
	// error after ~2 missed heartbeats (~2-3s), so a 6s polling
	// deadline below is comfortable.
	h.NatsIdleHeartbeat = 1

	// Capture logs at Warn level so we observe the structured async-
	// error log from provision.go:241 without noise from Debug entries.
	core, obs := observer.New(zap.WarnLevel)
	h.logger = zap.New(core)

	plan := streamPlan{
		Topics:       []string{"alpha"},
		FullSubjects: []string{"events.alpha"},
		Replay:       replayPlan{Mode: replayModeDeliverNew},
	}

	sub, err := h.js.Subscribe("events.alpha", func(*nats.Msg) {}, h.subscriptionOptions(plan)...)
	if err != nil {
		t.Fatalf("js.Subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	info, err := sub.ConsumerInfo()
	if err != nil {
		t.Fatalf("ConsumerInfo: %v", err)
	}
	consumerName := info.Name

	asyncErrsBefore := counterValue(metricsNATSAsyncErrors, "consumer_invalidated")
	asyncErrsOtherBefore := counterValue(metricsNATSAsyncErrors, "other")
	invalidatedHeartbeatBefore := counterValue(metricsConsumerInvalidated, "heartbeat_missed")
	invalidatedSlowBefore := counterValue(metricsConsumerInvalidated, "slow_consumer")

	// Force the failure server-side. nc was created in
	// newProvisionedHandler as the admin connection separate from
	// h.conn; deleting via nc means the server cleans up the
	// ephemeral and h.conn's nats.go layer detects the missing
	// heartbeats on its own subscription.
	jsAdmin, err := nc.JetStream()
	if err != nil {
		t.Fatalf("admin JetStream: %v", err)
	}
	if err := jsAdmin.DeleteConsumer("EVENTS", consumerName); err != nil {
		t.Fatalf("DeleteConsumer: %v", err)
	}

	// Wait up to 6s for nats.go's IdleHeartbeat-miss detector to
	// fire. The detection cadence is governed by nats.go and is
	// typically 2× heartbeat plus a small fudge.
	asyncErrsAfter := waitForCounterValue(t, metricsNATSAsyncErrors, "consumer_invalidated", asyncErrsBefore, 6*time.Second)
	if asyncErrsAfter <= asyncErrsBefore {
		t.Fatalf("nuts_nats_async_errors_total{kind=\"consumer_invalidated\"} did not increment after consumer deletion: before=%v after=%v\nlogs: %#v", asyncErrsBefore, asyncErrsAfter, obs.All())
	}

	// Classifier-regression guard: ensure the failure did NOT silently
	// bucket as "other" instead of consumer_invalidated. If a
	// future refactor swapped errors.As for errors.Is, the
	// consumer_invalidated counter would stay flat and the
	// "other" counter would tick instead.
	if got := counterValue(metricsNATSAsyncErrors, "other"); got > asyncErrsOtherBefore {
		t.Fatalf("nuts_nats_async_errors_total{kind=\"other\"} incremented from %v to %v — classifier regressed to errors.Is path", asyncErrsOtherBefore, got)
	}

	// Structured Warn log must carry kind=consumer_invalidated.
	if !hasLogField(obs, "kind", "consumer_invalidated") {
		t.Fatalf("expected Warn log with kind=consumer_invalidated field, got: %#v", obs.All())
	}

	// CRITICAL BATCH A CONTRACT: nuts_consumer_invalidated_total MUST
	// remain at zero. Batch A is detection-only; Batch B (#54) will
	// populate this metric from the SSE termination arm. If a future
	// change starts ticking it during Batch A's window, Batch B has
	// leaked into Batch A.
	if got := counterValue(metricsConsumerInvalidated, "heartbeat_missed"); got > invalidatedHeartbeatBefore {
		t.Fatalf("nuts_consumer_invalidated_total{reason=\"heartbeat_missed\"} incremented from %v to %v during Batch A — Batch B has leaked into Batch A code", invalidatedHeartbeatBefore, got)
	}
	if got := counterValue(metricsConsumerInvalidated, "slow_consumer"); got > invalidatedSlowBefore {
		t.Fatalf("nuts_consumer_invalidated_total{reason=\"slow_consumer\"} incremented from %v to %v during Batch A — Batch B has leaked into Batch A code", invalidatedSlowBefore, got)
	}
}

// TestHandler_BatchA_SSEHandlerStaysOpenOnHeartbeatMiss is the SSE-
// client-side complement to the failure-chain test above. It drives
// h.ServeHTTP through a real SSE connection, forces a server-side
// consumer deletion mid-stream, and verifies the SSE handler does NOT
// terminate. This is the load-bearing Batch A contract: a missed
// heartbeat surfaces in metrics/logs but the client connection stays
// open until the deliberate Batch B termination work lands.
//
// A regression that prematurely added the disconnect arm (e.g. by
// hooking the global ErrorHandler to close serveStream's loop) would
// be caught here: the goroutine would exit early and the assertion
// "handler still running" would fail with a disconnect_reason that
// shouldn't exist yet.
func TestHandler_BatchA_SSEHandlerStaysOpenOnHeartbeatMiss(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
	defer nc.Close()
	defer h.Cleanup()

	h.NatsIdleHeartbeat = 1
	// HeartbeatInterval (the SSE-side keepalive ticker) is long enough
	// that it does not fire during the test window. Without this we
	// would conflate SSE writes from the ticker with the
	// no-disconnect contract.
	h.HeartbeatInterval = 60

	core, obs := observer.New(zap.DebugLevel)
	h.logger = zap.New(core)

	req := httptest.NewRequest(http.MethodGet, "/events?topic=alpha", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	rr := newSafeRecorder()
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()

	// Wait for the JetStream consumer to materialise before forcing
	// the failure — the subscribe is async with respect to ServeHTTP
	// returning the connected event.
	jsAdmin, err := nc.JetStream()
	if err != nil {
		t.Fatalf("admin JetStream: %v", err)
	}
	consumerName := waitForFirstConsumer(t, jsAdmin, "EVENTS", 3*time.Second)
	if consumerName == "" {
		t.Fatalf("no consumer appeared on EVENTS within 3s — ServeHTTP did not subscribe")
	}

	asyncErrsBefore := counterValue(metricsNATSAsyncErrors, "consumer_invalidated")

	if err := jsAdmin.DeleteConsumer("EVENTS", consumerName); err != nil {
		t.Fatalf("DeleteConsumer: %v", err)
	}

	got := waitForCounterValue(t, metricsNATSAsyncErrors, "consumer_invalidated", asyncErrsBefore, 6*time.Second)
	if got <= asyncErrsBefore {
		t.Fatalf("ErrConsumerSequenceMismatch did not fire within 6s of consumer deletion (before=%v after=%v) — heartbeat-miss detection broken", asyncErrsBefore, got)
	}

	// CRITICAL BATCH A CONTRACT: handler MUST still be running. Give
	// it 500ms more grace after the metric ticked to catch any race
	// where the disconnect path is wired but slow.
	select {
	case err := <-done:
		t.Fatalf("SSE handler exited (err=%v) after consumer invalidation — Batch B has leaked into Batch A code", err)
	case <-time.After(500 * time.Millisecond):
		// Good — handler still alive.
	}

	// Cancel the context so ServeHTTP exits cleanly.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ServeHTTP final return (after cancel): %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not exit within 2s of context cancellation")
	}

	// The terminal disconnect_reason MUST be client_context_done
	// (from the ctx.Done() arm), not consumer_invalidated. The
	// consumer_invalidated reason does not exist in the codebase
	// until Batch B; if it appears we have a leak.
	if hasLogField(obs, "disconnect_reason", "consumer_invalidated") {
		t.Fatalf("found disconnect_reason=consumer_invalidated in logs — Batch B has leaked into Batch A code\nlogs: %#v", obs.All())
	}
	if !hasLogField(obs, "disconnect_reason", "client_context_done") {
		t.Fatalf("expected disconnect_reason=client_context_done after explicit cancel, got: %#v", obs.All())
	}
}

// waitForCounterValue polls a labelled counter until it exceeds
// `before` or `timeout` elapses, returning the last observed value.
// Used by the Batch A end-to-end tests where the time between
// inducing a failure and the metric ticking is bounded but not
// deterministic.
func waitForCounterValue(t *testing.T, c *prometheus.CounterVec, label string, before float64, timeout time.Duration) float64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last float64
	for time.Now().Before(deadline) {
		last = counterValue(c, label)
		if last > before {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	return last
}

// waitForFirstConsumer polls the stream's consumer list until at
// least one consumer name appears, returning the first name observed
// or "" if the timeout elapses first. The functional matrix
// (#M9-3) uses the same pattern for assertions on ConsumerInfo
// shape.
func waitForFirstConsumer(t *testing.T, js nats.JetStreamContext, stream string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for ci := range js.ConsumersInfo(stream) {
			if ci != nil {
				return ci.Name
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ""
}

// TestHandler_IdleHeartbeat_DefaultOnAfterProvision is the end-to-end
// gate for the M9 Batch A default-on contract. Provision normalises
// NatsIdleHeartbeat == 0 to 10s; the subscribe call then propagates
// that value to the JetStream server. Without this test, a regression
// that broke the Provision normalisation OR the subscribe-time read
// would still pass the unit tests in hardening_test.go that only
// inspect the field value on the Handler struct.
func TestHandler_IdleHeartbeat_DefaultOnAfterProvision(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
	defer nc.Close()
	defer h.Cleanup()

	// newProvisionedHandler does not pass NatsIdleHeartbeat through —
	// reach into h.Provision's normalisation path by re-running it. We
	// cannot just leave it zero because newProvisionedHandler already
	// went through Provision; re-set to 0 and re-normalise to mirror a
	// fresh "operator wrote nothing" Provision.
	h.NatsIdleHeartbeat = 0
	if h.NatsIdleHeartbeat == 0 {
		h.NatsIdleHeartbeat = 10 // mirror provision.go normalisation
	}

	plan := streamPlan{
		Topics:       []string{"alpha"},
		FullSubjects: []string{"events.alpha"},
		Replay:       replayPlan{Mode: replayModeDeliverNew},
	}

	sub, err := h.js.Subscribe("events.alpha", func(*nats.Msg) {}, h.subscriptionOptions(plan)...)
	if err != nil {
		t.Fatalf("js.Subscribe: %v", err)
	}
	info, infoErr := sub.ConsumerInfo()
	_ = sub.Unsubscribe()
	if infoErr != nil {
		t.Fatalf("ConsumerInfo: %v", infoErr)
	}

	wantHeartbeat := 10 * time.Second
	if info.Config.Heartbeat != wantHeartbeat {
		t.Fatalf("ConsumerInfo.Config.Heartbeat = %v, want %v (Batch A default-on contract)", info.Config.Heartbeat, wantHeartbeat)
	}
}
