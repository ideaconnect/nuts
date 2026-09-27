package nuts

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// ── Embedded NATS and JetStream ───────────────────────────────────────────

// startJetStreamServer starts an embedded JetStream server on a free port
// with its own store directory; options adjust the server's options before it
// starts. The server shuts down when the test ends, if the test has not shut
// it down already.
func startJetStreamServer(t *testing.T, options ...func(*server.Options)) *server.Server {
	t.Helper()
	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1, // a free port
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	for _, option := range options {
		option(opts)
	}
	ns, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("failed to create NATS server: %v", err)
	}
	go ns.Start()
	// Registered before the readiness check, so a server slow to start does
	// not outlive a failed test while holding its port.
	t.Cleanup(ns.Shutdown)
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS server not ready")
	}
	return ns
}

// startRestartableJetStreamServer starts a server like startJetStreamServer
// and returns a function that starts it again, once the test has shut it
// down, on the same port and store directory: clients reconnect to the same
// URL and file-backed streams survive. The first start takes a free port
// itself, so no other process can grab the port before the server holds it.
func startRestartableJetStreamServer(t *testing.T) (*server.Server, func() *server.Server) {
	t.Helper()
	storeDir := t.TempDir()
	ns := startJetStreamServer(t, func(o *server.Options) { o.StoreDir = storeDir })
	port := ns.Addr().(*net.TCPAddr).Port
	restart := func() *server.Server {
		t.Helper()
		return startJetStreamServer(t, func(o *server.Options) {
			o.Port = port
			o.StoreDir = storeDir
		})
	}
	return ns, restart
}

// createTestStream creates a JetStream stream for testing
func createTestStream(t *testing.T, nc *nats.Conn, streamName string, subjects []string) {
	t.Helper()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("failed to get JetStream context: %v", err)
	}
	_, err = js.AddStream(&nats.StreamConfig{
		Name:     streamName,
		Subjects: subjects,
		Storage:  nats.MemoryStorage,
	})
	if err != nil {
		t.Fatalf("failed to create stream: %v", err)
	}
}

// mustJetStream returns a jetstream API handle on conn.
func mustJetStream(t *testing.T, conn *nats.Conn) jetstream.JetStream {
	t.Helper()
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	return js
}

func publishRange(t *testing.T, js nats.JetStreamContext, subject string, from, to int) {
	t.Helper()
	for i := from; i <= to; i++ {
		if _, err := js.PublishAsync(subject, []byte(`{"n":`+strconv.Itoa(i)+`}`)); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	select {
	case <-js.PublishAsyncComplete():
	case <-time.After(10 * time.Second):
		t.Fatal("publishes not acknowledged")
	}
}

func waitForConsumerCount(t *testing.T, js jetstream.JetStream, stream string, want int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if consumerCount(js, stream) == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return consumerCount(js, stream) == want
}

// consumerCount returns the stream's consumer count, or -1 when the stream
// cannot be read.
func consumerCount(js jetstream.JetStream, stream string) int {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s, err := js.Stream(ctx, stream)
	if err != nil {
		return -1
	}
	return s.CachedInfo().State.Consumers
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

// blackholeProxy forwards TCP between NUTS and nats-server. discard() makes
// it swallow traffic in both directions without closing anything, like a link
// that died silently; cut() then closes every connection so the client
// notices and reconnects through the proxy.
type blackholeProxy struct {
	ln      net.Listener
	target  string
	discard atomic.Bool
	mu      sync.Mutex
	conns   []net.Conn
}

func (p *blackholeProxy) url() string { return "nats://" + p.ln.Addr().String() }

func (p *blackholeProxy) acceptLoop() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, client, server)
		p.mu.Unlock()
		go p.pipe(server, client)
		go p.pipe(client, server)
	}
}

func (p *blackholeProxy) pipe(src, dst net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 && !p.discard.Load() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				_ = src.Close()
				return
			}
		}
		if err != nil {
			_ = dst.Close()
			return
		}
	}
}

func (p *blackholeProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

func newBlackholeProxy(t *testing.T, target string) *blackholeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &blackholeProxy{ln: ln, target: target}
	go p.acceptLoop()
	t.Cleanup(func() { _ = ln.Close(); p.cut() })
	return p
}

// ── Handlers ──────────────────────────────────────────────────────────────

