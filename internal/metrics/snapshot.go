package metrics

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// MetricValue is one metric series in a Snapshot. For counters and gauges,
// Value holds the current value. For histograms, Histogram is set and Value is
// zero.
type MetricValue struct {
	Name      string             `json:"name"`
	Help      string             `json:"help,omitempty"`
	Type      MetricType         `json:"type"`
	Labels    Labels             `json:"labels,omitempty"`
	Value     float64            `json:"value"`
	Histogram *HistogramSnapshot `json:"histogram,omitempty"`
}

// Snapshot is a serializable point-in-time view of a Registry, or of several
// registries merged together for aggregation.
type Snapshot struct {
	Metrics []MetricValue `json:"metrics"`
}

// Sort orders metrics by name, then type, then canonical labels. Exporters
// call it before rendering so output is deterministic.
func (s *Snapshot) Sort() {
	if s == nil {
		return
	}
	sort.SliceStable(s.Metrics, func(i, j int) bool {
		a, b := s.Metrics[i], s.Metrics[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		return a.Labels.key() < b.Labels.key()
	})
}

// Merge appends the metrics of other snapshots to s. It does not mutate the
// other snapshots. Metric labels are left untouched, so callers aggregating
// independent sources should add a source label with WithLabel first.
func (s *Snapshot) Merge(others ...*Snapshot) {
	if s == nil {
		return
	}
	for _, other := range others {
		if other == nil {
			continue
		}
		s.Metrics = append(s.Metrics, other.Metrics...)
	}
}

// Clone returns a deep copy of the snapshot's mutable parts. Histogram
// snapshots are immutable and may be shared.
func (s *Snapshot) Clone() *Snapshot {
	if s == nil {
		return &Snapshot{}
	}
	out := &Snapshot{Metrics: make([]MetricValue, len(s.Metrics))}
	for i, m := range s.Metrics {
		m.Labels = m.Labels.clone()
		out.Metrics[i] = m
	}
	return out
}

// WithLabel returns a copy of the snapshot with key=value added to every
// metric. Existing values of key are overwritten. The receiver is unchanged.
func (s *Snapshot) WithLabel(key, value string) *Snapshot {
	out := s.Clone()
	for i := range out.Metrics {
		out.Metrics[i].Labels = out.Metrics[i].Labels.with(key, value)
	}
	return out
}

// WriteJSON writes the snapshot as JSON. Metrics are sorted first, so the
// output is stable for a given snapshot.
func (s *Snapshot) WriteJSON(w io.Writer) error {
	normalized := s.Clone()
	normalized.Sort()
	return json.NewEncoder(w).Encode(normalized)
}

// ReadSnapshot decodes a snapshot written by WriteJSON.
func ReadSnapshot(r io.Reader) (*Snapshot, error) {
	var s Snapshot
	dec := json.NewDecoder(r)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("metrics: decode snapshot: %w", err)
	}
	s.Sort()
	return &s, nil
}
