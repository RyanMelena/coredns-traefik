package traefik

import (
	"github.com/coredns/coredns/plugin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The failure mode worth alerting on is not a crash - it is this plugin quietly
// serving an empty or stale zone while looking healthy. records and
// last_success_timestamp_seconds are the two series that expose that.
var (
	recordCount = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "records",
		Help:      "The number of A records currently served.",
	})

	lastSuccess = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "last_success_timestamp_seconds",
		Help:      "The timestamp of the last successful poll of the Traefik API.",
	})

	pollFailures = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "consecutive_poll_failures",
		Help:      "The number of polls that have failed since the last successful one.",
	})

	pollCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "polls_total",
		Help:      "Counter of Traefik API polls by outcome.",
	}, []string{"status"})

	pollDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "poll_duration_seconds",
		Help:      "Histogram of the time taken to poll the Traefik API.",
		Buckets:   prometheus.DefBuckets,
	})
)