// newProvisionedHandler starts NATS, creates the EVENTS test stream with
// memory storage, and runs the real Provision so streaming tests see the
// production defaults and lifecycle (idle heartbeat, topic cap, shutdown
// channel). The logger is a no-op; tests that assert logs replace it. The
// server stops when the test ends; callers clean up the handler and close
// the connection.
func newProvisionedHandler(t *testing.T) (*Handler, *server.Server, *nats.Conn) {
	t.Helper()
	ns := startJetStreamServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
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
	if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
		nc.Close()
		t.Fatalf("Provision: %v", err)
	}
	return h, ns, nc
}

// connectHandler connects h to NATS and gives it a JetStream context without
// running Provision, for tests that need settings Provision would reject or
// replace. It returns the context and cleans the handler up when the test
// ends.
func connectHandler(t *testing.T, h *Handler) jetstream.JetStream {
	t.Helper()
	if err := h.connectNATS(); err != nil {
		t.Fatalf("connectNATS: %v", err)
	}
	t.Cleanup(func() { _ = h.Cleanup() })
	js, err := jetstream.New(h.conn)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	h.mu.Lock()
	h.js = js
	h.mu.Unlock()
	return js
}

// provisionOnStream creates the stream described by cfg on a fresh embedded
// server and provisions a handler for it, for tests that need stream
// settings createTestStream does not offer. The handler logs nowhere unless
// configure sets a logger. Everything is torn down when the test ends.
func provisionOnStream(t *testing.T, cfg jetstream.StreamConfig, configure func(*Handler)) (*Handler, *nats.Conn) {
	t.Helper()
	ns := startJetStreamServer(t)
	t.Cleanup(ns.Shutdown)
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := mustJetStream(t, nc).CreateStream(ctx, cfg); err != nil {
		t.Fatalf("CreateStream: %v", err)
	}
	h := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        cfg.Name,
		TopicPrefix:       "events.",
		HeartbeatInterval: 30,
		MaxEventSize:      -1,
		logger:            zap.NewNop(),
	}
	if configure != nil {
		configure(h)
	}
	if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() { _ = h.Cleanup() })
	return h, nc
}

// newContractServer provisions a handler against natsURL and serves it over
// real HTTP. The handler logs nowhere unless configure sets a logger.
func newContractServer(t *testing.T, natsURL string, configure func(*Handler)) (*Handler, *httptest.Server) {
	t.Helper()
	h := &Handler{NatsURL: natsURL, StreamName: "EVENTS", TopicPrefix: "events.", ReconnectWait: 1, MaxReconnects: intPtr(-1), logger: zap.NewNop()}
	if configure != nil {
		configure(h)
	}
	if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() { _ = h.Cleanup() })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = h.ServeHTTP(w, r, nil) }))
	t.Cleanup(srv.Close)
	return h, srv
}

// intPtr is a small helper for constructing *int fields in struct literals.
func intPtr(v int) *int { return &v }

// ── Response writers ──────────────────────────────────────────────────────

// flushRecorder wraps httptest.ResponseRecorder to implement http.Flusher
type flushRecorder struct {
	*httptest.ResponseRecorder
}

func (f *flushRecorder) Flush() {
	// No-op for testing, actual flushing happens in real HTTP response
}

type plainRecorder struct {
	header     http.Header
	statusCode int
	body       strings.Builder
}

func (p *plainRecorder) Header() http.Header {
	return p.header
}

func (p *plainRecorder) Write(data []byte) (int, error) {
	if p.statusCode == 0 {
		p.statusCode = http.StatusOK
	}
	return p.body.Write(data)
}

func (p *plainRecorder) WriteHeader(statusCode int) {
	p.statusCode = statusCode
}

func newPlainRecorder() *plainRecorder {
	return &plainRecorder{header: make(http.Header)}
}

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

func newSafeRecorder() *safeFlushRecorder {
	return &safeFlushRecorder{header: make(http.Header)}
}

// slowSafeRecorder is a safeFlushRecorder whose writes take `delay`, for
// driving a slow but live reader while the test polls the body.
type slowSafeRecorder struct {
	*safeFlushRecorder
	delay time.Duration
}

func (s *slowSafeRecorder) Write(p []byte) (int, error) {
	time.Sleep(s.delay)
	return s.safeFlushRecorder.Write(p)
}

// gatedWriter lets the first allow writes through and blocks the rest until
// gate is closed, standing in for a client that has stopped reading.
type gatedWriter struct {
	*safeFlushRecorder
	allow int32
	gate  chan struct{}
	n     atomic.Int32
}

func (g *gatedWriter) Write(p []byte) (int, error) {
	if g.n.Add(1) > g.allow {
		<-g.gate
	}
	return g.safeFlushRecorder.Write(p)
}

