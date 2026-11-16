// Package metrics provides a small, dependency-free toolkit for counters,
// gauges, latency histograms, snapshots, Prometheus text exposition, and SLO
// reports.
//
// The package has no dependencies outside the standard library and does not
// import the rest of the repository, so it can be used as a leaf package by
// KV, SQL, Raft, or command code that is instrumented later. Metrics are safe
// for concurrent use. Snapshot returns a point-in-time copy; under concurrent
// writers an individual snapshot is best-effort rather than a transactionally
// consistent cut, which is the usual tradeoff for lock-free hot-path counters.
//
// Metric names and label names are written to the exposition unchanged, so
// callers must pass valid Prometheus identifiers (for example
// "kv_rpc_requests_total" and "operation").
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Labels is a set of Prometheus-style labels attached to one metric series.
// The map must not be mutated after it is passed to a constructor.
type Labels map[string]string

func (l Labels) clone() Labels {
	if len(l) == 0 {
		return nil
	}
	out := make(Labels, len(l))
	for k, v := range l {
		out[k] = v
	}
	return out
}

func (l Labels) with(key, value string) Labels {
	out := make(Labels, len(l)+1)
	for k, v := range l {
		out[k] = v
	}
	out[key] = value
	return out
}

// key returns a deterministic representation used to detect duplicate series.
func (l Labels) key() string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(l[k])
		b.WriteByte(0)
	}
	return b.String()
}

// MetricType is the exposition type of a metric.
type MetricType string

const (
	// CounterType is a monotonically increasing counter.
	CounterType MetricType = "counter"
	// GaugeType is a value that can go up and down.
	GaugeType MetricType = "gauge"
	// HistogramType is a bucketed distribution.
	HistogramType MetricType = "histogram"
)

type metricDesc struct {
	name   string
	help   string
	typ    MetricType
	labels Labels
}

func newMetricDesc(name, help string, typ MetricType, labels Labels) metricDesc {
	return metricDesc{name: name, help: help, typ: typ, labels: labels.clone()}
}

func (d metricDesc) key() string {
	return string(d.typ) + "\x00" + d.name + "\x00" + d.labels.key()
}

func (d metricDesc) labelString() string {
	if len(d.labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(d.labels))
	for k := range d.labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(d.labels[k])
	}
	b.WriteByte('}')
	return b.String()
}

type metricSnapshotter interface {
	metricSnapshot() MetricValue
}

type registeredMetric struct {
	desc   metricDesc
	metric metricSnapshotter
}

// Registry owns a set of metrics and can render them as a Prometheus text
// exposition. A Registry is safe for concurrent use. Metrics must be created
// through a Registry; duplicate name+label series and one name used with two
// different types panic, because they cannot form a valid exposition.
type Registry struct {
	mu    sync.Mutex
	order []registeredMetric
	keys  map[string]struct{}
	types map[string]MetricType
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		keys:  make(map[string]struct{}),
		types: make(map[string]MetricType),
	}
}

func (r *Registry) add(desc metricDesc, m metricSnapshotter) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := desc.key()
	if _, ok := r.keys[key]; ok {
		panic(fmt.Sprintf("metrics: duplicate metric %s%s", desc.name, desc.labelString()))
	}
	if typ, ok := r.types[desc.name]; ok && typ != desc.typ {
		panic(fmt.Sprintf("metrics: metric %s already registered as %s, cannot register as %s", desc.name, typ, desc.typ))
	}
	r.keys[key] = struct{}{}
	r.types[desc.name] = desc.typ
	r.order = append(r.order, registeredMetric{desc: desc, metric: m})
}

// NewCounter creates and registers a counter.
func (r *Registry) NewCounter(name, help string, labels Labels) *Counter {
	c := &Counter{desc: newMetricDesc(name, help, CounterType, labels)}
	r.add(c.desc, c)
	return c
}

// NewGauge creates and registers a gauge.
func (r *Registry) NewGauge(name, help string, labels Labels) *Gauge {
	g := &Gauge{desc: newMetricDesc(name, help, GaugeType, labels)}
	r.add(g.desc, g)
	return g
}

// NewHistogram creates and registers a histogram. Passing nil or an empty
// bucket slice uses DefaultLatencyBuckets.
func (r *Registry) NewHistogram(name, help string, labels Labels, buckets []time.Duration) *Histogram {
	h := newHistogram(newMetricDesc(name, help, HistogramType, labels), buckets)
	r.add(h.desc, h)
	return h
}

// Snapshot returns a sorted copy of the current metric values.
func (r *Registry) Snapshot() *Snapshot {
	r.mu.Lock()
	entries := make([]registeredMetric, len(r.order))
	copy(entries, r.order)
	r.mu.Unlock()

	s := &Snapshot{Metrics: make([]MetricValue, 0, len(entries))}
	for _, e := range entries {
		s.Metrics = append(s.Metrics, e.metric.metricSnapshot())
	}
	s.Sort()
	return s
}

// WritePrometheus writes the registry in Prometheus text exposition format.
func (r *Registry) WritePrometheus(w io.Writer) error {
	return r.Snapshot().WritePrometheus(w)
}

// Counter is a monotonically increasing counter.
type Counter struct {
	desc metricDesc
	v    atomic.Uint64
}

// Inc adds one to the counter.
func (c *Counter) Inc() { c.v.Add(1) }

// Add adds delta to the counter.
func (c *Counter) Add(delta uint64) { c.v.Add(delta) }

// Value returns the current counter value.
func (c *Counter) Value() uint64 { return c.v.Load() }

func (c *Counter) metricSnapshot() MetricValue {
	return MetricValue{
		Name:   c.desc.name,
		Help:   c.desc.help,
		Type:   c.desc.typ,
		Labels: c.desc.labels.clone(),
		Value:  float64(c.v.Load()),
	}
}

// Gauge is a value that can go up and down.
type Gauge struct {
	desc metricDesc
	v    atomic.Int64
}

// Set stores v.
func (g *Gauge) Set(v int64) { g.v.Store(v) }

// Add adds delta to the gauge.
func (g *Gauge) Add(delta int64) { g.v.Add(delta) }

// Inc adds one to the gauge.
func (g *Gauge) Inc() { g.v.Add(1) }

// Dec subtracts one from the gauge.
func (g *Gauge) Dec() { g.v.Add(-1) }

// Value returns the current gauge value.
func (g *Gauge) Value() int64 { return g.v.Load() }

func (g *Gauge) metricSnapshot() MetricValue {
	return MetricValue{
		Name:   g.desc.name,
		Help:   g.desc.help,
		Type:   g.desc.typ,
		Labels: g.desc.labels.clone(),
		Value:  float64(g.v.Load()),
	}
}
