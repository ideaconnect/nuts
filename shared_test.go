package nuts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func sharedFrame(seq uint64) formattedMessageEvent {
	return formattedMessageEvent{StreamSequence: seq, HasStreamSequence: true, Frame: "frame"}
}

// drain returns the sequences queued for a client without blocking.
func drain(c *sharedClient) []uint64 {
	var seqs []uint64
	for {
		select {
		case frame, ok := <-c.frames:
			if !ok {
				return seqs
			}
			seqs = append(seqs, frame.StreamSequence)
		default:
			return seqs
		}
	}
}

func TestSharedSub_RingEvictionRaisesTheFloor(t *testing.T) {
	sub, _ := newTestSharedSub(0)
	for seq := uint64(1); seq <= sharedRingFrames+6; seq++ {
		sub.publish(sharedFrame(seq))
	}
	if len(sub.ring) != sharedRingFrames || sub.floor != 6 || sub.ring[0].StreamSequence != 7 {
		t.Fatalf("ring len=%d floor=%d first=%d, want %d frames after floor 6", len(sub.ring), sub.floor, sub.ring[0].StreamSequence, sharedRingFrames)
	}

	big, _ := newTestSharedSub(0)
	mib := strings.Repeat("x", 1<<20)
	for seq := uint64(1); seq <= 6; seq++ {
		big.publish(formattedMessageEvent{StreamSequence: seq, HasStreamSequence: true, Frame: mib})
	}
	if len(big.ring) != 4 || big.floor != 2 || big.ringBytes != 4<<20 {
		t.Fatalf("byte-bounded ring len=%d floor=%d bytes=%d, want 4 frames of 1 MiB after floor 2", len(big.ring), big.floor, big.ringBytes)
	}

	unordered, _ := newTestSharedSub(0)
	unordered.publish(formattedMessageEvent{Frame: "no metadata"})
	if len(unordered.ring) != 0 {
		t.Fatal("a frame without a stream sequence was remembered")
	}
}

func TestSharedSub_AttachNeedsEveryMessageAfterLastSeq(t *testing.T) {
	sub, _ := newTestSharedSub(10)
	for seq := uint64(11); seq <= 20; seq++ {
		sub.publish(sharedFrame(seq))
	}
	if c := sub.attach(9, 64); c != nil {
		t.Fatal("attached below the floor, where message 10 may be missing")
	}
	atFloor := sub.attach(10, 64)
	if got := drain(atFloor); len(got) != 10 || got[0] != 11 || got[9] != 20 {
		t.Fatalf("attach(10) queued %v, want 11..20", got)
	}
	middle := sub.attach(15, 64)
	if got := drain(middle); len(got) != 5 || got[0] != 16 {
		t.Fatalf("attach(15) queued %v, want 16..20", got)
	}
	ahead := sub.attach(25, 64)
	for seq := uint64(21); seq <= 30; seq++ {
		sub.publish(sharedFrame(seq))
	}
	if got := drain(ahead); len(got) != 5 || got[0] != 26 || got[4] != 30 {
		t.Fatalf("a client ahead of the subscription got %v, want only 26..30", got)
	}
	if tooFar := sub.attach(10, 5); tooFar != nil {
		t.Fatal("attached with more remembered frames than the queue holds")
	}
}

func TestSharedSub_FullQueueMovesTheClientOff(t *testing.T) {
	sub, _ := newTestSharedSub(0)
	slow := sub.attach(0, 2)
	fast := sub.attach(0, 64)
	before := metricValue(t, metricsSharedTransitions.WithLabelValues(sharedTransitionFellBehind))
	for seq := uint64(1); seq <= 3; seq++ {
		sub.publish(sharedFrame(seq))
	}
	if got := drain(slow); len(got) != 2 {
		t.Fatalf("slow client got %v, want the 2 frames that fit", got)
	}
	if _, open := <-slow.frames; open {
		t.Fatal("slow client's queue still open after it overflowed")
	}
	if slow.reason != sharedTransitionFellBehind || slow.lastSeq != 2 {
		t.Fatalf("slow client reason=%q lastSeq=%d, want fell_behind after 2", slow.reason, slow.lastSeq)
	}
	if got := drain(fast); len(got) != 3 {
		t.Fatalf("fast client got %v, want all 3", got)
	}
	if got := metricValue(t, metricsSharedTransitions.WithLabelValues(sharedTransitionFellBehind)); got != before+1 {
		t.Fatalf("shared_transitions_total{fell_behind} = %v, want %v", got, before+1)
	}
}

