package nuts

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	performanceConcurrentClients       = 16
	performanceConcurrentMessages      = 40
	performanceConcurrentBudget        = 5 * time.Second
	performanceSlowDisconnectBudget    = 3 * time.Second
	performanceGoroutineSlack          = 3
	performanceReplayRetainedMessages  = 160
	performanceReplayCap               = 25
	performanceReplayBudget            = 5 * time.Second
	performanceMemoryGrowthBudgetBytes = 32 * 1024 * 1024
	performanceLargePayloadBytes       = 64 * 1024
	performanceSharedBurstBudget       = 2 * time.Second
)

var (
	benchmarkFormatted formattedMessageEvent
	benchmarkBool      bool
)

func TestPerformance_ConcurrentSSEClientsReceiveRealisticMessageRate(t *testing.T) {
	t.Parallel() // asserts no process-wide metric
	h, _, nc := newProvisionedHandler(t)
	defer nc.Close()
	defer h.Cleanup()
	h.ClientBufferSize = 128

	ctx, cancelAll := context.WithCancel(context.Background())
	defer cancelAll()

	recorders := make([]*safeFlushRecorder, performanceConcurrentClients)
	done := make([]chan error, performanceConcurrentClients)
	for i := range recorders {
		req := httptest.NewRequest("GET", "/events?topic=load", nil).WithContext(ctx)
		rr := newSafeRecorder()
		recorders[i] = rr
		done[i] = make(chan error, 1)
		go func(done chan<- error) { done <- h.ServeHTTP(rr, req, nil) }(done[i])
	}

	for i, rr := range recorders {
		if !waitForSSEBody(rr, "event: connected", 2*time.Second) {
			cancelAll()
			waitForDone(t, done)
			t.Fatalf("client %d did not connect; body=%s", i, rr.Body())
		}
	}

	jsPub, _ := nc.JetStream()
	start := time.Now()
	for i := 1; i <= performanceConcurrentMessages; i++ {
		payload := []byte(`{"seq":` + strconv.Itoa(i) + `,"kind":"load"}`)
		if _, err := jsPub.Publish("events.load", payload); err != nil {
			cancelAll()
			waitForDone(t, done)
			t.Fatalf("publish %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	needle := `"seq":` + strconv.Itoa(performanceConcurrentMessages)
	deadline := time.Now().Add(performanceConcurrentBudget)
	for i, rr := range recorders {
		remaining := time.Until(deadline)
		if remaining <= 0 || !waitForSSEBody(rr, needle, remaining) {
			cancelAll()
			waitForDone(t, done)
			t.Fatalf("client %d did not receive final message within %s; body=%s", i, performanceConcurrentBudget, rr.Body())
		}
	}
	if elapsed := time.Since(start); elapsed > performanceConcurrentBudget {
		cancelAll()
		waitForDone(t, done)
		t.Fatalf("%d clients x %d messages took %s, budget %s", performanceConcurrentClients, performanceConcurrentMessages, elapsed, performanceConcurrentBudget)
	}

	cancelAll()
	waitForDone(t, done)
}

func TestPerformance_SlowReaderDisconnectsWithoutGoroutineLeak(t *testing.T) {
	h, _, nc := newProvisionedHandler(t)
	defer nc.Close()
	defer h.Cleanup()
	h.WriteTimeout = 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = h.ServeHTTP(w, r, nil) }))
	defer srv.Close()
	jsPub, _ := nc.JetStream()
	slowBefore := metricValue(t, metricsSlowClientDisconnects)
	baselineGoroutines := runtime.NumGoroutine()

	// A client that sends the request and never reads the response.
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := fmt.Fprint(conn, "GET /events?topic=slow-load HTTP/1.1\r\nHost: nuts\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if !waitForConsumerCount(t, mustJetStream(t, nc), "EVENTS", 1, 3*time.Second) {
		t.Fatal("stalled client never subscribed")
	}

	// Enough data to fill the kernel socket buffers on both ends, so the
	// server's writes block and hit write_timeout.
	payload := []byte(`{"blob":"` + strings.Repeat("x", 60*1024) + `"}`)
	for i := 0; i < 400; i++ {
		if _, err := jsPub.Publish("events.slow-load", payload); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	start := time.Now()
	for metricValue(t, metricsSlowClientDisconnects) == slowBefore {
		if time.Since(start) > performanceSlowDisconnectBudget {
			t.Fatalf("stalled reader not disconnected within %s", performanceSlowDisconnectBudget)
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = conn.Close()
	if !waitForGoroutinesAtMost(baselineGoroutines+performanceGoroutineSlack, 5*time.Second) {
		t.Fatalf("goroutines did not return to baseline: before=%d after=%d slack=%d", baselineGoroutines, runtime.NumGoroutine(), performanceGoroutineSlack)
	}
}

func TestPerformance_ReplayLoadWithAndWithoutFallbackCaps(t *testing.T) {
	t.Run("uncapped fallback replays retained backlog", func(t *testing.T) {
		delivered, elapsed, heapGrowth := runReplayLoadScenario(t, 0)
		if delivered != performanceReplayRetainedMessages {
			t.Fatalf("delivered %d messages, want retained backlog %d", delivered, performanceReplayRetainedMessages)
		}
		if elapsed > performanceReplayBudget {
			t.Fatalf("uncapped replay took %s, budget %s", elapsed, performanceReplayBudget)
		}
		if heapGrowth > performanceMemoryGrowthBudgetBytes {
			t.Fatalf("uncapped replay heap growth = %d bytes, budget %d", heapGrowth, performanceMemoryGrowthBudgetBytes)
		}
	})

	t.Run("fallback cap bounds replay delivery", func(t *testing.T) {
		delivered, elapsed, heapGrowth := runReplayLoadScenario(t, performanceReplayCap)
		if delivered != performanceReplayCap {
			t.Fatalf("delivered %d messages, want replay cap %d", delivered, performanceReplayCap)
		}
		if elapsed > performanceReplayBudget {
			t.Fatalf("capped replay took %s, budget %s", elapsed, performanceReplayBudget)
		}
		if heapGrowth > performanceMemoryGrowthBudgetBytes {
			t.Fatalf("capped replay heap growth = %d bytes, budget %d", heapGrowth, performanceMemoryGrowthBudgetBytes)
		}
	})
}

func TestPerformance_MemoryGrowthLargePayloadFormattingWithinBudget(t *testing.T) {
	h := &Handler{TopicPrefix: "events.", MaxEventSize: -1}
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	payload := []byte(`{"blob":"` + strings.Repeat("x", performanceLargePayloadBytes) + `"}`)
	msg := streamMessage{Subject: "events.large", Data: payload}

	runtime.GC()
	before := readMemStats()
	for i := 0; i < 128; i++ {
		formatted := h.formatMessageEvent(msg, now)
		// The payload must survive formatting intact: a regression that
		// truncated or dropped it would otherwise also shrink the heap
		// growth this test measures and pass.
		if !strings.Contains(formatted.Frame, string(payload)) {
			t.Fatalf("formatted frame lost the payload: frame_len=%d payload_len=%d", len(formatted.Frame), len(payload))
		}
		benchmarkFormatted = formatted
	}
	runtime.GC()
	after := readMemStats()
	growth := heapGrowthBytes(before, after)
	t.Logf("large payload formatting heap_growth=%d total_alloc_delta=%d", growth, after.TotalAlloc-before.TotalAlloc)
	if growth > performanceMemoryGrowthBudgetBytes {
		t.Fatalf("large payload formatting heap growth = %d bytes, budget %d", growth, performanceMemoryGrowthBudgetBytes)
	}
}

// TestPerformance_StalledClientHoldsABoundedBacklog pins the per-connection
// memory bound (#120). A client that stops reading holds at most
// client_buffer_size messages prefetched from JetStream and the frames on
// their way to it: one the feed holds, feedHandoffFrames handed to the
// writer, and the batch being written (one frame, as these frames are larger
// than a batch). Then the consumer stops pulling, however much is published.
// With shared subscriptions the client falls behind instead, holding its
// queue; its own consumer starts only once it reads again. Either way,
// nothing is lost once it does.
func TestPerformance_StalledClientHoldsABoundedBacklog(t *testing.T) {
	deliveryModes(t, func(t *testing.T, mode func(*Handler)) {
		const bufferSize, published = 8, 100
		// The frames on their way to a stalled client, as docs/PERFORMANCE.md
		// counts them: 1 in the feed, 16 handed to the writer, 1 being written.
		const pipelineFrames = 18
		h, nc := provisionOnStream(t, jetstream.StreamConfig{Name: "EVENTS", Subjects: []string{"events.>"}, Storage: jetstream.MemoryStorage}, func(h *Handler) {
			h.ClientBufferSize = bufferSize
			h.MaxEventSize = 70 << 10
			h.WriteTimeout = 60 // longer than the test: the client stays stalled
			mode(h)
		})
		admin := mustJetStream(t, nc)

		stalled := &gatedFlushLog{flushLog: newFlushLog(), allow: 1, gate: make(chan struct{})}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req := httptest.NewRequest(http.MethodGet, "/events?topic=stall", nil).WithContext(ctx)
		done := make(chan error, 1)
		go func() { done <- h.ServeHTTP(stalled, req, nil) }()
		released := false
		defer func() {
			if !released {
				close(stalled.gate)
			}
			cancel()
			<-done
		}()
		if !stalled.waitFor("event: connected", 3*time.Second) {
			t.Fatalf("stalled client never connected; flushed %q", stalled.written())
		}

		js, _ := nc.JetStream()
		blob := strings.Repeat("x", 65<<10) // each frame outgrows a 64 KiB batch
		for i := 1; i <= published; i++ {
			if _, err := js.PublishAsync("events.stall", []byte(`{"n":`+strconv.Itoa(i)+`,"blob":"`+blob+`"}`)); err != nil {
				t.Fatalf("publish %d: %v", i, err)
			}
		}
		select {
		case <-js.PublishAsyncComplete():
		case <-time.After(10 * time.Second):
			t.Fatal("publishes not acknowledged")
		}

		consumers, delivered := settledConsumers(t, admin, "EVENTS")
		t.Logf("stalled client: %d consumers, %d messages delivered, bound %d", consumers, delivered, bufferSize+pipelineFrames)
		if h.SharedSubscriptions {
			// The shared consumer closed with its only client gone, and the
			// client's own consumer waits for it to read again.
			if consumers != 0 {
				t.Fatalf("%d consumers pulling for a stalled client, want none", consumers)
			}
		} else if consumers != 1 || delivered <= pipelineFrames || delivered > bufferSize+pipelineFrames {
			t.Fatalf("stalled client: %d consumers, %d messages delivered; want 1 consumer with more than %d and at most %d",
				consumers, delivered, pipelineFrames, bufferSize+pipelineFrames)
		}

		released = true
		close(stalled.gate)
		if !stalled.waitFor(`{"n":`+strconv.Itoa(published)+`,`, 10*time.Second) {
			t.Fatalf("the client never caught up; ids=%v", lastN(parseSSEIDs(t, stalled.written()), 5))
		}
		// The write that stalled carried a single frame: a batch stops growing
		// once it passes 64 KiB, which one of these frames does.
		if batches := stalled.batches(); len(batches) < 2 || strings.Count(batches[1], "event: message") != 1 {
			t.Fatalf("the stalled write carried %d frames, want 1", strings.Count(batches[1], "event: message"))
		}
		ids := parseSSEIDs(t, stalled.written())
		if len(ids) == 0 || ids[0] != 0 {
			t.Fatalf("ids start %v, want the connected cursor first", head(ids, 3))
		}
		assertContiguousIDs(t, ids[1:], 1, published)
	})
}

// gatedFlushLog is a flushLog whose writes after the first allow block until
// gate is closed: a client that stopped reading, whose flushes still show how
// the writer batched what it held.
type gatedFlushLog struct {
	*flushLog
	allow int32
	gate  chan struct{}
	n     atomic.Int32
}

func (g *gatedFlushLog) Write(p []byte) (int, error) {
	if g.n.Add(1) > g.allow {
		<-g.gate
	}
	return g.flushLog.Write(p)
}

// written is everything flushed so far.
func (g *gatedFlushLog) written() string { return strings.Join(g.batches(), "") }

// waitFor polls until needle has been flushed or timeout passes.
func (g *gatedFlushLog) waitFor(needle string, timeout time.Duration) bool {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if strings.Contains(g.written(), needle) {
			return true
		}
	}
	return false
}

// settledConsumers waits until the stream's consumers stop changing, then
// reports how many there are and how many messages the server delivered to
// them. Only a quiet window can show that nothing more is pulled.
func settledConsumers(t *testing.T, js jetstream.JetStream, stream string) (int, uint64) {
	t.Helper()
	read := func() (int, uint64) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s, err := js.Stream(ctx, stream)
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		count, delivered := 0, uint64(0)
		lister := s.ListConsumers(ctx)
		for info := range lister.Info() {
			count++
			delivered += info.Delivered.Consumer
		}
		if err := lister.Err(); err != nil {
			t.Fatalf("list consumers: %v", err)
		}
		return count, delivered
	}
	const quiet = 500 * time.Millisecond
	count, delivered := read()
	since := time.Now()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
		c, d := read()
		if c != count || d != delivered {
			count, delivered, since = c, d, time.Now()
			continue
		}
		if time.Since(since) >= quiet {
			return count, delivered
		}
	}
	t.Fatalf("consumers still changing after 10 s: %d consumers, %d delivered", count, delivered)
	return 0, 0
}

