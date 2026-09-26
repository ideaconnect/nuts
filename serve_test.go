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
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestHandler_ParseStreamRequestBuildsStreamPlan(t *testing.T) {
	h := &Handler{TopicPrefix: "events.", logger: zap.NewNop()}
	req := httptest.NewRequest("GET", "/events?topic=alpha&topic=alpha&topic=beta&last-id=41", nil)

	plan, requestErr := h.parseStreamRequest(req)
	if requestErr != nil {
		t.Fatalf("parseStreamRequest returned error: %v", requestErr)
	}
	if !reflect.DeepEqual(plan.Topics, []string{"alpha", "beta"}) {
		t.Fatalf("Topics = %#v, want alpha/beta without duplicates", plan.Topics)
	}
	if !reflect.DeepEqual(plan.FullSubjects, []string{"events.alpha", "events.beta"}) {
		t.Fatalf("FullSubjects = %#v", plan.FullSubjects)
	}
	if _, ok := plan.RequestedSubjects["events.alpha"]; !ok {
		t.Fatal("RequestedSubjects missing events.alpha")
	}
	if plan.Replay.Mode != replayModeStartSequence || !plan.Replay.HasLastID || plan.Replay.StartSequence != 42 {
		t.Fatalf("Replay = %#v, want start sequence 42", plan.Replay)
	}
}

func TestHandler_ParseStreamRequestUsesPathTopic(t *testing.T) {
	h := &Handler{TopicPrefix: "events.", logger: zap.NewNop()}
	req := httptest.NewRequest("GET", "/orders/new", nil)

	plan, requestErr := h.parseStreamRequest(req)
	if requestErr != nil {
		t.Fatalf("parseStreamRequest returned error: %v", requestErr)
	}
	if !reflect.DeepEqual(plan.Topics, []string{"orders.new"}) {
		t.Fatalf("Topics = %#v, want path-derived orders.new", plan.Topics)
	}
	if !reflect.DeepEqual(plan.FullSubjects, []string{"events.orders.new"}) {
		t.Fatalf("FullSubjects = %#v", plan.FullSubjects)
	}
	if plan.Replay.Mode != replayModeDeliverNew {
		t.Fatalf("Replay mode = %s, want deliver_new", plan.Replay.Mode)
	}
}

func TestHandler_PlanSubscriptionSelectsReplayModes(t *testing.T) {
	basePlan := streamPlan{
		Topics:       []string{"alpha"},
		FullSubjects: []string{"events.alpha"},
		Replay: replayPlan{
			HasLastID:     true,
			Mode:          replayModeStartSequence,
			StartSequence: 10,
		},
	}

	t.Run("start sequence inside retention", func(t *testing.T) {
		h := &Handler{}
		plan := h.planSubscription(basePlan, streamInfoSnapshot{HasSnapshot: true, FirstSeq: 5, LastSeq: 20})
		if plan.Replay.Mode != replayModeStartSequence {
			t.Fatalf("Replay mode = %s, want start_sequence", plan.Replay.Mode)
		}
		if plan.Replay.StartSequence != 10 {
			t.Fatalf("StartSequence = %d, want 10", plan.Replay.StartSequence)
		}
	})

	t.Run("below retention falls back to deliver all", func(t *testing.T) {
		h := &Handler{}
		plan := h.planSubscription(basePlan, streamInfoSnapshot{HasSnapshot: true, FirstSeq: 20, LastSeq: 30})
		if plan.Replay.Mode != replayModeFallbackDeliverAll {
			t.Fatalf("Replay mode = %s, want fallback_deliver_all", plan.Replay.Mode)
		}
		if plan.Replay.FallbackReason != "sequence below retention" {
			t.Fatalf("FallbackReason = %q", plan.Replay.FallbackReason)
		}
	})

	t.Run("below retention uses replay window", func(t *testing.T) {
		h := &Handler{ReplayWindow: 30}
		plan := h.planSubscription(basePlan, streamInfoSnapshot{HasSnapshot: true, FirstSeq: 20, LastSeq: 30})
		if plan.Replay.Mode != replayModeFallbackStartTime {
			t.Fatalf("Replay mode = %s, want fallback_start_time", plan.Replay.Mode)
		}
		if plan.Replay.StartTime.IsZero() {
			t.Fatal("StartTime was not set for replay-window fallback")
		}
	})

	t.Run("valid retained sequence outside replay window falls back", func(t *testing.T) {
		h := &Handler{ReplayWindow: 30}
		plan := h.planSubscription(basePlan, streamInfoSnapshot{
			HasSnapshot:          true,
			FirstSeq:             5,
			LastSeq:              20,
			StartSequenceTime:    time.Now().Add(-time.Minute),
			HasStartSequenceTime: true,
		})
		if plan.Replay.Mode != replayModeFallbackStartTime {
			t.Fatalf("Replay mode = %s, want fallback_start_time", plan.Replay.Mode)
		}
		if plan.Replay.StartTime.IsZero() {
			t.Fatal("StartTime was not set for replay-window fallback")
		}
	})

	t.Run("valid retained sequence inside replay window keeps sequence", func(t *testing.T) {
		h := &Handler{ReplayWindow: 30}
		plan := h.planSubscription(basePlan, streamInfoSnapshot{
			HasSnapshot:          true,
			FirstSeq:             5,
			LastSeq:              20,
			StartSequenceTime:    time.Now(),
			HasStartSequenceTime: true,
		})
		if plan.Replay.Mode != replayModeStartSequence {
			t.Fatalf("Replay mode = %s, want start_sequence", plan.Replay.Mode)
		}
	})

	// Pins the HasSnapshot propagation introduced in pass 7. A regression
	// that dropped `plan.Replay.HasSnapshot = snapshot.HasSnapshot` would
	// re-introduce the bug where live messages count toward
	// replay_max_messages on a transient StreamInfo failure. The two
	// sub-cases (snapshot observed vs not) lock the propagation in both
	// directions so a future refactor can't invert the assignment either.
	t.Run("propagates HasSnapshot=true to plan.Replay", func(t *testing.T) {
		h := &Handler{}
		plan := h.planSubscription(basePlan, streamInfoSnapshot{
			HasSnapshot: true, FirstSeq: 5, LastSeq: 20,
		})
		if !plan.Replay.HasSnapshot {
			t.Fatal("plan.Replay.HasSnapshot was not propagated from observed snapshot")
		}
	})

	t.Run("propagates HasSnapshot=false when StreamInfo failed", func(t *testing.T) {
		h := &Handler{}
		plan := h.planSubscription(basePlan, streamInfoSnapshot{HasSnapshot: false})
		if plan.Replay.HasSnapshot {
			t.Fatal("plan.Replay.HasSnapshot must stay false when no snapshot was observed; otherwise countsTowardReplayCap would re-enable on a transient StreamInfo blip")
		}
	})
}

func TestShouldSkipReplayWindowMessage(t *testing.T) {
	windowStart := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	plan := streamPlan{Replay: replayPlan{Mode: replayModeFallbackStartTime, StartTime: windowStart}}

	if !shouldSkipReplayWindowMessage(plan, formattedMessageEvent{MessageTime: windowStart.Add(-time.Nanosecond), HasMessageTime: true}) {
		t.Fatal("message before replay window should be skipped")
	}
	if shouldSkipReplayWindowMessage(plan, formattedMessageEvent{MessageTime: windowStart, HasMessageTime: true}) {
		t.Fatal("message at replay window start should be delivered")
	}
	if shouldSkipReplayWindowMessage(plan, formattedMessageEvent{MessageTime: windowStart.Add(time.Second), HasMessageTime: true}) {
		t.Fatal("message inside replay window should be delivered")
	}
	if shouldSkipReplayWindowMessage(streamPlan{Replay: replayPlan{Mode: replayModeStartSequence}}, formattedMessageEvent{MessageTime: windowStart.Add(-time.Second), HasMessageTime: true}) {
		t.Fatal("non-fallback replay should not apply replay-window skip")
	}
}

func TestHandler_ShouldUseReplayWindowWithoutStartSequenceTime(t *testing.T) {
	h := &Handler{ReplayWindow: 60}
	replay := replayPlan{HasLastID: true, StartSequence: 10}
	snapshot := streamInfoSnapshot{LastSeq: 20}

	if !h.shouldUseReplayWindow(replay, snapshot) {
		t.Fatal("missing start-sequence timestamp should force replay-window fallback")
	}
}

