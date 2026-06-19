// metrics.go — Prometheus metrics for NUTS.
//
// All metrics are registered on init via promauto, which means they
// automatically appear on Caddy's /metrics endpoint when the admin API
// or a metrics handler is enabled.
package nuts

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// nuts_active_connections is a gauge tracking how many SSE clients
	// are currently connected.
	metricsActiveConnections = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "nuts",
		Name:      "active_connections",
		Help:      "Number of active SSE client connections.",
	})

	// nuts_messages_delivered_total counts all SSE message events that
	// were successfully written to clients.
	metricsMessagesDelivered = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "messages_delivered_total",
		Help:      "Total number of SSE message events delivered to clients.",
	})

	// nuts_messages_dropped_total counts messages that were dropped during
	// SSE formatting. Labelled by reason so operators can distinguish a
	// pure-NATS oversize (the inbound JetStream payload exceeded
	// max_event_size) from a post-envelope oversize (the SSE frame after
	// JSON wrap exceeded max_event_size). The two are tuned differently:
	// raw_payload usually points at producer-side issues, formatted_sse_message
	// at SSE envelope overhead on small but pathological payloads.
	metricsMessagesDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "messages_dropped_total",
		Help:      "Total number of messages dropped during SSE formatting. Labelled by drop reason.",
	}, []string{"reason"})

	// nuts_wildcard_filter_drops_total counts how many JetStream messages
	// were silently filtered client-side by the multi-topic wildcard
	// fallback path (servers older than NATS 2.10 that lack
	// ConsumerFilterSubjects). A non-zero value means the wildcard
	// subscription is wasting bandwidth on subjects no client requested;
	// operators can use this to size the move to NATS >= 2.10.
	metricsWildcardFilterDrops = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "wildcard_filter_drops_total",
		Help:      "Total number of messages dropped by the multi-topic wildcard fallback's client-side filter (subjects not requested by the client).",
	})

	// nuts_slow_client_disconnects_total counts clients that were
	// disconnected because their per-connection buffer was full.
	metricsSlowClientDisconnects = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "slow_client_disconnects_total",
		Help:      "Total number of clients disconnected due to slow consumption.",
	})

	// nuts_replay_requests_total counts how many times clients connected
	// with a last-id or Last-Event-ID for message replay.
	metricsReplayRequests = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "replay_requests_total",
		Help:      "Total number of SSE connections requesting message replay.",
	})

	// nuts_replay_fallbacks_total counts how many times NUTS used a fallback
	// replay strategy: either because the requested sequence was purged
	// (below retention) or because it was older than the configured
	// replay_window.
	metricsReplayFallbacks = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "replay_fallbacks_total",
		Help:      "Total number of replay requests that used fallback replay (purged sequence or older than replay_window).",
	})

	// nuts_subscription_errors_total counts failed JetStream subscribe attempts.
	metricsSubscriptionErrors = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "subscription_errors_total",
		Help:      "Total number of failed JetStream subscription attempts.",
	})

	// nuts_connections_rejected_total counts SSE connections rejected before
	// streaming started, labelled by reason (e.g. "max_connections").
	metricsConnectionsRejected = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "connections_rejected_total",
		Help:      "Total number of SSE connections rejected before streaming started, labelled by reason (max_connections, auth_missing_token, auth_invalid_token, auth_topic_forbidden).",
	}, []string{"reason"})

	// nuts_replay_cap_reached_total counts replaying SSE connections closed
	// after delivering replay_max_messages historical events.
	metricsReplayCapReached = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "replay_cap_reached_total",
		Help:      "Total number of replaying SSE connections closed after replay_max_messages was reached.",
	})

	// nuts_dispatch_timeout_total counts how often the NATS callback gave up
	// waiting to signal a slow SSE client because dispatch_timeout fired.
	// Distinct from nuts_slow_client_disconnects_total: that counter ticks
	// when the SSE loop observed the slow-client signal and disconnected;
	// this one ticks when the signal itself could not be delivered.
	metricsDispatchTimeouts = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "dispatch_timeout_total",
		Help:      "Total number of NATS callbacks that timed out signalling a slow SSE client.",
	})

	// nuts_nats_async_errors_total counts asynchronous errors reported by
	// the NATS client. Labelled by kind so operators can distinguish
	// slow-consumer drops at the library layer from connection-state
	// transitions and from JetStream consumer-invalidation events.
	//
	// Bounded label set (kept in lockstep with classifyNATSAsyncError —
	// helpers.go is the source of truth and pins the mapping in
	// regression tests):
	//
	//   - slow_consumer:        nats.ErrSlowConsumer (nats.go's per-
	//                           subscription internal buffer overflowed
	//                           and messages were silently dropped at
	//                           the library layer — upstream of NUTS'
	//                           bounded msgChan).
	//   - timeout:              nats.ErrTimeout.
	//   - connection_state:     nats.ErrConnectionClosed,
	//                           nats.ErrConnectionDraining.
	//   - consumer_invalidated: nats.ErrConsumerNotActive (primary
	//                           heartbeat-miss path; raised by
	//                           activityCheck on IdleHeartbeat timeout),
	//                           nats.ErrConsumerDeleted (kept as
	//                           forward-compat — see helpers.go),
	//                           *nats.ErrConsumerSequenceMismatch
	//                           (sequence drift while heartbeats are
	//                           still arriving).
	//   - other:                everything else.
	metricsNATSAsyncErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "nats_async_errors_total",
		Help:      "Total number of asynchronous NATS client errors observed by the registered ErrorHandler. Labelled by kind: slow_consumer, timeout, connection_state, consumer_invalidated, other.",
	}, []string{"kind"})

	// nuts_consumer_invalidated_total — declared in M9 Batch A
	// (#M9-1) so the series is visible at /metrics from day one;
	// populated by M9 Batch B (#M9-5) from the serveStream
	// consumer_invalidated termination arm. During Batch A the
	// series is permastuck at zero and the SSE handler is NOT
	// disconnected on heartbeat miss; the Help string below is
	// phrased to reflect that current behaviour rather than the
	// post-Batch-B contract, so an operator reading /metrics
	// without source access can tell which milestone has shipped.
	//
	// Reason labels (kept in lockstep with the classifyNATSAsync
	// Error label set, so a Batch B implementer routing the
	// classifier output to a reason label has one mapping to
	// follow, not two):
	//
	//   - heartbeat_missed: from kind=consumer_invalidated. Covers
	//                      all three nats.go errors that classifier
	//                      buckets there: ErrConsumerNotActive
	//                      (primary heartbeat-miss path),
	//                      ErrConsumerDeleted (forward-compat),
	//                      *ErrConsumerSequenceMismatch (sequence
	//                      drift). The earlier Phase 1 attribution
	//                      that wired heartbeat_missed only to
	//                      *ErrConsumerSequenceMismatch was the bug
	//                      the Batch A hardening (104299f) caught
	//                      and corrected — this comment is the
	//                      authoritative wiring spec for Batch B.
	//   - slow_consumer:   from kind=slow_consumer (nats.ErrSlowConsumer
	//                      at the nats.go library layer; distinct from
	//                      nuts_slow_client_disconnects_total which
	//                      fires when the SSE writer falls behind
	//                      NUTS' bounded msgChan one layer downstream).
	metricsConsumerInvalidated = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "consumer_invalidated_total",
		Help:      "Total JetStream push-consumer invalidation events. Declared in M9 Batch A (#M9-1) and currently always 0 — Batch A is detection-only; M9 Batch B (#M9-5) will increment this when an invalidated consumer triggers an SSE disconnect with reason=consumer_invalidated. Labels: heartbeat_missed, slow_consumer.",
	}, []string{"reason"})

	// nuts_write_disconnects_total counts SSE streams that ended because a
	// write to the response writer failed (typically the deadline imposed
	// by write_timeout fired). Labelled by site so operators can tell
	// whether the failing write was the initial connected event, a regular
	// message frame, or a heartbeat.
	metricsWriteDisconnects = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "write_disconnects_total",
		Help:      "Total number of SSE streams terminated by a response-writer write error.",
	}, []string{"site"})

	// nuts_readiness_failures_total counts /readyz probe responses that
	// returned 503 because a dependency was degraded. Labelled by cause
	// so operators can distinguish a NATS-link outage from a JetStream
	// context loss from a StreamInfo lookup failure.
	metricsReadinessFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "readiness_failures_total",
		Help:      "Total number of /readyz probe responses returning 503, labelled by cause.",
	}, []string{"cause"})

	// nuts_nats_connection_events_total counts NATS connection-state
	// transitions reported by the registered Disconnect/Reconnect/Closed
	// handlers. A flapping broker is invisible to nuts_nats_async_errors_total
	// when the in-flight client surface is quiet, so this counter is the
	// canonical signal for clean disconnect+reconnect cycles. Alert on
	// e.g. increase(...{event="reconnect"}[10m]) > 3 for flap detection.
	metricsNATSConnectionEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "nats_connection_events_total",
		Help:      "Total NATS connection-state transitions, labelled by event (disconnect, reconnect, closed).",
	}, []string{"event"})
)
