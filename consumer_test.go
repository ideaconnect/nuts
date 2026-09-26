package nuts

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

var testFeedPlan = streamPlan{Topics: []string{"alpha"}, FullSubjects: []string{"events.alpha"}}

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
	feed := h.startStreamFeed(it, testFeedPlan)
	defer feed.stop()

	for seq := uint64(1); seq <= 3; seq++ {
		it.msgs <- newFakeJSMsg("events.alpha", seq, "nuts_x_1", `{"n":1}`)
	}
	for seq := uint64(1); seq <= 3; seq++ {
		frame := receiveFrame(t, feed)
		if frame.StreamSequence != seq || frame.ConsumerName != "nuts_x_1" {
			t.Fatalf("frame %d: seq=%d consumer=%q", seq, frame.StreamSequence, frame.ConsumerName)
		}
		if !strings.HasPrefix(frame.Frame, "id: ") || !strings.Contains(frame.Frame, `"topic":"alpha"`) {
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
	feed := h.startStreamFeed(it, testFeedPlan)
	defer feed.stop()
	before := counterValue(metricsMessagesDropped, dropReasonRawPayload)

	it.msgs <- newFakeJSMsg("events.alpha", 1, "c_1", `{"ok":1}`)
	it.msgs <- newFakeJSMsg("events.alpha", 2, "c_1", `{"blob":"`+strings.Repeat("x", 200)+`"}`)
	it.msgs <- newFakeJSMsg("events.alpha", 3, "c_1", `{"ok":3}`)

	if got := receiveFrame(t, feed).StreamSequence; got != 1 {
		t.Fatalf("first frame seq = %d, want 1", got)
	}
	if got := receiveFrame(t, feed).StreamSequence; got != 3 {
		t.Fatalf("second frame seq = %d, want 3 (2 is oversized)", got)
	}
	if got := counterValue(metricsMessagesDropped, dropReasonRawPayload); got != before+1 {
		t.Fatalf("messages_dropped_total{raw_payload} = %v, want %v", got, before+1)
	}
}

// TestStreamFeed_StopsPullingWhileTheWriterIsBusy is the backpressure
// contract: with the writer not reading, the feed holds one frame in the
// hand-off, one in hand, and does not keep pulling from JetStream.
func TestStreamFeed_StopsPullingWhileTheWriterIsBusy(t *testing.T) {
	h := &Handler{TopicPrefix: "events.", MaxEventSize: -1}
	it := newFakeIterator(100)
	for seq := uint64(1); seq <= 100; seq++ {
		it.msgs <- newFakeJSMsg("events.alpha", seq, "c_1", `{}`)
	}
	feed := h.startStreamFeed(it, testFeedPlan)
	defer feed.stop()

	time.Sleep(100 * time.Millisecond)
	if got := it.nextCalls(); got > 2 {
		t.Fatalf("feed pulled %d messages with nobody reading, want at most 2", got)
	}
	if got := receiveFrame(t, feed).StreamSequence; got != 1 {
		t.Fatalf("first frame seq = %d, want 1", got)
	}
}

func TestStreamFeed_ReportsIteratorFailureOnce(t *testing.T) {
	h := &Handler{TopicPrefix: "events.", MaxEventSize: -1}
	it := newFakeIterator(1)
	feed := h.startStreamFeed(it, testFeedPlan)
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

func TestStreamFeed_StopEndsPullingAndIsIdempotent(t *testing.T) {
	h := &Handler{TopicPrefix: "events.", MaxEventSize: -1}
	it := newFakeIterator(1)
	feed := h.startStreamFeed(it, testFeedPlan)

	feed.stop()
	feed.stop()
	select {
	case <-it.Closed():
	case <-time.After(time.Second):
		t.Fatal("stop did not stop the iterator")
	}
	select {
	case err := <-feed.errs:
		t.Fatalf("a closed iterator must not be reported as a failure, got %v", err)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestStreamFeed_MessageWithoutMetadataIsSentWithoutID(t *testing.T) {
	core, obs := observer.New(zap.WarnLevel)
	h := &Handler{TopicPrefix: "events.", MaxEventSize: -1, logger: zap.New(core)}
	it := newFakeIterator(1)
	feed := h.startStreamFeed(it, testFeedPlan)
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
	before := counterValue(metricsConsumerInvalidated, "unrecoverable")

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
	if got := counterValue(metricsConsumerInvalidated, "unrecoverable"); got != before+1 {
		t.Fatalf("consumer_invalidated_total{unrecoverable} = %v, want %v", got, before+1)
	}
	if !hasLogField(obs, "disconnect_reason", "consumer_unrecoverable") {
		t.Fatalf("missing disconnect_reason=consumer_unrecoverable: %v", obs.All())
	}
}

// fakeDeleteJS is a jetstream.JetStream whose DeleteConsumer returns err.
type fakeDeleteJS struct {
	jetstream.JetStream
	err error
}

func (f fakeDeleteJS) DeleteConsumer(context.Context, string, string) error { return f.err }

// fakeConsumer is a jetstream.Consumer with fixed cached info.
type fakeConsumer struct {
	jetstream.Consumer
	info *jetstream.ConsumerInfo
}

func (f fakeConsumer) CachedInfo() *jetstream.ConsumerInfo { return f.info }

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
