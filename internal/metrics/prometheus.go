package metrics

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

var (
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\r", `\r`)
	labelEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
)

// WritePrometheus writes the snapshot in Prometheus text exposition format
// (version 0.0.4). Every metric family gets one # HELP and # TYPE line, even
// when it has several label series. Histograms are expanded into _bucket
// (cumulative, including +Inf), _sum, and _count samples.
func (s *Snapshot) WritePrometheus(w io.Writer) error {
	values := s.Clone()
	values.Sort()

	bw := bufio.NewWriter(w)
	seen := make(map[string]MetricType, len(values.Metrics))
	for _, m := range values.Metrics {
		if prev, ok := seen[m.Name]; !ok {
			if _, err := fmt.Fprintf(bw, "# HELP %s %s\n", m.Name, helpEscaper.Replace(m.Help)); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(bw, "# TYPE %s %s\n", m.Name, m.Type); err != nil {
				return err
			}
			seen[m.Name] = m.Type
		} else if prev != m.Type {
			return fmt.Errorf("metrics: metric %s has conflicting types %s and %s", m.Name, prev, m.Type)
		}

		switch m.Type {
		case CounterType, GaugeType:
			if err := writeSample(bw, m.Name, m.Labels, m.Value); err != nil {
				return err
			}
		case HistogramType:
			if m.Histogram == nil {
				return fmt.Errorf("metrics: histogram %s has no values", m.Name)
			}
			if err := writeHistogram(bw, m.Name, m.Labels, m.Histogram); err != nil {
				return err
			}
		default:
			return fmt.Errorf("metrics: metric %s has unknown type %q", m.Name, m.Type)
		}
	}
	return bw.Flush()
}

func writeSample(w io.Writer, name string, labels Labels, value float64) error {
	_, err := fmt.Fprintf(w, "%s%s %s\n", name, formatLabels(labels), formatFloat(value))
	return err
}

func writeHistogram(w io.Writer, name string, labels Labels, h *HistogramSnapshot) error {
	if len(h.Bounds) != len(h.Buckets) {
		return fmt.Errorf("metrics: histogram %s has %d bounds but %d buckets", name, len(h.Bounds), len(h.Buckets))
	}
	for i, bound := range h.Bounds {
		if _, err := fmt.Fprintf(w, "%s_bucket%s %d\n", name, formatLabels(labels.with("le", formatFloat(bound))), h.Buckets[i]); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "%s_bucket%s %d\n", name, formatLabels(labels.with("le", "+Inf")), h.Count); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%s_sum%s %s\n", name, formatLabels(labels), formatFloat(h.Sum)); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "%s_count%s %d\n", name, formatLabels(labels), h.Count)
	return err
}

func formatLabels(labels Labels) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
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
		b.WriteString(`="`)
		b.WriteString(labelEscaper.Replace(labels[k]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	default:
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
}
