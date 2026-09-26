package nuts

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
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
		if err := writeSSEChunkWithTimeout(rr, http.NewResponseController(&noDeadlineRecorder{ResponseRecorder: rr}), "data: x\n\n", 0); err != nil {
			t.Fatalf("err = %v", err)
		}
		if rr.Body.String() != "data: x\n\n" {
			t.Fatalf("body = %q", rr.Body.String())
		}
	})
	t.Run("falls back when deadline unsupported", func(t *testing.T) {
		nr := &noDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		if err := writeSSEChunkWithTimeout(nr, http.NewResponseController(nr), "data: x\n\n", time.Second); err != nil {
			t.Fatalf("err = %v", err)
		}
		if nr.Body.String() != "data: x\n\n" {
			t.Fatalf("body = %q", nr.Body.String())
		}
	})
	t.Run("propagates non-not-supported deadline error", func(t *testing.T) {
		boom := errors.New("boom")
		er := &errOnSetDeadlineRecorder{ResponseRecorder: httptest.NewRecorder(), err: boom}
		err := writeSSEChunkWithTimeout(er, http.NewResponseController(er), "data: x\n\n", time.Second)
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

// flushErrorOnlyWriter has the shape of caddyhttp's responseRecorder (used when
// access logs or HTTP metrics are on): it wraps a ResponseWriter and exposes
// FlushError and Unwrap, but not http.Flusher.
type flushErrorOnlyWriter struct {
	http.ResponseWriter
	flushErr error
	flushes  int
}

func (f *flushErrorOnlyWriter) FlushError() error {
	f.flushes++
	return f.flushErr
}

func (f *flushErrorOnlyWriter) Unwrap() http.ResponseWriter { return f.ResponseWriter }

type unwrapOnlyWriter struct{ http.ResponseWriter }

func (u *unwrapOnlyWriter) Unwrap() http.ResponseWriter { return u.ResponseWriter }

type nilUnwrapWriter struct{ http.ResponseWriter }

func (n *nilUnwrapWriter) Unwrap() http.ResponseWriter { return nil }

func TestSupportsFlush(t *testing.T) {
	cases := []struct {
		name string
		w    http.ResponseWriter
		want bool
	}{
		{name: "http.Flusher", w: httptest.NewRecorder(), want: true},
		{name: "FlushError only (Caddy recorder shape)", w: &flushErrorOnlyWriter{ResponseWriter: newPlainRecorder()}, want: true},
		{name: "Unwrap to a Flusher", w: &unwrapOnlyWriter{ResponseWriter: httptest.NewRecorder()}, want: true},
		{name: "Unwrap chain to a Flusher", w: &unwrapOnlyWriter{ResponseWriter: &unwrapOnlyWriter{ResponseWriter: httptest.NewRecorder()}}, want: true},
		{name: "no flush support", w: newPlainRecorder(), want: false},
		{name: "Unwrap to a non-flusher", w: &unwrapOnlyWriter{ResponseWriter: newPlainRecorder()}, want: false},
		{name: "Unwrap returns nil", w: &nilUnwrapWriter{ResponseWriter: newPlainRecorder()}, want: false},
		{name: "nil writer", w: nil, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := supportsFlush(c.w); got != c.want {
				t.Fatalf("supportsFlush() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestWriteSSEChunk_FlushesThroughFlushErrorAndReturnsItsError(t *testing.T) {
	t.Run("success flushes exactly once", func(t *testing.T) {
		rec := newPlainRecorder()
		fw := &flushErrorOnlyWriter{ResponseWriter: rec}
		if err := writeSSEChunk(fw, http.NewResponseController(fw), "data: x\n\n"); err != nil {
			t.Fatalf("writeSSEChunk: %v", err)
		}
		if fw.flushes != 1 {
			t.Fatalf("flushes = %d, want 1", fw.flushes)
		}
		if got := rec.body.String(); got != "data: x\n\n" {
			t.Fatalf("body = %q", got)
		}
	})
	t.Run("flush error is returned", func(t *testing.T) {
		boom := errors.New("flush failed")
		fw := &flushErrorOnlyWriter{ResponseWriter: newPlainRecorder(), flushErr: boom}
		if err := writeSSEChunk(fw, http.NewResponseController(fw), "data: x\n\n"); !errors.Is(err, boom) {
			t.Fatalf("writeSSEChunk err = %v, want %v", err, boom)
		}
	})
	t.Run("flush error is returned under a write deadline", func(t *testing.T) {
		boom := errors.New("flush failed")
		fw := &flushErrorOnlyWriter{ResponseWriter: newPlainRecorder(), flushErr: boom}
		if err := writeSSEChunkWithTimeout(fw, http.NewResponseController(fw), "data: x\n\n", time.Second); !errors.Is(err, boom) {
			t.Fatalf("writeSSEChunkWithTimeout err = %v, want %v", err, boom)
		}
	})
}

func TestToJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    interface{}
		expected string
	}{
		{
			name:     "simple map",
			input:    map[string]string{"key": "value"},
			expected: `{"key":"value"}`,
		},
		{
			name:     "slice of strings",
			input:    []string{"a", "b", "c"},
			expected: `["a","b","c"]`,
		},
		{
			name:     "nested structure",
			input:    map[string]interface{}{"nested": map[string]int{"count": 42}},
			expected: `{"nested":{"count":42}}`,
		},
		{
			name:     "marshal error returns empty object",
			input:    math.Inf(1),
			expected: `{}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := toJSON(tt.input)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestIsValidTopic(t *testing.T) {
	tests := []struct {
		name  string
		topic string
		want  bool
	}{
		{"simple", "events.test", true},
		{"with dashes", "my-topic", true},
		{"with dots", "a.b.c", true},
		{"empty", "", false},
		{"double dot", "a..b", false},
		{"control char", "a\x00b", false},
		{"newline", "a\nb", false},
		{"wildcard star", "events.*", false},
		{"wildcard gt", "events.>", false},
		{"dollar prefix", "$SYS.test", false},
		{"dollar JS", "$JS.API", false},
		{"max length", strings.Repeat("a", 256), true},
		{"over max length", strings.Repeat("a", 257), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isValidTopic(tt.topic); got != tt.want {
				t.Errorf("isValidTopic(%q) = %v, want %v", tt.topic, got, tt.want)
			}
		})
	}
}

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"no userinfo", "nats://localhost:4222", "nats://localhost:4222"},
		{"with token", "nats://secret@localhost:4222", "nats://REDACTED@localhost:4222"},
		{"with user:pass", "nats://user:pass@localhost:4222", "nats://REDACTED@localhost:4222"},
		{"invalid url", "://broken", "://broken"},
		{"credentials in a later server of a list", "nats://a:4222,nats://user:pass@b:4222", "nats://a:4222,nats://REDACTED@b:4222"},
		{"credentials in every server of a list", "nats://u:p@a:4222, tls://t@b:4222", "nats://REDACTED@a:4222,tls://REDACTED@b:4222"},
		{"server without a scheme", "user:pass@localhost:4222", "REDACTED@localhost:4222"},
		{"server without a scheme or credentials", "localhost:4222", "localhost:4222"},
		{"unparseable with credentials", "nats://user:pa ss@localhost:4222", "nats://REDACTED@localhost:4222"},
		{"unparseable without credentials", "nats://local host:4222", "nats://local host:4222"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactURL(tt.raw); got != tt.want {
				t.Errorf("redactURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

type deadlineFlushRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (r *deadlineFlushRecorder) Flush() {}

func (r *deadlineFlushRecorder) SetWriteDeadline(deadline time.Time) error {
	r.deadlines = append(r.deadlines, deadline)
	return nil
}

// TestWriteSSEChunkWithTimeout_DisabledSetsNoDeadline pins write_timeout -1:
// a negative timeout writes without touching the connection deadline.
func TestWriteSSEChunkWithTimeout_DisabledSetsNoDeadline(t *testing.T) {
	rr := &deadlineFlushRecorder{ResponseRecorder: httptest.NewRecorder()}
	timeout := time.Duration(writeTimeoutDisabledSentinel) * time.Second
	if err := writeSSEChunkWithTimeout(rr, http.NewResponseController(rr), "event: ping\n\n", timeout); err != nil {
		t.Fatalf("writeSSEChunkWithTimeout: %v", err)
	}
	if len(rr.deadlines) != 0 {
		t.Fatalf("deadline calls = %d, want 0 with write_timeout disabled", len(rr.deadlines))
	}
	if got := rr.Body.String(); got != "event: ping\n\n" {
		t.Fatalf("body = %q", got)
	}
}

func TestWriteSSEChunkWithTimeout_SetsAndClearsDeadline(t *testing.T) {
	rr := &deadlineFlushRecorder{ResponseRecorder: httptest.NewRecorder()}
	if err := writeSSEChunkWithTimeout(rr, http.NewResponseController(rr), "event: ping\n\n", time.Second); err != nil {
		t.Fatalf("writeSSEChunkWithTimeout: %v", err)
	}
	if got := rr.Body.String(); got != "event: ping\n\n" {
		t.Fatalf("body = %q", got)
	}
	if len(rr.deadlines) != 2 {
		t.Fatalf("deadline calls = %d, want 2", len(rr.deadlines))
	}
	if rr.deadlines[0].IsZero() {
		t.Fatal("first deadline should set a non-zero write deadline")
	}
	if !rr.deadlines[1].IsZero() {
		t.Fatalf("second deadline = %v, want zero reset", rr.deadlines[1])
	}
}