// TestSharedSub_ClosesWhenEveryClientFellBehind: a subscription whose last
// connection fell behind must not keep its consumer pulling for nobody.
func TestSharedSub_ClosesWhenEveryClientFellBehind(t *testing.T) {
	sub, stopped := newTestSharedSub(0)
	sub.publish(sharedFrame(1)) // no clients yet: a new subscription stays open
	if sub.isClosed() {
		t.Fatal("a subscription without clients closed before anyone attached")
	}
	a := sub.attach(1, 1)
	b := sub.attach(1, 1)
	sub.publish(sharedFrame(2))
	if sub.isClosed() {
		t.Fatal("closed while its clients kept up")
	}
	sub.publish(sharedFrame(3)) // both queues are full now
	if !sub.isClosed() || *stopped != 1 || sub.registry.subs[testSharedKey] != nil {
		t.Fatalf("closed=%v stopped=%d registered=%v after every client fell behind", sub.isClosed(), *stopped, sub.registry.subs[testSharedKey] != nil)
	}
	for _, c := range []*sharedClient{a, b} {
		if drain(c); c.reason != sharedTransitionFellBehind || c.lastSeq != 2 {
			t.Fatalf("client reason=%q lastSeq=%d, want fell_behind after 2", c.reason, c.lastSeq)
		}
	}
}

func TestSharedSub_LastLeaveClosesTheSubscription(t *testing.T) {
	sub, stopped := newTestSharedSub(0)
	gauge := metricValue(t, metricsSharedSubscriptions)
	a := sub.attach(0, 4)
	b := sub.attach(0, 4)
	sub.leave(a)
	sub.leave(a) // a second leave is a no-op
	if sub.isClosed() || *stopped != 0 {
		t.Fatal("subscription closed while a client remained")
	}
	sub.leave(b)
	if !sub.isClosed() || *stopped != 1 {
		t.Fatalf("closed=%v stopped=%d after the last client left", sub.isClosed(), *stopped)
	}
	if sub.registry.subs[testSharedKey] != nil {
		t.Fatal("closed subscription still registered")
	}
	if got := metricValue(t, metricsSharedSubscriptions); got != gauge-1 {
		t.Fatalf("shared_subscriptions = %v, want %v", got, gauge-1)
	}
	if c := sub.attach(0, 4); c != nil {
		t.Fatal("attached to a closed subscription")
	}
}

func TestSharedSub_FailureMovesEveryClientOff(t *testing.T) {
	sub, _ := newTestSharedSub(0)
	a := sub.attach(0, 4)
	b := sub.attach(0, 4)
	sub.publish(sharedFrame(1))
	sub.close(sharedTransitionSharedFailed)
	sub.close(sharedTransitionSharedFailed) // idempotent
	for _, c := range []*sharedClient{a, b} {
		drain(c)
		if _, open := <-c.frames; open || c.reason != sharedTransitionSharedFailed || c.lastSeq != 1 {
			t.Fatalf("client open=%v reason=%q lastSeq=%d, want closed shared_failed after 1", open, c.reason, c.lastSeq)
		}
	}
	sub.publish(sharedFrame(2)) // ignored once closed
}

// TestSharedKey_IgnoresTopicOrderButNotTheGeneration: a topic set is shared
// whatever the order of its topics, but only within a stream generation, so
// no connection joins a subscription positioned on a stream that has since
// been recreated or rewound (#133).
func TestSharedKey_IgnoresTopicOrderButNotTheGeneration(t *testing.T) {
	gen := newStreamGeneration()
	a := sharedKey(streamPlan{FullSubjects: []string{"events.b", "events.a"}, Generation: gen})
	b := sharedKey(streamPlan{FullSubjects: []string{"events.a", "events.b"}, Generation: gen})
	if a != b || a.subjects != "events.a,events.b" || a.generation != gen {
		t.Fatalf("keys %+v and %+v, want both events.a,events.b on the plan's generation", a, b)
	}
	if next := sharedKey(streamPlan{FullSubjects: []string{"events.a", "events.b"}, Generation: newStreamGeneration()}); next == a {
		t.Fatal("the topic set has the same key on another stream generation")
	}
}

func sharedStreamConfig() jetstream.StreamConfig {
	return jetstream.StreamConfig{Name: "EVENTS", Subjects: []string{"events.>"}, Storage: jetstream.MemoryStorage}
}