func runReplayLoadScenario(t *testing.T, replayMaxMessages int) (int, time.Duration, uint64) {
	t.Helper()
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
	defer nc.Close()
	defer h.Cleanup()
	h.ReplayMaxMessages = replayMaxMessages
	h.MaxEventSize = -1
	h.ClientBufferSize = performanceReplayRetainedMessages + 80

	jsPub, _ := nc.JetStream()
	totalMessages := performanceReplayRetainedMessages + 80
	for i := 0; i < totalMessages; i++ {
		payload := []byte(`{"i":` + strconv.Itoa(i) + `,"blob":"` + strings.Repeat("r", 256) + `"}`)
		if _, err := jsPub.Publish("events.replay-load", payload); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	firstRetainedSequence := uint64(totalMessages - performanceReplayRetainedMessages + 1)
	if err := jsPub.PurgeStream("EVENTS", &nats.StreamPurgeRequest{Sequence: firstRetainedSequence}); err != nil {
		t.Fatalf("purge: %v", err)
	}

	runtime.GC()
	before := readMemStats()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req := httptest.NewRequest("GET", "/events?topic=replay-load&last-id=1", nil).WithContext(ctx)
	rr := newSafeRecorder()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- h.ServeHTTP(rr, req, nil) }()

	if replayMaxMessages > 0 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("ServeHTTP returned error: %v", err)
			}
		case <-time.After(performanceReplayBudget):
			cancel()
			<-done
			t.Fatalf("capped replay did not finish within %s", performanceReplayBudget)
		}
	} else {
		needle := `"i":` + strconv.Itoa(totalMessages-1)
		if !waitForSSEBody(rr, needle, performanceReplayBudget) {
			cancel()
			<-done
			t.Fatalf("uncapped replay did not deliver final retained message; body length=%d", len(rr.Body()))
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("ServeHTTP returned error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("ServeHTTP did not return after cancel")
		}
	}
	elapsed := time.Since(start)
	after := readMemStats()
	return countSSEMessages(rr.Body()), elapsed, heapGrowthBytes(before, after)
}

