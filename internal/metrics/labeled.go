package metrics

import (
	"sort"
	"time"
)

// LabeledSLOConfig configures a LabeledSLO. Metric names follow the SLO
// convention (prefix_requests_total, prefix_errors_total,
// prefix_request_duration_seconds), but every series carries one extra label
// whose value is one of Values.
type LabeledSLOConfig struct {
	// Prefix is prepended to the metric names, for example "sql_statement"
	// produces sql_statement_requests_total.
	Prefix string
	// Labels are attached to every series in addition to Label.
	Labels Labels
	// Buckets overrides the latency histogram bounds.
	Buckets []time.Duration
	// Label is the name of the discriminating label, for example "kind".
	// It defaults to "label".
	Label string
	// Values is the closed set of label values. Duplicates are ignored. An
	// empty Values slice produces a single fallback series.
	Values []string
	// Fallback receives observations whose value is not in Values, which is
	// how callers keep cardinality bounded even when input is not trusted.
	// It defaults to "other" and is added to Values when missing.
	Fallback string
}

// LabeledSLO is a set of SLOs that share metric names and one closed-set
// label. It is safe for concurrent use, and observations with unknown label
// values are folded into the fallback series instead of creating new ones.
//
// The contained SLOs register one metric series per configured value at
// construction, so the label cardinality cannot grow at runtime.
type LabeledSLO struct {
	label    string
	fallback string
	values   []string
	slos     map[string]*SLO
}

// NewLabeledSLO creates one SLO per configured label value in reg. A nil
// registry creates a private one, matching NewSLO.
func NewLabeledSLO(reg *Registry, cfg LabeledSLOConfig) *LabeledSLO {
	if reg == nil {
		reg = NewRegistry()
	}
	label := cfg.Label
	if label == "" {
		label = "label"
	}
	fallback := cfg.Fallback
	if fallback == "" {
		fallback = "other"
	}

	values := make([]string, 0, len(cfg.Values)+1)
	seen := make(map[string]struct{}, len(cfg.Values)+1)
	for _, v := range cfg.Values {
		if v == "" {
			// An empty label value is valid Prometheus, but it is almost
			// always a caller bug and makes the fallback ambiguous.
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		values = append(values, v)
	}
	if _, ok := seen[fallback]; !ok {
		seen[fallback] = struct{}{}
		values = append(values, fallback)
	}
	sort.Strings(values)

	out := &LabeledSLO{
		label:    label,
		fallback: fallback,
		values:   values,
		slos:     make(map[string]*SLO, len(values)),
	}
	for _, v := range values {
		labels := cfg.Labels.with(label, v)
		out.slos[v] = NewSLO(reg, SLOConfig{
			Prefix:  cfg.Prefix,
			Labels:  labels,
			Buckets: cfg.Buckets,
		})
	}
	return out
}

// For returns the SLO for one label value. Unknown values return the fallback
// SLO rather than creating a new series.
func (l *LabeledSLO) For(value string) *SLO {
	if l == nil {
		return nil
	}
	if slo, ok := l.slos[value]; ok {
		return slo
	}
	return l.slos[l.fallback]
}

// Observe records one completed request for value.
func (l *LabeledSLO) Observe(value string, latency time.Duration, err error) {
	if l == nil {
		return
	}
	l.For(value).ObserveRequest(latency, err)
}

// Values returns the sorted, closed set of label values, including the
// fallback. The returned slice is a copy.
func (l *LabeledSLO) Values() []string {
	if l == nil {
		return nil
	}
	out := make([]string, len(l.values))
	copy(out, l.values)
	return out
}

// Fallback returns the label value used for unknown observations.
func (l *LabeledSLO) Fallback() string {
	if l == nil {
		return ""
	}
	return l.fallback
}

// LabelName returns the name of the discriminating label.
func (l *LabeledSLO) LabelName() string {
	if l == nil {
		return ""
	}
	return l.label
}
