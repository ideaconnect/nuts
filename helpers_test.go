package nuts

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func TestIsValidCookieName_Cases(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{name: "", want: false},
		{name: "session", want: true},
		{name: "Session_Id-2", want: true},
		{name: "with space", want: false},
		{name: "with;semicolon", want: false},
		{name: "non=equal", want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isValidCookieName(c.name); got != c.want {
				t.Fatalf("isValidCookieName(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

type noDeadlineRecorder struct {
	*httptest.ResponseRecorder
}

func (n *noDeadlineRecorder) Flush() {}

type errOnSetDeadlineRecorder struct {
	*httptest.ResponseRecorder
	err error
}

func (e *errOnSetDeadlineRecorder) Flush()                             {}
func (e *errOnSetDeadlineRecorder) SetWriteDeadline(_ time.Time) error { return e.err }

func TestWriteSSEChunkWithTimeout_FallbackAndErrors(t *testing.T) {
	t.Run("zero timeout writes through", func(t *testing.T) {
		rr := httptest.NewRecorder()
		if err := writeSSEChunkWithTimeout(rr, &noDeadlineRecorder{ResponseRecorder: rr}, "data: x\n\n", 0); err != nil {
			t.Fatalf("err = %v", err)
		}
		if rr.Body.String() != "data: x\n\n" {
			t.Fatalf("body = %q", rr.Body.String())
		}
	})
	t.Run("falls back when deadline unsupported", func(t *testing.T) {
		nr := &noDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		if err := writeSSEChunkWithTimeout(nr, nr, "data: x\n\n", time.Second); err != nil {
			t.Fatalf("err = %v", err)
		}
		if nr.Body.String() != "data: x\n\n" {
			t.Fatalf("body = %q", nr.Body.String())
		}
	})
	t.Run("propagates non-not-supported deadline error", func(t *testing.T) {
		boom := errors.New("boom")
		er := &errOnSetDeadlineRecorder{ResponseRecorder: httptest.NewRecorder(), err: boom}
		err := writeSSEChunkWithTimeout(er, er, "data: x\n\n", time.Second)
		if err == nil || !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
	})
}

func TestClassifyNATSAsyncError(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want string
	}{
		// Production never calls classifyNATSAsyncError with a nil error
		// (provision.go's ErrorHandler short-circuits on nil), but the
		// classifier still tolerates it by falling through to the "other"
		// bucket via errors.Is(nil, target) returning false.
		{"nil falls through to other", nil, "other"},
		{"slow consumer", nats.ErrSlowConsumer, "slow_consumer"},
		{"slow consumer wrapped", fmt.Errorf("subscribe: %w", nats.ErrSlowConsumer), "slow_consumer"},
		{"timeout", nats.ErrTimeout, "timeout"},
		{"timeout wrapped", fmt.Errorf("op: %w", nats.ErrTimeout), "timeout"},
		{"connection closed", nats.ErrConnectionClosed, "connection_state"},
		{"connection closed wrapped", fmt.Errorf("op: %w", nats.ErrConnectionClosed), "connection_state"},
		{"connection draining", nats.ErrConnectionDraining, "connection_state"},
		{"connection draining wrapped", fmt.Errorf("op: %w", nats.ErrConnectionDraining), "connection_state"},
		// consumer_invalidated covers the three nats.go error paths
		// that all mean the JetStream push consumer is unusable. The
		// hardening test TestHandler_BatchA_HeartbeatMissTriggers
		// ClassifierAndLog uncovered that nats.go's IdleHeartbeat-miss
		// detector raises nats.ErrConsumerNotActive (sentinel), NOT
		// *nats.ErrConsumerSequenceMismatch (typed struct). Without
		// the ErrConsumerNotActive arm, Phase 1's metric label would
		// never tick in production for the primary failure mode M9
		// targets. Pin every error type explicitly here so a future
		// nats.go change that re-routes one of them is caught at unit
		// time rather than in production.
		{"ErrConsumerNotActive (primary heartbeat-miss path)", nats.ErrConsumerNotActive, "consumer_invalidated"},
		{"ErrConsumerNotActive wrapped", fmt.Errorf("activity check: %w", nats.ErrConsumerNotActive), "consumer_invalidated"},
		{"ErrConsumerDeleted", nats.ErrConsumerDeleted, "consumer_invalidated"},
		{"ErrConsumerDeleted wrapped", fmt.Errorf("delivery: %w", nats.ErrConsumerDeleted), "consumer_invalidated"},
		// *nats.ErrConsumerSequenceMismatch is the secondary path:
		// heartbeats arrive but the sequence drifted. Bare + wrapped
		// + zero-value sub-cases pin the errors.As contract: future
		// refactors that swap As for Is would silently fall through
		// to "other" because Is would compare against a zero-value
		// sentinel struct and fail.
		{"ErrConsumerSequenceMismatch populated", &nats.ErrConsumerSequenceMismatch{StreamResumeSequence: 42, ConsumerSequence: 1, LastConsumerSequence: 2}, "consumer_invalidated"},
		{"ErrConsumerSequenceMismatch wrapped", fmt.Errorf("delivered: %w", &nats.ErrConsumerSequenceMismatch{StreamResumeSequence: 7}), "consumer_invalidated"},
		{"ErrConsumerSequenceMismatch zero value", &nats.ErrConsumerSequenceMismatch{}, "consumer_invalidated"},
		{"unknown error", errors.New("some unrelated"), "other"},
		{"unknown wrapped", fmt.Errorf("op: %w", errors.New("some unrelated")), "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyNATSAsyncError(tc.in); got != tc.want {
				t.Errorf("classifyNATSAsyncError(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestClassifyNATSAsyncError_ConsumerSequenceMismatch_RequiresErrorsAs pins
// the contract that *nats.ErrConsumerSequenceMismatch is matched via
// errors.As, NOT errors.Is. nats.ErrConsumerSequenceMismatch is a struct
// type carrying recovery sequence numbers — an errors.Is check against
// the zero value would compare by equality and fail for any real error
// instance with non-zero fields. If a future refactor accidentally swaps
// As→Is the classifier would silently route this M9 Batch B termination
// trigger into the generic "other" bucket and SSE clients would stay
// attached to zombie consumers. Guard explicitly.
func TestClassifyNATSAsyncError_ConsumerSequenceMismatch_RequiresErrorsAs(t *testing.T) {
	// Use a populated typed error so an accidental errors.Is(err, &nats.
	// ErrConsumerSequenceMismatch{}) would compare against the zero value
	// and fail — the very regression we're guarding against.
	err := &nats.ErrConsumerSequenceMismatch{StreamResumeSequence: 99, ConsumerSequence: 5, LastConsumerSequence: 6}
	if got := classifyNATSAsyncError(err); got != "consumer_invalidated" {
		t.Fatalf("populated sequence-mismatch error classified as %q, want consumer_invalidated — likely errors.Is regression", got)
	}

	// And confirm errors.Is itself would NOT match — this guards future
	// code from "fixing" the As-vs-Is choice by trying Is and finding it
	// works in some narrow case.
	if errors.Is(err, &nats.ErrConsumerSequenceMismatch{}) {
		t.Fatalf("errors.Is unexpectedly matched a populated sequence-mismatch against zero-value sentinel; the classifier must rely on errors.As")
	}
}

// TestClassifyNATSAsyncError_ConsumerNotActive_PrimaryHeartbeatMissPath
// pins the contract that nats.ErrConsumerNotActive — the actual error
// raised by nats.go's activityCheck when the IdleHeartbeat timeout
// elapses without a heartbeat arriving — buckets as consumer_invalidated.
// The Phase 1 implementation originally only covered *ErrConsumerSequence
// Mismatch; TestHandler_BatchA_HeartbeatMissTriggersClassifierAndLog
// uncovered this hole because consumer deletion via the JetStream admin
// API surfaces as ErrConsumerNotActive, not the sequence-mismatch type.
// Without this regression guard a future refactor could silently drop
// the primary detection path and the metric would never tick in
// production.
func TestClassifyNATSAsyncError_ConsumerNotActive_PrimaryHeartbeatMissPath(t *testing.T) {
	if got := classifyNATSAsyncError(nats.ErrConsumerNotActive); got != "consumer_invalidated" {
		t.Fatalf("nats.ErrConsumerNotActive classified as %q, want consumer_invalidated — the primary IdleHeartbeat-miss signal would never reach the metric label", got)
	}
	wrapped := fmt.Errorf("activity check fired: %w", nats.ErrConsumerNotActive)
	if got := classifyNATSAsyncError(wrapped); got != "consumer_invalidated" {
		t.Fatalf("wrapped ErrConsumerNotActive classified as %q, want consumer_invalidated", got)
	}
}

// TestMetrics_ConsumerInvalidated_RegisteredWithExpectedLabels locks the
// public contract for the metric Batch B (#M9-5) will populate. Batch A
// declares the CounterVec so /metrics exposes a zero-value series before
// the routing code lands — dashboards and alerts can reference the
// series name without breaking on first deploy.
//
// The two reason values (heartbeat_missed, slow_consumer) match the
// two errors classifyNATSAsyncError surfaces for routed termination
// (consumer_invalidated and slow_consumer respectively); Batch B
// maps one to the other inside the serveStream termination arm.
func TestMetrics_ConsumerInvalidated_RegisteredWithExpectedLabels(t *testing.T) {
	for _, reason := range []string{"heartbeat_missed", "slow_consumer"} {
		t.Run(reason, func(t *testing.T) {
			// WithLabelValues panics on label-count mismatch or unregistered
			// vector — recover surfaces both as a test failure with the
			// pre-panic diagnostic intact.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("metricsConsumerInvalidated.WithLabelValues(%q) panicked: %v", reason, r)
				}
			}()
			c := metricsConsumerInvalidated.WithLabelValues(reason)
			if c == nil {
				t.Fatalf("metricsConsumerInvalidated.WithLabelValues(%q) returned nil", reason)
			}
			// Zero-counted series must be visible in /metrics; assert we
			// can read the count without registration error. Batch A
			// MUST NOT increment this metric; the assertion below pins
			// that contract — if Phase 3 (or any pre-Batch-B change)
			// starts ticking it, this test will catch it.
			if v := counterValue(metricsConsumerInvalidated, reason); v != 0 {
				t.Fatalf("metricsConsumerInvalidated{reason=%q} = %v before Batch B; expected 0 (Batch A is declaration-only)", reason, v)
			}
		})
	}
}
