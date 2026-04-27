package nuts

import (
	"context"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

const (
	performanceConcurrentClients       = 16
	performanceConcurrentMessages      = 40
	performanceConcurrentBudget        = 5 * time.Second
	performanceSlowDisconnectBudget    = 3 * time.Second
	performanceGoroutineSlack          = 20
	performanceReplayRetainedMessages  = 160
	performanceReplayCap               = 25
	performanceReplayBudget            = 5 * time.Second
	performanceMemoryGrowthBudgetBytes = 32 * 1024 * 1024
	performanceLargePayloadBytes       = 64 * 1024
)

var (
	benchmarkFormatted formattedMessageEvent
	benchmarkParsed    interface{}
	benchmarkBool      bool
	benchmarkString    string
	benchmarkInt       int
)

func TestPerformance_ConcurrentSSEClientsReceiveRealisticMessageRate(t *testing.T) {
	h, ns, nc := newProvisionedHandler(t)
	defer ns.Shutdown()
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
	h, ns, nc := newProvisionedHandler(t)
	h.ClientBufferSize = 4

	jsPub, _ := nc.JetStream()
	var firstSequence uint64
	for i := 0; i < 128; i++ {
		ack, err := jsPub.Publish("events.slow-load", []byte(`{"i":`+strconv.Itoa(i)+`}`))
		if err != nil {
			h.Cleanup()
			nc.Close()
			ns.Shutdown()
			t.Fatalf("publish %d: %v", i, err)
		}
		if i == 0 {
			firstSequence = ack.Sequence
		}
	}

	baselineGoroutines := runtime.NumGoroutine()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req := httptest.NewRequest("GET", "/events?topic=slow-load", nil).WithContext(ctx)
	req.Header.Set("Last-Event-ID", strconv.FormatUint(firstSequence-1, 10))
	rr := &slowFlushRecorder{ResponseRecorder: httptest.NewRecorder(), writeDelay: 25 * time.Millisecond}
	done := make(chan error, 1)

	start := time.Now()
	go func() { done <- h.ServeHTTP(rr, req, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeHTTP returned error: %v", err)
		}
	case <-time.After(performanceSlowDisconnectBudget):
		cancel()
		<-done
		t.Fatalf("slow reader did not disconnect within %s", performanceSlowDisconnectBudget)
	}
	if elapsed := time.Since(start); elapsed > performanceSlowDisconnectBudget {
		t.Fatalf("slow reader disconnect took %s, budget %s", elapsed, performanceSlowDisconnectBudget)
	}

	if err := h.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	nc.Close()
	ns.Shutdown()
	if !waitForGoroutinesAtMost(baselineGoroutines+performanceGoroutineSlack, 3*time.Second) {
		t.Fatalf("goroutines did not return near baseline: before=%d after=%d slack=%d", baselineGoroutines, runtime.NumGoroutine(), performanceGoroutineSlack)
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
	msg := &nats.Msg{Subject: "events.large", Data: payload}

	runtime.GC()
	before := readMemStats()
	for i := 0; i < 128; i++ {
		formatted := h.formatMessageEvent(msg, now)
		if formatted.Dropped {
			t.Fatalf("large payload was unexpectedly dropped: %#v", formatted)
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
	msg := &nats.Msg{
		Subject: "events.bench",
		Data:    []byte(`{"kind":"bench","value":123,"nested":{"ok":true}}`),
		Reply:   jetStreamAckReply("EVENTS", "consumer", 1, 42, 1, now, 0),
		Sub:     &nats.Subscription{},
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchmarkFormatted = h.formatMessageEvent(msg, now)
	}
	if benchmarkFormatted.Dropped {
		b.Fatalf("formatted event was unexpectedly dropped: %#v", benchmarkFormatted)
	}
}

func BenchmarkTryParseJSON(b *testing.B) {
	cases := []struct {
		name string
		data []byte
	}{
		{name: "small-json", data: []byte(`{"value": 123, "ok": true}`)},
		{name: "large-json", data: []byte(`{"blob":"` + strings.Repeat("x", performanceLargePayloadBytes) + `"}`)},
		{name: "raw-string", data: []byte(strings.Repeat("not-json", 128))},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				benchmarkParsed = tryParseJSON(tc.data)
			}
		})
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

func BenchmarkCommonSubjectFilter(b *testing.B) {
	subjects := make([]string, 64)
	for i := range subjects {
		subjects[i] = "events.tenant." + strconv.Itoa(i) + ".updates"
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchmarkString = commonSubjectFilter(subjects)
	}
}

func BenchmarkMultiTopicRequestedMessageHandler(b *testing.B) {
	requestedSubjects := map[string]struct{}{
		"events.a": {},
		"events.b": {},
		"events.c": {},
		"events.d": {},
	}
	plan := streamPlan{RequestedSubjects: requestedSubjects}
	messages := []*nats.Msg{
		{Subject: "events.a"},
		{Subject: "events.x"},
		{Subject: "events.b"},
		{Subject: "events.y"},
		{Subject: "events.c"},
		{Subject: "events.z"},
		{Subject: "events.d"},
		{Subject: "events.unrequested"},
	}
	delivered := 0
	filter := plan.requestedMessageHandler(func(*nats.Msg) { delivered++ })

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		filter(messages[i%len(messages)])
	}
	benchmarkInt = delivered
}
