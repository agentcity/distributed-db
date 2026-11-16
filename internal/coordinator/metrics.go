package coordinator

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// coordinatorMetrics holds the coordinator's private Prometheus registry and
// collectors. The registry is deliberately not the global default one, so
// several coordinators (or a node and a coordinator in one test binary) do not
// conflict.
type coordinatorMetrics struct {
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	load     *prometheus.GaugeVec
}

func newCoordinatorMetrics() *coordinatorMetrics {
	reg := prometheus.NewRegistry()
	m := &coordinatorMetrics{
		registry: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "db_coordinator_requests_total",
			Help: "Total number of requests handled by the coordinator, by operation, shard and result.",
		}, []string{"op", "shard", "result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "db_coordinator_request_duration_seconds",
			Help:    "Duration of coordinator requests in seconds, by operation and shard.",
			Buckets: prometheus.DefBuckets,
		}, []string{"op", "shard"}),
		load: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "db_coordinator_load",
			Help: "Last load sample reported by a node, by node and shard.",
		}, []string{"node", "shard"}),
	}
	reg.MustRegister(m.requests, m.duration, m.load)
	return m
}

// metricsHandler serves the private registry.
func (c *Coordinator) metricsHandler() http.Handler {
	return promhttp.HandlerFor(c.metrics.registry, promhttp.HandlerOpts{})
}

// observeRequest records one handled operation. shard may be "" when no shard
// could be selected.
func (c *Coordinator) observeRequest(op, shard, result string, start time.Time) {
	c.metrics.requests.WithLabelValues(op, shard, result).Inc()
	c.metrics.duration.WithLabelValues(op, shard).Observe(time.Since(start).Seconds())
}