// stalledDeadlineWriter lets the first `allowed` writes through and blocks
// every later write until the write deadline set through
// http.ResponseController passes, then fails it with os.ErrDeadlineExceeded,
// like a TCP peer that stopped reading.
type stalledDeadlineWriter struct {
	mu       sync.Mutex
	header   http.Header
	body     strings.Builder
	allowed  int
	writes   int
	deadline time.Time
}

func (s *stalledDeadlineWriter) Header() http.Header { return s.header }
func (s *stalledDeadlineWriter) WriteHeader(int)     {}
func (s *stalledDeadlineWriter) Flush()              {}

func (s *stalledDeadlineWriter) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	s.deadline = t
	s.mu.Unlock()
	return nil
}

func (s *stalledDeadlineWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	if s.writes < s.allowed {
		s.writes++
		n, err := s.body.Write(p)
		s.mu.Unlock()
		return n, err
	}
	deadline := s.deadline
	s.mu.Unlock()
	if deadline.IsZero() {
		return 0, errors.New("stalledDeadlineWriter: write blocked with no deadline set")
	}
	time.Sleep(time.Until(deadline))
	return 0, os.ErrDeadlineExceeded
}

func (s *stalledDeadlineWriter) Body() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.String()
}

func newStalledDeadlineWriter(allowed int) *stalledDeadlineWriter {
	return &stalledDeadlineWriter{header: make(http.Header), allowed: allowed}
}

// flushLog is a response writer that records what was written before each
// flush, and every write deadline, safe to read while a stream writes.
type flushLog struct {
	mu        sync.Mutex
	header    http.Header
	pending   strings.Builder
	flushed   []string
	deadlines []time.Time
}

func (f *flushLog) Header() http.Header { return f.header }
func (f *flushLog) WriteHeader(int)     {}

func (f *flushLog) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending.Write(p)
}

func (f *flushLog) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushed = append(f.flushed, f.pending.String())
	f.pending.Reset()
}

func (f *flushLog) SetWriteDeadline(deadline time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deadlines = append(f.deadlines, deadline)
	return nil
}

// batches returns what each flush carried.
func (f *flushLog) batches() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.flushed...)
}

func newFlushLog() *flushLog { return &flushLog{header: make(http.Header)} }

// ── SSE streams ───────────────────────────────────────────────────────────

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

func parseSSEIDs(t *testing.T, body string) []uint64 {
	t.Helper()
	var ids []uint64
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "id: ") {
			continue
		}
		id, err := strconv.ParseUint(strings.TrimPrefix(line, "id: "), 10, 64)
		if err != nil {
			t.Fatalf("parse SSE id from %q: %v", line, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// Delivery-contract tests: end-to-end properties of the SSE stream that the
// v0.4 push-consumer pipeline violated. Each one publishes through a separate
// NATS connection and reads the stream over real HTTP.

// sseReader reads `id:` values from a live SSE response.
type sseReader struct {
	resp   *http.Response
	ids    chan uint64
	closed chan struct{}
}

// collectIDs reads ids until `want` of them arrived, the stream closed or the
// timeout passed.
func (r *sseReader) collectIDs(want int, timeout time.Duration) []uint64 {
	var got []uint64
	deadline := time.After(timeout)
	for len(got) < want {
		select {
		case id := <-r.ids:
			got = append(got, id)
		case <-r.closed:
			for len(r.ids) > 0 {
				got = append(got, <-r.ids)
			}
			return got
		case <-deadline:
			return got
		}
	}
	return got
}

func openSSEStream(t *testing.T, url, lastEventID string) *sseReader {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	r := &sseReader{resp: resp, ids: make(chan uint64, 16384), closed: make(chan struct{})}
	go func() {
		defer close(r.closed)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
		for sc.Scan() {
			// The connected event's id arrives first; callers account for it.
			if v, ok := strings.CutPrefix(sc.Text(), "id: "); ok {
				id, _ := strconv.ParseUint(v, 10, 64)
				r.ids <- id
			}
		}
	}()
	t.Cleanup(func() { _ = resp.Body.Close() })
	return r
}

// assertContiguousIDs fails unless ids is exactly from, from+1, ..., to.
func assertContiguousIDs(t *testing.T, ids []uint64, from, to uint64) {
	t.Helper()
	if uint64(len(ids)) != to-from+1 {
		t.Fatalf("got %d ids, want %d (%d..%d); first=%v last=%v", len(ids), to-from+1, from, to, head(ids, 5), lastN(ids, 5))
	}
	for i, id := range ids {
		if id != from+uint64(i) {
			t.Fatalf("ids[%d] = %d, want %d: gap or duplicate; around=%v", i, id, from+uint64(i), ids[max(0, i-3):min(len(ids), i+3)])
		}
	}
}

func head(ids []uint64, n int) []uint64  { return ids[:min(n, len(ids))] }
func lastN(ids []uint64, n int) []uint64 { return ids[max(0, len(ids)-n):] }

// assertRetryAfter checks the Retry-After header rejectTransient sends to
// clients that do not accept an event stream: whole seconds covering the
// jittered delay.
func assertRetryAfter(t *testing.T, header http.Header) {
	t.Helper()
	got, err := strconv.Atoi(header.Get("Retry-After"))
	if err != nil || got < 3 || got > 8 {
		t.Fatalf("Retry-After = %q, want whole seconds in [3, 8]", header.Get("Retry-After"))
	}
}

// assertRetryStream checks the answer rejectTransient gives EventSource
// clients: a 200 event stream holding only a comment with the reason and a
// jittered retry: delay, so the browser reconnects instead of giving up.
func assertRetryStream(t *testing.T, rr *httptest.ResponseRecorder, reason string) {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 so EventSource keeps reconnecting; body=%q", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := rr.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q on a retry stream, want none", got)
	}
	body := rr.Body.String()
	prefix := ": " + reason + "\nretry: "
	if !strings.HasPrefix(body, prefix) || !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("body = %q, want %q + delay + blank line", body, prefix)
	}
	ms, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(body, prefix), "\n\n"))
	if err != nil || ms < 2500 || ms >= 7500 {
		t.Fatalf("retry = %q ms, want [2500, 7500)", strings.TrimSuffix(strings.TrimPrefix(body, prefix), "\n\n"))
	}
}

