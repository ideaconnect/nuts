package nuts

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
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
	h.logger = zap.NewNop()
	t.Cleanup(func() { _ = h.Cleanup() })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = h.ServeHTTP(w, r, nil) }))
	t.Cleanup(srv.Close)
	return h, srv
}

// deliveryModes runs a delivery contract test twice: with every connection on
// its own consumer, and with shared subscriptions.
func deliveryModes(t *testing.T, test func(t *testing.T, mode func(*Handler))) {
	t.Run("own consumers", func(t *testing.T) { test(t, func(*Handler) {}) })
	t.Run("shared subscriptions", func(t *testing.T) { test(t, func(h *Handler) { h.SharedSubscriptions = true }) })
}

// TestDeliveryContract_NATSLinkLossLeavesNoHole: messages the server pushed
// into a NATS connection that died silently used to be lost for good (#99).
// The ordered consumer notices the reconnect and resumes after the last
// message it actually delivered.
func TestDeliveryContract_NATSLinkLossLeavesNoHole(t *testing.T) {
	deliveryModes(t, func(t *testing.T, mode func(*Handler)) {
		ns := startJetStreamServer(t)
		t.Cleanup(ns.Shutdown) // outlives the handler, whose Cleanup deletes consumers
		nc, err := nats.Connect(ns.ClientURL())
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(nc.Close)
		createTestStream(t, nc, "EVENTS", []string{"events.>"})
		js, _ := nc.JetStream()
		proxy := newBlackholeProxy(t, ns.Addr().String())
		h, srv := newContractServer(t, proxy.url(), mode)

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
		waitForNATSReconnect(t, h)
		publishRange(t, js, "events.alpha", 7, 9)

		assertContiguousIDs(t, stream.collectIDs(6, 15*time.Second), 4, 9)
		select {
		case <-stream.closed:
			t.Fatal("stream closed; the client would have had to reconnect")
		default:
		}
	})
}

// TestDeliveryContract_LinkLossBeforeFirstMessageNeitherReplaysNorSkips: an
// ordered consumer that resets before delivering anything re-applies its
// original deliver policy. With DeliverNew that skipped everything published
// during the outage, and the legacy nats.go ordered consumer restarted from
// sequence 1 and replayed the whole stream. Requests without a cursor start
// at an explicit LastSeq+1, which survives the reset.
func TestDeliveryContract_LinkLossBeforeFirstMessageNeitherReplaysNorSkips(t *testing.T) {
	deliveryModes(t, func(t *testing.T, mode func(*Handler)) {
		ns := startJetStreamServer(t)
		t.Cleanup(ns.Shutdown) // outlives the handler, whose Cleanup deletes consumers
		nc, err := nats.Connect(ns.ClientURL())
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(nc.Close)
		createTestStream(t, nc, "EVENTS", []string{"events.>"})
		js, _ := nc.JetStream()
		publishRange(t, js, "events.alpha", 1, 3) // history the stream must not replay
		proxy := newBlackholeProxy(t, ns.Addr().String())
		h, srv := newContractServer(t, proxy.url(), mode)

		stream := openSSEStream(t, srv.URL+"/events?topic=alpha", "")
		if connected := stream.collectIDs(1, 3*time.Second); len(connected) != 1 || connected[0] != 3 {
			t.Fatalf("connected id = %v, want [3]", connected)
		}
		proxy.discard.Store(true)
		publishRange(t, js, "events.alpha", 4, 6) // published while NUTS is cut off
		time.Sleep(300 * time.Millisecond)
		proxy.cut()
		proxy.discard.Store(false)
		waitForNATSReconnect(t, h)
		publishRange(t, js, "events.alpha", 7, 8)

		assertContiguousIDs(t, stream.collectIDs(5, 15*time.Second), 4, 8)
	})
}

// waitForNATSReconnect waits until the handler's NATS connection has
// reconnected at least once and its JetStream context is usable again.
func waitForNATSReconnect(t *testing.T, h *Handler) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rt := h.currentStreamRuntime()
		h.mu.RLock()
		conn := h.conn
		h.mu.RUnlock()
		if rt.js != nil && conn != nil && conn.IsConnected() && conn.Stats().Reconnects > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("NUTS did not reconnect through the proxy")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestDeliveryContract_LargeBacklogOnOneConnection: a client reconnecting
