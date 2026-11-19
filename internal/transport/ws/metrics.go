package ws

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	connections = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "cloute_ws_connections",
		Help: "Open WebSocket connections.",
	})
	broadcastSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "cloute_broadcast_seconds",
		Help:    "Latency from receiving operations to writing them to each other client.",
		Buckets: prometheus.ExponentialBuckets(0.0005, 2, 14),
	})
)
