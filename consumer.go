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

// errHandlerClosing rejects new streams once Cleanup has started.
var errHandlerClosing = errors.New("handler is shutting down")

const (
	// streamWatchInterval is how often a handler reads its stream when no
	// request has, so that a stream recreated or rewound under open SSE
	// streams is noticed (#133). A rewind counts once a read at least half
	// an interval after the first read that found it still finds it, so the
	// next poll decides.
	streamWatchInterval = 10 * time.Second

	// Reasons a stream generation ends. Each is also the disconnect_reason,
	// and the consumer_invalidated reason, of the SSE streams it closes.
	streamRecreated = "stream_recreated"
	streamRewound   = "stream_rewound"
)

// streamGeneration is the configured stream as a run of reads saw it: one
// creation time, and sequences that only grow. A request is planned from a
// read and keeps that read's generation. When a later read finds the stream
// recreated or rewound, the generation ends and its SSE streams close (#133):
// the ordered consumer of each would recreate itself after the last sequence
// it delivered, a position the stream no longer has, and wait there, skipping
// every message the stream gets until it passes that position.
type streamGeneration struct {
	ended chan struct{}
	// reason is streamRecreated or streamRewound, set before ended closes.
	reason string
}

func newStreamGeneration() *streamGeneration {
	return &streamGeneration{ended: make(chan struct{})}
}

// done returns a channel that is closed when the generation ends. A nil
// generation never ends.
func (g *streamGeneration) done() <-chan struct{} {
	if g == nil {
		return nil
	}
	return g.ended
}

// streamWatch follows the stream through the reads of streamReads, which run
// one after another, so it sees the stream's states in the order the server
// reported them.
type streamWatch struct {
	// confirm is how long after the first read that found the stream
	// rewound a read must still find it so.
	confirm time.Duration

	mu      sync.Mutex
	gen     *streamGeneration
	created time.Time
	// lastSeq is the highest last sequence this generation's reads saw.
	lastSeq uint64
	// behindSince is when a read first found the stream below lastSeq; zero
	// unless the reads since then all did.
	behindSince time.Time
}

func newStreamWatch(confirm time.Duration) *streamWatch {
	return &streamWatch{confirm: confirm, gen: newStreamGeneration()}
}

// current returns the generation of the latest read, for a request whose own
// read failed. A nil watch returns nil.
func (w *streamWatch) current() *streamGeneration {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.gen
}

// observe records a read of the stream made at now. It returns the
// generation the read belongs to and, when the read ended the previous one,
// the reason.
//
// A different creation time means the stream was deleted and created again,
// or restored by nats-server 2.15, which gives a restored stream a new
// creation time. A last sequence below one already seen, with the same
// creation time, means it was restored by an earlier server, which keeps the
// creation time, or from a copy of its store directory. That only counts once
// the reads have shown it for confirm, and never from a replica that answered
// while its group had no leader: such a replica may not have applied the
// stream's latest messages.
func (w *streamWatch) observe(info *jetstream.StreamInfo, now time.Time) (*streamGeneration, string) {
	if w == nil {
		return nil, ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if info == nil || info.Created.IsZero() {
		return w.gen, "" // nothing to place the read by
	}
	if w.created.IsZero() {
		w.created, w.lastSeq = info.Created, info.State.LastSeq
		return w.gen, ""
	}
	if !info.Created.Equal(w.created) {
		return w.end(streamRecreated, info), streamRecreated
	}
	if info.State.LastSeq >= w.lastSeq {
		w.lastSeq, w.behindSince = info.State.LastSeq, time.Time{}
		return w.gen, ""
	}
	if info.Cluster != nil && info.Cluster.Leader == "" {
		return w.gen, "" // a replica without a leader; its state may lag
	}
	if w.behindSince.IsZero() {
		w.behindSince = now
	}
	if now.Sub(w.behindSince) < w.confirm {
		return w.gen, ""
	}
	return w.end(streamRewound, info), streamRewound
}

// end ends the current generation for reason and starts the next one from
// info.
func (w *streamWatch) end(reason string, info *jetstream.StreamInfo) *streamGeneration {
	w.gen.reason = reason
	close(w.gen.ended)
	w.gen = newStreamGeneration()
	w.created, w.lastSeq, w.behindSince = info.Created, info.State.LastSeq, time.Time{}
	return w.gen
}

// observeStream feeds a stream read to the watch, logs a generation it
// ends, and returns the read's generation.
func (h *Handler) observeStream(stream jetstream.Stream) *streamGeneration {
	info := stream.CachedInfo()
	gen, ended := h.watch.observe(info, time.Now())
	switch ended {
	case streamRecreated:
		h.log().Warn("JetStream stream was recreated; closing the SSE streams positioned on the old one, whose clients reconnect from the start of the new one",
			zap.String("stream", h.StreamName),
			zap.Time("stream_created", info.Created),
		)
	case streamRewound:
		h.log().Warn("JetStream stream went back to an earlier sequence; closing the SSE streams positioned on it before, whose clients reconnect with their last event ID",
			zap.String("stream", h.StreamName),
			zap.Uint64("stream_last_sequence", info.State.LastSeq),
		)
	}
	return gen
}

// watchStream reads the stream every interval until shutdown, so that a
// recreated or rewound stream is noticed even when no request reads it: the
// SSE streams it strands receive nothing, and nothing else would notice.
func (h *Handler) watchStream(interval time.Duration, shutdown <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-shutdown:
			return
		case <-ticker.C:
		}
		if js := h.currentStreamRuntime().js; js != nil {
			_, _, _ = h.streamReads.read(context.Background(), func() (jetstream.Stream, error) {
				ctx, cancel := context.WithTimeout(context.Background(), defaultMetadataReadTimeout)
				defer cancel()
				return js.Stream(ctx, h.StreamName)
			})
		}
	}
}

