package nuts

import (
	"context"
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
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
)

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
// until a message, an error or Stop arrives, like the real iterator.
type fakeIterator struct {
	msgs    chan jetstream.Msg
	errs    chan error
	stopped chan struct{}
	once    sync.Once
	nexts   int
	mu      sync.Mutex
}

func newFakeIterator(buffer int) *fakeIterator {
	return &fakeIterator{
		msgs:    make(chan jetstream.Msg, buffer),
		errs:    make(chan error, 1),
		stopped: make(chan struct{}),
	}
}

func (f *fakeIterator) Next(...jetstream.NextOpt) (jetstream.Msg, error) {
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
	}
}

func (f *fakeIterator) Stop()                   { f.once.Do(func() { close(f.stopped) }) }
func (f *fakeIterator) Drain()                  { f.Stop() }
func (f *fakeIterator) Closed() <-chan struct{} { return f.stopped }

// nextCalls reports how many times Next has been called.
func (f *fakeIterator) nextCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nexts
}

var errFakeResetFailed = errors.New("ordered consumer reset failed")

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

func newStalledDeadlineWriter(allowed int) *stalledDeadlineWriter {
	return &stalledDeadlineWriter{header: make(http.Header), allowed: allowed}
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

// mustJetStream returns a jetstream API handle on conn.
func mustJetStream(t *testing.T, conn *nats.Conn) jetstream.JetStream {
	t.Helper()
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	return js
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

// provisionOnStream creates the stream described by cfg on a fresh embedded
// server and provisions a handler for it, for tests that need stream
// settings createTestStream does not offer. Everything is torn down when the
// test ends.
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
	}
	if configure != nil {
		configure(h)
	}
	if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	h.logger = zap.NewNop()
	t.Cleanup(func() { _ = h.Cleanup() })
	return h, nc
}
