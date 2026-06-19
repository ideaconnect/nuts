package nuts

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
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
	// Snapshot was observed; message has no stream-sequence metadata.
	// We conservatively count (the cap bounds total delivery rather
	// than only historical replay — see countsTowardReplayCap comment).
	plan := streamPlan{Replay: replayPlan{HasLastID: true, HasSnapshot: true, CapSequence: 20}}

	if !h.countsTowardReplayCap(plan, formattedMessageEvent{}) {
		t.Fatal("message without stream sequence should conservatively count toward replay cap when a snapshot was observed")
	}
}

// TestHandler_CountsTowardReplayCap_NoSnapshotSkipsCap covers pass-7's
// HasSnapshot fix: when readStreamSnapshot's StreamInfo call failed
// (HasSnapshot=false) the replay cap MUST NOT enforce, because the
// CapSequence we have is 0-by-default and the conservative "count it"
// branch would silently retarget the cap at live traffic — closing a
// long-lived SSE session with disconnect_reason=replay_cap_reached
// after N live messages of any age.
func TestHandler_CountsTowardReplayCap_NoSnapshotSkipsCap(t *testing.T) {
	h := &Handler{ReplayMaxMessages: 2}
	plan := streamPlan{Replay: replayPlan{HasLastID: true, HasSnapshot: false, CapSequence: 0}}

	// Even though the cap is configured and the request asked for replay,
	// no snapshot means we cannot tell live from historical — skip the
	// cap entirely.
	if h.countsTowardReplayCap(plan, formattedMessageEvent{HasStreamSequence: true, StreamSequence: 100}) {
		t.Fatal("live message must not count toward replay cap when HasSnapshot=false")
	}
	if h.countsTowardReplayCap(plan, formattedMessageEvent{}) {
		t.Fatal("metadata-less message must not count toward replay cap when HasSnapshot=false")
	}
}

func TestHandler_RecordDroppedMessageLogsFormattedEvent(t *testing.T) {
	h := &Handler{MaxEventSize: 64, logger: zap.NewNop()}
	before := counterValue(metricsMessagesDropped, dropReasonFormattedSSEMessage)

	h.recordDroppedMessage(formattedMessageEvent{
		Subject:    "events.big",
		DropReason: dropReasonFormattedSSEMessage,
		DropSize:   128,
	})

	if got := counterValue(metricsMessagesDropped, dropReasonFormattedSSEMessage); got <= before {
		t.Fatalf("messages dropped metric (reason=formatted_sse_message) did not increment: before=%v got=%v", before, got)
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

	// Pass-6 contract: a single 503 response must bump exactly one cause
	// label (the first matched). The body still reports every degradation
	// it observed (nats=disconnected AND stream=unavailable), but the
	// metric must keep its 1:1 with probe-failure count so
	// sum(rate(nuts_readiness_failures_total[...])) tracks the real
	// /readyz error rate.
	beforeNats := counterValue(metricsReadinessFailures, "nats_disconnected")
	beforeJS := counterValue(metricsReadinessFailures, "jetstream_missing")
	beforeStream := counterValue(metricsReadinessFailures, "stream_info_error")

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
	if got := counterValue(metricsReadinessFailures, "nats_disconnected"); got != beforeNats+1 {
		t.Errorf("nuts_readiness_failures_total{cause=nats_disconnected} = %v, want %v (exactly one increment)", got, beforeNats+1)
	}
	// Subsequent causes must NOT increment for the same 503 — the one-shot
	// guard prevents the documented 1:1 contract from being violated.
	if got := counterValue(metricsReadinessFailures, "jetstream_missing"); got != beforeJS {
		t.Errorf("nuts_readiness_failures_total{cause=jetstream_missing} = %v, want %v (must not double-count when nats_disconnected already fired)", got, beforeJS)
	}
	if got := counterValue(metricsReadinessFailures, "stream_info_error"); got != beforeStream {
		t.Errorf("nuts_readiness_failures_total{cause=stream_info_error} = %v, want %v", got, beforeStream)
	}
}

func TestHandler_ExecuteSubscriptionPlan_PlanningRejectionBumpsMetric(t *testing.T) {
	h := &Handler{logger: zap.NewNop()}
	plan := streamPlan{
		Topics:       []string{"allowed", "blocked"},
		FullSubjects: []string{"events.allowed", "events.blocked"},
		FailedTopics: []string{"blocked"},
	}

	before := counterVal(t, metricsSubscriptionErrors)
	got := h.executeSubscriptionPlan(nil, nil, plan, nil, nil)
	if len(got.FailedTopics) != 1 || got.FailedTopics[0] != "blocked" {
		t.Fatalf("FailedTopics = %#v, want [blocked]", got.FailedTopics)
	}
	if after := counterVal(t, metricsSubscriptionErrors); after <= before {
		t.Errorf("nuts_subscription_errors_total did not increment on planning-time rejection: %v -> %v", before, after)
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
		{"orders", "orders.>", false},               // FEWER tokens than filter requires
		{"orders.created", "orders.>", true},        // exactly one trailing token
		{"orders.created.gold", "orders.>", true},   // multiple trailing tokens
		{"events.x.y.z", "events.>", true},          // deep nesting
		{"events", "events.>", false},               // root token only
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