func TestHandler_OrderedConsumerConfig(t *testing.T) {
	windowStart := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	base := streamPlan{Topics: []string{"alpha", "beta"}, FullSubjects: []string{"events.alpha", "events.beta"}}
	cases := []struct {
		name        string
		replay      replayPlan
		wantPolicy  jetstream.DeliverPolicy
		wantSeq     uint64
		wantStartAt *time.Time
	}{
		{name: "no cursor and no snapshot delivers new", replay: replayPlan{Mode: replayModeDeliverNew}, wantPolicy: jetstream.DeliverNewPolicy},
		{name: "no cursor with snapshot starts after LastSeq", replay: replayPlan{Mode: replayModeDeliverNew, StartSequence: 42}, wantPolicy: jetstream.DeliverByStartSequencePolicy, wantSeq: 42},
		{name: "cursor starts at last-id+1", replay: replayPlan{Mode: replayModeStartSequence, HasLastID: true, StartSequence: 10}, wantPolicy: jetstream.DeliverByStartSequencePolicy, wantSeq: 10},
		{name: "time-bounded fallback uses the planned start", replay: replayPlan{Mode: replayModeFallbackStartTime, HasLastID: true, StartSequence: 10, StartTime: windowStart}, wantPolicy: jetstream.DeliverByStartTimePolicy, wantStartAt: &windowStart},
		{name: "deliver-all fallback", replay: replayPlan{Mode: replayModeFallbackDeliverAll, HasLastID: true, StartSequence: 10}, wantPolicy: jetstream.DeliverAllPolicy},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &Handler{StreamName: "EVENTS", ReplayWindow: 60}
			plan := base
			plan.Replay = c.replay
			cfg := h.orderedConsumerConfig(plan)
			if cfg.DeliverPolicy != c.wantPolicy {
				t.Fatalf("DeliverPolicy = %v, want %v", cfg.DeliverPolicy, c.wantPolicy)
			}
			if cfg.OptStartSeq != c.wantSeq {
				t.Fatalf("OptStartSeq = %d, want %d", cfg.OptStartSeq, c.wantSeq)
			}
			if c.wantStartAt == nil && cfg.OptStartTime != nil {
				t.Fatalf("OptStartTime = %v, want unset", cfg.OptStartTime)
			}
			if c.wantStartAt != nil && (cfg.OptStartTime == nil || !cfg.OptStartTime.Equal(*c.wantStartAt)) {
				t.Fatalf("OptStartTime = %v, want %v", cfg.OptStartTime, *c.wantStartAt)
			}
			if !reflect.DeepEqual(cfg.FilterSubjects, plan.FullSubjects) {
				t.Fatalf("FilterSubjects = %v, want %v", cfg.FilterSubjects, plan.FullSubjects)
			}
			if cfg.InactiveThreshold != defaultConsumerInactiveThreshold {
				t.Fatalf("InactiveThreshold = %v, want %v", cfg.InactiveThreshold, defaultConsumerInactiveThreshold)
			}
			if cfg.MaxResetAttempts != defaultConsumerMaxResetAttempts {
				t.Fatalf("MaxResetAttempts = %d, want %d", cfg.MaxResetAttempts, defaultConsumerMaxResetAttempts)
			}
			if !strings.HasPrefix(cfg.NamePrefix, consumerNamePrefix) || len(cfg.NamePrefix) <= len(consumerNamePrefix) {
				t.Fatalf("NamePrefix = %q, want %q plus a unique suffix", cfg.NamePrefix, consumerNamePrefix)
			}
		})
	}

	t.Run("time-bounded fallback without a planned start uses now-window", func(t *testing.T) {
		h := &Handler{StreamName: "EVENTS", ReplayWindow: 60}
		plan := base
		plan.Replay = replayPlan{Mode: replayModeFallbackStartTime, HasLastID: true, StartSequence: 10}
		before := time.Now().Add(-60 * time.Second)
		cfg := h.orderedConsumerConfig(plan)
		after := time.Now().Add(-60 * time.Second)
		if cfg.OptStartTime == nil || cfg.OptStartTime.Before(before) || cfg.OptStartTime.After(after) {
			t.Fatalf("OptStartTime = %v, want within [%v, %v]", cfg.OptStartTime, before, after)
		}
	})

	t.Run("each consumer gets a distinct name prefix", func(t *testing.T) {
		h := &Handler{StreamName: "EVENTS"}
		if a, b := h.orderedConsumerConfig(base).NamePrefix, h.orderedConsumerConfig(base).NamePrefix; a == b {
			t.Fatalf("two consumers share NamePrefix %q", a)
		}
	})
}

func TestHandler_PullOptions(t *testing.T) {
	cases := []struct {
		name          string
		buffer        int
		heartbeat     int
		wantMax       int
		wantHeartbeat time.Duration
	}{
		{name: "defaults", buffer: 0, heartbeat: 0, wantMax: defaultClientBufferSize},
		{name: "custom buffer and heartbeat", buffer: 8, heartbeat: 5, wantMax: 8, wantHeartbeat: 5 * time.Second},
		{name: "disabled heartbeat leaves the library default", buffer: 16, heartbeat: natsIdleHeartbeatDisabledSentinel, wantMax: 16},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &Handler{ClientBufferSize: c.buffer, NatsIdleHeartbeat: c.heartbeat}
			var gotMax int
			var gotHeartbeat time.Duration
			for _, opt := range h.pullOptions() {
				switch o := opt.(type) {
				case jetstream.PullMaxMessages:
					gotMax = int(o)
				case jetstream.PullHeartbeat:
					gotHeartbeat = time.Duration(o)
				default:
					t.Fatalf("unexpected pull option %T", opt)
				}
			}
			if gotMax != c.wantMax {
				t.Fatalf("PullMaxMessages = %d, want %d", gotMax, c.wantMax)
			}
			if gotHeartbeat != c.wantHeartbeat {
				t.Fatalf("PullHeartbeat = %v, want %v", gotHeartbeat, c.wantHeartbeat)
			}
		})
	}
}

func TestHandler_LogSubscriptionCountsOnlyFallbacks(t *testing.T) {
	h := &Handler{ReplayWindow: 60, logger: zap.NewNop()}
	for _, c := range []struct {
		mode replayMode
		want float64
	}{
		{replayModeDeliverNew, 0},
		{replayModeStartSequence, 0},
		{replayModeFallbackStartTime, 1},
		{replayModeFallbackDeliverAll, 1},
	} {
		before := metricValue(t, metricsReplayFallbacks)
		h.logSubscription(streamPlan{Replay: replayPlan{Mode: c.mode}})
		if got := metricValue(t, metricsReplayFallbacks) - before; got != c.want {
			t.Errorf("mode %s: nuts_replay_fallbacks_total delta = %v, want %v", c.mode, got, c.want)
		}
	}
}

func TestReplayHistory(t *testing.T) {
	seq := func(n uint64) formattedMessageEvent {
		return formattedMessageEvent{HasStreamSequence: true, StreamSequence: n}
	}

	t.Run("requests without a cursor have no history", func(t *testing.T) {
		r := newReplayHistory(streamPlan{Replay: replayPlan{HasSnapshot: true, CapSequence: 20}})
		if r.isHistory(seq(1)) {
			t.Fatal("message counted as history for a request without a cursor")
		}
	})

	t.Run("snapshot boundary is the planned LastSeq", func(t *testing.T) {
		r := newReplayHistory(streamPlan{Replay: replayPlan{HasLastID: true, HasSnapshot: true, CapSequence: 20}})
		if !r.isHistory(seq(20)) {
			t.Fatal("sequence at the boundary is history")
		}
		if r.isHistory(seq(21)) {
			t.Fatal("sequence past the boundary is live")
		}
	})

	t.Run("empty stream at plan time means every message is live", func(t *testing.T) {
		r := newReplayHistory(streamPlan{Replay: replayPlan{HasLastID: true, HasSnapshot: true, CapSequence: 0}})
		if r.isHistory(seq(1)) {
			t.Fatal("message on a stream that was empty at plan time counted as history")
		}
	})

	t.Run("message without metadata counts", func(t *testing.T) {
		r := newReplayHistory(streamPlan{Replay: replayPlan{HasLastID: true, HasSnapshot: true, CapSequence: 20}})
		if !r.isHistory(formattedMessageEvent{}) {
			t.Fatal("metadata-less message must count toward the replay budget")
		}
	})

	t.Run("without a snapshot the first pending count sizes the backlog", func(t *testing.T) {
		r := newReplayHistory(streamPlan{Replay: replayPlan{HasLastID: true}})
		first := seq(5)
		first.NumPending = 2
		got := []bool{r.isHistory(first), r.isHistory(seq(6)), r.isHistory(seq(7)), r.isHistory(seq(8))}
		if want := []bool{true, true, true, false}; !reflect.DeepEqual(got, want) {
			t.Fatalf("history = %v, want %v", got, want)
		}
	})
}

func TestHandler_RecordDroppedMessageLogsFormattedEvent(t *testing.T) {
	h := &Handler{MaxEventSize: 64, logger: zap.NewNop()}
	before := metricValue(t, metricsMessagesDropped.WithLabelValues(dropReasonFormattedSSEMessage))

	h.recordDroppedMessage(formattedMessageEvent{
		Subject:    "events.big",
		DropReason: dropReasonFormattedSSEMessage,
		DropSize:   128,
	})

	if got := metricValue(t, metricsMessagesDropped.WithLabelValues(dropReasonFormattedSSEMessage)); got != before+1 {
		t.Fatalf("messages dropped metric (reason=formatted_sse_message) did not increment: before=%v got=%v", before, got)
	}
}

