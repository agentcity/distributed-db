package sqlengine

import (
	"context"
	"strings"
	"sync"
	"testing"

	"distributed-db/internal/metrics"
)

func TestMetricsRecordsStatementsAndErrors(t *testing.T) {
	engine := New(newFakeClient())
	reg := metrics.NewRegistry()
	sqlm := NewSQLMetrics(reg, metrics.SLOConfig{Prefix: "sql"})
	engine.SetMetrics(sqlm)

	ctx := context.Background()
	for _, sql := range []string{
		"CREATE TABLE users (id INT PRIMARY KEY, age INT)",
		"INSERT INTO users VALUES (1, 30)",
		"CREATE INDEX idx_age ON users (age)",
		"SELECT * FROM users WHERE age = 30",
		"UPDATE users SET age = 31 WHERE id = 1",
		"DELETE FROM users WHERE id = 1",
		"DROP INDEX idx_age ON users",
		"DROP TABLE users",
	} {
		if _, err := engine.Execute(ctx, sql); err != nil {
			t.Fatalf("Execute(%q): %v", sql, err)
		}
	}
	if _, err := engine.Execute(ctx, "SELEC definitely not sql"); err == nil {
		t.Fatal("invalid statement succeeded")
	}

	if got := sqlm.SLO.Requests.Value(); got != 9 {
		t.Fatalf("total requests = %d, want 9", got)
	}
	if got := sqlm.SLO.Errors.Value(); got != 1 {
		t.Fatalf("total errors = %d, want 1", got)
	}
	wantByKind := map[string]uint64{
		StatementDDL:    4,
		StatementInsert: 1,
		StatementSelect: 1,
		StatementUpdate: 1,
		StatementDelete: 1,
		StatementOther:  1,
	}
	for kind, want := range wantByKind {
		slo := sqlm.Kinds.For(kind)
		if got := slo.Requests.Value(); got != want {
			t.Errorf("%s requests = %d, want %d", kind, got, want)
		}
		if got := slo.Latency.Count(); got != want {
			t.Errorf("%s latency count = %d, want %d", kind, got, want)
		}
		if got := slo.Errors.Value(); got != 0 {
			// Only OTHER carries the parse error.
			if kind != StatementOther || got != 1 {
				t.Errorf("%s errors = %d, want 0", kind, got)
			}
		}
	}

	// Statement kinds are a closed set: SQL text and literals never become
	// label values.
	var buf strings.Builder
	if err := reg.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	out := buf.String()
	for _, leaked := range []string{"users", "idx_age", "definitely", "not sql"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("metrics exposition leaked SQL text %q:\n%s", leaked, out)
		}
	}
	if !strings.Contains(out, `sql_statement_requests_total{kind="DDL"} 4`) {
		t.Fatalf("missing bounded DDL series:\n%s", out)
	}
}

func TestMetricsCanBeDisabledAndReplaced(t *testing.T) {
	engine := New(newFakeClient())
	reg := metrics.NewRegistry()
	sqlm := NewSQLMetrics(reg, metrics.SLOConfig{Prefix: "sql"})
	engine.SetMetrics(sqlm)
	if _, err := engine.Execute(context.Background(), "CREATE TABLE t (id INT PRIMARY KEY)"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := sqlm.SLO.Requests.Value(); got != 1 {
		t.Fatalf("requests after first execute = %d, want 1", got)
	}

	engine.SetMetrics(nil)
	if _, err := engine.Execute(context.Background(), "SELEC broken"); err == nil {
		t.Fatal("invalid statement succeeded")
	}
	if got := sqlm.SLO.Requests.Value(); got != 1 {
		t.Fatalf("requests after disabling = %d, want 1", got)
	}

	engine.SetMetrics(sqlm)
	if _, err := engine.Execute(context.Background(), "SELEC broken"); err == nil {
		t.Fatal("invalid statement succeeded")
	}
	if got := sqlm.SLO.Requests.Value(); got != 2 || sqlm.SLO.Errors.Value() != 1 {
		t.Fatalf("requests/errors after re-enabling = %d/%d, want 2/1", sqlm.SLO.Requests.Value(), sqlm.SLO.Errors.Value())
	}
}

func TestSetMetricsConcurrentWithExecute(t *testing.T) {
	engine := New(newFakeClient())
	sqlm := NewSQLMetrics(metrics.NewRegistry(), metrics.SLOConfig{Prefix: "sql"})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			engine.SetMetrics(sqlm)
			engine.SetMetrics(nil)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_, _ = engine.Execute(context.Background(), "SELECT * FROM missing")
		}
	}()
	wg.Wait()
}

func TestStatementKindBounded(t *testing.T) {
	kinds := StatementKinds()
	if len(kinds) != 6 {
		t.Fatalf("StatementKinds() = %v, want 6 values", kinds)
	}
	seen := make(map[string]struct{}, len(kinds))
	for _, kind := range kinds {
		seen[kind] = struct{}{}
	}
	if _, ok := seen[StatementOther]; !ok {
		t.Fatal("StatementKinds() is missing OTHER")
	}
	if got := StatementKind(nil); got != StatementOther {
		t.Fatalf("StatementKind(nil) = %q, want OTHER", got)
	}
}

var (
	_ Metrics = (*SQLMetrics)(nil)
	_ Metrics = (*metrics.SLO)(nil)
)

func TestSetMetricsAcceptsPlainSLO(t *testing.T) {
	engine := New(newFakeClient())
	slo := metrics.NewSLO(metrics.NewRegistry(), metrics.SLOConfig{Prefix: "sql"})
	engine.SetMetrics(slo)
	if _, err := engine.Execute(context.Background(), "CREATE TABLE t (id INT PRIMARY KEY)"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := engine.Execute(context.Background(), "SELEC broken"); err == nil {
		t.Fatal("invalid statement succeeded")
	}
	if got := slo.Requests.Value(); got != 2 {
		t.Fatalf("plain SLO requests = %d, want 2", got)
	}
	if got := slo.Errors.Value(); got != 1 {
		t.Fatalf("plain SLO errors = %d, want 1", got)
	}
}