// consumerStream is the JetStream side of a single SSE request: its ordered
// consumer and the feed that pulls from it.
type consumerStream struct {
	js       jetstream.JetStream
	stream   string
	consumer jetstream.Consumer
	feed     *streamFeed
	// release tells Cleanup that this stream's consumer is gone.
	release   func()
	log       *zap.Logger
	plan      streamPlan
	closeOnce sync.Once
}

// openFeed starts the frames for one SSE request: from the request's own
// consumer, or through the shared subscriptions when they are enabled.
func (h *Handler) openFeed(ctx context.Context, js jetstream.JetStream, plan streamPlan) (*streamFeed, error) {
	if h.SharedSubscriptions && h.shared != nil {
		return h.startHybridFeed(ctx, js, plan)
	}
	cs, err := h.openConsumerStream(ctx, js, plan)
	if err != nil {
		return nil, err
	}
	return &streamFeed{frames: cs.feed.frames, errs: cs.feed.errs, stop: cs.close}, nil
}

// openConsumerStream creates the request's ordered consumer and starts pulling.
// Consumer creation happens synchronously with a single attempt, so server-side
// refusals (stream missing, consumer limits) surface here as an error the
// caller turns into an HTTP response.
func (h *Handler) openConsumerStream(ctx context.Context, js jetstream.JetStream, plan streamPlan) (*consumerStream, error) {
	if !h.trackStream() {
		return nil, errHandlerClosing
	}
	createCtx, cancel := context.WithTimeout(ctx, defaultConsumerCreateTimeout)
	defer cancel()
	consumer, err := js.OrderedConsumer(createCtx, h.StreamName, h.orderedConsumerConfig(plan))
	if err != nil {
		h.streams.Done()
		return nil, err
	}
	cs := &consumerStream{js: js, stream: h.StreamName, consumer: consumer, release: h.streams.Done, log: h.log(), plan: plan}
	iterator, err := consumer.Messages(h.pullOptions()...)
	if err != nil {
		cs.deleteConsumer()
		return nil, err
	}
	h.logSubscription(plan)
	cs.feed = h.startStreamFeed(iterator, plan)
	return cs, nil
}

// trackStream registers a stream whose consumer Cleanup waits for. It fails
// once Cleanup has started, so no stream is added while Cleanup waits.
func (h *Handler) trackStream() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closing {
		return false
	}
	h.streams.Add(1)
	return true
}

