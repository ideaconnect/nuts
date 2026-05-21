package nuts

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
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
		plan := h.planSubscription(basePlan, streamInfoSnapshot{FirstSeq: 5})
		if plan.Replay.Mode != replayModeStartSequence {
			t.Fatalf("Replay mode = %s, want start_sequence", plan.Replay.Mode)
		}
		if plan.Replay.StartSequence != 10 {
			t.Fatalf("StartSequence = %d, want 10", plan.Replay.StartSequence)
		}
	})

	t.Run("below retention falls back to deliver all", func(t *testing.T) {
		h := &Handler{}
		plan := h.planSubscription(basePlan, streamInfoSnapshot{FirstSeq: 20})
		if plan.Replay.Mode != replayModeFallbackDeliverAll {
			t.Fatalf("Replay mode = %s, want fallback_deliver_all", plan.Replay.Mode)
		}
		if plan.Replay.FallbackReason != "sequence below retention" {
			t.Fatalf("FallbackReason = %q", plan.Replay.FallbackReason)
		}
	})

	t.Run("below retention uses replay window", func(t *testing.T) {
		h := &Handler{ReplayWindow: 30}
		plan := h.planSubscription(basePlan, streamInfoSnapshot{FirstSeq: 20})
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
			FirstSeq:             5,
			LastSeq:              20,
			StartSequenceTime:    time.Now(),
			HasStartSequenceTime: true,
		})
		if plan.Replay.Mode != replayModeStartSequence {
			t.Fatalf("Replay mode = %s, want start_sequence", plan.Replay.Mode)
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

func TestStreamPlan_RequestedMessageHandlerFiltersSubjects(t *testing.T) {
	plan := streamPlan{RequestedSubjects: map[string]struct{}{
		"events.allowed": {},
	}}
	var delivered []string
	handler := plan.requestedMessageHandler(func(msg *nats.Msg) {
		delivered = append(delivered, msg.Subject)
	})

	handler(&nats.Msg{Subject: "events.blocked"})
	handler(&nats.Msg{Subject: "events.allowed"})

	if !reflect.DeepEqual(delivered, []string{"events.allowed"}) {
		t.Fatalf("delivered subjects = %#v, want only events.allowed", delivered)
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

func TestHandler_SubscriptionOptionsFallbackStartTimeDefaultsWindow(t *testing.T) {
	h := &Handler{StreamName: "EVENTS", ReplayWindow: 60, logger: zap.NewNop()}
	plan := streamPlan{
		Topics:       []string{"alpha"},
		FullSubjects: []string{"events.alpha"},
		Replay: replayPlan{
			HasLastID:     true,
			Mode:          replayModeFallbackStartTime,
			StartSequence: 10,
		},
	}

	before := counterVal(t, metricsReplayFallbacks)
	opts := h.subscriptionOptions(plan)
	if len(opts) < 3 {
		t.Fatalf("subscriptionOptions returned %d opts, want fallback StartTime option included", len(opts))
	}
	if got := counterVal(t, metricsReplayFallbacks); got <= before {
		t.Fatalf("replay fallback metric did not increment: before=%v got=%v", before, got)
	}
}

func TestHandler_CountsTowardReplayCapWithoutSequenceMetadata(t *testing.T) {
	h := &Handler{ReplayMaxMessages: 2}
	plan := streamPlan{Replay: replayPlan{HasLastID: true, CapSequence: 20}}

	if !h.countsTowardReplayCap(plan, formattedMessageEvent{}) {
		t.Fatal("message without stream sequence should conservatively count toward replay cap")
	}
}

func TestHandler_RecordDroppedMessageLogsFormattedEvent(t *testing.T) {
	h := &Handler{MaxEventSize: 64, logger: zap.NewNop()}
	before := counterVal(t, metricsMessagesDropped)

	h.recordDroppedMessage(formattedMessageEvent{
		Subject:    "events.big",
		DropReason: dropReasonFormattedSSEMessage,
		DropSize:   128,
	})

	if got := counterVal(t, metricsMessagesDropped); got <= before {
		t.Fatalf("messages dropped metric did not increment: before=%v got=%v", before, got)
	}
}

func TestConnectedServerVersionNil(t *testing.T) {
	if got := connectedServerVersion(nil); got != "" {
		t.Fatalf("connectedServerVersion(nil) = %q, want empty string", got)
	}
}

func TestHandler_ServeReadinessCheckReportsMissingRuntime(t *testing.T) {
	h := &Handler{logger: zap.NewNop()}
	rr := httptest.NewRecorder()

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
	msg := &nats.Msg{Subject: "events.json", Data: []byte(`{"n":900719925474099312345}`)}

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
	msg := &nats.Msg{Subject: "events.raw", Data: []byte("plain text")}

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
	msg := &nats.Msg{
		Subject: "events.meta",
		Data:    []byte(`{"ok":true}`),
		Reply:   jetStreamAckReply("EVENTS", "consumer", 1, 42, 1, metaTime, 0),
		Sub:     &nats.Subscription{},
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
}

func TestHandler_FormatMessageEventRejectsOversizedEvents(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	t.Run("raw payload", func(t *testing.T) {
		h := &Handler{TopicPrefix: "events.", MaxEventSize: 4}
		formatted := h.formatMessageEvent(&nats.Msg{Subject: "events.big", Data: []byte("12345")}, now)
		if !formatted.Dropped || formatted.DropReason != dropReasonRawPayload || formatted.DropSize != 5 {
			t.Fatalf("formatted = %#v, want raw payload drop", formatted)
		}
	})

	t.Run("formatted SSE event", func(t *testing.T) {
		h := &Handler{TopicPrefix: "events.", MaxEventSize: 10}
		formatted := h.formatMessageEvent(&nats.Msg{Subject: "events.small", Data: []byte(`{}`)}, now)
		if !formatted.Dropped || formatted.DropReason != dropReasonFormattedSSEMessage {
			t.Fatalf("formatted = %#v, want formatted SSE drop", formatted)
		}
		if formatted.DropSize <= len(`{}`) {
			t.Fatalf("DropSize = %d, want formatted event size", formatted.DropSize)
		}
	})
}

func jetStreamAckReply(stream, consumer string, delivered, streamSeq, consumerSeq uint64, timestamp time.Time, pending uint64) string {
	return fmt.Sprintf("$JS.ACK.%s.%s.%d.%d.%d.%d.%d", stream, consumer, delivered, streamSeq, consumerSeq, timestamp.UnixNano(), pending)
}

// stubStreamMetadata is a minimal streamMetadataReader for testing the
// error paths of readStreamSnapshot without standing up a JetStream
// connection.
type stubStreamMetadata struct {
	info      *nats.StreamInfo
	infoErr   error
	msg       *nats.RawStreamMsg
	getMsgErr error
}

func (s stubStreamMetadata) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	return s.info, s.infoErr
}

func (s stubStreamMetadata) GetMsg(name string, seq uint64, opts ...nats.JSOpt) (*nats.RawStreamMsg, error) {
	return s.msg, s.getMsgErr
}

func TestHandler_ReadStreamSnapshot_StreamInfoErrorReturnsEmptySnapshot(t *testing.T) {
	h := &Handler{StreamName: "EVENTS", logger: zap.NewNop()}
	plan := streamPlan{Replay: replayPlan{HasLastID: true}}
	stub := stubStreamMetadata{infoErr: errors.New("stream info boom")}

	snapshot := h.readStreamSnapshot(stub, plan)

	if !reflect.DeepEqual(snapshot, streamInfoSnapshot{}) {
		t.Errorf("readStreamSnapshot with StreamInfo error: got %+v, want zero-value snapshot", snapshot)
	}
}

func TestHandler_ReadStreamSnapshot_GetMsgErrorKeepsSnapshotWithoutStartTime(t *testing.T) {
	h := &Handler{StreamName: "EVENTS", ReplayWindow: 60, logger: zap.NewNop()}
	plan := streamPlan{Replay: replayPlan{HasLastID: true, StartSequence: 5}}
	stub := stubStreamMetadata{
		info: &nats.StreamInfo{
			State:  nats.StreamState{FirstSeq: 1, LastSeq: 10},
			Config: nats.StreamConfig{Subjects: []string{"events.>"}},
		},
		getMsgErr: errors.New("get msg boom"),
	}

	snapshot := h.readStreamSnapshot(stub, plan)

	if snapshot.HasStartSequenceTime {
		t.Errorf("HasStartSequenceTime = true on GetMsg error, want false")
	}
	if snapshot.FirstSeq != 1 || snapshot.LastSeq != 10 {
		t.Errorf("Seq range: got FirstSeq=%d LastSeq=%d, want 1/10", snapshot.FirstSeq, snapshot.LastSeq)
	}
}

func TestHandler_FinalizeStreamedMessage(t *testing.T) {
	h := &Handler{logger: zap.NewNop()}

	cases := []struct {
		name  string
		plan  streamPlan
		event formattedMessageEvent
		want  bool
	}{
		{
			name:  "normal message delivers",
			plan:  streamPlan{},
			event: formattedMessageEvent{Subject: "events.x"},
			want:  true,
		},
		{
			name:  "dropped message skips",
			plan:  streamPlan{},
			event: formattedMessageEvent{Subject: "events.x", Dropped: true, DropReason: "queue_full", DropSize: 100},
			want:  false,
		},
		{
			name: "out-of-replay-window message skips",
			plan: streamPlan{Replay: replayPlan{Mode: replayModeFallbackStartTime, StartTime: time.Unix(1_700_000_000, 0)}},
			event: formattedMessageEvent{
				Subject:        "events.x",
				MessageTime:    time.Unix(1_699_999_000, 0),
				HasMessageTime: true,
			},
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := h.finalizeStreamedMessage(c.plan, c.event); got != c.want {
				t.Errorf("finalizeStreamedMessage = %v, want %v", got, c.want)
			}
		})
	}
}