// serveQueuedFrames runs serveStream over a feed whose frames are all queued
// before the stream starts, and returns once it has written wantFlushes
// flushes (the connected event included) or ended.
func serveQueuedFrames(t *testing.T, h *Handler, plan streamPlan, frames []formattedMessageEvent, wantFlushes int) (*flushLog, bool) {
	t.Helper()
	ch := make(chan formattedMessageEvent, len(frames))
	for _, f := range frames {
		ch <- f
	}
	feed := &streamFeed{frames: ch, errs: make(chan error), stop: func() {}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newFlushLog()
	done := make(chan struct{})
	go func() {
		_ = h.serveStream(w, httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx), plan, feed, nil)
		close(done)
	}()
	deadline := time.After(3 * time.Second)
	for len(w.batches()) < wantFlushes {
		select {
		case <-done:
			return w, true
		case <-deadline:
			t.Fatalf("stream flushed %d times, want %d", len(w.batches()), wantFlushes)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
	return w, false
}

// ── Fakes for the stream path ─────────────────────────────────────────────

// fakeJSMsg is a jetstream.Msg with fixed subject, data and metadata. Methods
// the stream path never calls panic through the nil embedded interface.
type fakeJSMsg struct {
	jetstream.Msg
	subject string
	data    []byte
	header  nats.Header
	meta    *jetstream.MsgMetadata
	metaErr error
}

func (m fakeJSMsg) Subject() string      { return m.subject }
func (m fakeJSMsg) Data() []byte         { return m.data }
func (m fakeJSMsg) Headers() nats.Header { return m.header }
func (m fakeJSMsg) Metadata() (*jetstream.MsgMetadata, error) {
	return m.meta, m.metaErr
}

// newFakeJSMsg builds a message delivered by consumer at stream sequence seq.
func newFakeJSMsg(subject string, seq uint64, consumer string, data string) fakeJSMsg {
	return fakeJSMsg{
		subject: subject,
		data:    []byte(data),
		meta: &jetstream.MsgMetadata{
			Sequence:  jetstream.SequencePair{Stream: seq, Consumer: seq},
			Consumer:  consumer,
			Stream:    "EVENTS",
			Timestamp: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC),
		},
	}
}

// fakeIterator is a jetstream.MessagesContext fed from a channel. Next blocks
// until a message, an error, Stop or the end of ctx arrives, like the real
// iterator given ctx through jetstream.NextContext; a feed under test is
// started with ctx and cancel (startFeed). Stop records whether it was
// called while a Next was waiting, which the real ordered consumer does not
// allow (see startStreamFeed).
type fakeIterator struct {
	msgs    chan jetstream.Msg
	errs    chan error
	stopped chan struct{}
	once    sync.Once
	nexts   int
	mu      sync.Mutex

	ctx    context.Context
	cancel context.CancelFunc
	// inNext counts Next calls in progress; stopDuringNext is set when Stop
	// is called while one is.
	inNext         atomic.Int32
	stopDuringNext atomic.Bool
}

func (f *fakeIterator) Next(...jetstream.NextOpt) (jetstream.Msg, error) {
	f.inNext.Add(1)
	defer f.inNext.Add(-1)
	f.mu.Lock()
	f.nexts++
	f.mu.Unlock()
	select {
	case <-f.stopped:
		return nil, jetstream.ErrMsgIteratorClosed
	default:
	}
	select {
	case msg := <-f.msgs:
		return msg, nil
	case err := <-f.errs:
		return nil, err
	case <-f.stopped:
		return nil, jetstream.ErrMsgIteratorClosed
	case <-f.ctx.Done():
		return nil, f.ctx.Err()
	}
}

func (f *fakeIterator) Stop() {
	if f.inNext.Load() > 0 {
		f.stopDuringNext.Store(true)
	}
	f.once.Do(func() { close(f.stopped) })
}

// startFeed starts a stream feed over the fake with the fake's context.
func (f *fakeIterator) startFeed(h *Handler, plan streamPlan) *streamFeed {
	return h.startStreamFeed(f.ctx, f.cancel, f, plan)
}
func (f *fakeIterator) Drain()                  { f.Stop() }
func (f *fakeIterator) Closed() <-chan struct{} { return f.stopped }

// nextCalls reports how many times Next has been called.
func (f *fakeIterator) nextCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nexts
}

