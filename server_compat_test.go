package nuts

// Integration tests for nats-server behaviour NUTS has to accommodate:
// stream consumer limits, consumer inactive-threshold limits, server-written
// control messages, lame duck mode, and connection teardown. They run
// against the embedded nats-server (v2.15).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// TestServeHTTP_StreamConsumerLimitIsRetryable covers #110: nats-server 2.15
// caps a stream at 1000 consumers by default and NUTS needs one per
// connection. A request over the limit is counted and logged as
// stream_consumer_limit and answered as retryable (#105), not as a generic
// subscribe failure.
func TestServeHTTP_StreamConsumerLimitIsRetryable(t *testing.T) {
	h, _ := provisionOnStream(t, jetstream.StreamConfig{
		Name:         "EVENTS",
		Subjects:     []string{"events.>"},
		Storage:      jetstream.MemoryStorage,
		MaxConsumers: 1,
	}, nil)
	core, obs := observer.New(zap.WarnLevel)
	h.logger = zap.New(core)
	_, cancel, done := startSSE(t, h, "/events?topic=a", "")
	defer stopSSE(t, cancel, done)
	rejectedBefore := counterValue(metricsConnectionsRejected, "stream_consumer_limit")
	errorsBefore := counterVal(t, metricsSubscriptionErrors)

	rr := httptest.NewRecorder()
	if err := h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/events?topic=b", nil), nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "Stream consumer limit reached") {
		t.Fatalf("over-limit request = %d %q, want 503 Stream consumer limit reached", rr.Code, rr.Body.String())
	}
	assertRetryAfter(t, rr.Header())

	req := httptest.NewRequest(http.MethodGet, "/events?topic=b", nil)
	req.Header.Set("Accept", "text/event-stream")
	rr = httptest.NewRecorder()
	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	assertRetryStream(t, rr, "Stream consumer limit reached")

	if got := counterValue(metricsConnectionsRejected, "stream_consumer_limit"); got != rejectedBefore+2 {
		t.Fatalf("connections_rejected_total{stream_consumer_limit} = %v, want %v", got, rejectedBefore+2)
	}
	if got := counterVal(t, metricsSubscriptionErrors); got != errorsBefore {
		t.Fatalf("subscription_errors_total moved %v -> %v for a limit rejection", errorsBefore, got)
	}
	if !hasLogField(obs, "disconnect_reason", "stream_consumer_limit") {
		t.Fatalf("missing disconnect_reason=stream_consumer_limit: %v", obs.All())
	}
}