func TestHandler_ServeReadinessCheckReportsMissingRuntime(t *testing.T) {
	h := &Handler{logger: zap.NewNop()}
	rr := httptest.NewRecorder()

	// Pass-6 contract: a single 503 response must bump exactly one cause
	// label (the first matched). The body still reports every degradation
	// it observed (nats=disconnected AND stream=unavailable), but the
	// metric must keep its 1:1 with probe-failure count so
	// sum(rate(nuts_readiness_failures_total[...])) tracks the real
	// /readyz error rate.
	beforeNats := metricValue(t, metricsReadinessFailures.WithLabelValues("nats_disconnected"))
	beforeJS := metricValue(t, metricsReadinessFailures.WithLabelValues("jetstream_missing"))
	beforeStream := metricValue(t, metricsReadinessFailures.WithLabelValues("stream_info_error"))

	if err := h.serveReadinessCheck(rr); err != nil {
		t.Fatalf("serveReadinessCheck: %v", err)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
	for _, needle := range []string{`"status":"degraded"`, `"nats":"disconnected"`, `"stream":"unavailable"`} {
		if !strings.Contains(rr.Body.String(), needle) {
			t.Fatalf("readiness body missing %s: %s", needle, rr.Body.String())
		}
	}
	// First matched cause (nats_disconnected) increments by exactly 1.
	if got := metricValue(t, metricsReadinessFailures.WithLabelValues("nats_disconnected")); got != beforeNats+1 {
		t.Errorf("nuts_readiness_failures_total{cause=nats_disconnected} = %v, want %v (exactly one increment)", got, beforeNats+1)
	}
	// Subsequent causes must NOT increment for the same 503 — the one-shot
	// guard prevents the documented 1:1 contract from being violated.
	if got := metricValue(t, metricsReadinessFailures.WithLabelValues("jetstream_missing")); got != beforeJS {
		t.Errorf("nuts_readiness_failures_total{cause=jetstream_missing} = %v, want %v (must not double-count when nats_disconnected already fired)", got, beforeJS)
	}
	if got := metricValue(t, metricsReadinessFailures.WithLabelValues("stream_info_error")); got != beforeStream {
		t.Errorf("nuts_readiness_failures_total{cause=stream_info_error} = %v, want %v", got, beforeStream)
	}
}

func TestHandler_PlanSubscriptionRejectsSingleTopicOutsideStream(t *testing.T) {
	h := &Handler{}
	plan := streamPlan{Topics: []string{"orders"}, FullSubjects: []string{"orders"}, Replay: replayPlan{Mode: replayModeDeliverNew}}

	got := h.planSubscription(plan, streamInfoSnapshot{HasSnapshot: true, Subjects: []string{"events.>"}, LastSeq: 5})
	if !reflect.DeepEqual(got.FailedTopics, []string{"orders"}) {
		t.Fatalf("FailedTopics = %#v, want [orders]", got.FailedTopics)
	}
	if got.Replay.StartSequence != 0 {
		t.Fatalf("StartSequence = %d, want 0 for a rejected plan", got.Replay.StartSequence)
	}
}

func TestHandler_PlanSubscriptionGivesNoCursorRequestsAnExplicitStart(t *testing.T) {
	h := &Handler{}
	plan := streamPlan{Topics: []string{"alpha"}, FullSubjects: []string{"events.alpha"}, Replay: replayPlan{Mode: replayModeDeliverNew}}

	got := h.planSubscription(plan, streamInfoSnapshot{HasSnapshot: true, Subjects: []string{"events.>"}, FirstSeq: 3, LastSeq: 41})
	if got.Replay.Mode != replayModeDeliverNew || got.Replay.StartSequence != 42 || got.Replay.HasLastID {
		t.Fatalf("Replay = %+v, want deliver_new starting at 42", got.Replay)
	}
	if id, ok := connectedEventID(got); !ok || id != 41 {
		t.Fatalf("connectedEventID = %d, %v; want 41, true", id, ok)
	}

	empty := h.planSubscription(plan, streamInfoSnapshot{HasSnapshot: true})
	if empty.Replay.StartSequence != 1 {
		t.Fatalf("empty stream StartSequence = %d, want 1", empty.Replay.StartSequence)
	}
	if id, ok := connectedEventID(empty); !ok || id != 0 {
		t.Fatalf("empty stream connectedEventID = %d, %v; want 0, true", id, ok)
	}

	noSnapshot := h.planSubscription(plan, streamInfoSnapshot{})
	if noSnapshot.Replay.StartSequence != 0 {
		t.Fatalf("StartSequence without snapshot = %d, want 0 (DeliverNew)", noSnapshot.Replay.StartSequence)
	}
	if _, ok := connectedEventID(noSnapshot); ok {
		t.Fatal("connectedEventID must be absent without a snapshot")
	}
}

func TestConnectedEventID(t *testing.T) {
	cases := []struct {
		name   string
		replay replayPlan
		wantID uint64
		wantOK bool
	}{
		{name: "deliver new without a start", replay: replayPlan{Mode: replayModeDeliverNew}},
		{name: "deliver new after LastSeq", replay: replayPlan{Mode: replayModeDeliverNew, StartSequence: 8}, wantID: 7, wantOK: true},
		{name: "cursor resumes at last-id", replay: replayPlan{Mode: replayModeStartSequence, HasLastID: true, StartSequence: 11}, wantID: 10, wantOK: true},
		{name: "time-bounded fallback keeps the client cursor", replay: replayPlan{Mode: replayModeFallbackStartTime, HasLastID: true, StartSequence: 11}},
		{name: "deliver-all fallback keeps the client cursor", replay: replayPlan{Mode: replayModeFallbackDeliverAll, HasLastID: true, StartSequence: 11}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, ok := connectedEventID(streamPlan{Replay: c.replay})
			if id != c.wantID || ok != c.wantOK {
				t.Fatalf("connectedEventID = %d, %v; want %d, %v", id, ok, c.wantID, c.wantOK)
			}
		})
	}
}

func TestFormatConnectedEvent(t *testing.T) {
	withID := formatConnectedEvent(streamPlan{Topics: []string{"a", "b"}, Replay: replayPlan{Mode: replayModeDeliverNew, StartSequence: 43}})
	if withID != "event: connected\ndata: {\"topics\":[\"a\",\"b\"]}\nid: 42\n\n" {
		t.Fatalf("connected event with id = %q", withID)
	}
	withoutID := formatConnectedEvent(streamPlan{Topics: []string{"a"}, Replay: replayPlan{Mode: replayModeDeliverNew}})
	if withoutID != "event: connected\ndata: {\"topics\":[\"a\"]}\n\n" {
		t.Fatalf("connected event without id = %q", withoutID)
	}
}

func TestSubjectMatchesFilterRejectsEmptyValues(t *testing.T) {
	if subjectMatchesFilter("", ">") {
		t.Fatal("empty subject must not match bare wildcard")
	}
	if subjectMatchesFilter("orders.created", "") {
		t.Fatal("non-empty subject must not match empty filter")
	}
	if subjectMatchesFilter("orders", "orders.created") {
		t.Fatal("short subject must not match a longer exact filter")
	}
}

// TestSubjectMatchesFilter_GreaterThanTerminatedFilterTokenCount covers
// the specific edge case the original example tests missed: a subject
// with FEWER tokens than a `>`-terminated filter. NATS requires `X.>` to
// match `X.<one-or-more-tokens>`, so a bare `orders` (no dots) must NOT
// match `orders.>`. Symmetrically the multi-token case must match.
func TestSubjectMatchesFilter_GreaterThanTerminatedFilterTokenCount(t *testing.T) {
	cases := []struct {
		subject string
		filter  string
		want    bool
	}{
		{"orders", "orders.>", false},             // FEWER tokens than filter requires
		{"orders.created", "orders.>", true},      // exactly one trailing token
		{"orders.created.gold", "orders.>", true}, // multiple trailing tokens
		{"events.x.y.z", "events.>", true},        // deep nesting
		{"events", "events.>", false},             // root token only
		{"events.foo.bar.baz", "events.foo.>", true},
		{"events.foo", "events.foo.>", false},
	}
	for _, c := range cases {
		t.Run(c.subject+" vs "+c.filter, func(t *testing.T) {
			if got := subjectMatchesFilter(c.subject, c.filter); got != c.want {
				t.Errorf("subjectMatchesFilter(%q, %q) = %v, want %v", c.subject, c.filter, got, c.want)
			}
		})
	}
}

// TestHandler_ParseStreamRequest_TopicPrefixVariants verifies subject
// construction in parseStreamRequest across non-`events.` prefix shapes
// the existing tests never exercised: empty prefix (path-shorthand),
// no-trailing-dot prefix (legacy operator typo), and multi-segment
// prefix. Sibling test TestHandler_PlanSubscriptionDetectsMultiTopicStream-
// Mismatch covers planSubscription itself; this test deliberately
// targets the earlier subject-assembly step.
func TestHandler_ParseStreamRequest_TopicPrefixVariants(t *testing.T) {
	cases := []struct {
		name             string
		topicPrefix      string
		topics           []string
		wantFullSubjects []string
	}{
		{"empty prefix yields bare topics", "", []string{"orders.new"}, []string{"orders.new"}},
		{"trailing-dot prefix concatenates", "events.", []string{"new"}, []string{"events.new"}},
		{"no-trailing-dot prefix concatenates as-is", "tenant1", []string{"orders.new"}, []string{"tenant1orders.new"}},
		{"multi-segment prefix is preserved", "team.alpha.", []string{"alerts"}, []string{"team.alpha.alerts"}},
		{"multiple topics under multi-segment prefix", "team.alpha.", []string{"alerts", "logs.error"}, []string{"team.alpha.alerts", "team.alpha.logs.error"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &Handler{TopicPrefix: c.topicPrefix, logger: zap.NewNop()}
			q := ""
			for _, top := range c.topics {
				if q != "" {
					q += "&"
				}
				q += "topic=" + top
			}
			req := httptest.NewRequest(http.MethodGet, "/events?"+q, nil)
			plan, reqErr := h.parseStreamRequest(req)
			if reqErr != nil {
				t.Fatalf("parseStreamRequest: %#v", reqErr)
			}
			if !reflect.DeepEqual(plan.FullSubjects, c.wantFullSubjects) {
				t.Errorf("FullSubjects = %#v, want %#v", plan.FullSubjects, c.wantFullSubjects)
			}
		})
	}
}

func TestMatchesConfiguredPathNormalizesConfiguredPath(t *testing.T) {
	if !matchesConfiguredPath("/events/live", "live", defaultLivePath) {
		t.Fatal("configured path without leading slash should match as a path suffix")
	}
}

func TestAllowedMethodsHeaderFallsBackWhenNoServedMethodsConfigured(t *testing.T) {
	if got := allowedMethodsHeader([]string{"POST", "TRACE"}); got != "GET, OPTIONS" {
		t.Fatalf("allowedMethodsHeader() = %q, want GET, OPTIONS", got)
	}
}

func TestHandler_PlanSubscriptionDetectsMultiTopicStreamMismatch(t *testing.T) {
	h := &Handler{}
	plan := streamPlan{
		Topics:       []string{"allowed", "blocked"},
		FullSubjects: []string{"events.allowed", "events.blocked"},
		Replay:       replayPlan{Mode: replayModeDeliverNew},
	}

	got := h.planSubscription(plan, streamInfoSnapshot{Subjects: []string{"events.allowed"}})
	if !reflect.DeepEqual(got.FailedTopics, []string{"blocked"}) {
		t.Fatalf("FailedTopics = %#v, want blocked", got.FailedTopics)
	}
}

