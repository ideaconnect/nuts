package nuts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// deliveryModes runs a delivery contract test twice, in parallel: with every
// connection on its own consumer, and with shared subscriptions. Each mode
// has its own embedded server; the tests assert per-connection ids, and the
// only metric they check (slow-client disconnects) must not move in either.
func deliveryModes(t *testing.T, test func(t *testing.T, mode func(*Handler))) {
	t.Run("own consumers", func(t *testing.T) {
		t.Parallel()
		test(t, func(*Handler) {})
	})
	t.Run("shared subscriptions", func(t *testing.T) {
		t.Parallel()
		test(t, func(h *Handler) { h.SharedSubscriptions = true })
	})
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

		admin := mustJetStream(t, nc)
		waitForConsumer(t, admin, "EVENTS", "has a pull request waiting", func(c *jetstream.ConsumerInfo) bool { return c.NumWaiting > 0 })
		proxy.discard.Store(true)
		publishRange(t, js, "events.alpha", 4, 6) // pushed into the dead link
		waitForConsumer(t, admin, "EVENTS", "was sent 4..6", func(c *jetstream.ConsumerInfo) bool { return c.Delivered.Stream >= 6 })
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

// TestDeliveryContract_NATSRestartLeavesNoHole: a restarted NATS server has
// forgotten NUTS' ordered consumers, which only live in its memory. Each
// stream recreates its consumer after the last message it delivered, so it
// goes on without a hole or a repeat, and the client never reconnects.
func TestDeliveryContract_NATSRestartLeavesNoHole(t *testing.T) {
	deliveryModes(t, func(t *testing.T, mode func(*Handler)) {
		ns, restart := startRestartableJetStreamServer(t)
		nc, err := nats.Connect(ns.ClientURL(), nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(nc.Close)
		// On disk, so the messages outlive the restart.
		if _, err := mustJetStream(t, nc).CreateStream(context.Background(), jetstream.StreamConfig{
			Name: "EVENTS", Subjects: []string{"events.>"}, Storage: jetstream.FileStorage,
		}); err != nil {
			t.Fatalf("CreateStream: %v", err)
		}
		js, _ := nc.JetStream()
		// A restarted server keeps the stream's creation time, so the watch
		// for recreated streams (#133), polling fast here, must not end it.
		h, srv := newContractServer(t, ns.ClientURL(), func(h *Handler) {
			h.watchInterval = 100 * time.Millisecond
			mode(h)
		})

		stream := openSSEStream(t, srv.URL+"/events?topic=alpha", "")
		if connected := stream.collectIDs(1, 3*time.Second); len(connected) != 1 || connected[0] != 0 {
			t.Fatalf("connected id = %v, want [0]", connected)
		}
		publishRange(t, js, "events.alpha", 1, 3)
		assertContiguousIDs(t, stream.collectIDs(3, 3*time.Second), 1, 3)

		ns.Shutdown()
		restart()
		waitForNATSReconnect(t, h)
		publishRange(t, js, "events.alpha", 4, 6)

		assertContiguousIDs(t, stream.collectIDs(3, 15*time.Second), 4, 6)
		select {
		case <-stream.closed:
			t.Fatal("stream closed; the client would have had to reconnect")
		default:
		}
	})
}

// TestDeliveryContract_RecreatedStreamSendsClientsToTheNewOne covers #133: a
// stream deleted and created again under an open SSE stream would leave its
// consumer waiting at the old stream's position, skipping everything the new
// stream gets before it. The SSE stream closes instead, after setting the
// client's last event ID to 0, so the reconnect replays the new stream from
// its start, even once the new stream has passed the old position.
func TestDeliveryContract_RecreatedStreamSendsClientsToTheNewOne(t *testing.T) {
	deliveryModes(t, func(t *testing.T, mode func(*Handler)) {
		ns := startJetStreamServer(t)
		nc, err := nats.Connect(ns.ClientURL())
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(nc.Close)
		createTestStream(t, nc, "EVENTS", []string{"events.>"})
		js, _ := nc.JetStream()
		core, obs := observer.New(zap.InfoLevel)
		_, srv := newContractServer(t, ns.ClientURL(), func(h *Handler) {
			h.watchInterval = 100 * time.Millisecond
			h.logger = zap.New(core)
			mode(h)
		})

		stream := openSSEStream(t, srv.URL+"/events?topic=alpha", "")
		stream.collectIDs(1, 3*time.Second)
		publishRange(t, js, "events.alpha", 1, 3)
		assertContiguousIDs(t, stream.collectIDs(3, 3*time.Second), 1, 3)

		if err := mustJetStream(t, nc).DeleteStream(context.Background(), "EVENTS"); err != nil {
			t.Fatalf("DeleteStream: %v", err)
		}
		createTestStream(t, nc, "EVENTS", []string{"events.>"})
		// Past the old position: resuming from the old cursor would skip 1..3.
		publishRange(t, js, "events.alpha", 1, 5)

		select {
		case <-stream.closed:
		case <-time.After(5 * time.Second):
			t.Fatal("the SSE stream stayed open on the recreated stream")
		}
		// The consumer may have delivered the new stream's 4 and 5 under the
		// old numbering first; the reset replays them.
		ids := stream.collectIDs(10, time.Second)
		if len(ids) == 0 || ids[len(ids)-1] != 0 {
			t.Fatalf("ids after the recreation = %v, want them to end with the reset to 0", ids)
		}
		if !hasLogField(obs, "disconnect_reason", streamRecreated) {
			t.Fatalf("no stream_recreated disconnect logged: %+v", obs.All())
		}
		if n := obs.FilterMessageSnippet("JetStream stream was recreated;").Len(); n != 1 {
			t.Fatalf("the recreation was logged %d times, want once", n)
		}

		resumed := openSSEStream(t, srv.URL+"/events?topic=alpha", "0")
		ids = resumed.collectIDs(6, 3*time.Second)
		if len(ids) != 6 || ids[0] != 0 {
			t.Fatalf("resumed ids = %v, want the connected id 0, then 1..5", ids)
		}
		assertContiguousIDs(t, ids[1:], 1, 5)
	})
}

// TestDeliveryContract_RewoundStreamSendsClientsToTheRetainedReplay covers
// the other half of #133: a stream restored from a copy of its store
// directory, like a restore on nats-server 2.14 and earlier, keeps its
// creation time, but its sequence goes back, and an open SSE stream's
// consumer would wait past the restored stream's end. Once the stream has
// stayed behind for the confirmation time, the SSE stream closes and the
// client keeps its cursor. The cursor is ahead of the stream, so the
// reconnect falls back to the retained replay (#103), which has the messages
// published since the restore.
func TestDeliveryContract_RewoundStreamSendsClientsToTheRetainedReplay(t *testing.T) {
	deliveryModes(t, func(t *testing.T, mode func(*Handler)) {
		ns, restart := startRestartableJetStreamServer(t)
		streamDir := filepath.Join(ns.StoreDir(), "$G", "streams", "EVENTS")
		backup := filepath.Join(t.TempDir(), "EVENTS")
		nc, err := nats.Connect(ns.ClientURL(), nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(nc.Close)
		if _, err := mustJetStream(t, nc).CreateStream(context.Background(), jetstream.StreamConfig{
			Name: "EVENTS", Subjects: []string{"events.>"}, Storage: jetstream.FileStorage,
		}); err != nil {
			t.Fatalf("CreateStream: %v", err)
		}
		js, _ := nc.JetStream()
		publishRange(t, js, "events.alpha", 1, 3)
		ns.Shutdown() // a consistent backup of the stream at 3
		if err := os.CopyFS(backup, os.DirFS(streamDir)); err != nil {
			t.Fatalf("back up the stream: %v", err)
		}
		ns = restart()

		core, obs := observer.New(zap.InfoLevel)
		h, srv := newContractServer(t, ns.ClientURL(), func(h *Handler) {
			h.watchInterval = 100 * time.Millisecond
			h.logger = zap.New(core)
			mode(h)
		})
		stream := openSSEStream(t, srv.URL+"/events?topic=alpha", "")
		stream.collectIDs(1, 3*time.Second)
		publishRange(t, js, "events.alpha", 4, 6)
		assertContiguousIDs(t, stream.collectIDs(3, 3*time.Second), 4, 6)
		waitForWatchedSequence(t, h, 6)

		ns.Shutdown()
		if err := os.RemoveAll(streamDir); err != nil {
			t.Fatalf("remove the stream: %v", err)
		}
		if err := os.CopyFS(streamDir, os.DirFS(backup)); err != nil {
			t.Fatalf("restore the stream: %v", err)
		}
		restart()
		waitForNATSReconnect(t, h)
		// Still behind the client's 6: resuming from it would wait for 7.
		publishRange(t, js, "events.alpha", 4, 5)

		select {
		case <-stream.closed:
		case <-time.After(10 * time.Second):
			t.Fatal("the SSE stream stayed open on the rewound stream")
		}
		if ids := stream.collectIDs(10, time.Second); len(ids) != 0 {
			t.Fatalf("ids after the rewind = %v, want none: the client keeps its cursor", ids)
		}
		if !hasLogField(obs, "disconnect_reason", streamRewound) {
			t.Fatalf("no stream_rewound disconnect logged: %+v", obs.All())
		}
		if n := obs.FilterMessageSnippet("JetStream stream went back to an earlier sequence").Len(); n != 1 {
			t.Fatalf("the rewind was logged %d times, want once", n)
		}

		resumed := openSSEStream(t, srv.URL+"/events?topic=alpha", "6")
		assertContiguousIDs(t, resumed.collectIDs(5, 3*time.Second), 1, 5)
	})
}

// waitForWatchedSequence waits until the handler's stream watch has seen the
// stream reach lastSeq.
func waitForWatchedSequence(t *testing.T, h *Handler, lastSeq uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.watch.mu.Lock()
		seen := h.watch.lastSeq
		h.watch.mu.Unlock()
		if seen >= lastSeq {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stream watch saw the stream at %d, want %d", seen, lastSeq)
		}
		time.Sleep(10 * time.Millisecond)
	}
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
		admin := mustJetStream(t, nc)
		waitForConsumer(t, admin, "EVENTS", "has a pull request waiting", func(c *jetstream.ConsumerInfo) bool { return c.NumWaiting > 0 })
		proxy.discard.Store(true)
		publishRange(t, js, "events.alpha", 4, 6) // published while NUTS is cut off
		waitForConsumer(t, admin, "EVENTS", "was sent 4..6", func(c *jetstream.ConsumerInfo) bool { return c.Delivered.Stream >= 6 })
		proxy.cut()
		proxy.discard.Store(false)
		waitForNATSReconnect(t, h)
		publishRange(t, js, "events.alpha", 7, 8)

		assertContiguousIDs(t, stream.collectIDs(5, 15*time.Second), 4, 8)
	})
}