// TestShared_LiveClientsShareOneConsumer: with shared_subscriptions on, live
// connections to the same topic set share one JetStream consumer, and each
// still receives every message in order.
func TestShared_LiveClientsShareOneConsumer(t *testing.T) {
	_, srv, nc := newSharedContractServer(t, nil)
	js, _ := nc.JetStream()
	gaugeBefore := metricValue(t, metricsSharedSubscriptions)

	var streams []*sseReader
	for i := 0; i < 5; i++ {
		stream := openSSEStream(t, srv.URL+"/events?topic=a", "")
		if ids := stream.collectIDs(1, 3*time.Second); len(ids) != 1 || ids[0] != 0 {
			t.Fatalf("client %d connected ids = %v, want [0]", i, ids)
		}
		streams = append(streams, stream)
	}
	if got := consumerCount(mustJetStream(t, nc), "EVENTS"); got != 1 {
		t.Fatalf("consumers for 5 live clients = %d, want 1 shared", got)
	}
	if got := metricValue(t, metricsSharedSubscriptions); got != gaugeBefore+1 {
		t.Fatalf("shared_subscriptions = %v, want %v", got, gaugeBefore+1)
	}
	publishRange(t, js, "events.a", 1, 20)
	for i, stream := range streams {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			assertContiguousIDs(t, stream.collectIDs(20, 3*time.Second), 1, 20)
		})
	}
}

// TestShared_ReplayClientJoinsAfterCatchingUp: a connection with history
// replays it on its own consumer, then joins the shared subscription without
// a gap or a duplicate, and its own consumer is deleted.
func TestShared_ReplayClientJoinsAfterCatchingUp(t *testing.T) {
	_, srv, nc := newSharedContractServer(t, nil)
	js, _ := nc.JetStream()
	admin := mustJetStream(t, nc)
	publishRange(t, js, "events.a", 1, 50)
	joinedBefore := metricValue(t, metricsSharedTransitions.WithLabelValues(sharedTransitionJoined))

	live := openSSEStream(t, srv.URL+"/events?topic=a", "")
	live.collectIDs(1, 3*time.Second)
	replaying := openSSEStream(t, srv.URL+"/events?topic=a", "10")
	assertContiguousIDs(t, replaying.collectIDs(41, 3*time.Second), 10, 50) // cursor, then 11..50
	if !waitForConsumerCount(t, admin, "EVENTS", 1, 3*time.Second) {
		t.Fatalf("consumers after the replay caught up = %d, want 1 shared", consumerCount(admin, "EVENTS"))
	}
	publishRange(t, js, "events.a", 51, 60)
	assertContiguousIDs(t, replaying.collectIDs(10, 3*time.Second), 51, 60)
	assertContiguousIDs(t, live.collectIDs(10, 3*time.Second), 51, 60)
	if got := metricValue(t, metricsSharedTransitions.WithLabelValues(sharedTransitionJoined)); got < joinedBefore+2 {
		t.Fatalf("shared_transitions_total{joined} = %v, want at least %v", got, joinedBefore+2)
	}
}