func TestHandler_FormatMessageEventEmbedsJSONPayload(t *testing.T) {
	h := &Handler{TopicPrefix: "events.", MaxEventSize: -1}
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	msg := streamMessage{Subject: "events.json", Data: []byte(`{"n":900719925474099312345}`)}

	formatted := h.formatMessageEvent(msg, now)
	if formatted.Dropped {
		t.Fatalf("event was unexpectedly dropped: %#v", formatted)
	}
	for _, needle := range []string{`event: message`, `"topic":"json"`, `"payload":{"n":900719925474099312345}`, `"time":"2026-04-28T12:00:00Z"`} {
		if !strings.Contains(formatted.Frame, needle) {
			t.Fatalf("formatted frame missing %q:\n%s", needle, formatted.Frame)
		}
	}
}

func TestHandler_FormatMessageEventEmbedsRawStringPayload(t *testing.T) {
	h := &Handler{TopicPrefix: "events.", MaxEventSize: -1}
	msg := streamMessage{Subject: "events.raw", Data: []byte("plain text")}

	formatted := h.formatMessageEvent(msg, time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC))
	if formatted.Dropped {
		t.Fatalf("event was unexpectedly dropped: %#v", formatted)
	}
	if !strings.Contains(formatted.Frame, `"payload":"plain text"`) {
		t.Fatalf("formatted frame did not encode raw payload as string:\n%s", formatted.Frame)
	}
}

func TestHandler_FormatMessageEventUsesJetStreamMetadata(t *testing.T) {
	h := &Handler{TopicPrefix: "events.", MaxEventSize: -1}
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	metaTime := time.Date(2026, 4, 28, 12, 1, 2, 0, time.UTC)
	msg := streamMessage{
		Subject:        "events.meta",
		Data:           []byte(`{"ok":true}`),
		HasMetadata:    true,
		StreamSequence: 42,
		ConsumerName:   "nuts_abc_1",
		Timestamp:      metaTime,
	}

	formatted := h.formatMessageEvent(msg, now)
	if formatted.MetadataErr != nil {
		t.Fatalf("MetadataErr = %v", formatted.MetadataErr)
	}
	if !strings.Contains(formatted.Frame, "id: 42\n") {
		t.Fatalf("formatted frame missing stream sequence id:\n%s", formatted.Frame)
	}
	if !strings.Contains(formatted.Frame, `"time":"2026-04-28T12:01:02Z"`) {
		t.Fatalf("formatted frame missing metadata timestamp:\n%s", formatted.Frame)
	}
	if strings.Contains(formatted.Frame, now.Format(time.RFC3339)) {
		t.Fatalf("formatted frame used fallback clock instead of metadata timestamp:\n%s", formatted.Frame)
	}
	if !formatted.HasStreamSequence || formatted.StreamSequence != 42 || formatted.ConsumerName != "nuts_abc_1" {
		t.Fatalf("formatted metadata = seq %d (has=%v) consumer %q", formatted.StreamSequence, formatted.HasStreamSequence, formatted.ConsumerName)
	}
	if !formatted.HasMessageTime || !formatted.MessageTime.Equal(metaTime) {
		t.Fatalf("MessageTime = %v (has=%v), want %v", formatted.MessageTime, formatted.HasMessageTime, metaTime)
	}
}

func TestHandler_FormatMessageEventRejectsOversizedEvents(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	t.Run("raw payload", func(t *testing.T) {
		h := &Handler{TopicPrefix: "events.", MaxEventSize: 4}
		formatted := h.formatMessageEvent(streamMessage{Subject: "events.big", Data: []byte("12345"), HasMetadata: true, StreamSequence: 7}, now)
		if !formatted.Dropped || formatted.DropReason != dropReasonRawPayload || formatted.DropSize != 5 {
			t.Fatalf("formatted = %#v, want raw payload drop", formatted)
		}
		if formatted.StreamSequence != 7 {
			t.Fatalf("dropped event StreamSequence = %d, want 7 for the drop log", formatted.StreamSequence)
		}
		if fits := h.formatMessageEvent(streamMessage{Subject: "events.fits", Data: []byte("1234")}, now); fits.DropReason == dropReasonRawPayload {
			t.Fatal("payload exactly at max_event_size must not be dropped as raw payload")
		}
	})

	t.Run("formatted SSE event", func(t *testing.T) {
		h := &Handler{TopicPrefix: "events.", MaxEventSize: 10}
		formatted := h.formatMessageEvent(streamMessage{Subject: "events.small", Data: []byte(`{}`)}, now)
		if !formatted.Dropped || formatted.DropReason != dropReasonFormattedSSEMessage {
			t.Fatalf("formatted = %#v, want formatted SSE drop", formatted)
		}
		if formatted.DropSize <= len(`{}`) {
			t.Fatalf("DropSize = %d, want formatted event size", formatted.DropSize)
		}
	})
}

// fakeStream stubs the two jetstream.Stream methods readStreamSnapshot uses;
// any other method panics through the nil embedded interface.
type fakeStream struct {
	jetstream.Stream
	info      *jetstream.StreamInfo
	msg       *jetstream.RawStreamMsg
	getMsgErr error
}

func (f fakeStream) CachedInfo() *jetstream.StreamInfo { return f.info }

func (f fakeStream) GetMsg(_ context.Context, _ uint64, _ ...jetstream.GetMsgOpt) (*jetstream.RawStreamMsg, error) {
	return f.msg, f.getMsgErr
}

// fakeStreamLookup is a streamLookup that returns a fixed stream or error.
type fakeStreamLookup struct {
	stream jetstream.Stream
	err    error
}

func (f fakeStreamLookup) Stream(_ context.Context, _ string) (jetstream.Stream, error) {
	return f.stream, f.err
}

func TestHandler_ReadStreamSnapshot_StreamInfoErrorReturnsEmptySnapshot(t *testing.T) {
	h := &Handler{StreamName: "EVENTS", logger: zap.NewNop()}
	plan := streamPlan{Replay: replayPlan{HasLastID: true}}

	snapshot := h.readStreamSnapshot(context.Background(), fakeStreamLookup{err: errors.New("stream info boom")}, plan)

	if !reflect.DeepEqual(snapshot, streamInfoSnapshot{}) {
		t.Errorf("readStreamSnapshot with StreamInfo error: got %+v, want zero-value snapshot", snapshot)
	}
}

func TestHandler_ReadStreamSnapshot_ReadsStateForEveryRequest(t *testing.T) {
	h := &Handler{StreamName: "EVENTS", logger: zap.NewNop()}
	stream := fakeStream{info: &jetstream.StreamInfo{
		State:  jetstream.StreamState{FirstSeq: 3, LastSeq: 9},
		Config: jetstream.StreamConfig{Subjects: []string{"events.>"}},
	}}
	plan := streamPlan{Topics: []string{"alpha"}, FullSubjects: []string{"events.alpha"}, Replay: replayPlan{Mode: replayModeDeliverNew}}

	snapshot := h.readStreamSnapshot(context.Background(), fakeStreamLookup{stream: stream}, plan)

	want := streamInfoSnapshot{HasSnapshot: true, FirstSeq: 3, LastSeq: 9, Subjects: []string{"events.>"}}
	if !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("snapshot = %+v, want %+v", snapshot, want)
	}
}

func TestHandler_ReadStreamSnapshot_ReadsStartSequenceTimeUnderReplayWindow(t *testing.T) {
	h := &Handler{StreamName: "EVENTS", ReplayWindow: 60, logger: zap.NewNop()}
	published := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	stream := fakeStream{
		info: &jetstream.StreamInfo{State: jetstream.StreamState{FirstSeq: 1, LastSeq: 10}},
		msg:  &jetstream.RawStreamMsg{Sequence: 5, Time: published},
	}
	plan := streamPlan{Replay: replayPlan{HasLastID: true, StartSequence: 5}}

	snapshot := h.readStreamSnapshot(context.Background(), fakeStreamLookup{stream: stream}, plan)

	if !snapshot.HasStartSequenceTime || !snapshot.StartSequenceTime.Equal(published) {
		t.Fatalf("StartSequenceTime = %v (has=%v), want %v", snapshot.StartSequenceTime, snapshot.HasStartSequenceTime, published)
	}

	// Below retention the message cannot exist, so it is not looked up.
	belowRetention := h.readStreamSnapshot(context.Background(), fakeStreamLookup{stream: stream}, streamPlan{Replay: replayPlan{HasLastID: true, StartSequence: 0}})
	if belowRetention.HasStartSequenceTime {
		t.Fatal("StartSequenceTime read for a sequence below FirstSeq")
	}
	// Without replay_window the timestamp is not needed.
	h.ReplayWindow = 0
	if noWindow := h.readStreamSnapshot(context.Background(), fakeStreamLookup{stream: stream}, plan); noWindow.HasStartSequenceTime {
		t.Fatal("StartSequenceTime read without replay_window")
	}
}

func TestHandler_ReadStreamSnapshot_GetMsgErrorKeepsSnapshotWithoutStartTime(t *testing.T) {
	h := &Handler{StreamName: "EVENTS", ReplayWindow: 60, logger: zap.NewNop()}
	plan := streamPlan{Replay: replayPlan{HasLastID: true, StartSequence: 5}}
	stream := fakeStream{
		info: &jetstream.StreamInfo{
			State:  jetstream.StreamState{FirstSeq: 1, LastSeq: 10},
			Config: jetstream.StreamConfig{Subjects: []string{"events.>"}},
		},
		getMsgErr: errors.New("get msg boom"),
	}

	snapshot := h.readStreamSnapshot(context.Background(), fakeStreamLookup{stream: stream}, plan)

	if snapshot.HasStartSequenceTime {
		t.Errorf("HasStartSequenceTime = true on GetMsg error, want false")
	}
	if snapshot.FirstSeq != 1 || snapshot.LastSeq != 10 {
		t.Errorf("Seq range: got FirstSeq=%d LastSeq=%d, want 1/10", snapshot.FirstSeq, snapshot.LastSeq)
	}
}