func waitForDone(t *testing.T, done []chan error) {
	t.Helper()
	for i, ch := range done {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("client %d ServeHTTP returned error: %v", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("client %d ServeHTTP did not return", i)
		}
	}
}

func waitForGoroutinesAtMost(limit int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= limit {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	runtime.GC()
	return runtime.NumGoroutine() <= limit
}

func countSSEMessages(body string) int {
	return strings.Count(body, "event: message\n")
}

func readMemStats() runtime.MemStats {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats
}

func heapGrowthBytes(before, after runtime.MemStats) uint64 {
	if after.HeapAlloc <= before.HeapAlloc {
		return 0
	}
	return after.HeapAlloc - before.HeapAlloc
}

func BenchmarkFormatMessageEvent(b *testing.B) {
	h := &Handler{TopicPrefix: "events.", MaxEventSize: -1}
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	msg := streamMessage{
		Subject:        "events.bench",
		Data:           []byte(`{"kind":"bench","value":123,"nested":{"ok":true}}`),
		HasMetadata:    true,
		StreamSequence: 42,
		Timestamp:      now,
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchmarkFormatted = h.formatMessageEvent(msg, now)
	}
	if benchmarkFormatted.Dropped {
		b.Fatalf("formatted event was unexpectedly dropped: %#v", benchmarkFormatted)
	}
}

func BenchmarkIsValidTopic(b *testing.B) {
	topics := []string{
		"tenant.alpha.orders.created",
		"tenant-beta.updates_1",
		"invalid.>",
		strings.Repeat("a", 257),
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchmarkBool = isValidTopic(topics[i%len(topics)])
	}
}

// BenchmarkStreamFeed measures the per-message path from the JetStream
// iterator to the SSE writer: read metadata, format the frame, and hand it
// over the feed channel to a reader that drains as fast as possible.
func BenchmarkStreamFeed(b *testing.B) {
	h := &Handler{TopicPrefix: "events.", MaxEventSize: -1}
	it := newFakeIterator(1024)
	feed := h.startStreamFeed(it, streamPlan{Topics: []string{"bench"}, FullSubjects: []string{"events.bench"}})
	defer feed.stop()
	msg := newFakeJSMsg("events.bench", 42, "nuts_bench_1", `{"kind":"bench","value":123,"nested":{"ok":true}}`)
	go func() {
		for i := 0; i < b.N; i++ {
			it.msgs <- msg
		}
	}()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkFormatted = <-feed.frames
	}
}

