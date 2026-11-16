package sqlhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"distributed-db/internal/metrics"
	"distributed-db/internal/sqlengine"
)

type nopClient struct{}

func (nopClient) Begin(context.Context) (sqlengine.Txn, error) { return nopTxn{}, nil }

type nopTxn struct{}

func (nopTxn) Get(context.Context, []byte) ([]byte, error)                       { return nil, sqlengine.ErrNotFound }
func (nopTxn) Set([]byte, []byte)                                                {}
func (nopTxn) Delete([]byte)                                                     {}
func (nopTxn) Scan(context.Context, []byte, []byte, int) ([]sqlengine.KV, error) { return nil, nil }
func (nopTxn) Commit(context.Context) error                                      { return nil }
func (nopTxn) Rollback(context.Context) error                                    { return nil }

func TestHandlerBasics(t *testing.T) {
	h := New(sqlengine.New(nopClient{}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("health: %v %v", err, resp)
	}
	_ = resp.Body.Close()

	resp, err = http.Post(srv.URL+"/v1/sql", "application/json", bytes.NewReader([]byte("{")))
	if err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid JSON status = %v, %v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()

	body, _ := json.Marshal(map[string]string{"sql": "SELECT * FROM missing"})
	resp, err = http.Post(srv.URL+"/v1/sql", "application/json", bytes.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing table status = %v, %v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()
}

func newMetricsTestServer(t *testing.T) (*httptest.Server, *metrics.Registry, *sqlengine.SQLMetrics) {
	t.Helper()
	engine := sqlengine.New(nopClient{})
	reg := metrics.NewRegistry()
	sqlm := sqlengine.NewSQLMetrics(reg, metrics.SLOConfig{Prefix: "sql"})
	engine.SetMetrics(sqlm)
	h := NewWithConfig(Config{Engine: engine, Metrics: reg, SLO: sqlm.SLO})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, reg, sqlm
}

func postSQL(t *testing.T, srv *httptest.Server, sql string) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]string{"sql": sql})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(srv.URL+"/v1/sql", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/sql: %v", err)
	}
	return resp
}

func TestMetricsEndpointsAfterSQL(t *testing.T) {
	srv, _, sqlm := newMetricsTestServer(t)

	resp := postSQL(t, srv, "CREATE TABLE metrics_test (id INT PRIMARY KEY)")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CREATE status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp = postSQL(t, srv, "SELECT * FROM missing WHERE id = 424242")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("failing SELECT status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	if got := sqlm.SLO.Requests.Value(); got != 2 {
		t.Fatalf("SQL SLO requests = %d, want 2", got)
	}
	if got := sqlm.SLO.Errors.Value(); got != 1 {
		t.Fatalf("SQL SLO errors = %d, want 1", got)
	}

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		"sql_requests_total 2",
		"sql_errors_total 1",
		`sql_statement_requests_total{kind="DDL"} 1`,
		`sql_statement_errors_total{kind="SELECT"} 1`,
		`sql_statement_request_duration_seconds_count{kind="SELECT"} 1`,
		"# TYPE sql_requests_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/metrics missing %q:\n%s", want, body)
		}
	}
	for _, leaked := range []string{"424242", "metrics_test", "missing"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("/metrics leaked SQL text %q:\n%s", leaked, body)
		}
	}

	resp, err = http.Get(srv.URL + "/debug/slo")
	if err != nil {
		t.Fatalf("GET /debug/slo: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/debug/slo status = %d, want 200", resp.StatusCode)
	}
	var report metrics.Report
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		t.Fatalf("decode /debug/slo: %v", err)
	}
	if report.Requests != 2 || report.Errors != 1 || report.ErrorRate != 0.5 {
		t.Fatalf("report = %+v, want requests=2 errors=1 rate=0.5", report)
	}

	resp, err = http.Get(srv.URL + "/debug/slo?format=text")
	if err != nil {
		t.Fatalf("GET /debug/slo?format=text: %v", err)
	}
	defer resp.Body.Close()
	raw, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "requests: 2") {
		t.Fatalf("/debug/slo text = %q", raw)
	}
}

func TestMetricsEndpointsWithoutConfiguration(t *testing.T) {
	srv := httptest.NewServer(New(sqlengine.New(nopClient{})))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/debug/slo")
	if err != nil {
		t.Fatalf("GET /debug/slo: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/debug/slo status = %d, want 404", resp.StatusCode)
	}
}