func newFakeIterator(buffer int) *fakeIterator {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeIterator{
		msgs:    make(chan jetstream.Msg, buffer),
		errs:    make(chan error, 1),
		stopped: make(chan struct{}),
		ctx:     ctx,
		cancel:  cancel,
	}
}

// closedChannel returns a channel that is already closed.
func closedChannel() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

var errFakeResetFailed = errors.New("ordered consumer reset failed")

// fakeConsumer is a jetstream.Consumer with fixed cached info.
type fakeConsumer struct {
	jetstream.Consumer
	info *jetstream.ConsumerInfo
}

func (f fakeConsumer) CachedInfo() *jetstream.ConsumerInfo { return f.info }

var testFeedPlan = streamPlan{Topics: []string{"alpha"}, FullSubjects: []string{"events.alpha"}}

// newTestSharedSub builds a shared subscription without a JetStream consumer,
// registered under testSharedKey. stopped counts calls that would stop its
// feed.
var testSharedKey = sharedTopics{subjects: "k"}

func newTestSharedSub(floor uint64) (*sharedSub, *int) {
	stopped := 0
	registry := newSharedRegistry()
	sub := &sharedSub{
		h:        &Handler{logger: zap.NewNop()},
		registry: registry,
		key:      testSharedKey,
		stream: &consumerStream{
			feed:     &streamFeed{stop: func() { stopped++ }, exited: closedChannel()},
			consumer: fakeConsumer{},
			release:  func() {},
			log:      zap.NewNop(),
		},
		done:    make(chan struct{}),
		floor:   floor,
		clients: map[*sharedClient]struct{}{},
	}
	registry.subs[testSharedKey] = sub
	metricsSharedSubscriptions.Inc()
	return sub, &stopped
}

// ── Metrics and logs ──────────────────────────────────────────────────────

// metricValue reads a counter's or a gauge's current value. For one series
// of a labelled metric, pass vec.WithLabelValues(labels...).
func metricValue(t testing.TB, m prometheus.Metric) float64 {
	t.Helper()
	pb := &iopm.Metric{}
	if err := m.Write(pb); err != nil {
		t.Fatalf("read metric: %v", err)
	}
	if pb.Gauge != nil {
		return pb.GetGauge().GetValue()
	}
	return pb.GetCounter().GetValue()
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

// hasIntLogField reports whether any observed entry carries an integer field
// with the given key and value.
func hasIntLogField(obs *observer.ObservedLogs, key string, value int64) bool {
	for _, entry := range obs.All() {
		for _, f := range entry.Context {
			if f.Key == key && f.Integer == value {
				return true
			}
		}
	}
	return false
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ── Subscriber JWTs ───────────────────────────────────────────────────────

func signTestSubscriberJWT(t *testing.T, secret string, claims map[string]interface{}) string {
	t.Helper()
	encodedHeader := encodeTestJWTPart(t, map[string]interface{}{"alg": "HS256", "typ": "JWT"})
	encodedPayload := encodeTestJWTPart(t, claims)
	signed := encodedHeader + "." + encodedPayload
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signed))
	return signed + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func encodeTestJWTPart(t *testing.T, value interface{}) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JWT part: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
