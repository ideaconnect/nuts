package nuts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func receiveFrame(t *testing.T, feed *streamFeed) formattedMessageEvent {
	t.Helper()
	select {
	case frame := <-feed.frames:
		return frame
	case err := <-feed.errs:
		t.Fatalf("feed failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("no frame within 2s")
	}
	return formattedMessageEvent{}
}

func TestStreamFeed_DeliversFormattedFramesInOrder(t *testing.T) {
	h := &Handler{TopicPrefix: "events.", MaxEventSize: -1}
	it := newFakeIterator(8)
	feed := it.startFeed(h, testFeedPlan)
	defer feed.stop()

	for seq := uint64(1); seq <= 3; seq++ {
		it.msgs <- newFakeJSMsg("events.alpha", seq, "nuts_x_1", `{"n":1}`)
	}
	for seq := uint64(1); seq <= 3; seq++ {
		frame := receiveFrame(t, feed)
		if frame.StreamSequence != seq || frame.ConsumerName != "nuts_x_1" {
			t.Fatalf("frame %d: seq=%d consumer=%q", seq, frame.StreamSequence, frame.ConsumerName)
		}
		if !strings.HasSuffix(frame.Frame, fmt.Sprintf("\nid: %d\n\n", seq)) || !strings.Contains(frame.Frame, `"topic":"alpha"`) {
			t.Fatalf("frame %d not rendered: %q", seq, frame.Frame)
		}
	}
}

// TestStreamFeed_DropsOversizedPayloadsBeforeTheWriter: an oversized message
// is dropped and counted in the feed, so it never occupies the hand-off to
// the writer and never reaches the client.
func TestStreamFeed_DropsOversizedPayloadsBeforeTheWriter(t *testing.T) {
	// Small frames render to ~90 bytes; the 200-byte blob exceeds the limit
	// before formatting.
	h := &Handler{TopicPrefix: "events.", MaxEventSize: 150, logger: zap.NewNop()}
	it := newFakeIterator(8)
	feed := it.startFeed(h, testFeedPlan)
	defer feed.stop()
	before := metricValue(t, metricsMessagesDropped.WithLabelValues(dropReasonRawPayload))

	it.msgs <- newFakeJSMsg("events.alpha", 1, "c_1", `{"ok":1}`)
	it.msgs <- newFakeJSMsg("events.alpha", 2, "c_1", `{"blob":"`+strings.Repeat("x", 200)+`"}`)
	it.msgs <- newFakeJSMsg("events.alpha", 3, "c_1", `{"ok":3}`)

	if got := receiveFrame(t, feed).StreamSequence; got != 1 {
		t.Fatalf("first frame seq = %d, want 1", got)
	}
	if got := receiveFrame(t, feed).StreamSequence; got != 3 {
		t.Fatalf("second frame seq = %d, want 3 (2 is oversized)", got)
	}
	if got := metricValue(t, metricsMessagesDropped.WithLabelValues(dropReasonRawPayload)); got != before+1 {
		t.Fatalf("messages_dropped_total{raw_payload} = %v, want %v", got, before+1)
	}
}

// TestStreamFeed_StopsPullingWhileTheWriterIsBusy is the backpressure
// contract: with the writer not reading, the feed fills the hand-off, holds
// one more frame in hand, and stops pulling from JetStream. synctest.Wait
// returns once the feed is blocked for good, so the count is exact.
func TestStreamFeed_StopsPullingWhileTheWriterIsBusy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &Handler{TopicPrefix: "events.", MaxEventSize: -1}
		it := newFakeIterator(100)
		for seq := uint64(1); seq <= 100; seq++ {
			it.msgs <- newFakeJSMsg("events.alpha", seq, "c_1", `{}`)
		}
		feed := it.startFeed(h, testFeedPlan)
		defer feed.stop()

		synctest.Wait()
		if got := it.nextCalls(); got != feedHandoffFrames+1 {
			t.Fatalf("feed pulled %d messages with nobody reading, want %d (the hand-off plus one in hand)", got, feedHandoffFrames+1)
		}
		if got := receiveFrame(t, feed).StreamSequence; got != 1 {
			t.Fatalf("first frame seq = %d, want 1", got)
		}
	})
}

