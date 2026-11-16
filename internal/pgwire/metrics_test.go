package pgwire

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"distributed-db/internal/metrics"
	"distributed-db/internal/sqlengine"
)

func startMetricsTestServer(t *testing.T) (*Server, string, *metrics.SLO, *metrics.Registry) {
	t.Helper()
	engine := sqlengine.New(newFakeClient())
	srv := New(engine, slog.New(slog.NewTextHandler(io.Discard, nil)))
	reg := metrics.NewRegistry()
	slo := metrics.NewSLO(reg, metrics.SLOConfig{Prefix: "pgwire"})
	srv.SetMetrics(reg, slo)
	addr, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	return srv, addr, slo, reg
}

func TestMetricsTracksConnectionsAndQueries(t *testing.T) {
	_, addr, slo, reg := startMetricsTestServer(t)
	c := dialTestConn(t, addr)
	c.startup(map[string]string{"user": "alice", "database": "testdb"})

	msgs := c.query("CREATE TABLE metrics_users (id INT PRIMARY KEY, name TEXT)")
	c.assertNoError(msgs)

	msgs = c.query("SELECT * FROM metrics_users WHERE id = 999")
	c.assertNoError(msgs)

	// A failing statement counts as one request and one error.
	msgs = c.query("SELECT * FROM missing_table WHERE id = 12345")
	if msgOfType(msgs, msgErrorResponse) == nil {
		t.Fatalf("expected ErrorResponse for missing table, got %v", msgTypes(msgs))
	}

	if got := slo.Requests.Value(); got != 3 {
		t.Fatalf("query requests = %d, want 3", got)
	}
	if got := slo.Errors.Value(); got != 1 {
		t.Fatalf("query errors = %d, want 1", got)
	}

	var buf strings.Builder
	if err := reg.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"pgwire_connections_total 1",
		"pgwire_connections_active 1",
		"pgwire_requests_total 3",
		"pgwire_errors_total 1",
		"pgwire_request_duration_seconds_count 3",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing %q:\n%s", want, out)
		}
	}

	// Terminate the connection; the active gauge must return to zero.
	c.extSend(msgTerminate, nil)
	c.conn.Close()
	waitForGauge(t, reg, "pgwire_connections_active", 0)
}

func TestMetricsTracksExtendedExecuteOnly(t *testing.T) {
	_, addr, slo, _ := startMetricsTestServer(t)
	c := dialTestConn(t, addr)
	c.startup(map[string]string{"user": "alice", "database": "testdb"})
	c.assertNoError(c.query("CREATE TABLE users (id INT PRIMARY KEY, name TEXT)"))
	c.assertNoError(c.query("INSERT INTO users VALUES (1, 'alice')"))

	before := slo.Requests.Value()

	c.extSend(msgParse, extParseMessage("s1", "SELECT id, name FROM users WHERE id = $1", []uint32{20}))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgBind, extBindMessage("p1", "s1", []int16{0}, []extBindParam{extTextParam("1")}, nil))
	c.extExpectMsg(msgBindComplete)
	c.extSend(msgExecute, extExecuteMessage("p1", 0))
	if row := parseDataRow(t, c.extExpectMsg(msgDataRow).body); len(row) != 2 || row[0] != "1" || row[1] != "alice" {
		t.Fatalf("row = %v, want [1 alice]", row)
	}
	assertCommandTag(t, []wireMsg{c.extExpectMsg(msgCommandComplete)}, "SELECT 1")
	c.extSync()

	if got := slo.Requests.Value() - before; got != 1 {
		t.Fatalf("extended query requests = %d, want 1", got)
	}
	if got := slo.Errors.Value(); got != 0 {
		t.Fatalf("extended query errors = %d, want 0", got)
	}
}

func waitForGauge(t *testing.T, reg *metrics.Registry, name string, want float64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		snap := reg.Snapshot()
		found := false
		for _, m := range snap.Metrics {
			if m.Name == name {
				found = true
				if m.Value == want {
					return
				}
			}
		}
		if !found {
			t.Fatalf("%s not found in registry", name)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not reach %v", name, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
