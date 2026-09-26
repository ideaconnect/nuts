// shared.go — shared live subscriptions (shared_subscriptions).
//
// By default every SSE connection owns a JetStream consumer, so nats-server,
// the NATS link and the formatter all repeat their work for each connection:
// a burst to a topic with N subscribers crosses NUTS' single NATS connection
// N times. With shared_subscriptions on, connections that are caught up with
// the live stream share one consumer per topic set instead. Each message is
// pulled and formatted once, and the frame is handed to every connection's
// queue.
//
// A connection with history to replay reads from its own consumer until it has
// caught up, then joins the shared subscription. A connection that falls
// behind the shared subscription, or loses it, goes back to its own consumer,
// starting right after the last message it was given. Hand-offs go by stream
// sequence, and the shared subscription keeps its most recent frames so a
// joining connection does not miss the ones delivered while it caught up.
package nuts

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
)

const (
	// sharedRingFrames and sharedRingBytes bound the recent frames a shared
	// subscription keeps for connections that join it.
	sharedRingFrames = 1024
	sharedRingBytes  = 4 << 20

	// sharedJoinBackoff spaces out join attempts that failed, so a
	// connection on its own consumer does not try to create a shared
	// subscription with every message while, for example, the stream's
	// consumer limit is reached.
	sharedJoinBackoff = 5 * time.Second

	// Reasons a connection moves off a shared subscription, used as the
	// transition label of nuts_shared_transitions_total.
	sharedTransitionJoined       = "joined"
	sharedTransitionFellBehind   = "fell_behind"
	sharedTransitionSharedFailed = "shared_failed"
)

// sharedRegistry holds a handler's shared subscriptions by topic set.
type sharedRegistry struct {
	mu   sync.Mutex
	subs map[string]*sharedSub
}

func newSharedRegistry() *sharedRegistry {
	return &sharedRegistry{subs: map[string]*sharedSub{}}
}

// sharedKey identifies a topic set regardless of the order of its topics.
// Topics cannot contain commas.
func sharedKey(plan streamPlan) string {
	subjects := slices.Clone(plan.FullSubjects)
	slices.Sort(subjects)
	return strings.Join(subjects, ",")
}

// sharedSub is one shared subscription: a consumer, the frames it delivered
// most recently, and the connections it feeds.
type sharedSub struct {
	h        *Handler
	registry *sharedRegistry
	key      string
	plan     streamPlan
	stream   *consumerStream
	done     chan struct{}

	mu        sync.Mutex
	ring      []formattedMessageEvent
	ringBytes int
	// floor is the highest stream sequence that may be missing from ring:
	// every message of the topic set after it is in ring or still to come.
	floor   uint64
	clients map[*sharedClient]struct{}
	closed  bool
}

// sharedClient is one connection's place on a shared subscription.
type sharedClient struct {
	frames chan formattedMessageEvent
	// lastSeq is the stream sequence of the last frame queued for the
	// connection. Guarded by the sharedSub's mutex until frames is closed.
	lastSeq uint64
	// reason says why the connection was moved off the subscription. It is
	// written before frames is closed.
	reason string
}

// joinShared attaches a connection whose last message was lastSeq to the
// topic set's shared subscription, starting one if there is none. It returns
// nil when the connection cannot join without a gap, for example because the
// shared subscription has moved on further than it remembers.
func (h *Handler) joinShared(ctx context.Context, js jetstream.JetStream, plan streamPlan, lastSeq uint64, capacity int) (*sharedSub, *sharedClient) {
	key := sharedKey(plan)
	for attempt := 0; attempt < 2; attempt++ {
		sub, err := h.shared.getOrStart(ctx, h, js, plan, key, lastSeq)
		if err != nil {
			h.log().Debug("could not start a shared subscription", appendStreamLogFields(plan, zap.Error(err))...)
			return nil, nil
		}
		if client := sub.attach(lastSeq, capacity); client != nil {
			return sub, client
		}
		if !sub.isClosed() {
			return nil, nil
		}
		// The subscription closed while joining; start a new one.
	}
	return nil, nil
}

// getOrStart returns the topic set's shared subscription, starting one that
// begins right after lastSeq when there is none. ctx bounds only the
// creation: the subscription outlives the connection that started it.
func (r *sharedRegistry) getOrStart(ctx context.Context, h *Handler, js jetstream.JetStream, plan streamPlan, key string, lastSeq uint64) (*sharedSub, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sub := r.subs[key]; sub != nil && !sub.isClosed() {
		return sub, nil
	}
	startPlan := plan
	startPlan.Replay = replayPlan{Mode: replayModeDeliverNew, StartSequence: lastSeq + 1}
	stream, err := h.openConsumerStream(ctx, js, startPlan)
	if err != nil {
		return nil, err
	}
	sub := &sharedSub{
		h:        h,
		registry: r,
		key:      key,
		plan:     startPlan,
		stream:   stream,
		done:     make(chan struct{}),
		floor:    lastSeq,
		clients:  map[*sharedClient]struct{}{},
	}
	r.subs[key] = sub
	metricsSharedSubscriptions.Inc()
	go sub.run()
	return sub, nil
}

