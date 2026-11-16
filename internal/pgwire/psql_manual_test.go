package pgwire

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"distributed-db/internal/sqlengine"
)

func TestPSQLManual(t *testing.T) {
	psql := os.Getenv("PGWIRE_PSQL_BIN")
	if psql == "" {
		t.Skip("PGWIRE_PSQL_BIN not set")
	}
	engine := sqlengine.New(newFakeClient())
	srv := New(engine, slog.New(slog.NewTextHandler(io.Discard, nil)))
	addr, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	args := []string{
		"-X", "-v", "ON_ERROR_STOP=1",
		"-h", host, "-p", port, "-U", "alice", "-d", "testdb",
		"-c", "CREATE TABLE users (id INT PRIMARY KEY, name TEXT)",
		"-c", "INSERT INTO users VALUES (1, 'alice')",
		"-c", "SELECT id, name FROM users",
	}
	cmd := exec.CommandContext(ctx, psql, args...)
	cmd.Env = append(os.Environ(), "PGCONNECT_TIMEOUT=5")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err = cmd.Run()
	t.Logf("psql stdout:\n%s", out.String())
	t.Logf("psql stderr:\n%s", errb.String())
	if err != nil {
		t.Fatalf("psql failed: %v", err)
	}
	if !strings.Contains(out.String(), "alice") {
		t.Fatalf("psql output does not contain selected value: %q", out.String())
	}
	if strings.Contains(errb.String(), "ERROR:") {
		t.Fatalf("psql reported errors: %q", errb.String())
	}
}