// behind a large backlog used to be cut off as a slow client after a handful
// of messages, needing hundreds of reconnects to catch up (#100).
func TestDeliveryContract_LargeBacklogOnOneConnection(t *testing.T) {
	deliveryModes(t, func(t *testing.T, mode func(*Handler)) {
		ns := startJetStreamServer(t)
		t.Cleanup(ns.Shutdown) // outlives the handler, whose Cleanup deletes consumers
		nc, _ := nats.Connect(ns.ClientURL())
		t.Cleanup(nc.Close)
		createTestStream(t, nc, "EVENTS", []string{"events.>"})
		js, _ := nc.JetStream()
		_, srv := newContractServer(t, ns.ClientURL(), mode) // default client_buffer_size
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
	})
}

// TestDeliveryContract_LiveBurstKeepsFastClientsConnected: a single publish
// batch larger than client_buffer_size used to disconnect every connected
// client (#100).
func TestDeliveryContract_LiveBurstKeepsFastClientsConnected(t *testing.T) {
	deliveryModes(t, func(t *testing.T, mode func(*Handler)) {
		ns := startJetStreamServer(t)
		t.Cleanup(ns.Shutdown) // outlives the handler, whose Cleanup deletes consumers
		nc, _ := nats.Connect(ns.ClientURL())
		t.Cleanup(nc.Close)
		createTestStream(t, nc, "EVENTS", []string{"events.>"})
		js, _ := nc.JetStream()
		_, srv := newContractServer(t, ns.ClientURL(), mode)
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
	})
}

// TestDeliveryContract_CursorFromARecreatedStreamStillDelivers covers #103: a
// cursor from before the stream was recreated used to park the consumer until
// the new stream reached it, silently skipping everything before.
func TestDeliveryContract_CursorFromARecreatedStreamStillDelivers(t *testing.T) {
	deliveryModes(t, func(t *testing.T, mode func(*Handler)) {
		ns := startJetStreamServer(t)
		t.Cleanup(ns.Shutdown) // outlives the handler, whose Cleanup deletes consumers
		nc, _ := nats.Connect(ns.ClientURL())
		t.Cleanup(nc.Close)
		createTestStream(t, nc, "EVENTS", []string{"events.>"})
		js, _ := nc.JetStream()
		_, srv := newContractServer(t, ns.ClientURL(), mode)
		publishRange(t, js, "events.alpha", 1, 50)

		if err := js.DeleteStream("EVENTS"); err != nil {
			t.Fatalf("DeleteStream: %v", err)
		}
		createTestStream(t, nc, "EVENTS", []string{"events.>"})
		publishRange(t, js, "events.alpha", 1, 5)

		stream := openSSEStream(t, srv.URL+"/events?topic=alpha", "50")
		// The fallback's connected event carries no id, so the first id is
		// the recreated stream's first message.
		assertContiguousIDs(t, stream.collectIDs(5, 5*time.Second), 1, 5)
	})
}

// TestDeliveryContract_DeletedResumeMessageDoesNotReplayHistory covers #115:
// with replay_window set, a deleted resume message used to force a
// time-window fallback that re-sent messages the client already had.
func TestDeliveryContract_DeletedResumeMessageDoesNotReplayHistory(t *testing.T) {
	deliveryModes(t, func(t *testing.T, mode func(*Handler)) {
		ns := startJetStreamServer(t)
		t.Cleanup(ns.Shutdown) // outlives the handler, whose Cleanup deletes consumers
		nc, _ := nats.Connect(ns.ClientURL())
		t.Cleanup(nc.Close)
		createTestStream(t, nc, "EVENTS", []string{"events.>"})
		js, _ := nc.JetStream()
		_, srv := newContractServer(t, ns.ClientURL(), func(h *Handler) { mode(h); h.ReplayWindow = 3600 })
		publishRange(t, js, "events.alpha", 1, 5)
		if err := js.DeleteMsg("EVENTS", 3); err != nil {
			t.Fatalf("DeleteMsg: %v", err)
		}

		stream := openSSEStream(t, srv.URL+"/events?topic=alpha", "2")
		ids := stream.collectIDs(3, 3*time.Second)
		if want := []uint64{2, 4, 5}; !reflect.DeepEqual(ids, want) {
			t.Fatalf("ids = %v, want %v (connected cursor 2, then 4 and 5; nothing before the cursor)", ids, want)
		}
		if extra := stream.collectIDs(1, 300*time.Millisecond); len(extra) != 0 {
			t.Fatalf("unexpected extra ids %v", extra)
		}
	})
}

