package metrics

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLabeledSLOBoundedCardinality(t *testing.T) {
	reg := NewRegistry()
	l := NewLabeledSLO(reg, LabeledSLOConfig{
		Prefix:   "sql_statement",
		Label:    "kind",
		Values:   []string{"SELECT", "INSERT", "SELECT"},
		Fallback: "OTHER",
		Buckets:  []time.Duration{time.Millisecond, 10 * time.Millisecond},
	})

	l.Observe("SELECT", 2*time.Millisecond, nil)
	l.Observe("INSERT", 3*time.Millisecond, errors.New("boom"))
	for i := 0; i < 100; i++ {
		l.Observe(fmt.Sprintf("SELECT secret %d", i), time.Millisecond, nil)
	}

	if got := l.LabelName(); got != "kind" {
		t.Fatalf("LabelName() = %q, want kind", got)
	}
	if got := l.Fallback(); got != "OTHER" {
		t.Fatalf("Fallback() = %q, want OTHER", got)
	}
	values := l.Values()
	if got := strings.Join(values, ","); got != "INSERT,OTHER,SELECT" {
		t.Fatalf("Values() = %q, want INSERT,OTHER,SELECT", got)
	}
	if got := l.For("SELECT secret 1"); got != l.For("OTHER") {
		t.Fatal("unknown label value did not route to the fallback SLO")
	}
	if got := l.For("SELECT").Requests.Value(); got != 1 {
		t.Fatalf("SELECT requests = %d, want 1", got)
	}
	if got := l.For("INSERT").Requests.Value(); got != 1 {
		t.Fatalf("INSERT requests = %d, want 1", got)
	}
	if got := l.For("INSERT").Errors.Value(); got != 1 {
		t.Fatalf("INSERT errors = %d, want 1", got)
	}
	if got := l.For("OTHER").Requests.Value(); got != 100 {
		t.Fatalf("OTHER requests = %d, want 100", got)
	}

	snap := reg.Snapshot()
	kinds := make(map[string]struct{})
	for _, m := range snap.Metrics {
		if kind, ok := m.Labels["kind"]; ok {
			kinds[kind] = struct{}{}
			for _, v := range m.Labels {
				if strings.Contains(v, "secret") {
					t.Fatalf("metric labels leaked high-cardinality SQL text: %s=%q", m.Name, v)
				}
			}
		}
	}
	if len(kinds) != 3 {
		t.Fatalf("distinct kind labels = %d (%v), want 3", len(kinds), kinds)
	}

	// Every series must share the same metric family so Prometheus can scrape
	// a single histogram family with one label dimension.
	var buf strings.Builder
	if err := reg.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	out := buf.String()
	if strings.Count(out, "# TYPE sql_statement_requests_total counter") != 1 {
		t.Fatalf("unexpected HELP/TYPE duplication:\n%s", out)
	}
	if !strings.Contains(out, `sql_statement_requests_total{kind="SELECT"} 1`) {
		t.Fatalf("missing bounded SELECT series:\n%s", out)
	}
	if strings.Contains(out, "secret") {
		t.Fatalf("exposition leaked SQL text:\n%s", out)
	}
}

func TestLabeledSLODefaultsAndNil(t *testing.T) {
	reg := NewRegistry()
	l := NewLabeledSLO(reg, LabeledSLOConfig{Prefix: "x"})
	if got := l.Values(); len(got) != 1 || got[0] != "other" {
		t.Fatalf("default Values() = %v, want [other]", got)
	}
	l.Observe("anything", time.Millisecond, nil)
	if got := l.For("other").Requests.Value(); got != 1 {
		t.Fatalf("fallback requests = %d, want 1", got)
	}

	var nilLabeled *LabeledSLO
	nilLabeled.Observe("SELECT", time.Millisecond, nil)
	if nilLabeled.For("SELECT") != nil || nilLabeled.Values() != nil || nilLabeled.Fallback() != "" {
		t.Fatal("nil LabeledSLO accessors are not zero-safe")
	}
}
