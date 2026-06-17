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
		{"nil", nil, "unknown"},
		{"slow consumer", nats.ErrSlowConsumer, "slow_consumer"},
		{"slow consumer wrapped", fmt.Errorf("subscribe: %w", nats.ErrSlowConsumer), "slow_consumer"},
		{"timeout", nats.ErrTimeout, "timeout"},
		{"connection closed", nats.ErrConnectionClosed, "connection_state"},
		{"connection draining", nats.ErrConnectionDraining, "connection_state"},
		{"unknown error", errors.New("some unrelated"), "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyNATSAsyncError(tc.in); got != tc.want {
				t.Errorf("classifyNATSAsyncError(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