// TestDeliveryContract_EventSourceReconnectWithURLCursorMakesProgress covers
// #102: the documented EventSource pattern puts ?last-id= in the URL, which
// the browser resends on every auto-reconnect. With replay_max_messages the
// URL cursor used to win, replaying the same first N messages forever.
func TestDeliveryContract_EventSourceReconnectWithURLCursorMakesProgress(t *testing.T) {
	deliveryModes(t, func(t *testing.T, mode func(*Handler)) {
		ns := startJetStreamServer(t)
		t.Cleanup(ns.Shutdown) // outlives the handler, whose Cleanup deletes consumers
		nc, _ := nats.Connect(ns.ClientURL())
		t.Cleanup(nc.Close)
		createTestStream(t, nc, "EVENTS", []string{"events.>"})
		js, _ := nc.JetStream()
		_, srv := newContractServer(t, ns.ClientURL(), func(h *Handler) { mode(h); h.ReplayMaxMessages = 5 })
		publishRange(t, js, "events.alpha", 1, 20)

		lastEventID := ""
		var received []uint64
		for reconnect := 0; reconnect < 4; reconnect++ {
			stream := openSSEStream(t, srv.URL+"/events?topic=alpha&last-id=0", lastEventID)
			ids := stream.collectIDs(6, 3*time.Second) // connected cursor + 5 replayed
			if len(ids) != 6 {
				t.Fatalf("reconnect %d: ids = %v, want the cursor and 5 messages", reconnect, ids)
			}
			received = append(received, ids[1:]...)
			lastEventID = strconv.FormatUint(ids[len(ids)-1], 10)
			<-stream.closed // replay_max_messages closes the stream
		}
		assertContiguousIDs(t, received, 1, 20)
	})
}

// TestJetStream_StartSequenceOutsideTheStream pins the server behaviour that
// replay planning relies on. A start below retention is clamped to FirstSeq
// without an error, which is why NUTS no longer retries the subscribe (#97).
// A start past the end is accepted and parks the consumer until the stream
// reaches it, which is why planning falls back for such cursors (#103).
func TestJetStream_StartSequenceOutsideTheStream(t *testing.T) {
	ns := startJetStreamServer(t)
	defer ns.Shutdown()
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})
	legacy, _ := nc.JetStream()
	publishRange(t, legacy, "events.alpha", 1, 5)
	if err := legacy.PurgeStream("EVENTS", &nats.StreamPurgeRequest{Sequence: 4}); err != nil {
		t.Fatalf("purge: %v", err)
	}
	js := mustJetStream(t, nc)

	firstDelivered := func(t *testing.T, startSeq uint64) (uint64, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cons, err := js.OrderedConsumer(ctx, "EVENTS", jetstream.OrderedConsumerConfig{
			FilterSubjects: []string{"events.alpha"},
			DeliverPolicy:  jetstream.DeliverByStartSequencePolicy,
			OptStartSeq:    startSeq,
		})
		if err != nil {
			t.Fatalf("OrderedConsumer(OptStartSeq=%d): %v", startSeq, err)
		}
		msg, err := cons.Next(jetstream.FetchMaxWait(time.Second))
		if err != nil {
			return 0, err
		}
		meta, err := msg.Metadata()
		if err != nil {
			t.Fatalf("metadata: %v", err)
		}
		return meta.Sequence.Stream, nil
	}

	t.Run("below retention starts at FirstSeq", func(t *testing.T) {
		if got, err := firstDelivered(t, 1); err != nil || got != 4 {
			t.Fatalf("first delivered = %d (err %v), want 4", got, err)
		}
	})
	t.Run("past the end delivers nothing yet", func(t *testing.T) {
		if got, err := firstDelivered(t, 50); !errors.Is(err, nats.ErrTimeout) {
			t.Fatalf("first delivered = %d (err %v), want a timeout", got, err)
		}
	})
}
