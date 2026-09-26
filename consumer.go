// consumer.go — the JetStream side of one SSE stream.
//
// Every SSE request gets its own ordered pull consumer from the nats.go
// jetstream package. Pulling gives end-to-end backpressure: the feed goroutine
// only takes the next message from JetStream once the SSE writer has taken the
// previous one, so a burst or a long replay waits in the stream instead of
// overflowing a per-connection queue. The ordered consumer also recreates
// itself from the last delivered sequence after a delivery gap, a NATS
// reconnect or missed heartbeats, so a dropped link or a reaped consumer no
// longer leaves a hole the client cannot detect.
package nuts

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nuid"
	"go.uber.org/zap"
)

// streamLookup is the part of jetstream.JetStream that request planning needs.
// Tests stub it to control stream metadata without a live server.
type streamLookup interface {
	Stream(ctx context.Context, name string) (jetstream.Stream, error)
}

// consumerStream is the JetStream side of a single SSE request: its ordered
// consumer and the feed that pulls from it.
type consumerStream struct {
	js       jetstream.JetStream
	stream   string
	consumer jetstream.Consumer
	feed     *streamFeed
}

// openConsumerStream creates the request's ordered consumer and starts pulling.
// Consumer creation happens synchronously with a single attempt, so server-side
// refusals (stream missing, consumer limits) surface here as an error the
// caller turns into an HTTP response.
func (h *Handler) openConsumerStream(ctx context.Context, js jetstream.JetStream, plan streamPlan) (*consumerStream, error) {
	createCtx, cancel := context.WithTimeout(ctx, defaultConsumerCreateTimeout)
	defer cancel()
	consumer, err := js.OrderedConsumer(createCtx, h.StreamName, h.orderedConsumerConfig(plan))
	if err != nil {
		metricsSubscriptionErrors.Inc()
		return nil, err
	}
	iterator, err := consumer.Messages(h.pullOptions()...)
	if err != nil {
		metricsSubscriptionErrors.Inc()
		cs := &consumerStream{js: js, stream: h.StreamName, consumer: consumer}
		cs.deleteConsumer()
		return nil, err
	}
	h.logSubscription(plan)
	return &consumerStream{
		js:       js,
		stream:   h.StreamName,
		consumer: consumer,
		feed:     h.startStreamFeed(iterator, plan),
	}, nil
}

// close stops pulling and removes the consumer from the server.
func (cs *consumerStream) close() {
	cs.feed.stop()
	cs.deleteConsumer()
}

// deleteConsumer removes the current server-side consumer instead of leaving it
// to InactiveThreshold, so disconnect churn does not pile up consumers against
// the stream's consumer limit. It runs in the background so the request, and
// its max_connections slot, is released without waiting on the JetStream API.
func (cs *consumerStream) deleteConsumer() {
	info := cs.consumer.CachedInfo()
	if info == nil {
		return
	}
	name := info.Name
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), defaultMetadataReadTimeout)
		defer cancel()
		_ = cs.js.DeleteConsumer(ctx, cs.stream, name)
	}()
}

// logSubscription records which start position the new consumer uses, and
// counts fallback replays once the consumer actually exists.
func (h *Handler) logSubscription(plan streamPlan) {
	switch plan.Replay.Mode {
	case replayModeFallbackStartTime:
		metricsReplayFallbacks.Inc()
		h.log().Warn("replay fallback: using time-bounded window",
			appendStreamLogFields(plan,
				zap.Uint64("requested_sequence", plan.Replay.StartSequence),
				zap.String("reason", plan.Replay.FallbackReason),
				zap.Int("replay_window_seconds", h.ReplayWindow),
				zap.Time("replay_window_start", h.replayStartTime(plan)),
			)...,
		)
	case replayModeFallbackDeliverAll:
		metricsReplayFallbacks.Inc()
		h.log().Warn("replay fallback: delivering all retained messages",
			appendStreamLogFields(plan,
				zap.Uint64("requested_sequence", plan.Replay.StartSequence),
				zap.String("reason", plan.Replay.FallbackReason),
			)...,
		)
	default:
		h.log().Debug("subscribed to topics", streamLogFields(plan)...)
	}
}

// replayStartTime is the start instant of a time-bounded fallback, defaulting
// to now-ReplayWindow when the plan did not record one.
func (h *Handler) replayStartTime(plan streamPlan) time.Time {
	if plan.Replay.StartTime.IsZero() {
		return h.replayWindowStart()
	}
	return plan.Replay.StartTime
}