// TestShared_SlowClientFallsBackWithoutLoss: a connection that falls more
// than client_buffer_size frames behind leaves the shared subscription and
// catches up on its own consumer, without holding up the others and without
// missing a message.
func TestShared_SlowClientFallsBackWithoutLoss(t *testing.T) {
	h, nc := provisionOnStream(t, sharedStreamConfig(), func(h *Handler) {
		h.SharedSubscriptions = true
		h.ClientBufferSize = 4
	})
	js, _ := nc.JetStream()
	fellBefore := metricValue(t, metricsSharedTransitions.WithLabelValues(sharedTransitionFellBehind))

	fast, cancelFast, doneFast := startSSE(t, h, "/events?topic=a", "")
	defer stopSSE(t, cancelFast, doneFast)
	slow := &gatedWriter{safeFlushRecorder: newSafeRecorder(), allow: 1, gate: make(chan struct{})}
	req := httptest.NewRequest(http.MethodGet, "/events?topic=a", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(slow, req.WithContext(ctx), nil) }()
	if !waitForSSEBody(slow.safeFlushRecorder, "event: connected", 3*time.Second) {
		t.Fatalf("slow client never connected; body=%q", slow.Body())
	}
	joinedAfterConnect := metricValue(t, metricsSharedTransitions.WithLabelValues(sharedTransitionJoined))

	// Two messages at a time, each pair drained by the fast client before the
	// next: its queue never holds more than 2 of its 4 frames, so only the
	// gated client can fall behind. A 30-message burst could also overflow
	// the fast client's queue before its writer runs, a legitimate second
	// fall-behind that would make the count below flaky.
	for n := 1; n <= 30; n += 2 {
		publishRange(t, js, "events.a", n, n+1)
		if !waitForSSEBody(fast, `{"n":`+strconv.Itoa(n+1)+`}`, 3*time.Second) {
			t.Fatalf("the fast client was held up by the slow one; body=%q", fast.Body())
		}
	}
	if got := parseSSEIDs(t, fast.Body()); len(got) != 31 {
		t.Fatalf("fast client ids = %v, want the cursor and 1..30", got)
	}
	close(slow.gate)
	if !waitForSSEBody(slow.safeFlushRecorder, `{"n":30}`, 5*time.Second) {
		t.Fatalf("slow client never caught up; ids=%v", parseSSEIDs(t, slow.Body()))
	}
	ids := parseSSEIDs(t, slow.Body())
	if len(ids) == 0 || ids[0] != 0 {
		t.Fatalf("slow client ids = %v, want the connected cursor first", ids)
	}
	assertContiguousIDs(t, ids[1:], 1, 30)
	if got := metricValue(t, metricsSharedTransitions.WithLabelValues(sharedTransitionFellBehind)); got != fellBefore+1 {
		t.Fatalf("shared_transitions_total{fell_behind} = %v, want %v", got, fellBefore+1)
	}
	// Caught up, the connection waits sharedJoinBackoff before rejoining
	// rather than trying on every frame: neither its last catch-up frame nor
	// a new message takes it back to the shared subscription yet.
	publishRange(t, js, "events.a", 31, 31)
	if !waitForSSEBody(slow.safeFlushRecorder, `{"n":31}`, 3*time.Second) {
		t.Fatalf("the caught-up client missed message 31; ids=%v", lastN(parseSSEIDs(t, slow.Body()), 3))
	}
	if got := metricValue(t, metricsSharedTransitions.WithLabelValues(sharedTransitionJoined)); got != joinedAfterConnect {
		t.Fatalf("shared_transitions_total{joined} moved %v -> %v within sharedJoinBackoff", joinedAfterConnect, got)
	}
	cancel()
	<-done
}

// TestShared_DeletedSharedConsumerIsRecreated: the shared consumer recovers
// like any ordered consumer, and every connection continues without a gap.
func TestShared_DeletedSharedConsumerIsRecreated(t *testing.T) {
	_, srv, nc := newSharedContractServer(t, func(h *Handler) { h.NatsIdleHeartbeat = 1 })
	js, _ := nc.JetStream()
	admin := mustJetStream(t, nc)
	a := openSSEStream(t, srv.URL+"/events?topic=a", "")
	b := openSSEStream(t, srv.URL+"/events?topic=a", "")
	a.collectIDs(1, 3*time.Second)
	b.collectIDs(1, 3*time.Second)
	publishRange(t, js, "events.a", 1, 2)
	info := waitForFirstConsumer(t, admin, "EVENTS", time.Second)
	if info == nil {
		t.Fatal("no shared consumer")
	}
	if err := admin.DeleteConsumer(context.Background(), "EVENTS", info.Name); err != nil {
		t.Fatalf("DeleteConsumer: %v", err)
	}
	publishRange(t, js, "events.a", 3, 5)
	assertContiguousIDs(t, a.collectIDs(5, 10*time.Second), 1, 5)
	assertContiguousIDs(t, b.collectIDs(5, 10*time.Second), 1, 5)
}

// TestShared_CleanupDeletesTheSharedConsumer: Cleanup waits for the shared
// consumer's delete like for any stream's.
func TestShared_CleanupDeletesTheSharedConsumer(t *testing.T) {
	h, nc := provisionOnStream(t, sharedStreamConfig(), func(h *Handler) { h.SharedSubscriptions = true })
	admin := mustJetStream(t, nc)
	for i := 0; i < 3; i++ {
		_, cancel, _ := startSSE(t, h, "/events?topic=a", "")
		defer cancel()
	}
	if got := consumerCount(admin, "EVENTS"); got != 1 {
		t.Fatalf("consumers = %d, want 1 shared", got)
	}
	if err := h.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if got := consumerCount(admin, "EVENTS"); got != 0 {
		t.Fatalf("consumers right after Cleanup = %d, want 0", got)
	}
}

