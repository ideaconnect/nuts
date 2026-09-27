// metrics.go — Prometheus metrics for NUTS.
//
// The metrics are package-level collectors, registered on the default
// Prometheus registry via promauto. Caddy does not serve that registry: its
// metrics handler and admin /metrics endpoint expose a registry of their own,
// created for every config load. Provision therefore also registers every
// collector there (registerMetrics); without that, no nuts_* series ever
// reaches a scrape.
package nuts

import (
	"errors"

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

	// nuts_messages_dropped_total counts messages that were not delivered,
	// by reason: raw_payload (the inbound JetStream payload exceeded
	// max_event_size) and formatted_sse_message (the SSE frame after the JSON
	// wrap did) are tuned differently — the first usually points at the
	// producer, the second at envelope overhead on small but pathological
	// payloads. replay_window counts replayed messages older than the window,
	// control_message the server's subject delete markers and schedule
	// definitions, raw_not_text the payloads payload_format raw cannot send
	// (#143).
	metricsMessagesDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "messages_dropped_total",
		Help:      "Total number of messages not delivered to a client, labelled by reason (raw_payload, formatted_sse_message, replay_window, control_message, raw_not_text).",
	}, []string{"reason"})

	// nuts_wildcard_filter_drops_total is deprecated: it counted messages the
	// pre-NATS-2.10 multi-topic wildcard fallback filtered client-side. That
	// fallback is gone (nats-server >= 2.10 is required), so the series stays
	// at zero until it is removed in the next major release.
	metricsWildcardFilterDrops = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "wildcard_filter_drops_total",
		Help:      "Deprecated, always 0: the pre-NATS-2.10 multi-topic wildcard fallback was removed.",
	})

	// nuts_slow_client_disconnects_total counts clients disconnected because
	// a write missed its write_timeout deadline: the client stopped reading.
	// A client that is merely slower than JetStream is throttled by the pull
	// consumer instead and never counted here.
	metricsSlowClientDisconnects = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "slow_client_disconnects_total",
		Help:      "Total number of clients disconnected because a write missed its write_timeout deadline.",
	})

	// nuts_replay_requests_total counts how many times clients connected
	// with a last-id or Last-Event-ID for message replay.
	metricsReplayRequests = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "replay_requests_total",
		Help:      "Total number of SSE connections requesting message replay.",
	})

	// nuts_replay_fallbacks_total counts replay requests served from a
	// fallback start position, once their consumer exists.
	metricsReplayFallbacks = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "replay_fallbacks_total",
		Help:      "Total number of replay requests served from a fallback start position (sequence below retention, outside replay_window, ahead of the stream, or stream info unavailable), counted once the consumer exists.",
	})

	// nuts_subscription_errors_total counts requests refused because their
	// topics are outside the stream or their consumer could not be created.
	// Consumer-limit refusals are counted in connections_rejected_total.
	metricsSubscriptionErrors = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "subscription_errors_total",
		Help:      "Total number of stream requests refused because a topic is outside the stream or the JetStream consumer could not be created.",
	})

	// nuts_connections_rejected_total counts SSE connections rejected before
	// streaming started, labelled by reason.
	metricsConnectionsRejected = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "connections_rejected_total",
		Help:      "Total number of SSE connections rejected before streaming started, labelled by reason (max_connections, stream_consumer_limit, auth_missing_token, auth_invalid_token, auth_topic_forbidden).",
	}, []string{"reason"})

	// nuts_replay_cap_reached_total counts replaying SSE connections closed
	// after delivering replay_max_messages historical events.
	metricsReplayCapReached = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "replay_cap_reached_total",
		Help:      "Total number of replaying SSE connections closed after replay_max_messages was reached.",
	})

	// nuts_dispatch_timeout_total is deprecated along with dispatch_timeout:
	// the pull consumer has no queue hand-off that can time out. The series
	// stays at zero until it is removed in the next major release.
	metricsDispatchTimeout = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "dispatch_timeout_total",
		Help:      "Deprecated, always 0: dispatch_timeout has no effect since the pull consumer replaced the push queue.",
	})

	// nuts_nats_async_errors_total counts asynchronous errors reported by
	// the NATS client's ErrorHandler.
	//
	// Bounded label set (kept in lockstep with classifyNATSAsyncError —
	// helpers.go is the source of truth and pins the mapping in
	// regression tests):
	//
	//   - slow_consumer:        nats.ErrSlowConsumer (a nats.go subscription
	//                           buffer overflowed, upstream of NUTS). Each
	//                           stream pulls at most client_buffer_size
	//                           messages at a time, so this should stay 0.
	//   - timeout:              nats.ErrTimeout.
	//   - connection_state:     nats.ErrConnectionClosed,
	//                           nats.ErrConnectionDraining.
	//   - consumer_invalidated: heartbeat and sequence errors of legacy
	//                           push subscriptions. The ordered pull
	//                           consumers handle these themselves (see
	//                           consumer_invalidated_total), so it stays 0.
	//   - other:                everything else.
	metricsNATSAsyncErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "nats_async_errors_total",
		Help:      "Total number of asynchronous NATS client errors observed by the registered ErrorHandler, labelled by kind (slow_consumer, timeout, connection_state, consumer_invalidated, other). consumer_invalidated only applies to legacy push subscriptions and stays 0.",
	}, []string{"kind"})

	// nuts_consumer_invalidated_total counts JetStream consumer failures
	// under live SSE streams, by reason:
	//
	//   - recreated:     the ordered consumer recreated itself after a
	//                    delivery gap, a NATS reconnect or missed
	//                    heartbeats (consumer reaped, deleted or lost), and
	//                    delivery resumed after the last delivered message.
	//                    The client noticed nothing.
	//   - unrecoverable: recreation kept failing, so the SSE stream closed
	//                    with disconnect_reason=consumer_unrecoverable and
	//                    the client reconnects with Last-Event-ID.
	//   - stream_recreated: the stream itself was deleted and created again
	//                    (its creation time changed), so the SSE stream
	//                    closed and the client reconnects from the start of
	//                    the new stream (#133).
	//   - stream_rewound: the stream went back to an earlier sequence with its
	//                    creation time unchanged, as after a restore on
	//                    nats-server 2.14 or earlier, so the SSE stream
	//                    closed and the client replays it from its start
	//                    (#133, #137).
	metricsConsumerInvalidated = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "consumer_invalidated_total",
		Help:      "Total JetStream consumer failures under live SSE streams. reason: recreated (the ordered consumer recovered after a gap, reconnect or missed heartbeats), unrecoverable (recreation failed and the stream closed), stream_recreated (the stream was deleted and created again; the SSE stream closed and the client reconnects from the start of the new one), stream_rewound (the stream went back to an earlier sequence, as after a restore that kept its creation time; the SSE stream closed and the client replays it from its start).",
	}, []string{"reason"})

	// nuts_write_disconnects_total counts SSE streams that ended because a
	// write to the response writer failed (typically the deadline imposed
	// by write_timeout fired). Labelled by site so operators can tell
	// whether the failing write was the initial connected event, a regular
	// message frame, or a heartbeat.
	metricsWriteDisconnects = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "write_disconnects_total",
		Help:      "Total number of SSE streams terminated by a failed write, labelled by site (connected, message, heartbeat).",
	}, []string{"site"})

	// nuts_readiness_failures_total counts /readyz probe responses that
	// returned 503 because a dependency was degraded. Labelled by cause
	// so operators can distinguish a NATS-link outage from a JetStream
	// context loss from a StreamInfo lookup failure.
	metricsReadinessFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "readiness_failures_total",
		Help:      "Total number of /readyz probe responses returning 503, labelled by cause (nats_disconnected, jetstream_missing, stream_info_error).",
	}, []string{"cause"})

	// nuts_nats_connection_events_total counts NATS connection-state
	// transitions reported by the registered Disconnect/Reconnect/Closed/
	// LameDuckMode handlers. Closing the connection on Cleanup reports
	// nothing (NoCallbacksAfterClientClose). A flapping broker is invisible to nuts_nats_async_errors_total
	// when the in-flight client surface is quiet, so this counter is the
	// canonical signal for clean disconnect+reconnect cycles. Alert on
	// e.g. increase(...{event="reconnect"}[10m]) > 3 for flap detection.
	// nuts_shared_subscriptions is the number of shared subscriptions
	// (shared_subscriptions on): one JetStream consumer per topic set that
	// caught-up connections share.
	metricsSharedSubscriptions = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "nuts",
		Name:      "shared_subscriptions",
		Help:      "Number of shared subscriptions, each one JetStream consumer shared by the caught-up connections of a topic set (shared_subscriptions on).",
	})

	// nuts_shared_transitions_total counts connections moving onto and off
	// shared subscriptions: joined (caught up and attached), fell_behind
	// (its queue overflowed; it continues on its own consumer), and
	// shared_failed (the shared consumer could not be recreated).
	metricsSharedTransitions = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "shared_transitions_total",
		Help:      "Total number of connections moving onto or off shared subscriptions, labelled by transition (joined, fell_behind, shared_failed).",
	}, []string{"transition"})

	metricsNATSConnectionEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "nats_connection_events_total",
		Help:      "Total NATS connection-state transitions, labelled by event (disconnect, reconnect, closed, lame_duck).",
	}, []string{"event"})
)