// TestStreamFeed_StopReleasesAFeedBlockedOnTheWriter: when a stream whose
// writer stopped reading ends, its feed is blocked handing over a frame. It
// must return rather than stay blocked, holding the frame, for good; the
// bubble fails the test if the feed goroutine is still blocked at the end.
func TestStreamFeed_StopReleasesAFeedBlockedOnTheWriter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &Handler{TopicPrefix: "events.", MaxEventSize: -1}
		it := newFakeIterator(100)
		for seq := uint64(1); seq <= 100; seq++ {
			it.msgs <- newFakeJSMsg("events.alpha", seq, "c_1", `{}`)
		}
		feed := it.startFeed(h, testFeedPlan)
		synctest.Wait() // the hand-off is full and the feed blocked on it
		feed.stop()
		synctest.Wait()
	})
}

func TestStreamFeed_ReportsIteratorFailureOnce(t *testing.T) {
	h := &Handler{TopicPrefix: "events.", MaxEventSize: -1}
	it := newFakeIterator(1)
	feed := it.startFeed(h, testFeedPlan)
	defer feed.stop()

	it.errs <- errFakeResetFailed
	select {
	case err := <-feed.errs:
		if !errors.Is(err, errFakeResetFailed) {
			t.Fatalf("feed error = %v, want %v", err, errFakeResetFailed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("iterator failure not reported")
	}
	select {
	case frame := <-feed.frames:
		t.Fatalf("frame after a terminal failure: %+v", frame)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestStreamFeed_StopEndsPullingAndIsIdempotent: stop ends a feed whose
// Next is waiting for messages, and the feed's own goroutine stops the
// iterator once Next has returned. A Stop from another goroutine while Next
// runs races with nats.go's ordered consumer swapping in a recreated
// consumer's subscription (see startStreamFeed).
func TestStreamFeed_StopEndsPullingAndIsIdempotent(t *testing.T) {
	h := &Handler{TopicPrefix: "events.", MaxEventSize: -1}
	it := newFakeIterator(1)
	feed := it.startFeed(h, testFeedPlan)
	for it.inNext.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	feed.stop()
	feed.stop()
	select {
	case <-feed.exited:
	case <-time.After(time.Second):
		t.Fatal("stop did not end the feed")
	}
	select {
	case <-it.Closed():
	default:
		t.Fatal("the feed ended without stopping its iterator")
	}
	if it.stopDuringNext.Load() {
		t.Fatal("the iterator was stopped while Next was waiting")
	}
	select {
	case err := <-feed.errs:
		t.Fatalf("a stopped feed must not report a failure, got %v", err)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestStreamFeed_MessageWithoutMetadataIsSentWithoutID(t *testing.T) {
	core, obs := observer.New(zap.WarnLevel)
	h := &Handler{TopicPrefix: "events.", MaxEventSize: -1, logger: zap.New(core)}
	it := newFakeIterator(1)
	feed := it.startFeed(h, testFeedPlan)
	defer feed.stop()

	it.msgs <- fakeJSMsg{subject: "events.alpha", data: []byte(`{}`), metaErr: jetstream.ErrNotJSMessage}
	frame := receiveFrame(t, feed)
	if strings.Contains(frame.Frame, "id: ") || frame.HasStreamSequence {
		t.Fatalf("frame without metadata carries an id: %q", frame.Frame)
	}
	if !hasLogField(obs, "message_subject", "events.alpha") {
		t.Fatalf("metadata failure not logged: %v", obs.All())
	}
}

// TestServeStream_ClosesWhenTheConsumerCannotBeRecreated: once the ordered
// consumer gives up recreating itself, the stream ends so the client
// reconnects with Last-Event-ID instead of waiting on a dead consumer.
func TestServeStream_ClosesWhenTheConsumerCannotBeRecreated(t *testing.T) {
	core, obs := observer.New(zap.WarnLevel)
	h := &Handler{HeartbeatInterval: 60, logger: zap.New(core)}
	errs := make(chan error, 1)
	errs <- errFakeResetFailed
	feed := &streamFeed{frames: make(chan formattedMessageEvent), errs: errs, stop: func() {}}
	before := metricValue(t, metricsConsumerInvalidated.WithLabelValues("unrecoverable"))

	done := make(chan error, 1)
	go func() {
		done <- h.serveStream(newSafeRecorder(), httptest.NewRequest(http.MethodGet, "/events?topic=alpha", nil), testFeedPlan, feed, nil)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveStream: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serveStream kept running after the consumer failed for good")
	}
	if got := metricValue(t, metricsConsumerInvalidated.WithLabelValues("unrecoverable")); got != before+1 {
		t.Fatalf("consumer_invalidated_total{unrecoverable} = %v, want %v", got, before+1)
	}
	if !hasLogField(obs, "disconnect_reason", "consumer_unrecoverable") {
		t.Fatalf("missing disconnect_reason=consumer_unrecoverable: %v", obs.All())
	}
}

// fakeDeleteJS is a jetstream.JetStream whose DeleteConsumer returns err.
type fakeDeleteJS struct {
	jetstream.JetStream
	err     error
	conn    *nats.Conn
	deleted chan struct{}
}

func (f fakeDeleteJS) DeleteConsumer(context.Context, string, string) error {
	if f.deleted != nil {
		f.deleted <- struct{}{}
	}
	return f.err
}

func (f fakeDeleteJS) Conn() *nats.Conn { return f.conn }

// TestConsumerStream_DeleteFailureIsLogged covers #84: a failed consumer
// delete names the stream's topics and the consumer, so the lingering
// consumer can be traced to its request. A consumer that is already gone is
// not worth a warning.
func TestConsumerStream_DeleteFailureIsLogged(t *testing.T) {
	for _, c := range []struct {
		name    string
		err     error
		wantLog bool
	}{
		{name: "delete failed", err: errors.New("timeout"), wantLog: true},
		{name: "consumer already gone", err: jetstream.ErrConsumerNotFound},
		{name: "deleted", err: nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			core, obs := observer.New(zap.WarnLevel)
			released := make(chan struct{})
			cs := &consumerStream{
				js:       fakeDeleteJS{err: c.err},
				stream:   "EVENTS",
				consumer: fakeConsumer{info: &jetstream.ConsumerInfo{Name: "nuts_x_1"}},
				release:  func() { close(released) },
				log:      zap.New(core),
				plan:     testFeedPlan,
			}
			cs.deleteConsumer()
			select {
			case <-released:
			case <-time.After(2 * time.Second):
				t.Fatal("delete never released the stream")
			}
			logged := hasLogField(obs, "consumer", "nuts_x_1") && hasLogField(obs, "subject_label", "events.alpha")
			if logged != c.wantLog || obs.Len() != btoi(c.wantLog) {
				t.Fatalf("logs = %v, want a warning naming the consumer and topics: %v", obs.All(), c.wantLog)
			}
		})
	}

	t.Run("no consumer info", func(t *testing.T) {
		released := false
		cs := &consumerStream{consumer: fakeConsumer{}, release: func() { released = true }, log: zap.NewNop()}
		cs.deleteConsumer()
		if !released {
			t.Fatal("a stream without consumer info was not released")
		}
	})
}

// TestConsumerStream_CloseDeletesTheConsumerAfterTheFeedStopped: close stops
// the feed and returns at once, and deletes the consumer only once the feed
// has stopped its iterator: a consumer deleted under a running ordered
// consumer would be recreated.
func TestConsumerStream_CloseDeletesTheConsumerAfterTheFeedStopped(t *testing.T) {
	deleted := make(chan struct{}, 1)
	exited := make(chan struct{})
	stopped := 0
	cs := &consumerStream{
		js:       fakeDeleteJS{deleted: deleted},
		stream:   "EVENTS",
		consumer: fakeConsumer{info: &jetstream.ConsumerInfo{Name: "nuts_x_1"}},
		release:  func() {},
		log:      zap.NewNop(),
		feed:     &streamFeed{stop: func() { stopped++ }, exited: exited},
	}
	cs.close()
	cs.close()
	if stopped != 1 {
		t.Fatalf("feed stopped %d times, want once", stopped)
	}
	select {
	case <-deleted:
		t.Fatal("the consumer was deleted before the feed stopped its iterator")
	case <-time.After(50 * time.Millisecond):
	}
	close(exited)
	select {
	case <-deleted:
	case <-time.After(2 * time.Second):
		t.Fatal("the consumer was not deleted once the feed stopped")
	}
}

// TestConsumerStream_NoDeleteWhileDisconnected: with the NATS connection
// down, a delete could only time out, holding Cleanup up for nothing.
func TestConsumerStream_NoDeleteWhileDisconnected(t *testing.T) {
	ns := startJetStreamServer(t)
	nc, err := nats.Connect(ns.ClientURL(), nats.MaxReconnects(-1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	ns.Shutdown()
	for nc.IsConnected() {
		time.Sleep(10 * time.Millisecond)
	}
	deleted := make(chan struct{}, 1)
	released := false
	cs := &consumerStream{
		js:       fakeDeleteJS{conn: nc, deleted: deleted},
		consumer: fakeConsumer{info: &jetstream.ConsumerInfo{Name: "nuts_x_1"}},
		release:  func() { released = true },
		log:      zap.NewNop(),
	}
	cs.deleteConsumer()
	if !released {
		t.Fatal("stream not released at once while NATS is down")
	}
	select {
	case <-deleted:
		t.Fatal("tried to delete the consumer over a dead connection")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestHandler_ConsumerDeletedMidStream_RecreatesAndResumes covers what M9
// Batch B was meant to fix: a consumer lost on the server (reaped, deleted,
// dropped with a leafnode route) used to leave the SSE stream open and silent.
// The ordered consumer detects the missing heartbeats, recreates itself from
// the last delivered sequence, and the stream continues without a gap.
func TestHandler_ConsumerDeletedMidStream_RecreatesAndResumes(t *testing.T) {
	h, _, nc := newProvisionedHandler(t)
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
	recreatedBefore := metricValue(t, metricsConsumerInvalidated.WithLabelValues("recreated"))
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
	if got := metricValue(t, metricsConsumerInvalidated.WithLabelValues("recreated")); got <= recreatedBefore {
		t.Fatalf("nuts_consumer_invalidated_total{reason=recreated} = %v, want > %v", got, recreatedBefore)
	}
	if !hasLogField(obs, "previous_consumer", info.Name) {
		t.Fatalf("no recreation log naming the deleted consumer %q; logs=%v", info.Name, obs.All())
	}
	stopSSE(t, cancel, done)
}

// TestStreamWatch_EndsAGenerationWhenTheStreamIsRecreatedOrRewound walks the
// watch through the reads of a stream's life (#133). A new creation time ends
// the generation at once. A last sequence below the highest seen ends it
// only once the reads have shown it for the whole confirmation time, not
// when a replica without a leader reports it, and not when the stream caught
// up in between. Each ended generation is replaced by an open one.
func TestStreamWatch_EndsAGenerationWhenTheStreamIsRecreatedOrRewound(t *testing.T) {
	var none *streamWatch
	if gen, reason := none.observe(&jetstream.StreamInfo{Created: time.Now()}, time.Now()); gen != nil || reason != "" || none.current() != nil {
		t.Fatal("a nil watch must do nothing")
	}
	var noGen *streamGeneration
	if noGen.done() != nil {
		t.Fatal("a nil generation must never end")
	}

	created := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	read := func(created time.Time, lastSeq uint64) *jetstream.StreamInfo {
		return &jetstream.StreamInfo{Created: created, State: jetstream.StreamState{LastSeq: lastSeq}}
	}
	leaderless := read(created, 5)
	leaderless.Cluster = &jetstream.ClusterInfo{Name: "c1"}
	led := read(created, 11)
	led.Cluster = &jetstream.ClusterInfo{Name: "c1", Leader: "n1"}
	const confirm = 10 * time.Second
	steps := []struct {
		name  string
		after time.Duration // since the previous read
		info  *jetstream.StreamInfo
		ended string
	}{
		{"the first read sets the baseline", 0, read(created, 10), ""},
		{"a read without a creation time is ignored", 0, read(time.Time{}, 0), ""},
		{"a missing read is ignored", 0, nil, ""},
		{"the stream grows", 0, read(created, 12), ""},
		{"a replica without a leader may lag", 0, leaderless, ""},
		{"a read below the highest sequence starts the clock", 5 * time.Second, read(created, 11), ""},
		{"still behind, not yet for the whole time", confirm - 1, read(created, 11), ""},
		{"caught up: the clock stops", 0, read(created, 12), ""},
		{"behind again, the clock starts again", time.Second, led, ""},
		{"behind for the whole time: rewound", confirm, led, streamRewound},
		{"behind the rewound stream, a new clock starts", 0, read(created, 10), ""},
		{"the rewound stream is the new baseline", confirm, read(created, 11), ""},
		{"a new creation time: recreated", 0, read(created.Add(time.Minute), 0), streamRecreated},
		{"the new stream grows", 0, read(created.Add(time.Minute), 1), ""},
		{"the old stream's highest sequence is forgotten", confirm, read(created.Add(time.Minute), 1), ""},
	}
	w := newStreamWatch(confirm)
	gen := w.current()
	now := created.Add(time.Hour)
	for _, step := range steps {
		now = now.Add(step.after)
		got, ended := w.observe(step.info, now)
		if ended != step.ended {
			t.Fatalf("%s: ended %q, want %q", step.name, ended, step.ended)
		}
		if got != w.current() {
			t.Fatalf("%s: the read's generation is not the current one", step.name)
		}
		select {
		case <-gen.done():
			if step.ended == "" || gen.reason != step.ended || got == gen {
				t.Fatalf("%s: generation ended with %q, and the read got the ended one: %v", step.name, gen.reason, got == gen)
			}
			select {
			case <-got.done():
				t.Fatalf("%s: the new generation started out ended", step.name)
			default:
			}
		default:
			if step.ended != "" || got != gen {
				t.Fatalf("%s: the generation did not end, but the read got another: %v", step.name, got != gen)
			}
		}
		gen = got
	}

	// The first read's last sequence is part of the baseline.
	w = newStreamWatch(confirm)
	w.observe(read(created, 10), now)
	w.observe(read(created, 9), now)
	if _, ended := w.observe(read(created, 9), now.Add(confirm)); ended != streamRewound {
		t.Fatalf("a stream rewound right after the first read: ended %q, want %q", ended, streamRewound)
	}
}

// TestStreamWatch_KeepsAContinuityMark: the watch marks a last sequence and
// the time of the message there for the poll's continuity check (#137). The
// generation's first read sets the mark and reads do not move it: only a
// check (advance) does, for the generation it started from, and an ended
// generation marks the stream as the ending read saw it.
func TestStreamWatch_KeepsAContinuityMark(t *testing.T) {
	var none *streamWatch
	none.advance(nil, nil)
	if gen, seq, at := none.mark(); gen != nil || seq != 0 || !at.IsZero() || none.rewound(nil, nil) {
		t.Fatal("a nil watch must do nothing")
	}
	created := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	read := func(created time.Time, lastSeq uint64, lastTime time.Time) *jetstream.StreamInfo {
		return &jetstream.StreamInfo{Created: created, State: jetstream.StreamState{LastSeq: lastSeq, LastTime: lastTime}}
	}
	at := func(seq uint64) time.Time { return created.Add(time.Duration(seq) * time.Second) }
	wantMark := func(w *streamWatch, step string, gen *streamGeneration, seq uint64, seen time.Time) {
		t.Helper()
		if g, s, a := w.mark(); g != gen || s != seq || !a.Equal(seen) {
			t.Fatalf("%s: mark = %d at %v (same generation: %v), want %d at %v", step, s, a, g == gen, seq, seen)
		}
	}
	w := newStreamWatch(time.Second)
	now := created.Add(time.Hour)

	w.observe(read(created, 10, at(10)), now)
	gen := w.current()
	wantMark(w, "first read", gen, 10, at(10))
	w.observe(read(created, 12, at(12)), now)
	wantMark(w, "a read of a stream that grew", gen, 10, at(10))
	w.advance(gen, read(created, 12, at(12)))
	wantMark(w, "advance", gen, 12, at(12))
	w.advance(newStreamGeneration(), read(created, 15, at(15)))
	wantMark(w, "advance for another generation", gen, 12, at(12))

	if !w.rewound(gen, read(created, 20, at(99))) {
		t.Fatal("rewound did not end the marked generation")
	}
	select {
	case <-gen.done():
		if gen.reason != streamRewound {
			t.Fatalf("generation ended with %q, want %q", gen.reason, streamRewound)
		}
	default:
		t.Fatal("the marked generation is still open")
	}
	next := w.current()
	wantMark(w, "after rewound", next, 20, at(99))
	if w.rewound(gen, read(created, 30, at(30))) {
		t.Fatal("rewound ended a generation twice")
	}
	w.advance(gen, read(created, 30, at(30)))
	wantMark(w, "a stale rewound and advance", next, 20, at(99))

	w.observe(read(created.Add(time.Minute), 1, at(1)), now)
	wantMark(w, "recreated", w.current(), 1, at(1))
}

// continuityStream is a jetstream.Stream whose GetMsg returns msg or err and
// counts its calls.
type continuityStream struct {
	jetstream.Stream
	info  *jetstream.StreamInfo
	msg   *jetstream.RawStreamMsg
	err   error
	calls int
}

func (s *continuityStream) CachedInfo() *jetstream.StreamInfo { return s.info }

func (s *continuityStream) GetMsg(context.Context, uint64, ...jetstream.GetMsgOpt) (*jetstream.RawStreamMsg, error) {
	s.calls++
	return s.msg, s.err
}

// TestHandler_CheckContinuity: a poll compares the message stored at the
// marked sequence with the one seen there. Another message there ends the
// generation (#137). The mark moves on once the message is found unchanged
// or cannot be compared, and stays while the stream is behind it, when the
// message cannot be read now, and for a generation a read ended meanwhile.
func TestHandler_CheckContinuity(t *testing.T) {
	created := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	seen := created.Add(time.Minute)
	later := seen.Add(time.Second)
	state := func(firstSeq, lastSeq uint64, lastTime time.Time) *jetstream.StreamInfo {
		return &jetstream.StreamInfo{Created: created, State: jetstream.StreamState{FirstSeq: firstSeq, LastSeq: lastSeq, LastTime: lastTime}}
	}
	msgAt := func(at time.Time) *jetstream.RawStreamMsg { return &jetstream.RawStreamMsg{Time: at} }
	for _, tt := range []struct {
		name     string
		info     *jetstream.StreamInfo
		seq      uint64
		seen     time.Time
		msg      *jetstream.RawStreamMsg
		err      error
		calls    int
		advanced bool
		ended    bool
		stale    bool // a read ended the generation before the check
	}{
		{name: "no stream info", info: nil, seq: 10, seen: seen},
		{name: "behind the mark: the reads handle it", info: state(1, 9, seen), seq: 10, seen: seen},
		{name: "nothing marked yet", info: state(1, 12, later), seq: 0, seen: time.Time{}, advanced: true},
		{name: "an empty stream marked", info: state(1, 12, later), seq: 10, seen: time.Time{}, advanced: true},
		{name: "the marked message was trimmed", info: state(11, 12, later), seq: 10, seen: seen, advanced: true},
		{name: "unchanged", info: state(1, 10, seen), seq: 10, seen: seen},
		{name: "grew, the same message", info: state(1, 12, later), seq: 10, seen: seen, msg: msgAt(seen), calls: 1, advanced: true},
		{name: "grew, the message was deleted", info: state(1, 12, later), seq: 10, seen: seen, err: jetstream.ErrMsgNotFound, calls: 1, advanced: true},
		{name: "grew, the read failed", info: state(1, 12, later), seq: 10, seen: seen, err: context.DeadlineExceeded, calls: 1},
		{name: "same sequence, another message", info: state(1, 10, later), seq: 10, seen: seen, msg: msgAt(later), calls: 1, ended: true},
		{name: "passed it, another message", info: state(1, 12, later), seq: 10, seen: seen, msg: msgAt(seen.Add(-time.Second)), calls: 1, ended: true},
		{name: "another message, generation already ended", info: state(1, 12, later), seq: 10, seen: seen, msg: msgAt(later), calls: 1, stale: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			core, obs := observer.New(zap.InfoLevel)
			h := &Handler{StreamName: "EVENTS", logger: zap.New(core), watch: newStreamWatch(time.Second)}
			gen := h.watch.current()
			if tt.stale {
				h.watch.rewound(gen, state(1, 1, seen))
			}
			_, markSeq, markTime := h.watch.mark()
			stream := &continuityStream{info: tt.info, msg: tt.msg, err: tt.err}
			h.checkContinuity(stream, gen, tt.seq, tt.seen)

			if stream.calls != tt.calls {
				t.Fatalf("GetMsg calls = %d, want %d", stream.calls, tt.calls)
			}
			select {
			case <-gen.done():
				if !tt.ended && !tt.stale {
					t.Fatal("the generation ended")
				}
			default:
				if tt.ended {
					t.Fatal("the generation did not end")
				}
			}
			if want := map[bool]int{true: 1, false: 0}[tt.ended]; obs.FilterMessageSnippet("has since passed it").Len() != want {
				t.Fatalf("the rewind was not logged %d times", want)
			}
			_, seq, at := h.watch.mark()
			moved := seq != markSeq || !at.Equal(markTime)
			if want := tt.advanced || tt.ended; moved != want {
				t.Fatalf("mark moved from %d at %v to %d at %v: %v, want %v", markSeq, markTime, seq, at, moved, want)
			}
			if moved && (seq != tt.info.State.LastSeq || !at.Equal(tt.info.State.LastTime)) {
				t.Fatalf("mark = %d at %v, want the stream as read: %d at %v", seq, at, tt.info.State.LastSeq, tt.info.State.LastTime)
			}
		})
	}
}

// fakeStreamJS is a jetstream.JetStream whose Stream returns stream.
type fakeStreamJS struct {
	jetstream.JetStream
	stream jetstream.Stream
}

func (f fakeStreamJS) Stream(context.Context, string) (jetstream.Stream, error) {
	return f.stream, nil
}

// TestHandler_WatchStreamReadsEveryIntervalUntilShutdown: with no request
// reading the stream, the handler reads it every interval, so a recreated
// stream is noticed with nobody asking (#133). It skips reads while there is
// no JetStream context and stops with the handler.
func TestHandler_WatchStreamReadsEveryIntervalUntilShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var reads atomic.Int32
		h := &Handler{StreamName: "EVENTS", logger: zap.NewNop()}
		h.js = fakeStreamJS{stream: fakeStream{info: &jetstream.StreamInfo{}}}
		h.streamReads.observe = func(jetstream.Stream) *streamGeneration {
			reads.Add(1)
			return nil
		}
		shutdown := make(chan struct{})
		done := make(chan struct{})
		go func() {
			h.watchStream(time.Second, shutdown)
			close(done)
		}()

		time.Sleep(3*time.Second + time.Millisecond)
		synctest.Wait()
		if got := reads.Load(); got != 3 {
			t.Fatalf("reads after 3 intervals = %d, want 3", got)
		}
		h.mu.Lock()
		h.js = nil
		h.mu.Unlock()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if got := reads.Load(); got != 3 {
			t.Fatalf("reads without a JetStream context = %d, want still 3", got)
		}
		close(shutdown)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("watchStream kept running after shutdown")
		}
	})
}

// TestHandler_EndGenerationResetsTheCursor: a stream closed because its
// JetStream stream was recreated or rewound ends with a reset event whose id,
// 0, makes EventSource replay the stream from its start (#133, #137). It is
// an event so that pages keeping the cursor see it, and it spreads the
// reconnects with a retry jittered around transient_retry (#145).
func TestHandler_EndGenerationResetsTheCursor(t *testing.T) {
	for _, tt := range []struct {
		reason    string
		transient int
		frame     string
	}{
		{streamRecreated, 0, "event: reset\ndata: {\"reason\":\"stream_recreated\"}\nid: 0\n\n"},
		{streamRewound, 30, "event: reset\ndata: {\"reason\":\"stream_rewound\"}\nid: 0\n\n"},
	} {
		t.Run(tt.reason, func(t *testing.T) {
			gen := newStreamGeneration()
			gen.reason = tt.reason
			core, obs := observer.New(zap.InfoLevel)
			h := &Handler{logger: zap.New(core), TransientRetry: tt.transient}
			before := metricValue(t, metricsConsumerInvalidated.WithLabelValues(tt.reason))
			w := newSafeRecorder()
			h.endGeneration(w, http.NewResponseController(w), streamPlan{Generation: gen}, time.Second)

			rest, ok := strings.CutPrefix(w.Body(), ": "+tt.reason+"\nretry: ")
			ms, end, _ := strings.Cut(rest, "\n")
			delay, err := strconv.Atoi(ms)
			if !ok || err != nil || end != tt.frame {
				t.Fatalf("frame %q, want a %s comment, a retry, then %q", w.Body(), tt.reason, tt.frame)
			}
			base := transientRetryBase
			if tt.transient > 0 {
				base = time.Duration(tt.transient) * time.Second
			}
			if d := time.Duration(delay) * time.Millisecond; d < base/2 || d >= 3*base/2 {
				t.Fatalf("retry %v outside [%v, %v)", d, base/2, 3*base/2)
			}
			if got := metricValue(t, metricsConsumerInvalidated.WithLabelValues(tt.reason)); got != before+1 {
				t.Fatalf("consumer_invalidated{reason=%s} = %v, want %v", tt.reason, got, before+1)
			}
			if !hasLogField(obs, "disconnect_reason", tt.reason) {
				t.Fatalf("no disconnect_reason=%s logged: %+v", tt.reason, obs.All())
			}
		})
	}
}