// TestVaryContains covers the token-membership semantic that backs
// setCORSHeaders' idempotent Vary handling. A simple
// slices.Contains-style element check would miss the case where an
// upstream middleware combined multiple Vary entries into a single
// comma-separated header value.
func TestVaryContains(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		token  string
		want   bool
	}{
		{"absent", nil, "Origin", false},
		{"single element exact", []string{"Origin"}, "Origin", true},
		{"single element case-insensitive", []string{"origin"}, "Origin", true},
		{"two separate Add calls", []string{"Accept-Encoding", "Origin"}, "Origin", true},
		{"combined value Origin first", []string{"Origin, Accept-Encoding"}, "Origin", true},
		{"combined value Origin last", []string{"Accept-Encoding, Origin"}, "Origin", true},
		{"combined value Origin middle", []string{"Accept-Encoding, Origin, User-Agent"}, "Origin", true},
		{"combined value without Origin", []string{"Accept-Encoding, User-Agent"}, "Origin", false},
		{"whitespace surrounding token", []string{" Origin "}, "Origin", true},
		{"prefix only match should not count", []string{"OriginHeader"}, "Origin", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := varyContains(c.values, c.token); got != c.want {
				t.Errorf("varyContains(%v, %q) = %v, want %v", c.values, c.token, got, c.want)
			}
		})
	}
}

// TestHandler_ParseStreamRequest_LastIDBoundary exercises the
// maxReplayCursor cut-off explicitly for both transport surfaces:
//
//   - `?last-id=<v>` query: a value of maxReplayCursor-1 or above must
//     400 because Subscribe with StartSequence(parsedID+1) would land
//     ON the maxReplayCursor sentinel (parsedID == maxReplayCursor-1)
//     or wrap past it (parsedID == maxReplayCursor). The highest
//     legitimate accepted value is therefore maxReplayCursor-2, whose
//     StartSequence resolves to maxReplayCursor-1.
//   - `Last-Event-ID: <v>` header: same overflow must NOT 400 — browsers
//     would loop forever on EventSource reconnect. Instead log a warning
//     and fall back to DeliverNew (HasLastID stays false).
//
// Without this test a regression that flips the comparison from `>=`
// back to `==`, drops the guard, or swaps which surface gets the soft
// fallback would land silently. Pass 7 tightened the cap from `==
// maxReplayCursor` to `>= maxReplayCursor-1` because parsedID+1 on the
// off-by-one input lands exactly on the reserved sentinel and JetStream
// silently parks the consumer at a sequence that will never arrive.
func TestHandler_ParseStreamRequest_LastIDBoundary(t *testing.T) {
	maxStr := strconv.FormatUint(maxReplayCursor, 10)            // 18446744073709551615 — uint64 max, reserved
	offByOneStr := strconv.FormatUint(maxReplayCursor-1, 10)     // 18446744073709551614 — parsedID+1 hits sentinel
	acceptedHighStr := strconv.FormatUint(maxReplayCursor-2, 10) // 18446744073709551613 — highest legitimate
	overflow21Str := "99999999999999999999"                      // 20 digits but > MaxUint64
	tooLongStr := strings.Repeat("9", 21)                        // length-cap triggered first

	cases := []struct {
		name        string
		query       string
		header      string
		wantStatus  int
		wantHasID   bool
		wantStartAt uint64
	}{
		{"query at max rejected", maxStr, "", http.StatusBadRequest, false, 0},
		{"query off-by-one rejected (parsedID+1 hits sentinel)", offByOneStr, "", http.StatusBadRequest, false, 0},
		{"query highest legitimate accepted", acceptedHighStr, "", 0, true, maxReplayCursor - 2},
		{"query parseuint overflow rejected", overflow21Str, "", http.StatusBadRequest, false, 0},
		{"query too long rejected", tooLongStr, "", http.StatusBadRequest, false, 0},
		{"header at max falls back to DeliverNew", "", maxStr, 0, false, 0},
		{"header off-by-one falls back", "", offByOneStr, 0, false, 0},
		{"header highest legitimate accepted", "", acceptedHighStr, 0, true, maxReplayCursor - 2},
		{"header parseuint overflow falls back", "", overflow21Str, 0, false, 0},
		{"header too long falls back", "", tooLongStr, 0, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &Handler{TopicPrefix: "events.", logger: zap.NewNop()}
			u := "/events?topic=x"
			if c.query != "" {
				u += "&last-id=" + c.query
			}
			req := httptest.NewRequest(http.MethodGet, u, nil)
			if c.header != "" {
				req.Header.Set("Last-Event-ID", c.header)
			}
			plan, reqErr := h.parseStreamRequest(req)
			if c.wantStatus != 0 {
				if reqErr == nil {
					t.Fatalf("expected streamRequestError with status %d, got nil", c.wantStatus)
				}
				if reqErr.status != c.wantStatus {
					t.Fatalf("status = %d, want %d (message=%q)", reqErr.status, c.wantStatus, reqErr.message)
				}
				return
			}
			if reqErr != nil {
				t.Fatalf("unexpected streamRequestError: status=%d message=%q", reqErr.status, reqErr.message)
			}
			if plan.Replay.HasLastID != c.wantHasID {
				t.Fatalf("HasLastID = %v, want %v", plan.Replay.HasLastID, c.wantHasID)
			}
			if c.wantHasID && plan.Replay.StartSequence != c.wantStartAt+1 {
				t.Fatalf("StartSequence = %d, want %d", plan.Replay.StartSequence, c.wantStartAt+1)
			}
		})
	}
}

func TestHandler_PlanSubscriptionReplayFallbacks(t *testing.T) {
	base := streamPlan{Topics: []string{"alpha"}, FullSubjects: []string{"events.alpha"}}
	cursor := func(startSeq uint64) streamPlan {
		p := base
		p.Replay = replayPlan{HasLastID: true, Mode: replayModeStartSequence, StartSequence: startSeq}
		return p
	}
	cases := []struct {
		name       string
		window     int
		plan       streamPlan
		snapshot   streamInfoSnapshot
		wantMode   replayMode
		wantReason string
	}{
		{name: "caught-up cursor resumes normally", plan: cursor(11), snapshot: streamInfoSnapshot{HasSnapshot: true, FirstSeq: 1, LastSeq: 10}, wantMode: replayModeStartSequence},
		// The caught-up check must run before the "start time unknown"
		// fallback: the next sequence does not exist yet, so its time can
		// never be read, and a caught-up client would replay the window.
		{name: "caught-up cursor under a window resumes normally", window: 60, plan: cursor(11), snapshot: streamInfoSnapshot{HasSnapshot: true, FirstSeq: 1, LastSeq: 10}, wantMode: replayModeStartSequence},
		{name: "cursor ahead of the stream falls back", plan: cursor(12), snapshot: streamInfoSnapshot{HasSnapshot: true, FirstSeq: 1, LastSeq: 10}, wantMode: replayModeFallbackDeliverAll, wantReason: "cursor ahead of stream"},
		{name: "cursor ahead of an empty recreated stream falls back", plan: cursor(51), snapshot: streamInfoSnapshot{HasSnapshot: true}, wantMode: replayModeFallbackDeliverAll, wantReason: "cursor ahead of stream"},
		{name: "cursor ahead with a window uses the window", window: 60, plan: cursor(12), snapshot: streamInfoSnapshot{HasSnapshot: true, FirstSeq: 1, LastSeq: 10}, wantMode: replayModeFallbackStartTime, wantReason: "cursor ahead of stream"},
		{name: "no snapshot with a window fails closed", window: 60, plan: cursor(5), snapshot: streamInfoSnapshot{}, wantMode: replayModeFallbackStartTime, wantReason: "stream info unavailable"},
		{name: "no snapshot without a window keeps the cursor", plan: cursor(5), snapshot: streamInfoSnapshot{}, wantMode: replayModeStartSequence},
		{name: "below retention falls back", plan: cursor(2), snapshot: streamInfoSnapshot{HasSnapshot: true, FirstSeq: 5, LastSeq: 10}, wantMode: replayModeFallbackDeliverAll, wantReason: "sequence below retention"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &Handler{ReplayWindow: c.window}
			got := h.planSubscription(c.plan, c.snapshot)
			if got.Replay.Mode != c.wantMode || got.Replay.FallbackReason != c.wantReason {
				t.Fatalf("plan = %s (%q), want %s (%q)", got.Replay.Mode, got.Replay.FallbackReason, c.wantMode, c.wantReason)
			}
			if got.Replay.StartSequence != c.plan.Replay.StartSequence {
				t.Fatalf("StartSequence = %d, want the requested %d kept for logs", got.Replay.StartSequence, c.plan.Replay.StartSequence)
			}
		})
	}
}

// countingStream serves GetMsg from a script, recording each call.
type countingStream struct {
	fakeStream
	getMsg []func() (*jetstream.RawStreamMsg, error)
	calls  int
}

func (c *countingStream) GetMsg(_ context.Context, _ uint64, _ ...jetstream.GetMsgOpt) (*jetstream.RawStreamMsg, error) {
	call := c.getMsg[c.calls]
	c.calls++
	return call()
}

func TestHandler_ReadStreamSnapshot_DeletedResumeMessageUsesTheNextMessage(t *testing.T) {
	h := &Handler{StreamName: "EVENTS", ReplayWindow: 3600, logger: zap.NewNop()}
	next := time.Date(2026, 9, 26, 11, 0, 0, 0, time.UTC)
	stream := &countingStream{
		fakeStream: fakeStream{info: &jetstream.StreamInfo{State: jetstream.StreamState{FirstSeq: 1, LastSeq: 5}}},
		getMsg: []func() (*jetstream.RawStreamMsg, error){
			func() (*jetstream.RawStreamMsg, error) { return nil, jetstream.ErrMsgNotFound },
			func() (*jetstream.RawStreamMsg, error) { return &jetstream.RawStreamMsg{Sequence: 4, Time: next}, nil },
		},
	}
	plan := streamPlan{Replay: replayPlan{HasLastID: true, StartSequence: 3}}

	snapshot := h.readStreamSnapshot(context.Background(), fakeStreamLookup{stream: stream}, plan)
	if stream.calls != 2 {
		t.Fatalf("GetMsg calls = %d, want 2 (exact, then next)", stream.calls)
	}
	if !snapshot.HasStartSequenceTime || !snapshot.StartSequenceTime.Equal(next) {
		t.Fatalf("StartSequenceTime = %v (has=%v), want the next message's time %v", snapshot.StartSequenceTime, snapshot.HasStartSequenceTime, next)
	}

	other := &countingStream{
		fakeStream: fakeStream{info: &jetstream.StreamInfo{State: jetstream.StreamState{FirstSeq: 1, LastSeq: 5}}},
		getMsg: []func() (*jetstream.RawStreamMsg, error){
			func() (*jetstream.RawStreamMsg, error) { return nil, errors.New("timeout") },
		},
	}
	if s := h.readStreamSnapshot(context.Background(), fakeStreamLookup{stream: other}, plan); s.HasStartSequenceTime || other.calls != 1 {
		t.Fatalf("a non-not-found error must not retry: calls=%d has=%v", other.calls, s.HasStartSequenceTime)
	}
}