// TestPerformance_SharedFanOutBurst: with shared_subscriptions, a burst of
// large messages to many connections crosses the NATS link once. Without
// sharing, 300 connections × 4 × 64 KiB queue about 75 MiB on NUTS' single
// NATS connection, past nats-server's 64 MiB slow-consumer limit, and the
// resulting reconnects take seconds to recover from.
func TestPerformance_SharedFanOutBurst(t *testing.T) {
	const clients, messages = 300, 4
	h, nc := provisionOnStream(t, jetstream.StreamConfig{
		Name: "EVENTS", Subjects: []string{"events.>"}, Storage: jetstream.MemoryStorage, MaxConsumers: clients + 10,
	}, func(h *Handler) {
		h.SharedSubscriptions = true
		h.MaxEventSize = -1
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = h.ServeHTTP(w, r, nil) }))
	t.Cleanup(srv.Close) // after the streams' own cleanups close their bodies
	streams := make([]*sseReader, clients)
	for i := range streams {
		streams[i] = openSSEStream(t, srv.URL+"/events?topic=burst", "")
		streams[i].collectIDs(1, 3*time.Second)
	}
	if got := consumerCount(mustJetStream(t, nc), "EVENTS"); got != 1 {
		t.Fatalf("consumers = %d, want 1 shared", got)
	}
	reconnectsBefore := metricValue(t, metricsNATSConnectionEvents.WithLabelValues("reconnect"))
	js, _ := nc.JetStream()
	payload := []byte(`{"blob":"` + strings.Repeat("x", 64*1024) + `"}`)
	start := time.Now()
	for i := 0; i < messages; i++ {
		if _, err := js.Publish("events.burst", payload); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	for i, stream := range streams {
		if ids := stream.collectIDs(messages, performanceSharedBurstBudget); len(ids) != messages {
			t.Fatalf("client %d got %d of %d messages within %s", i, len(ids), messages, performanceSharedBurstBudget)
		}
	}
	elapsed := time.Since(start)
	t.Logf("shared fan-out: %d clients × %d × 64 KiB delivered in %s", clients, messages, elapsed)
	if elapsed > performanceSharedBurstBudget {
		t.Fatalf("fan-out took %s, budget %s", elapsed, performanceSharedBurstBudget)
	}
	if got := metricValue(t, metricsNATSConnectionEvents.WithLabelValues("reconnect")); got != reconnectsBefore {
		t.Fatalf("NATS reconnected %v times during the burst", got-reconnectsBefore)
	}
}

// BenchmarkSharedFanOut measures handing one formatted 1 KiB frame to 100
// connections on a shared subscription. It replaces pulling and formatting
// the message once per connection, which the review measured at about 21 µs
// and 22 allocations per delivery.
func BenchmarkSharedFanOut(b *testing.B) {
	const clients = 100
	sub, _ := newTestSharedSub(0)
	var wg sync.WaitGroup
	for i := 0; i < clients; i++ {
		client := sub.attach(0, 1024)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range client.frames {
			}
		}()
	}
	frame := formattedMessageEvent{HasStreamSequence: true, Frame: strings.Repeat("x", 1024)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		frame.StreamSequence = uint64(i + 1)
		sub.publish(frame)
	}
	b.StopTimer()
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*clients), "ns/delivery")
	sub.close(sharedTransitionSharedFailed)
	wg.Wait()
}
