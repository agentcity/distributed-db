package metrics

import (
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
)

type Metrics struct {
    nodeState *prometheus.GaugeVec
    term *prometheus.Gauge
    commitIndex *prometheus.Gauge
    lastApplied *prometheus.Gauge
    nodesCount *prometheus.Gauge
    shardsCount *prometheus.Gauge
}

func NewMetrics() *Metrics{
    return &Metrics{
        nodeState: promauto.NewGaugeVec(prometheus.GaugeOpts{
            Name: "node_state",
            Help: "Current node state",
        }, []string{"state"}),
        term: promauto.NewGauge(prometheus.GaugeOpts{
            Name: "node_term",
            Help: "Current term",
        }),
        commitIndex: promauto.NewGauge(prometheus.GaugeOpts{
            Name: "node_commit_index",
            Help: "Current commit index",
        }),
        lastApplied: promauto.NewGauge(prometheus.GaugeOpts{
            Name: "node_last_applied",
            Help: "Current last applied",
        }),
        nodesCount: promauto.NewGauge(prometheus.GaugeOpts{
            Name: "node_count",
            Help: "Current count of nodes",
        }),
        shardsCount: promauto.NewGauge(prometheus.GaugeOpts{
            Name: "shard_count",
            Help: "Current count of shards",
      }),
    }
}
func (m *Metrics) RegisterMetrics() {
    prometheus.Register(m.nodeState)
    prometheus.Register(m.term)
    prometheus.Register(m.commitIndex)
    prometheus.Register(m.lastApplied)
    prometheus.Register(m.nodesCount)
    prometheus.Register(m.shardsCount)
}

func (m *Metrics) SetNodeState(state string){
    m.nodeState.With(prometheus.Labels{"state": state}).Set(1)
}
func (m *Metrics) SetTerm(term int){
    m.term.Set(float64(term))
}
func (m *Metrics) SetCommitIndex(index int64){
    m.commitIndex.Set(float64(index))
}
func (m *Metrics) SetLastApplied(index int64){
    m.lastApplied.Set(float64(index))
}
func (m *Metrics) SetNodesCount(count int64){
    m.nodesCount.Set(float64(count))
}
func (m *Metrics) SetShardsCount(count int64){
    m.shardsCount.Set(float64(count))
}