// runServeStreamWithFrames drives serveStream with scripted frames and
// returns the body once the frames are consumed or the stream ends.
func runServeStreamWithFrames(t *testing.T, h *Handler, plan streamPlan, frames []formattedMessageEvent) (string, bool) {
	t.Helper()
	ch := make(chan formattedMessageEvent)
	feed := &streamFeed{frames: ch, errs: make(chan error), stop: func() {}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rr := newSafeRecorder()
	done := make(chan struct{})
	go func() {
		_ = h.serveStream(rr, httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx), plan, feed, nil)
		close(done)
	}()
	for _, f := range frames {
		select {
		case ch <- f:
		case <-done:
			return rr.Body(), true
		}
	}
	cancel()
	<-done
	return rr.Body(), false
}

func frameAt(seq uint64, ts time.Time) formattedMessageEvent {
	return formattedMessageEvent{
		Frame:             fmt.Sprintf("id: %d\nevent: message\ndata: {}\n\n", seq),
		HasStreamSequence: true,
		StreamSequence:    seq,
		HasMessageTime:    true,
		MessageTime:       ts,
	}
}

// TestServeStream_ReplayWindowFilterSparesLiveMessages covers #106: a live
// message whose timestamp predates the window (mirror catching up, clock
// skew) is delivered; only replayed history is filtered.
func TestServeStream_ReplayWindowFilterSparesLiveMessages(t *testing.T) {
	h := &Handler{HeartbeatInterval: 60, logger: zap.NewNop()}
	windowStart := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	old := windowStart.Add(-time.Hour)
	plan := streamPlan{Replay: replayPlan{HasLastID: true, HasSnapshot: true, CapSequence: 10, Mode: replayModeFallbackStartTime, StartTime: windowStart}}
	before := metricValue(t, metricsMessagesDropped.WithLabelValues(dropReasonReplayWindow))

	body, _ := runServeStreamWithFrames(t, h, plan, []formattedMessageEvent{frameAt(9, old), frameAt(10, windowStart), frameAt(11, old)})

	if strings.Contains(body, "id: 9\n") {
		t.Fatal("replayed message older than the window was delivered")
	}
	if !strings.Contains(body, "id: 10\n") || !strings.Contains(body, "id: 11\n") {
		t.Fatalf("in-window history or the live message is missing; body=%q", body)
	}
	if got := metricValue(t, metricsMessagesDropped.WithLabelValues(dropReasonReplayWindow)); got != before+1 {
		t.Fatalf("messages_dropped_total{replay_window} = %v, want %v", got, before+1)
	}
}

// TestServeStream_ReplayCapHoldsWithoutSnapshot covers #98: when StreamInfo
// failed at plan time the first message's pending count bounds the replay,
// instead of the cap switching off.
func TestServeStream_ReplayCapHoldsWithoutSnapshot(t *testing.T) {
	h := &Handler{HeartbeatInterval: 60, ReplayMaxMessages: 2, logger: zap.NewNop()}
	plan := streamPlan{Replay: replayPlan{HasLastID: true, Mode: replayModeStartSequence, StartSequence: 1}}
	first := frameAt(1, time.Now())
	first.NumPending = 9
	before := metricValue(t, metricsReplayCapReached)

	body, ended := runServeStreamWithFrames(t, h, plan, []formattedMessageEvent{first, frameAt(2, time.Now()), frameAt(3, time.Now())})

	if !ended {
		t.Fatal("stream kept running past replay_max_messages without a snapshot")
	}
	if strings.Contains(body, "id: 3\n") {
		t.Fatalf("message past the cap was delivered; body=%q", body)
	}
	if got := metricValue(t, metricsReplayCapReached); got != before+1 {
		t.Fatalf("replay_cap_reached_total = %v, want %v", got, before+1)
	}
}

func TestParseReplayCursor(t *testing.T) {
	cases := []struct {
		value   string
		want    uint64
		problem cursorProblem
	}{
		{value: "", problem: cursorAbsent},
		{value: "0", want: 0, problem: cursorValid},
		{value: "41", want: 41, problem: cursorValid},
		{value: strconv.FormatUint(maxReplayCursor-2, 10), want: maxReplayCursor - 2, problem: cursorValid},
		{value: strconv.FormatUint(maxReplayCursor-1, 10), problem: cursorInvalid},
		{value: strconv.FormatUint(maxReplayCursor, 10), problem: cursorInvalid},
		{value: "abc", problem: cursorInvalid},
		{value: "-1", problem: cursorInvalid},
		{value: strings.Repeat("9", 20), problem: cursorInvalid},
		{value: strings.Repeat("1", 21), problem: cursorTooLong},
	}
	for _, c := range cases {
		t.Run(c.value, func(t *testing.T) {
			got, problem, _ := parseReplayCursor(c.value)
			if got != c.want || problem != c.problem {
				t.Fatalf("parseReplayCursor(%q) = %d, %v; want %d, %v", c.value, got, problem, c.want, c.problem)
			}
		})
	}
}

// TestHandler_ParseStreamRequest_CursorPrecedence covers #102: EventSource
// resends its original URL, ?last-id= included, on every auto-reconnect,
// together with a fresher Last-Event-ID. The header must win.
func TestHandler_ParseStreamRequest_CursorPrecedence(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		header     string
		wantStatus int
		wantStart  uint64
		wantWarn   string
	}{
		{name: "header wins over the URL cursor", query: "2", header: "7", wantStart: 8},
		{name: "URL cursor alone", query: "5", wantStart: 6},
		{name: "header alone", header: "7", wantStart: 8},
		{name: "malformed header falls back to the URL cursor", query: "2", header: "abc", wantStart: 3, wantWarn: "ignoring unparseable Last-Event-ID header"},
		{name: "oversized header falls back to the URL cursor", query: "2", header: strings.Repeat("1", 21), wantStart: 3, wantWarn: "ignoring oversized Last-Event-ID header"},
		{name: "malformed URL cursor is rejected even with a valid header", query: "abc", header: "7", wantStatus: http.StatusBadRequest},
		{name: "no cursor", wantStart: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			core, obs := observer.New(zap.WarnLevel)
			h := &Handler{TopicPrefix: "events.", logger: zap.New(core)}
			target := "/events?topic=x"
			if c.query != "" {
				target += "&last-id=" + c.query
			}
			req := httptest.NewRequest(http.MethodGet, target, nil)
			if c.header != "" {
				req.Header.Set("Last-Event-ID", c.header)
			}
			plan, reqErr := h.parseStreamRequest(req)
			if c.wantStatus != 0 {
				if reqErr == nil || reqErr.status != c.wantStatus {
					t.Fatalf("err = %+v, want status %d", reqErr, c.wantStatus)
				}
				return
			}
			if reqErr != nil {
				t.Fatalf("unexpected error %+v", reqErr)
			}
			if plan.Replay.StartSequence != c.wantStart || plan.Replay.HasLastID != (c.wantStart != 0) {
				t.Fatalf("Replay = %+v, want start %d", plan.Replay, c.wantStart)
			}
			if c.wantWarn != "" && obs.FilterMessage(c.wantWarn).Len() != 1 {
				t.Fatalf("expected warning %q, got %v", c.wantWarn, obs.All())
			}
		})
	}
}

func TestRetryDelay_StaysWithinTheJitterRange(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 1000; i++ {
		d := retryDelay()
		if d < transientRetryBase/2 || d >= transientRetryBase*3/2 {
			t.Fatalf("retryDelay() = %v, want [%v, %v)", d, transientRetryBase/2, transientRetryBase*3/2)
		}
		seen[d] = true
	}
	if len(seen) < 100 {
		t.Fatalf("retryDelay() produced %d distinct values in 1000 calls; want jitter", len(seen))
	}
}

func TestAcceptsEventStream(t *testing.T) {
	cases := []struct {
		accept []string
		want   bool
	}{
		{accept: nil, want: false},
		{accept: []string{"text/event-stream"}, want: true},
		{accept: []string{"Text/Event-Stream"}, want: true},
		{accept: []string{"application/json, text/event-stream;q=0.9"}, want: true},
		{accept: []string{"application/json", "text/event-stream"}, want: true},
		{accept: []string{"*/*"}, want: false},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/events?topic=a", nil)
		for _, v := range c.accept {
			req.Header.Add("Accept", v)
		}
		if got := acceptsEventStream(req); got != c.want {
			t.Errorf("acceptsEventStream(%q) = %v, want %v", c.accept, got, c.want)
		}
	}
}

// TestHandler_ServeHTTP_JetStreamUnavailableIsRetryable covers #105 for the
// NATS-outage path: plain clients keep the 503 with a Retry-After, while an
// EventSource gets a retry stream so it does not stop reconnecting for good.
func TestHandler_ServeHTTP_JetStreamUnavailableIsRetryable(t *testing.T) {
	h := &Handler{StreamName: "EVENTS", TopicPrefix: "events.", logger: zap.NewNop()}

	rr := httptest.NewRecorder()
	if err := h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/events?topic=a", nil), nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "JetStream not available") {
		t.Fatalf("plain client got %d %q, want 503 JetStream not available", rr.Code, rr.Body.String())
	}
	assertRetryAfter(t, rr.Header())

	req := httptest.NewRequest(http.MethodGet, "/events?topic=a", nil)
	req.Header.Set("Accept", "text/event-stream")
	rr = httptest.NewRecorder()
	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	assertRetryStream(t, rr, "JetStream not available")
}

