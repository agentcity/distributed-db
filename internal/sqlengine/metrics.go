package sqlengine

import (
	"strings"
	"time"

	"distributed-db/internal/metrics"
	"distributed-db/internal/sqlparser"
)

// Bounded values for the "kind" metric label. SQL text and literals are never
// used as label values, so statement cardinality stays constant.
const (
	StatementSelect = "SELECT"
	StatementInsert = "INSERT"
	StatementUpdate = "UPDATE"
	StatementDelete = "DELETE"
	StatementDDL    = "DDL"
	StatementOther  = "OTHER"
)

// statementKinds is the closed set of values accepted by SQLMetrics.
var statementKinds = []string{
	StatementSelect,
	StatementInsert,
	StatementUpdate,
	StatementDelete,
	StatementDDL,
	StatementOther,
}

// StatementKinds returns a copy of the bounded statement-kind label values.
func StatementKinds() []string {
	out := make([]string, len(statementKinds))
	copy(out, statementKinds)
	return out
}

// StatementKind classifies a parsed statement for metrics. DDL covers CREATE
// TABLE, DROP TABLE, CREATE INDEX, and DROP INDEX. Anything unknown, including
// a parse failure (where there is no statement), maps to StatementOther.
func StatementKind(stmt sqlparser.Statement) string {
	switch stmt.(type) {
	case *sqlparser.Select, sqlparser.Select:
		return StatementSelect
	case *sqlparser.Insert, sqlparser.Insert:
		return StatementInsert
	case *sqlparser.Update, sqlparser.Update:
		return StatementUpdate
	case *sqlparser.Delete, sqlparser.Delete:
		return StatementDelete
	case *sqlparser.CreateTable, sqlparser.CreateTable,
		*sqlparser.DropTable, sqlparser.DropTable,
		*sqlparser.CreateIndex, sqlparser.CreateIndex,
		*sqlparser.DropIndex, sqlparser.DropIndex:
		return StatementDDL
	default:
		return StatementOther
	}
}

// Metrics receives one observation per Engine.Execute call. kind is one of the
// bounded Statement* constants. Implementations must be safe for concurrent
// use.
type Metrics interface {
	ObserveStatement(kind string, latency time.Duration, err error)
}

// MetricsFunc adapts a function to Metrics. A nil MetricsFunc discards
// observations.
type MetricsFunc func(kind string, latency time.Duration, err error)

// ObserveStatement implements Metrics.
func (f MetricsFunc) ObserveStatement(kind string, latency time.Duration, err error) {
	if f != nil {
		f(kind, latency, err)
	}
}

// SQLMetrics is the ready-made Engine instrumentation: one aggregate SLO plus
// per-statement-kind request, error, inflight, and latency series. The kind
// label is bounded to StatementKinds().
type SQLMetrics struct {
	// SLO aggregates every statement, which is what /debug/slo reports.
	SLO *metrics.SLO
	// Kinds holds one SLO per statement kind in the same registry, with the
	// kind label attached. Unknown kinds are folded into StatementOther.
	Kinds *metrics.LabeledSLO
}

// NewSQLMetrics creates SQL engine instrumentation in reg. A nil registry
// creates a private one. cfg.Prefix defaults to "db"; the per-kind series use
// cfg.Prefix+"_statement" as their prefix.
func NewSQLMetrics(reg *metrics.Registry, cfg metrics.SLOConfig) *SQLMetrics {
	if reg == nil {
		reg = metrics.NewRegistry()
	}
	prefix := strings.TrimSuffix(cfg.Prefix, "_")
	if prefix == "" {
		prefix = metrics.DefaultPrefix
	}
	cfg.Prefix = prefix
	slo := metrics.NewSLO(reg, cfg)
	kinds := metrics.NewLabeledSLO(reg, metrics.LabeledSLOConfig{
		Prefix:   prefix + "_statement",
		Labels:   cfg.Labels,
		Buckets:  cfg.Buckets,
		Label:    "kind",
		Values:   statementKinds,
		Fallback: StatementOther,
	})
	return &SQLMetrics{SLO: slo, Kinds: kinds}
}

// ObserveStatement implements Metrics.
func (m *SQLMetrics) ObserveStatement(kind string, latency time.Duration, err error) {
	if m == nil {
		return
	}
	m.SLO.ObserveRequest(latency, err)
	m.Kinds.Observe(kind, latency, err)
}

// metricsHook wraps Metrics so the hook can be stored in an atomic pointer.
type metricsHook struct{ m Metrics }

// SetMetrics installs the instrumentation hook used by Execute. A nil hook
// disables instrumentation. SetMetrics is safe to call concurrently with
// Execute; observations already in flight finish under the previous hook.
func (e *Engine) SetMetrics(m Metrics) {
	if m == nil {
		e.instrumentation.Store(nil)
		return
	}
	e.instrumentation.Store(&metricsHook{m: m})
}
