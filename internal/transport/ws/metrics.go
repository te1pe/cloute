package ws

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics of the WebSocket transport, exported on /metrics.
var (
	connections = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "cloute_ws_connections",
		Help: "Open WebSocket connections.",
	})
	// From the server receiving operations to their broadcast being written
	// to another client's socket: hub queueing, the database write, the
	// client's outgoing queue and the write itself.
	broadcastSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "cloute_broadcast_seconds",
		Help:    "Latency from receiving operations to writing them to each other client.",
		Buckets: prometheus.ExponentialBuckets(0.0005, 2, 14), // 0.5 ms .. 4 s
	})
)