func TestHandler_RejectConsumerFailure(t *testing.T) {
	apiErr := &jetstream.APIError{ErrorCode: 10153, Code: 400, Description: "consumer inactive threshold exceeds system limit of 10s"}
	cases := []struct {
		name         string
		err          error
		wantBody     string
		wantReason   string
		wantRejected float64
		wantErrors   float64
		wantCode     int64
	}{
		{name: "consumer limit", err: jetstream.ErrMaximumConsumersLimit, wantBody: "Stream consumer limit reached", wantReason: "stream_consumer_limit", wantRejected: 1},
		{name: "handler closing", err: errHandlerClosing, wantBody: "JetStream not available", wantReason: "handler_shutdown"},
		{name: "server refusal", err: apiErr, wantBody: "Failed to subscribe to requested topics: alpha", wantReason: "subscription_failed", wantErrors: 1, wantCode: 10153},
		{name: "other failure", err: errors.New("timeout"), wantBody: "Failed to subscribe to requested topics: alpha", wantReason: "subscription_failed", wantErrors: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			core, obs := observer.New(zap.DebugLevel)
			h := &Handler{logger: zap.New(core)}
			rejectedBefore := metricValue(t, metricsConnectionsRejected.WithLabelValues("stream_consumer_limit"))
			errorsBefore := metricValue(t, metricsSubscriptionErrors)

			rr := httptest.NewRecorder()
			h.rejectConsumerFailure(rr, httptest.NewRequest(http.MethodGet, "/events?topic=alpha", nil), testFeedPlan, c.err)
			if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), c.wantBody) {
				t.Fatalf("response = %d %q, want 503 %q", rr.Code, rr.Body.String(), c.wantBody)
			}
			assertRetryAfter(t, rr.Header())
			if !hasLogField(obs, "disconnect_reason", c.wantReason) {
				t.Fatalf("missing disconnect_reason=%s: %v", c.wantReason, obs.All())
			}
			if got := metricValue(t, metricsConnectionsRejected.WithLabelValues("stream_consumer_limit")) - rejectedBefore; got != c.wantRejected {
				t.Fatalf("connections_rejected_total{stream_consumer_limit} moved by %v, want %v", got, c.wantRejected)
			}
			if got := metricValue(t, metricsSubscriptionErrors) - errorsBefore; got != c.wantErrors {
				t.Fatalf("subscription_errors_total moved by %v, want %v", got, c.wantErrors)
			}
			if c.wantCode != 0 && !hasIntLogField(obs, "jetstream_error_code", c.wantCode) {
				t.Fatalf("missing jetstream_error_code=%d: %v", c.wantCode, obs.All())
			}
		})
	}
}

func TestJetStreamErrorFields(t *testing.T) {
	wrapped := fmt.Errorf("create consumer: %w", &jetstream.APIError{ErrorCode: 10153})
	fields := jetStreamErrorFields(wrapped)
	if len(fields) != 1 || fields[0].Key != "jetstream_error_code" || fields[0].Integer != 10153 {
		t.Fatalf("fields = %+v, want jetstream_error_code=10153", fields)
	}
	if fields := jetStreamErrorFields(errors.New("plain")); fields != nil {
		t.Fatalf("fields for a non-API error = %+v, want none", fields)
	}
}

func TestConsumerInactiveThreshold(t *testing.T) {
	cases := []struct {
		limit, want time.Duration
	}{
		{limit: 0, want: defaultConsumerInactiveThreshold},
		{limit: 10 * time.Second, want: 10 * time.Second},
		{limit: defaultConsumerInactiveThreshold, want: defaultConsumerInactiveThreshold},
		{limit: defaultConsumerInactiveThreshold - time.Nanosecond, want: defaultConsumerInactiveThreshold - time.Nanosecond},
		{limit: time.Hour, want: defaultConsumerInactiveThreshold},
	}
	for _, c := range cases {
		if got := consumerInactiveThreshold(c.limit); got != c.want {
			t.Errorf("consumerInactiveThreshold(%v) = %v, want %v", c.limit, got, c.want)
		}
	}
	h := &Handler{StreamName: "EVENTS"}
	plan := testFeedPlan
	plan.ConsumerInactiveLimit = 10 * time.Second
	if got := h.orderedConsumerConfig(plan).InactiveThreshold; got != 10*time.Second {
		t.Fatalf("orderedConsumerConfig InactiveThreshold = %v, want the stream's 10s limit", got)
	}
}

func TestHandler_PlanSubscriptionCarriesTheConsumerInactiveLimit(t *testing.T) {
	h := &Handler{}
	snapshot := streamInfoSnapshot{HasSnapshot: true, LastSeq: 5, ConsumerInactiveLimit: 10 * time.Second}
	for _, replay := range []replayPlan{{Mode: replayModeDeliverNew}, {Mode: replayModeStartSequence, HasLastID: true, StartSequence: 3}} {
		plan := testFeedPlan
		plan.Replay = replay
		if got := h.planSubscription(plan, snapshot).ConsumerInactiveLimit; got != 10*time.Second {
			t.Fatalf("ConsumerInactiveLimit = %v, want 10s for %+v", got, replay)
		}
	}
}

func TestHandler_ReadStreamSnapshot_ReadsTheConsumerInactiveLimit(t *testing.T) {
	h := &Handler{StreamName: "EVENTS", logger: zap.NewNop()}
	info := &jetstream.StreamInfo{Config: jetstream.StreamConfig{
		Subjects:       []string{"events.>"},
		ConsumerLimits: jetstream.StreamConsumerLimits{InactiveThreshold: 10 * time.Second},
	}}
	snapshot := h.readStreamSnapshot(context.Background(), fakeStreamLookup{stream: fakeStream{info: info}}, testFeedPlan)
	if snapshot.ConsumerInactiveLimit != 10*time.Second {
		t.Fatalf("ConsumerInactiveLimit = %v, want 10s", snapshot.ConsumerInactiveLimit)
	}
}