// nutsCollectors lists every NUTS metric for registerMetrics.
func nutsCollectors() []prometheus.Collector {
	return []prometheus.Collector{
		metricsActiveConnections,
		metricsMessagesDelivered,
		metricsMessagesDropped,
		metricsWildcardFilterDrops,
		metricsSlowClientDisconnects,
		metricsReplayRequests,
		metricsReplayFallbacks,
		metricsSubscriptionErrors,
		metricsConnectionsRejected,
		metricsReplayCapReached,
		metricsDispatchTimeout,
		metricsNATSAsyncErrors,
		metricsConsumerInvalidated,
		metricsWriteDisconnects,
		metricsReadinessFailures,
		metricsSharedSubscriptions,
		metricsSharedTransitions,
		metricsNATSConnectionEvents,
	}
}

// registerMetrics registers the NUTS collectors with a Caddy config's metrics
// registry. Several nuts handlers in one config share the collectors, so a
// collector that is already registered is not an error.
func registerMetrics(registry *prometheus.Registry) error {
	if registry == nil {
		return nil
	}
	for _, collector := range nutsCollectors() {
		if err := registry.Register(collector); err != nil {
			var already prometheus.AlreadyRegisteredError
			if errors.As(err, &already) {
				continue
			}
			return err
		}
	}
	return nil
}
