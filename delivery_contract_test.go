package nuts

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/nats-io/nats.go"
)

// Delivery-contract tests: end-to-end properties of the SSE stream that the
// v0.4 push-consumer pipeline violated. Each one publishes through a separate
// NATS connection and reads the stream over real HTTP.

// sseReader reads `id:` values from a live SSE response.
type sseReader struct {
	resp   *http.Response
	ids    chan uint64
	closed chan struct{}
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

// newContractServer provisions a handler against natsURL and serves it over
// real HTTP.
func newContractServer(t *testing.T, natsURL string, configure func(*Handler)) (*Handler, *httptest.Server) {
	t.Helper()
	h := &Handler{NatsURL: natsURL, StreamName: "EVENTS", TopicPrefix: "events.", ReconnectWait: 1, MaxReconnects: intPtr(-1)}
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

// TestDeliveryContract_NATSLinkLossLeavesNoHole: messages the server pushed
// into a NATS connection that died silently used to be lost for good (#99).
// The ordered consumer notices the reconnect and resumes after the last
// message it actually delivered.
func TestDeliveryContract_NATSLinkLossLeavesNoHole(t *testing.T) {
	ns := startJetStreamServer(t)
	defer ns.Shutdown()
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})
	js, _ := nc.JetStream()
	proxy := newBlackholeProxy(t, ns.Addr().String())
	h, srv := newContractServer(t, proxy.url(), nil)

	stream := openSSEStream(t, srv.URL+"/events?topic=alpha", "")
	if connected := stream.collectIDs(1, 3*time.Second); len(connected) != 1 || connected[0] != 0 {
		t.Fatalf("connected id = %v, want [0]", connected)
	}
	publishRange(t, js, "events.alpha", 1, 3)
	assertContiguousIDs(t, stream.collectIDs(3, 3*time.Second), 1, 3)

	proxy.discard.Store(true)
	publishRange(t, js, "events.alpha", 4, 6) // pushed into the dead link
	time.Sleep(300 * time.Millisecond)
	proxy.cut()
	proxy.discard.Store(false)
	deadline := time.Now().Add(10 * time.Second)
	for {
		rt := h.currentStreamRuntime()
		h.mu.RLock()
		conn := h.conn
		h.mu.RUnlock()
		if rt.js != nil && conn != nil && conn.IsConnected() && conn.Stats().Reconnects > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("NUTS did not reconnect through the proxy")
		}
		time.Sleep(50 * time.Millisecond)
	}
	publishRange(t, js, "events.alpha", 7, 9)

	assertContiguousIDs(t, stream.collectIDs(6, 15*time.Second), 4, 9)
	select {
	case <-stream.closed:
		t.Fatal("stream closed; the client would have had to reconnect")
	default:
	}
}

// TestDeliveryContract_LargeBacklogOnOneConnection: a client reconnecting
// behind a large backlog used to be cut off as a slow client after a handful
// of messages, needing hundreds of reconnects to catch up (#100).
func TestDeliveryContract_LargeBacklogOnOneConnection(t *testing.T) {
	ns := startJetStreamServer(t)
	defer ns.Shutdown()
	nc, _ := nats.Connect(ns.ClientURL())
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})
	js, _ := nc.JetStream()
	_, srv := newContractServer(t, ns.ClientURL(), nil) // default client_buffer_size
	const backlog = 3000
	publishRange(t, js, "events.alpha", 1, backlog)
	slowBefore := counterVal(t, metricsSlowClientDisconnects)

	stream := openSSEStream(t, srv.URL+"/events?topic=alpha", "0")
	ids := stream.collectIDs(backlog+1, 20*time.Second)
	if len(ids) == 0 || ids[0] != 0 {
		t.Fatalf("first id = %v, want the connected cursor 0", head(ids, 1))
	}
	assertContiguousIDs(t, ids[1:], 1, backlog)
	if got := counterVal(t, metricsSlowClientDisconnects); got != slowBefore {
		t.Fatalf("slow_client_disconnects_total moved %v -> %v during a backlog replay", slowBefore, got)
	}
}

// TestDeliveryContract_LiveBurstKeepsFastClientsConnected: a single publish
// batch larger than client_buffer_size used to disconnect every connected
// client (#100).
func TestDeliveryContract_LiveBurstKeepsFastClientsConnected(t *testing.T) {
	ns := startJetStreamServer(t)
	defer ns.Shutdown()
	nc, _ := nats.Connect(ns.ClientURL())
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})
	js, _ := nc.JetStream()
	_, srv := newContractServer(t, ns.ClientURL(), nil)
	slowBefore := counterVal(t, metricsSlowClientDisconnects)

	var streams []*sseReader
	for i := 0; i < 5; i++ {
		s := openSSEStream(t, srv.URL+"/events?topic=alpha", "")
		if connected := s.collectIDs(1, 3*time.Second); len(connected) != 1 {
			t.Fatalf("client %d: no connected event", i)
		}
		streams = append(streams, s)
	}
	const burst = 1000
	publishRange(t, js, "events.alpha", 1, burst)
	for i, s := range streams {
		ids := s.collectIDs(burst, 15*time.Second)
		if len(ids) != burst {
			t.Fatalf("client %d received %d of %d burst messages", i, len(ids), burst)
		}
		assertContiguousIDs(t, ids, 1, burst)
	}
	if got := counterVal(t, metricsSlowClientDisconnects); got != slowBefore {
		t.Fatalf("slow_client_disconnects_total moved %v -> %v during a burst", slowBefore, got)
	}
}