// TestServeHTTP_StreamConsumerInactiveLimitIsHonoured covers #113: a stream
// whose consumer_limits.inactive_threshold is below NUTS' 30s default used to
// refuse every consumer (error 10153), so every SSE request failed while the
// readiness probe stayed green.
func TestServeHTTP_StreamConsumerInactiveLimitIsHonoured(t *testing.T) {
	h, nc := provisionOnStream(t, jetstream.StreamConfig{
		Name:           "EVENTS",
		Subjects:       []string{"events.>"},
		Storage:        jetstream.MemoryStorage,
		ConsumerLimits: jetstream.StreamConsumerLimits{InactiveThreshold: 10 * time.Second},
	}, nil)
	rr, cancel, done := startSSE(t, h, "/events?topic=a", "")
	defer stopSSE(t, cancel, done)

	info := waitForFirstConsumer(t, mustJetStream(t, nc), "EVENTS", time.Second)
	if info == nil || info.Config.InactiveThreshold != 10*time.Second {
		t.Fatalf("consumer = %+v, want InactiveThreshold 10s", info)
	}
	js, _ := nc.JetStream()
	if _, err := js.Publish("events.a", []byte(`{"n":1}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !waitForSSEBody(rr, `{"n":1}`, 3*time.Second) {
		t.Fatalf("message not delivered; body=%q", rr.Body())
	}
}

// TestServeStream_SkipsSubjectDeleteMarkers covers #112: with
// SubjectDeleteMarkerTTL, the server stores an empty marker when a subject's
// last message expires. It used to reach browsers as an empty message event.
func TestServeStream_SkipsSubjectDeleteMarkers(t *testing.T) {
	h, nc := provisionOnStream(t, jetstream.StreamConfig{
		Name:                   "EVENTS",
		Subjects:               []string{"events.>"},
		Storage:                jetstream.MemoryStorage,
		AllowMsgTTL:            true,
		SubjectDeleteMarkerTTL: time.Second,
	}, nil)
	js := mustJetStream(t, nc)
	rr, cancel, done := startSSE(t, h, "/events?topic=expiring", "")
	defer stopSSE(t, cancel, done)
	droppedBefore := counterValue(metricsMessagesDropped, dropReasonControlMessage)

	ctx, cancelPub := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelPub()
	if _, err := js.Publish(ctx, "events.expiring", []byte(`{"n":1}`), jetstream.WithMsgTTL(time.Second)); err != nil {
		t.Fatalf("publish with TTL: %v", err)
	}
	if !waitForSSEBody(rr, `{"n":1}`, 3*time.Second) {
		t.Fatalf("message not delivered; body=%q", rr.Body())
	}
	marker := waitForLastMsg(t, js, "events.expiring", func(m *jetstream.RawStreamMsg) bool {
		return m.Header.Get(jetstream.MarkerReasonHeader) != ""
	})
	if _, err := js.Publish(ctx, "events.expiring", []byte(`{"n":3}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !waitForSSEBody(rr, `{"n":3}`, 3*time.Second) {
		t.Fatalf("message after the marker not delivered; body=%q", rr.Body())
	}
	if got := parseSSEIDs(t, rr.Body()); !reflect.DeepEqual(got, []uint64{0, 1, marker.Sequence + 1}) {
		t.Fatalf("ids = %v, want [0 1 %d] (marker %d skipped)", got, marker.Sequence+1, marker.Sequence)
	}
	if strings.Contains(rr.Body(), `"payload":""`) {
		t.Fatalf("marker reached the client as an empty event; body=%q", rr.Body())
	}
	if got := counterValue(metricsMessagesDropped, dropReasonControlMessage); got != droppedBefore+1 {
		t.Fatalf("messages_dropped_total{control_message} = %v, want %v", got, droppedBefore+1)
	}
}

// TestServeStream_SkipsScheduleDefinitions covers #112 for message schedules:
// the schedule definition itself used to reach subscribers of its subject at
// publish time. The message the schedule produces is delivered.
func TestServeStream_SkipsScheduleDefinitions(t *testing.T) {
	h, nc := provisionOnStream(t, jetstream.StreamConfig{
		Name:              "EVENTS",
		Subjects:          []string{"events.>"},
		Storage:           jetstream.MemoryStorage,
		AllowMsgSchedules: true,
	}, nil)
	rr, cancel, done := startSSE(t, h, "/events?topic=schedules&topic=reminders", "")
	defer stopSSE(t, cancel, done)

	definition := nats.NewMsg("events.schedules")
	definition.Data = []byte(`{"remind":"soon"}`)
	definition.Header.Set(jetstream.ScheduleHeader, "@at "+time.Now().Add(time.Second).UTC().Format(time.RFC3339))
	definition.Header.Set("Nats-Schedule-Target", "events.reminders")
	ctx, cancelPub := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelPub()
	if _, err := mustJetStream(t, nc).PublishMsg(ctx, definition); err != nil {
		t.Fatalf("publish schedule: %v", err)
	}
	if !waitForSSEBody(rr, `"topic":"reminders"`, 5*time.Second) {
		t.Fatalf("scheduled message not delivered; body=%q", rr.Body())
	}
	if strings.Contains(rr.Body(), `"topic":"schedules"`) {
		t.Fatalf("schedule definition reached the client; body=%q", rr.Body())
	}
}

// waitForLastMsg polls the last message on subject until match accepts it.
func waitForLastMsg(t *testing.T, js jetstream.JetStream, subject string, match func(*jetstream.RawStreamMsg) bool) *jetstream.RawStreamMsg {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		stream, err := js.Stream(ctx, "EVENTS")
		if err == nil {
			if msg, err := stream.GetLastMsgForSubject(ctx, subject); err == nil && match(msg) {
				cancel()
				return msg
			}
		}
		cancel()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no matching last message on %s within 10s", subject)
	return nil
}

// TestConnectNATS_NoCallbacksAfterCleanup covers #73: without
// NoCallbacksAfterClientClose the closed callback ran after Cleanup returned,
// logging through a handler Caddy had already unloaded.
func TestConnectNATS_NoCallbacksAfterCleanup(t *testing.T) {
	ns := startJetStreamServer(t)
	defer ns.Shutdown()
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})
	core, obs := observer.New(zap.InfoLevel)
	h := &Handler{NatsURL: ns.ClientURL(), StreamName: "EVENTS", logger: zap.New(core)}
	if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	closedBefore := counterValue(metricsNATSConnectionEvents, "closed")

	if err := h.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // give a late callback time to run
	if obs.FilterMessage("NATS connection closed").Len() != 0 {
		t.Fatalf("closed callback ran after Cleanup: %v", obs.All())
	}
	if got := counterValue(metricsNATSConnectionEvents, "closed"); got != closedBefore {
		t.Fatalf("nats_connection_events_total{closed} moved %v -> %v after Cleanup", closedBefore, got)
	}
}