// orderedConsumerConfig translates the plan into the ordered consumer's start
// position and filters.
//
// Requests without a cursor start at an explicit sequence (the snapshot's
// LastSeq+1) rather than DeliverNew. The ordered consumer re-applies its
// original deliver policy when it resets before delivering anything, so with
// DeliverNew a reset during an outage would silently skip whatever was
// published in between. DeliverNew remains only as the fallback when no
// snapshot could be read.
func (h *Handler) orderedConsumerConfig(plan streamPlan) jetstream.OrderedConsumerConfig {
	cfg := jetstream.OrderedConsumerConfig{
		FilterSubjects:    plan.FullSubjects,
		InactiveThreshold: defaultConsumerInactiveThreshold,
		MaxResetAttempts:  defaultConsumerMaxResetAttempts,
		NamePrefix:        consumerNamePrefix + nuid.Next(),
	}
	switch plan.Replay.Mode {
	case replayModeStartSequence:
		cfg.DeliverPolicy = jetstream.DeliverByStartSequencePolicy
		cfg.OptStartSeq = plan.Replay.StartSequence
	case replayModeFallbackStartTime:
		start := h.replayStartTime(plan)
		cfg.DeliverPolicy = jetstream.DeliverByStartTimePolicy
		cfg.OptStartTime = &start
	case replayModeFallbackDeliverAll:
		cfg.DeliverPolicy = jetstream.DeliverAllPolicy
	default:
		if plan.Replay.StartSequence > 0 {
			cfg.DeliverPolicy = jetstream.DeliverByStartSequencePolicy
			cfg.OptStartSeq = plan.Replay.StartSequence
		} else {
			cfg.DeliverPolicy = jetstream.DeliverNewPolicy
		}
	}
	return cfg
}

// pullOptions sizes the consumer's prefetch from client_buffer_size and maps
// nats_idle_heartbeat onto the pull heartbeat. Prefetch is bounded by message
// count only: a byte limit below the largest message would stall the consumer
// on that message forever.
func (h *Handler) pullOptions() []jetstream.PullMessagesOpt {
	bufSize := h.ClientBufferSize
	if bufSize <= 0 {
		bufSize = defaultClientBufferSize
	}
	opts := []jetstream.PullMessagesOpt{jetstream.PullMaxMessages(bufSize)}
	if h.NatsIdleHeartbeat > 0 {
		opts = append(opts, jetstream.PullHeartbeat(time.Duration(h.NatsIdleHeartbeat)*time.Second))
	}
	return opts
}

// streamFeed carries formatted frames from the pulling goroutine to the SSE
// writer. errs receives at most one error: the consumer could not be
// recreated and the stream must end.
type streamFeed struct {
	frames <-chan formattedMessageEvent
	errs   <-chan error
	stop   func()
}

// startStreamFeed pulls from the iterator on its own goroutine, formats each
// message, drops the ones that cannot be sent, and hands the rest to the
// writer. The hand-off blocks while the writer is busy, which is what stops
// further pulls.
func (h *Handler) startStreamFeed(it jetstream.MessagesContext, plan streamPlan) *streamFeed {
	frames := make(chan formattedMessageEvent, 1)
	errs := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		for {
			msg, err := it.Next()
			if err != nil {
				if !errors.Is(err, jetstream.ErrMsgIteratorClosed) {
					errs <- err
				}
				return
			}
			formatted := h.formatMessageEvent(newStreamMessage(msg), time.Now())
			if formatted.MetadataErr != nil {
				h.log().Warn("failed to read JetStream metadata",
					appendStreamLogFields(plan,
						zap.String("message_subject", formatted.Subject),
						zap.Error(formatted.MetadataErr),
					)...,
				)
			}
			if formatted.Dropped {
				h.recordDroppedMessage(formatted)
				continue
			}
			select {
			case frames <- formatted:
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	return &streamFeed{
		frames: frames,
		errs:   errs,
		stop: func() {
			once.Do(func() {
				close(done)
				it.Stop()
			})
		},
	}
}

// streamMessage is the part of a JetStream message the formatter needs, read
// once so formatting does not depend on the jetstream.Msg interface.
type streamMessage struct {
	Subject        string
	Data           []byte
	Header         nats.Header
	StreamSequence uint64
	ConsumerName   string
	Timestamp      time.Time
	HasMetadata    bool
	MetadataErr    error
}

func newStreamMessage(msg jetstream.Msg) streamMessage {
	sm := streamMessage{Subject: msg.Subject(), Data: msg.Data(), Header: msg.Headers()}
	meta, err := msg.Metadata()
	if err != nil {
		sm.MetadataErr = err
		return sm
	}
	sm.HasMetadata = true
	sm.StreamSequence = meta.Sequence.Stream
	sm.ConsumerName = meta.Consumer
	sm.Timestamp = meta.Timestamp
	return sm
}
