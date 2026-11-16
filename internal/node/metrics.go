package node

import (
	"github.com/prometheus/client_golang/prometheus"

	"distributed-db/internal/raft"
)

// metrics owns a private Prometheus registry; nothing is registered with the
// global default registry or created through promauto. This keeps multiple
// nodes (for example in tests) from colliding and lets the caller decide where
// to expose the registry.
type metrics struct {
	registry *prometheus.Registry

	putTotal    prometheus.Counter
	getTotal    prometheus.Counter
	deleteTotal prometheus.Counter
	errorsTotal prometheus.Counter

	httpDuration prometheus.Histogram
}

func newMetrics(n *Node) *metrics {
	registry := prometheus.NewRegistry()

	m := &metrics{
		registry: registry,
		putTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "db_put_total",
			Help: "Total number of Put operations attempted.",
		}),
		getTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "db_get_total",
			Help: "Total number of Get operations attempted.",
		}),
		deleteTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "db_delete_total",
			Help: "Total number of Delete operations attempted.",
		}),
		errorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "db_errors_total",
			Help: "Total number of failed operations and Raft RPCs.",
		}),
		httpDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "db_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds.",
			Buckets: prometheus.DefBuckets,
		}),
	}

	registry.MustRegister(
		m.putTotal,
		m.getTotal,
		m.deleteTotal,
		m.errorsTotal,
		m.httpDuration,
		newRaftCollector(n),
	)
	return m
}

// raftCollector exposes Raft state as gauges. It is a custom collector, so the
// values are read at scrape time and always reflect the current status without
// a polling goroutine; node.mu makes the read race-free with Stop.
type raftCollector struct {
	node *Node

	term        *prometheus.Desc
	commitIndex *prometheus.Desc
	lastApplied *prometheus.Desc
	isLeader    *prometheus.Desc
}

func newRaftCollector(n *Node) *raftCollector {
	return &raftCollector{
		node: n,
		term: prometheus.NewDesc(
			"db_raft_term",
			"Current Raft term.",
			nil, nil,
		),
		commitIndex: prometheus.NewDesc(
			"db_raft_commit_index",
			"Highest log index known to be committed.",
			nil, nil,
		),
		lastApplied: prometheus.NewDesc(
			"db_raft_last_applied",
			"Highest log index applied to the state machine.",
			nil, nil,
		),
		isLeader: prometheus.NewDesc(
			"db_raft_is_leader",
			"1 if this node believes it is the Raft leader, 0 otherwise.",
			nil, nil,
		),
	}
}

func (c *raftCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.term
	ch <- c.commitIndex
	ch <- c.lastApplied
	ch <- c.isLeader
}

func (c *raftCollector) Collect(ch chan<- prometheus.Metric) {
	n := c.node
	n.mu.RLock()
	if n.stopped || n.raft == nil {
		n.mu.RUnlock()
		return
	}
	st := n.raft.Status()
	n.mu.RUnlock()

	leader := 0.0
	if st.Role == raft.Leader {
		leader = 1
	}

	ch <- prometheus.MustNewConstMetric(c.term, prometheus.GaugeValue, float64(st.Term))
	ch <- prometheus.MustNewConstMetric(c.commitIndex, prometheus.GaugeValue, float64(st.CommitIndex))
	ch <- prometheus.MustNewConstMetric(c.lastApplied, prometheus.GaugeValue, float64(st.LastApplied))
	ch <- prometheus.MustNewConstMetric(c.isLeader, prometheus.GaugeValue, leader)
}