// run fans out the shared consumer's frames until the subscription closes.
func (s *sharedSub) run() {
	for {
		select {
		case frame := <-s.stream.feed.frames:
			s.publish(frame)
		case err := <-s.stream.feed.errs:
			s.h.log().Warn("shared subscription lost its JetStream consumer; its connections move to their own consumers",
				appendStreamLogFields(s.plan, zap.Error(err))...,
			)
			s.close(sharedTransitionSharedFailed)
			return
		case <-s.done:
			return
		}
	}
}

// publish remembers a frame and queues it for every connection. A connection
// whose queue is full has fallen behind: it is moved off the subscription
// rather than stalling everyone else. When that leaves nobody, the
// subscription closes; the connections start a new one once caught up.
func (s *sharedSub) publish(frame formattedMessageEvent) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.remember(frame)
	detached := false
	for client := range s.clients {
		if frame.HasStreamSequence && frame.StreamSequence <= client.lastSeq {
			continue // the connection got it from its own consumer
		}
		select {
		case client.frames <- frame:
			if frame.HasStreamSequence {
				client.lastSeq = frame.StreamSequence
			}
		default:
			s.detachLocked(client, sharedTransitionFellBehind)
			detached = true
		}
	}
	empty := detached && len(s.clients) == 0
	s.mu.Unlock()
	if empty {
		s.close("")
	}
}

// remember adds a frame to the ring, evicting the oldest frames beyond its
// bounds and raising the floor past them.
func (s *sharedSub) remember(frame formattedMessageEvent) {
	if !frame.HasStreamSequence {
		return // cannot be placed in sequence order
	}
	s.ring = append(s.ring, frame)
	s.ringBytes += len(frame.Frame)
	for len(s.ring) > sharedRingFrames || (s.ringBytes > sharedRingBytes && len(s.ring) > 1) {
		evicted := s.ring[0]
		s.ring = s.ring[1:]
		s.ringBytes -= len(evicted.Frame)
		s.floor = evicted.StreamSequence
	}
}

// attach adds a connection whose last message was lastSeq. The frames the
// subscription remembers after lastSeq are queued first. It returns nil when
// a message after lastSeq may be missing, or when the remembered frames do
// not fit the connection's queue.
func (s *sharedSub) attach(lastSeq uint64, capacity int) *sharedClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || lastSeq < s.floor {
		return nil
	}
	client := &sharedClient{frames: make(chan formattedMessageEvent, capacity), lastSeq: lastSeq}
	for _, frame := range s.ring {
		if frame.StreamSequence <= lastSeq {
			continue
		}
		select {
		case client.frames <- frame:
			client.lastSeq = frame.StreamSequence
		default:
			return nil
		}
	}
	s.clients[client] = struct{}{}
	metricsSharedTransitions.WithLabelValues(sharedTransitionJoined).Inc()
	return client
}

// leave removes a connection that is done. The last connection to leave
// closes the subscription.
func (s *sharedSub) leave(client *sharedClient) {
	s.mu.Lock()
	if _, ok := s.clients[client]; !ok {
		s.mu.Unlock()
		return
	}
	delete(s.clients, client)
	close(client.frames)
	empty := len(s.clients) == 0
	s.mu.Unlock()
	if empty {
		s.close("")
	}
}

// detachLocked moves a connection off the subscription; it continues on its
// own consumer after client.lastSeq.
func (s *sharedSub) detachLocked(client *sharedClient, reason string) {
	delete(s.clients, client)
	client.reason = reason
	close(client.frames)
	metricsSharedTransitions.WithLabelValues(reason).Inc()
}

// close ends the subscription: remaining connections are moved to their own
// consumers with the given reason, and the shared consumer is deleted.
func (s *sharedSub) close(reason string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for client := range s.clients {
		s.detachLocked(client, reason)
	}
	s.mu.Unlock()

	s.registry.mu.Lock()
	if s.registry.subs[s.key] == s {
		delete(s.registry.subs, s.key)
	}
	s.registry.mu.Unlock()
	metricsSharedSubscriptions.Dec()
	close(s.done)
	s.stream.close()
}

func (s *sharedSub) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// hybridFeed delivers one connection's frames with shared_subscriptions on:
// from its own consumer while it has history to catch up on, and from the
// shared subscription once it is live.
type hybridFeed struct {
	h        *Handler
	js       jetstream.JetStream
	plan     streamPlan
	capacity int

	frames chan formattedMessageEvent
	errs   chan error
	done   chan struct{}
	// ctx is cancelled when the stream ends, so a consumer being created for
	// it is abandoned instead of holding up Cleanup.
	ctx    context.Context
	cancel context.CancelFunc

	// Owned by the run goroutine.
	own      *consumerStream
	sub      *sharedSub
	client   *sharedClient
	lastSeq  uint64
	nextJoin time.Time
}