// newSharedContractServer serves a handler with shared_subscriptions on over
// real HTTP, on a fresh embedded server.
func newSharedContractServer(t *testing.T, configure func(*Handler)) (*Handler, *httptest.Server, *nats.Conn) {
	t.Helper()
	ns := startJetStreamServer(t)
	t.Cleanup(ns.Shutdown)
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	createTestStream(t, nc, "EVENTS", []string{"events.>"})
	h, srv := newContractServer(t, ns.ClientURL(), func(h *Handler) {
		h.SharedSubscriptions = true
		if configure != nil {
			configure(h)
		}
	})
	return h, srv, nc
}

// TestShared_EndedStreamsLeaveNoGoroutinesBehind: whichever way a stream with
// shared subscriptions ends, nothing it started keeps running. Ten streams
// each end in one of three ways, so a leak of a goroutine per stream clears
// the slack: live streams whose subscriptions close with them; stalled
// streams that write_timeout ends while their feeds wait on the writer; and
// streams that fell behind, caught up on their own consumers and sit idle
// there, since a connection that left waits sharedJoinBackoff to rejoin.
func TestShared_EndedStreamsLeaveNoGoroutinesBehind(t *testing.T) {
	const streams = 10
	h, nc := provisionOnStream(t, sharedStreamConfig(), func(h *Handler) {
		h.SharedSubscriptions = true
		h.ClientBufferSize = 4
		h.WriteTimeout = 1
	})
	js, _ := nc.JetStream()
	baseline := runtime.NumGoroutine()
	serve := func(w http.ResponseWriter, target string) (context.CancelFunc, <-chan error) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		done := make(chan error, 1)
		go func() { done <- h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx), nil) }()
		return cancel, done
	}
	var ends []func()

	// Stalled: the connected event gets through, then every write waits
	// out write_timeout.
	stalledDone := make([]<-chan error, streams)
	for i := range stalledDone {
		w := newStalledDeadlineWriter(1)
		var cancel context.CancelFunc
		cancel, stalledDone[i] = serve(w, "/events?topic=stall")
		defer cancel()
		for deadline := time.Now().Add(3 * time.Second); !strings.Contains(w.Body(), "event: connected"); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("stalled stream %d never connected", i)
			}
		}
	}
	publishRange(t, js, "events.stall", 1, 60) // enough to fill each feed's hand-off

	// Fallen behind, then caught up on their own consumers.
	gates := make([]*gatedWriter, streams)
	for i := range gates {
		gates[i] = &gatedWriter{safeFlushRecorder: newSafeRecorder(), allow: 1, gate: make(chan struct{})}
		cancel, done := serve(gates[i], "/events?topic=behind")
		ends = append(ends, func() { cancel(); <-done })
		if !waitForSSEBody(gates[i].safeFlushRecorder, "event: connected", 3*time.Second) {
			t.Fatalf("gated stream %d never connected", i)
		}
	}
	publishRange(t, js, "events.behind", 1, 30)
	for i, g := range gates {
		close(g.gate)
		if !waitForSSEBody(g.safeFlushRecorder, `{"n":30}`, 5*time.Second) {
			t.Fatalf("gated stream %d never caught up; ids=%v", i, lastN(parseSSEIDs(t, g.Body()), 3))
		}
	}

	// Live, each on a subscription of its own.
	for i := 0; i < streams; i++ {
		topic := "live" + strconv.Itoa(i)
		rr, cancel, done := startSSE(t, h, "/events?topic="+topic, "")
		publishRange(t, js, "events."+topic, 1, 1)
		if !waitForSSEBody(rr, `{"n":1}`, 3*time.Second) {
			t.Fatalf("live stream %s got nothing", topic)
		}
		ends = append(ends, func() { stopSSE(t, cancel, done) })
	}

	for _, end := range ends {
		end()
	}
	for i, done := range stalledDone {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("stalled stream %d outlived write_timeout", i)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline+3 {
		if time.Now().After(deadline) {
			var dump strings.Builder
			_ = pprof.Lookup("goroutine").WriteTo(&dump, 1)
			var ours []string
			for _, stack := range strings.Split(dump.String(), "\n\n") {
				if strings.Contains(stack, "ideaconnect/nuts.") {
					ours = append(ours, stack)
				}
			}
			t.Fatalf("goroutines: %d after the streams ended, %d before; NUTS stacks:\n%s", runtime.NumGoroutine(), baseline, strings.Join(ours, "\n\n"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