// TestConnectNATS_LameDuckModeIsLoggedAndCounted covers #73: a server
// entering lame duck mode is about to go away, which operators want to see
// before the disconnect.
func TestConnectNATS_LameDuckModeIsLoggedAndCounted(t *testing.T) {
	ns, err := server.NewServer(&server.Options{
		Host:                "127.0.0.1",
		Port:                -1,
		JetStream:           true,
		StoreDir:            t.TempDir(),
		LameDuckDuration:    time.Second,
		LameDuckGracePeriod: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	defer ns.Shutdown()
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	createTestStream(t, nc, "EVENTS", []string{"events.>"})
	nc.Close()

	core, obs := observer.New(zap.WarnLevel)
	h := &Handler{NatsURL: ns.ClientURL(), StreamName: "EVENTS", logger: zap.New(core)}
	if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()
	before := counterValue(metricsNATSConnectionEvents, "lame_duck")

	go ns.LameDuckShutdown()
	deadline := time.Now().Add(5 * time.Second)
	for counterValue(metricsNATSConnectionEvents, "lame_duck") == before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := counterValue(metricsNATSConnectionEvents, "lame_duck"); got != before+1 {
		t.Fatalf("nats_connection_events_total{lame_duck} = %v, want %v", got, before+1)
	}
	if obs.FilterMessageSnippet("lame duck mode").Len() != 1 {
		t.Fatalf("missing lame duck warning: %v", obs.All())
	}
}

// TestCleanup_DeletesStreamConsumersBeforeClosing covers #75: Cleanup closed
// the NATS connection while the streams it had just woken were still
// deleting their consumers, leaving them on the server until
// InactiveThreshold reaped them.
func TestCleanup_DeletesStreamConsumersBeforeClosing(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
	defer nc.Close()
	admin := mustJetStream(t, nc)

	var dones []<-chan error
	for i := 0; i < 5; i++ {
		_, cancel, done := startSSE(t, h, "/events?topic=t"+strconv.Itoa(i), "")
		defer cancel()
		dones = append(dones, done)
	}
	if !waitForConsumerCount(t, admin, "EVENTS", 5, 3*time.Second) {
		t.Fatalf("consumers = %d, want 5", consumerCount(admin, "EVENTS"))
	}
	if err := h.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if got := consumerCount(admin, "EVENTS"); got != 0 {
		t.Fatalf("consumers right after Cleanup = %d, want 0", got)
	}
	for _, done := range dones {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("a stream kept running after Cleanup")
		}
	}

	// A request that arrives after Cleanup is turned away as retryable.
	rr := httptest.NewRecorder()
	if err := h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/events?topic=late", nil), nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("request after Cleanup = %d, want 503", rr.Code)
	}
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

// TestServeStream_PurgingOneTopicKeepsTheOthersPending covers #111: purging
// one subject of a multi-topic stream must not skip messages still pending
// on the server for the other topics. nats-server before 2.14.7 moves a
// multi-filter consumer past them (nats-server#8572), which is why 2.14.7 is
// the minimum for multi-topic subscriptions; the embedded server pins the
// fixed behaviour.
func TestServeStream_PurgingOneTopicKeepsTheOthersPending(t *testing.T) {
	h, nc := provisionOnStream(t, jetstream.StreamConfig{
		Name:     "EVENTS",
		Subjects: []string{"events.>"},
		Storage:  jetstream.MemoryStorage,
	}, func(h *Handler) { h.ClientBufferSize = 1 })
	js := mustJetStream(t, nc)

	w := &gatedWriter{safeFlushRecorder: newSafeRecorder(), allow: 1, gate: make(chan struct{})}
	req := httptest.NewRequest(http.MethodGet, "/events?topic=a&topic=b", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(w, req.WithContext(ctx), nil) }()
	if !waitForSSEBody(w.safeFlushRecorder, "event: connected", 3*time.Second) {
		t.Fatalf("no connected event; body=%q", w.Body())
	}

	pubCtx, cancelPub := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelPub()
	var wantB []uint64
	for i := 1; i <= 10; i++ {
		for _, topic := range []string{"a", "b"} {
			ack, err := js.Publish(pubCtx, "events."+topic, []byte(`{"n":`+strconv.Itoa(i)+`}`))
			if err != nil {
				t.Fatalf("publish: %v", err)
			}
			if topic == "b" {
				wantB = append(wantB, ack.Sequence)
			}
		}
	}
	// The stalled writer keeps most of the 20 messages pending on the server.
	stream, err := js.Stream(pubCtx, "EVENTS")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if err := stream.Purge(pubCtx, jetstream.WithPurgeSubject("events.a")); err != nil {
		t.Fatalf("purge: %v", err)
	}
	close(w.gate)

	last := strconv.FormatUint(wantB[len(wantB)-1], 10)
	if !waitForSSEBody(w.safeFlushRecorder, "id: "+last+"\n", 5*time.Second) {
		t.Fatalf("last message on b (seq %s) never arrived; ids=%v", last, parseSSEIDs(t, w.Body()))
	}
	got := map[uint64]bool{}
	for _, id := range parseSSEIDs(t, w.Body()) {
		got[id] = true
	}
	for _, seq := range wantB {
		if !got[seq] {
			t.Fatalf("message on b at seq %d was skipped by the purge of a; ids=%v", seq, parseSSEIDs(t, w.Body()))
		}
	}
	cancel()
	<-done
}

// TestServeHTTP_NATSDownIsRejectedAtOnce: while the NATS connection is down
// and reconnecting, a stream request used to wait for the stream-info read
// and the consumer create to time out (about 7 seconds) before failing. It is
// now told to retry at once.
func TestServeHTTP_NATSDownIsRejectedAtOnce(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer nc.Close()
	defer h.Cleanup()
	core, obs := observer.New(zap.WarnLevel)
	h.logger = zap.New(core)
	ns.Shutdown()
	deadline := time.Now().Add(5 * time.Second)
	for h.currentStreamRuntime().disconnected == false && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	req := httptest.NewRequest(http.MethodGet, "/events?topic=a", nil)
	req.Header.Set("Accept", "text/event-stream")
	rr := httptest.NewRecorder()
	start := time.Now()
	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("request took %v while NATS was down, want an immediate answer", elapsed)
	}
	assertRetryStream(t, rr, "JetStream not available")
	if !hasLogField(obs, "disconnect_reason", "jetstream_unavailable") {
		t.Fatalf("missing disconnect_reason=jetstream_unavailable: %v", obs.All())
	}
}