// close stops pulling and removes the consumer from the server. Only the
// first call does anything.
func (cs *consumerStream) close() {
	cs.closeOnce.Do(func() {
		cs.feed.stop()
		cs.deleteConsumer()
	})
}

// deleteConsumer removes the current server-side consumer instead of leaving it
// to InactiveThreshold, so disconnect churn does not pile up consumers against
// the stream's consumer limit. It runs in the background so the request, and
// its max_connections slot, is released without waiting on the JetStream API.
func (cs *consumerStream) deleteConsumer() {
	info := cs.consumer.CachedInfo()
	if info == nil {
		cs.release()
		return
	}
	name := info.Name
	if conn := cs.js.Conn(); conn != nil && !conn.IsConnected() {
		// The delete could only time out; the server removes the consumer
		// after its inactive threshold.
		cs.release()
		return
	}
	go func() {
		defer cs.release()
		ctx, cancel := context.WithTimeout(context.Background(), defaultMetadataReadTimeout)
		defer cancel()
		// A consumer that is already gone (reaped, or deleted by hand) needs
		// no mention; any other failure leaves it until InactiveThreshold.
		if err := cs.js.DeleteConsumer(ctx, cs.stream, name); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
			cs.log.Warn("failed to delete JetStream consumer; the server removes it after its inactive threshold",
				appendStreamLogFields(cs.plan, zap.String("consumer", name), zap.Error(err))...,
			)
		}
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
		InactiveThreshold: consumerInactiveThreshold(plan.ConsumerInactiveLimit),
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

// consumerInactiveThreshold is the InactiveThreshold a new consumer asks for:
// the default, lowered to the stream's consumer limit when one is set (#113).
// The server refuses a consumer that asks for more than the limit (error
// 10153), which would fail every request on such a stream.
func consumerInactiveThreshold(limit time.Duration) time.Duration {
	if limit > 0 && limit < defaultConsumerInactiveThreshold {
		return limit
	}
	return defaultConsumerInactiveThreshold
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

// feedHandoffFrames is how many formatted frames a feed hands ahead to its
// writer, so a burst can be written in batches (see maxBatchFrames).
const feedHandoffFrames = 16

// startStreamFeed pulls from the iterator on its own goroutine, formats each
// message, drops the ones that cannot be sent, and hands the rest to the
// writer. The hand-off holds feedHandoffFrames frames and blocks when full,
// which is what stops further pulls while the writer is busy.
func (h *Handler) startStreamFeed(it jetstream.MessagesContext, plan streamPlan) *streamFeed {
	frames := make(chan formattedMessageEvent, feedHandoffFrames)
	errs := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		consumerName := ""
		for {
			msg, err := it.Next()
			if err != nil {
				if !errors.Is(err, jetstream.ErrMsgIteratorClosed) {
					errs <- err
				}
				return
			}
			formatted := h.formatMessageEvent(newStreamMessage(msg), time.Now())
			consumerName = h.noteConsumerChange(plan, consumerName, formatted)
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

// noteConsumerChange counts and logs a consumer that the ordered consumer
// recreated: its messages arrive under a new consumer name. It returns the
// name to compare the next message with.
func (h *Handler) noteConsumerChange(plan streamPlan, previous string, formatted formattedMessageEvent) string {
	if formatted.ConsumerName == "" {
		return previous
	}
	if previous != "" && formatted.ConsumerName != previous {
		metricsConsumerInvalidated.WithLabelValues("recreated").Inc()
		h.log().Info("JetStream consumer recreated; delivery resumed after the last delivered message",
			appendStreamLogFields(plan,
				zap.String("previous_consumer", previous),
				zap.String("consumer", formatted.ConsumerName),
				zap.Uint64("resumed_at_sequence", formatted.StreamSequence),
			)...,
		)
	}
	return formatted.ConsumerName
}

// streamMessage is the part of a JetStream message the formatter needs, read
// once so formatting does not depend on the jetstream.Msg interface.
type streamMessage struct {
	Subject        string
	Data           []byte
	Header         nats.Header
	StreamSequence uint64
	ConsumerName   string
	NumPending     uint64
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
	sm.NumPending = meta.NumPending
	sm.Timestamp = meta.Timestamp
	return sm
}
