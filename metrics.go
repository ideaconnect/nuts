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

	// nuts_messages_dropped_total counts messages that were dropped
	// because they exceeded max_event_size.
	metricsMessagesDropped = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "messages_dropped_total",
		Help:      "Total number of messages dropped (oversized events).",
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

	// nuts_replay_fallbacks_total counts how many times the requested
	// sequence was purged and NUTS fell back to DeliverAll().
	metricsReplayFallbacks = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "replay_fallbacks_total",
		Help:      "Total number of replay requests that fell back to DeliverAll due to purged sequences.",
	})

	// nuts_subscription_errors_total counts failed JetStream subscribe attempts.
	metricsSubscriptionErrors = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "nuts",
		Name:      "subscription_errors_total",
		Help:      "Total number of failed JetStream subscription attempts.",
	})
)
