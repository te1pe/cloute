package doc

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics of the document hubs, exported on /metrics.
var (
	documentsLoaded = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "cloute_documents_loaded",
		Help: "Documents held in memory by a running hub.",
	})
	opsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cloute_ops_total",
		Help: "Operations accepted and stored, including those of version restores; rate() gives operations per second.",
	})
)