func TestIsControlMessage(t *testing.T) {
	cases := []struct {
		name   string
		header nats.Header
		want   bool
	}{
		{name: "no headers", header: nil, want: false},
		{name: "application headers", header: nats.Header{"Trace-Id": {"1"}}, want: false},
		{name: "subject delete marker", header: nats.Header{"Nats-Marker-Reason": {"MaxAge"}, "Nats-Rollup": {"sub"}}, want: true},
		{name: "schedule definition", header: nats.Header{"Nats-Schedule": {"@every 1m"}, "Nats-Schedule-Target": {"events.a"}}, want: true},
		{name: "message produced by a schedule", header: nats.Header{"Nats-Scheduler": {"events.sched"}, "Nats-Schedule-Next": {"purge"}}, want: false},
		{name: "per-message TTL only", header: nats.Header{"Nats-TTL": {"1s"}}, want: false},
	}
	for _, c := range cases {
		if got := isControlMessage(c.header); got != c.want {
			t.Errorf("%s: isControlMessage = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHandler_FormatMessageEvent_DropsControlMessages(t *testing.T) {
	h := &Handler{TopicPrefix: "events.", MaxEventSize: -1}
	marker := streamMessage{Subject: "events.a", Header: nats.Header{"Nats-Marker-Reason": {"MaxAge"}}, HasMetadata: true, StreamSequence: 7}
	got := h.formatMessageEvent(marker, time.Now())
	if !got.Dropped || got.DropReason != dropReasonControlMessage || got.Frame != "" {
		t.Fatalf("marker formatted as %+v, want a control_message drop", got)
	}
	if got.StreamSequence != 7 {
		t.Fatalf("dropped marker lost its stream sequence: %+v", got)
	}
	fired := streamMessage{Subject: "events.a", Data: []byte(`{"fired":true}`), Header: nats.Header{"Nats-Scheduler": {"events.sched"}}, HasMetadata: true, StreamSequence: 8}
	if got := h.formatMessageEvent(fired, time.Now()); got.Dropped || !strings.Contains(got.Frame, `"fired":true`) {
		t.Fatalf("scheduled message formatted as %+v, want it delivered", got)
	}
}

func TestHandler_LogStreamLimits(t *testing.T) {
	cases := []struct {
		name           string
		maxConnections int
		cfg            jetstream.StreamConfig
		wantInfo       bool
		wantWarn       bool
	}{
		{name: "no limits", maxConnections: 100},
		{name: "consumer inactive limit below the default", cfg: jetstream.StreamConfig{ConsumerLimits: jetstream.StreamConsumerLimits{InactiveThreshold: 10 * time.Second}}, wantInfo: true},
		{name: "consumer inactive limit above the default", cfg: jetstream.StreamConfig{ConsumerLimits: jetstream.StreamConsumerLimits{InactiveThreshold: time.Hour}}},
		{name: "max_consumers below max_connections", maxConnections: 100, cfg: jetstream.StreamConfig{MaxConsumers: 50}, wantWarn: true},
		{name: "max_consumers equal to max_connections", maxConnections: 50, cfg: jetstream.StreamConfig{MaxConsumers: 50}},
		{name: "unlimited max_consumers", maxConnections: 100, cfg: jetstream.StreamConfig{MaxConsumers: -1}},
		{name: "unlimited max_connections", cfg: jetstream.StreamConfig{MaxConsumers: 50}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			core, obs := observer.New(zap.InfoLevel)
			h := &Handler{MaxConnections: c.maxConnections, logger: zap.New(core)}
			h.logStreamLimits(&jetstream.StreamInfo{Config: c.cfg})
			gotInfo := obs.FilterMessage("consumer inactive threshold lowered to the stream's consumer limit").Len() == 1
			gotWarn := obs.FilterLevelExact(zap.WarnLevel).Len() == 1
			if gotInfo != c.wantInfo || gotWarn != c.wantWarn || obs.Len() != btoi(c.wantInfo)+btoi(c.wantWarn) {
				t.Fatalf("logs = %v, want info=%v warn=%v", obs.All(), c.wantInfo, c.wantWarn)
			}
		})
	}
	(&Handler{logger: zap.NewNop()}).logStreamLimits(nil) // must not panic
}

func TestHandler_OpenConsumerStream_RefusedOnceCleanupStarted(t *testing.T) {
	h := &Handler{closing: true}
	if _, err := h.openConsumerStream(context.Background(), nil, testFeedPlan); !errors.Is(err, errHandlerClosing) {
		t.Fatalf("openConsumerStream after Cleanup = %v, want errHandlerClosing", err)
	}
	h.waitForStreams(time.Second) // nothing was tracked, so this returns at once
}

func liveFrames(n int, size int) []formattedMessageEvent {
	frames := make([]formattedMessageEvent, n)
	for i := range frames {
		frames[i] = formattedMessageEvent{
			Frame:             fmt.Sprintf("id: %d\nevent: message\ndata: %s\n\n", i+1, strings.Repeat("x", size)),
			HasStreamSequence: true,
			StreamSequence:    uint64(i + 1),
		}
	}
	return frames
}

// TestServeStream_BatchesQueuedFrames covers #121: frames already waiting
// when the writer takes one are written with it and flushed together, in
// batches of at most maxBatchFrames or maxBatchBytes.
func TestServeStream_BatchesQueuedFrames(t *testing.T) {
	h := &Handler{HeartbeatInterval: 60, logger: zap.NewNop()}
	for _, c := range []struct {
		name       string
		frames     []formattedMessageEvent
		wantFrames []int // frames per flush after the connected event
	}{
		{name: "everything queued fits one batch", frames: liveFrames(10, 10), wantFrames: []int{10}},
		{name: "frame count bound", frames: liveFrames(maxBatchFrames+8, 10), wantFrames: []int{maxBatchFrames, 8}},
		{name: "byte bound", frames: liveFrames(5, 40<<10), wantFrames: []int{2, 2, 1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			deliveredBefore := metricValue(t, metricsMessagesDelivered)
			w, _ := serveQueuedFrames(t, h, testFeedPlan, c.frames, len(c.wantFrames)+1)
			batches := w.batches()[1:] // the connected event is flushed on its own
			var got []int
			for _, b := range batches {
				got = append(got, strings.Count(b, "event: message"))
			}
			if !reflect.DeepEqual(got, c.wantFrames) {
				t.Fatalf("frames per flush = %v, want %v", got, c.wantFrames)
			}
			if delivered := metricValue(t, metricsMessagesDelivered) - deliveredBefore; delivered != float64(len(c.frames)) {
				t.Fatalf("messages_delivered_total moved by %v, want %d", delivered, len(c.frames))
			}
		})
	}
}

// TestServeStream_OneDeadlinePerBatch: the write deadline is set once per
// batch and cleared after its flush.
func TestServeStream_OneDeadlinePerBatch(t *testing.T) {
	h := &Handler{HeartbeatInterval: 60, WriteTimeout: 30, logger: zap.NewNop()}
	w, _ := serveQueuedFrames(t, h, testFeedPlan, liveFrames(10, 10), 2)
	w.mu.Lock()
	defer w.mu.Unlock()
	// connected: set, clear; one batch: set, clear.
	if len(w.deadlines) != 4 || w.deadlines[2].IsZero() || !w.deadlines[3].IsZero() {
		t.Fatalf("deadlines = %v, want set/clear for the connected event and once for the batch", w.deadlines)
	}
}

// TestServeStream_ReplayCapEndsTheBatch: when replay_max_messages is reached
// inside a batch, the frames up to the cap are flushed and the stream ends.
func TestServeStream_ReplayCapEndsTheBatch(t *testing.T) {
	h := &Handler{HeartbeatInterval: 60, ReplayMaxMessages: 5, logger: zap.NewNop()}
	plan := testFeedPlan
	plan.Replay = replayPlan{Mode: replayModeStartSequence, HasLastID: true, HasSnapshot: true, StartSequence: 1, CapSequence: 100}
	capBefore := metricValue(t, metricsReplayCapReached)
	w, ended := serveQueuedFrames(t, h, plan, liveFrames(10, 10), 2)
	if !ended {
		t.Fatal("stream kept running after replay_max_messages")
	}
	batches := w.batches()
	if len(batches) != 2 || strings.Count(batches[1], "event: message") != 5 || !strings.Contains(batches[1], "id: 5\n") {
		t.Fatalf("flushes = %q, want the connected event and exactly messages 1..5", batches)
	}
	if got := metricValue(t, metricsReplayCapReached); got != capBefore+1 {
		t.Fatalf("replay_cap_reached_total = %v, want %v", got, capBefore+1)
	}
}

// TestStreamReads_LatecomersShareTheNextRead covers #123: callers arriving
// while a read runs share one later read, and nobody gets a read that
// started before it arrived.
func TestStreamReads_LatecomersShareTheNextRead(t *testing.T) {
	var reads streamReads
	var mu sync.Mutex
	var starts []time.Time
	release := make(chan struct{})
	fetch := func() (jetstream.Stream, error) {
		mu.Lock()
		starts = append(starts, time.Now())
		first := len(starts) == 1
		mu.Unlock()
		if first {
			<-release
		}
		return fakeStream{}, nil
	}

	firstDone := make(chan struct{})
	go func() {
		_, _ = reads.read(context.Background(), fetch)
		close(firstDone)
	}()
	for {
		mu.Lock()
		started := len(starts) == 1
		mu.Unlock()
		if started {
			break
		}
		time.Sleep(time.Millisecond)
	}

	var wg sync.WaitGroup
	arrivals := make([]time.Time, 10)
	for i := range arrivals {
		wg.Add(1)
		arrivals[i] = time.Now()
		go func() {
			defer wg.Done()
			if _, err := reads.read(context.Background(), fetch); err != nil {
				t.Errorf("read: %v", err)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond) // let the latecomers queue up
	close(release)
	<-firstDone
	wg.Wait()

	if len(starts) != 2 {
		t.Fatalf("reads = %d, want 2: the first and one shared by the 10 latecomers", len(starts))
	}
	for i, arrived := range arrivals {
		if starts[1].Before(arrived) {
			t.Fatalf("latecomer %d arrived at %v but got a read started at %v", i, arrived, starts[1])
		}
	}

	if _, err := reads.read(context.Background(), fetch); err != nil || len(starts) != 3 {
		t.Fatalf("a caller after the storm got reads=%d err=%v, want its own fresh read", len(starts), err)
	}
}

func TestStreamReads_ErrorsAndCancellation(t *testing.T) {
	var reads streamReads
	boom := errors.New("boom")
	if _, err := reads.read(context.Background(), func() (jetstream.Stream, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("read error = %v, want %v", err, boom)
	}

	release := make(chan struct{})
	defer close(release)
	go func() {
		_, _ = reads.read(context.Background(), func() (jetstream.Stream, error) { <-release; return fakeStream{}, nil })
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	for {
		reads.mu.Lock()
		running := reads.running != nil
		reads.mu.Unlock()
		if running {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := reads.read(ctx, func() (jetstream.Stream, error) { return fakeStream{}, nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting caller whose request ended got %v, want its context error", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("a cancelled caller kept waiting for the read")
	}
}

// TestHandler_SetSSEHeaders pins the response headers every stream needs:
// without X-Accel-Buffering nginx buffers events, without Cache-Control
// caches may store the stream, and without Connection HTTP/1.1 proxies may
// close it between events.
func TestHandler_SetSSEHeaders(t *testing.T) {
	for _, hub := range []string{"", "https://example.com/events"} {
		rr := httptest.NewRecorder()
		(&Handler{HubURL: hub}).setSSEHeaders(rr)
		want := map[string]string{
			"Content-Type":      "text/event-stream",
			"Cache-Control":     "no-cache",
			"Connection":        "keep-alive",
			"X-Accel-Buffering": "no",
		}
		if hub != "" {
			want["Link"] = `<https://example.com/events>; rel="nuts"`
		}
		if len(rr.Header()) != len(want) {
			t.Fatalf("headers = %v, want exactly %v", rr.Header(), want)
		}
		for name, value := range want {
			if got := rr.Header().Get(name); got != value {
				t.Fatalf("%s = %q, want %q", name, got, value)
			}
		}
	}
}

func TestSubjectAllowedByStream(t *testing.T) {
	tests := []struct {
		name           string
		subject        string
		streamSubjects []string
		want           bool
	}{
		{
			name:           "full wildcard allows nested subject",
			subject:        "events.alpha.beta",
			streamSubjects: []string{"events.>"},
			want:           true,
		},
		{
			name:           "single token wildcard allows one token",
			subject:        "events.alpha",
			streamSubjects: []string{"events.*"},
			want:           true,
		},
		{
			name:           "single token wildcard rejects nested token",
			subject:        "events.alpha.beta",
			streamSubjects: []string{"events.*"},
			want:           false,
		},
		{
			name:           "exact subject match",
			subject:        "events.alpha",
			streamSubjects: []string{"events.alpha"},
			want:           true,
		},
		{
			name:           "unmatched subject",
			subject:        "orders.alpha",
			streamSubjects: []string{"events.>"},
			want:           false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := subjectAllowedByStream(tt.subject, tt.streamSubjects); got != tt.want {
				t.Fatalf("subjectAllowedByStream(%q, %v) = %v, want %v", tt.subject, tt.streamSubjects, got, tt.want)
			}
		})
	}
}

func TestAllowedMethodsHeader_FiltersToServedMethods(t *testing.T) {
	got := allowedMethodsHeader([]string{"POST", "get", "GET", "OPTIONS", "TRACE"})
	if got != "GET, OPTIONS" {
		t.Fatalf("allowedMethodsHeader() = %q, want GET, OPTIONS", got)
	}
}