// startHybridFeed starts a connection's feed with shared subscriptions on. A
// connection without a cursor joins the shared subscription at once when the
// stream's end is known; any other starts on its own consumer.
func (h *Handler) startHybridFeed(ctx context.Context, js jetstream.JetStream, plan streamPlan) (*streamFeed, error) {
	hf := &hybridFeed{
		h:        h,
		js:       js,
		plan:     plan,
		capacity: h.clientBufferSize(),
		frames:   make(chan formattedMessageEvent, 1),
		errs:     make(chan error, 1),
		done:     make(chan struct{}),
	}
	hf.ctx, hf.cancel = context.WithCancel(ctx)
	if !plan.Replay.HasLastID && plan.Replay.StartSequence > 0 {
		hf.lastSeq = plan.Replay.StartSequence - 1
		hf.tryJoin()
	}
	if hf.client == nil {
		own, err := h.openConsumerStream(hf.ctx, js, plan)
		if err != nil {
			hf.cancel()
			return nil, err
		}
		hf.own = own
	}
	go hf.run()
	var once sync.Once
	return &streamFeed{
		frames: hf.frames,
		errs:   hf.errs,
		stop: func() {
			once.Do(func() {
				hf.cancel()
				close(hf.done)
			})
		},
	}, nil
}

func (hf *hybridFeed) run() {
	defer hf.release()
	for {
		if hf.client != nil {
			select {
			case frame, ok := <-hf.client.frames:
				if !ok {
					if !hf.fallBack() {
						return
					}
					continue
				}
				// The subscription only queues frames after the connection's
				// last sequence, so no de-duplication is needed here.
				if !hf.forward(frame) {
					return
				}
			case <-hf.done:
				return
			}
			continue
		}
		select {
		case frame := <-hf.own.feed.frames:
			if !hf.forward(frame) {
				return
			}
			if frame.HasStreamSequence && frame.NumPending == 0 && !time.Now().Before(hf.nextJoin) {
				hf.switchToShared()
			}
		case err := <-hf.own.feed.errs:
			hf.errs <- err
			return
		case <-hf.done:
			return
		}
	}
}

// forward hands a frame to the writer, waiting while it is busy.
func (hf *hybridFeed) forward(frame formattedMessageEvent) bool {
	select {
	case hf.frames <- frame:
		if frame.HasStreamSequence {
			hf.lastSeq = frame.StreamSequence
		}
		return true
	case <-hf.done:
		return false
	}
}

// tryJoin attaches to the shared subscription after hf.lastSeq, or delays
// the next attempt.
func (hf *hybridFeed) tryJoin() {
	sub, client := hf.h.joinShared(hf.ctx, hf.js, hf.plan, hf.lastSeq, hf.capacity)
	if client == nil {
		hf.nextJoin = time.Now().Add(sharedJoinBackoff)
		return
	}
	hf.sub, hf.client = sub, client
}

// switchToShared moves a caught-up connection from its own consumer to the
// shared subscription. Frames its own consumer had already pulled beyond
// lastSeq are dropped: the shared subscription delivers them.
func (hf *hybridFeed) switchToShared() {
	hf.tryJoin()
	if hf.client == nil {
		return
	}
	hf.own.close()
	hf.own = nil
}

// fallBack moves the connection back to its own consumer after the shared
// subscription dropped it, starting right after the last frame it was given.
func (hf *hybridFeed) fallBack() bool {
	hf.h.log().Debug("connection left its shared subscription; continuing on its own consumer",
		appendStreamLogFields(hf.plan,
			zap.String("reason", hf.client.reason),
			zap.Uint64("resume_after_sequence", hf.lastSeq),
		)...,
	)
	hf.client = nil
	hf.sub = nil
	hf.nextJoin = time.Now().Add(sharedJoinBackoff)
	plan := hf.plan
	plan.Replay = replayPlan{Mode: replayModeStartSequence, HasLastID: true, StartSequence: hf.lastSeq + 1}
	own, err := hf.h.openConsumerStream(hf.ctx, hf.js, plan)
	if err != nil {
		hf.errs <- err
		return false
	}
	hf.own = own
	return true
}

// release gives up whatever the connection holds when its stream ends.
func (hf *hybridFeed) release() {
	if hf.own != nil {
		hf.own.close()
	}
	if hf.client != nil && hf.sub != nil {
		hf.sub.leave(hf.client)
	}
}

// clientBufferSize is the configured client_buffer_size, or its default.
func (h *Handler) clientBufferSize() int {
	if h.ClientBufferSize > 0 {
		return h.ClientBufferSize
	}
	return defaultClientBufferSize
}