// waitForConsumer waits until a consumer of the stream satisfies cond.
func waitForConsumer(t *testing.T, js jetstream.JetStream, stream, what string, cond func(*jetstream.ConsumerInfo) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if s, err := js.Stream(ctx, stream); err == nil {
			lister := s.ListConsumers(ctx)
			for info := range lister.Info() {
				if cond(info) {
					cancel()
					return
				}
			}
		}
		cancel()
		if time.Now().After(deadline) {
			t.Fatalf("no consumer of %s %s", stream, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
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
		slowBefore := metricValue(t, metricsSlowClientDisconnects)

		stream := openSSEStream(t, srv.URL+"/events?topic=alpha", "0")
		ids := stream.collectIDs(backlog+1, 20*time.Second)
		if len(ids) == 0 || ids[0] != 0 {
			t.Fatalf("first id = %v, want the connected cursor 0", head(ids, 1))
		}
		assertContiguousIDs(t, ids[1:], 1, backlog)
		if got := metricValue(t, metricsSlowClientDisconnects); got != slowBefore {
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
		slowBefore := metricValue(t, metricsSlowClientDisconnects)

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
		if got := metricValue(t, metricsSlowClientDisconnects); got != slowBefore {
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
	t.Parallel() // asserts no process-wide metric
	ns := startJetStreamServer(t)
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
		msg, err := cons.Next(jetstream.FetchMaxWait(200 * time.Millisecond))
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

// TestDeliveryContract_CaughtUpCursorUnderReplayWindow: a client resuming
// from the newest message with replay_window set must get only new
// messages. Its resume point does not exist yet, so its time cannot be read;
// planning must see that it is caught up before treating the unknown time as
// "outside the window" and replaying the whole window (#128).
func TestDeliveryContract_CaughtUpCursorUnderReplayWindow(t *testing.T) {
	deliveryModes(t, func(t *testing.T, mode func(*Handler)) {
		ns := startJetStreamServer(t)
		t.Cleanup(ns.Shutdown)
		nc, _ := nats.Connect(ns.ClientURL())
		t.Cleanup(nc.Close)
		createTestStream(t, nc, "EVENTS", []string{"events.>"})
		js, _ := nc.JetStream()
		_, srv := newContractServer(t, ns.ClientURL(), func(h *Handler) { mode(h); h.ReplayWindow = 3600 })
		publishRange(t, js, "events.alpha", 1, 3)
		fallbacksBefore := metricValue(t, metricsReplayFallbacks)

		stream := openSSEStream(t, srv.URL+"/events?topic=alpha", "3")
		if ids := stream.collectIDs(1, 3*time.Second); len(ids) != 1 || ids[0] != 3 {
			t.Fatalf("connected id = %v, want [3]", ids)
		}
		publishRange(t, js, "events.alpha", 4, 4)
		assertContiguousIDs(t, stream.collectIDs(1, 3*time.Second), 4, 4)
		if extra := stream.collectIDs(1, 200*time.Millisecond); len(extra) != 0 {
			t.Fatalf("unexpected extra events %v", extra)
		}
		if got := metricValue(t, metricsReplayFallbacks); got != fallbacksBefore {
			t.Fatalf("a caught-up client fell back to window replay (fallbacks %v -> %v)", fallbacksBefore, got)
		}
	})
}
