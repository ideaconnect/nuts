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
		Help:      "Total number of SSE connections rejected before streaming started.",
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
	// the NATS client (most importantly nats.ErrSlowConsumer, which fires
	// when nats.go's per-subscription buffer overflows and silently drops
	// messages). Labelled by kind so operators can distinguish slow-consumer
	// drops at the library layer from other async error categories.
	metricsNATSAsyncErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "nats_async_errors_total",
		Help:      "Total number of asynchronous NATS client errors observed by the registered ErrorHandler.",
	}, []string{"kind"})

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
)
