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
		// consumer_sequence_mismatch is the M9 Batch B trigger: nats.go
		// detected a missed IdleHeartbeat (push consumer reaped / lost)
		// or a delivery-sequence gap. Pin both the bare typed-error path
		// and the fmt.Errorf-wrapped path because we use errors.As, not
		// errors.Is — verify both shapes resolve to the same label.
		{"consumer sequence mismatch", &nats.ErrConsumerSequenceMismatch{StreamResumeSequence: 42, ConsumerSequence: 1, LastConsumerSequence: 2}, "consumer_sequence_mismatch"},
		{"consumer sequence mismatch wrapped", fmt.Errorf("delivered: %w", &nats.ErrConsumerSequenceMismatch{StreamResumeSequence: 7}), "consumer_sequence_mismatch"},
		// Empty-struct case: errors.As checks the type, not the field
		// state — a zero-value mismatch error MUST still classify as
		// consumer_sequence_mismatch. Future code that filters on
		// non-zero StreamResumeSequence would silently regress without
		// this case.
		{"consumer sequence mismatch zero value", &nats.ErrConsumerSequenceMismatch{}, "consumer_sequence_mismatch"},
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
// the contract that the consumer_sequence_mismatch path is checked via
// errors.As, NOT errors.Is. nats.ErrConsumerSequenceMismatch is a struct
// type carrying recovery sequence numbers — an errors.Is check against
// the zero value would compare by equality and fail for any real error
// instance with non-zero fields. If a future refactor accidentally swaps
// As→Is the classifier would silently route the M9 Batch B termination
// trigger into the generic "other" bucket and SSE clients would stay
// attached to zombie consumers. Guard explicitly.
func TestClassifyNATSAsyncError_ConsumerSequenceMismatch_RequiresErrorsAs(t *testing.T) {
	// Use a populated typed error so an accidental errors.Is(err, &nats.
	// ErrConsumerSequenceMismatch{}) would compare against the zero value
	// and fail — the very regression we're guarding against.
	err := &nats.ErrConsumerSequenceMismatch{StreamResumeSequence: 99, ConsumerSequence: 5, LastConsumerSequence: 6}
	if got := classifyNATSAsyncError(err); got != "consumer_sequence_mismatch" {
		t.Fatalf("populated sequence-mismatch error classified as %q, want consumer_sequence_mismatch — likely errors.Is regression", got)
	}

	// And confirm errors.Is itself would NOT match — this guards future
	// code from "fixing" the As-vs-Is choice by trying Is and finding it
	// works in some narrow case.
	if errors.Is(err, &nats.ErrConsumerSequenceMismatch{}) {
		t.Fatalf("errors.Is unexpectedly matched a populated sequence-mismatch against zero-value sentinel; the classifier must rely on errors.As")
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
// (consumer_sequence_mismatch and slow_consumer respectively); Batch B
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
