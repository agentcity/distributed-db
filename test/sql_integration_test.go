package test

import (
	"context"
	"testing"
	"time"

	"distributed-db/internal/kvserver"
	"distributed-db/internal/metaserver"
	"distributed-db/internal/sqlengine"
	"distributed-db/internal/v2client"
)

func TestSQLP0OnKV(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	metaAddr := v2FreeAddr(t)
	ms, err := metaserver.New(metaserver.Config{Addr: metaAddr, Logger: v2Logger()})
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	if err := ms.Start(ctx); err != nil {
		t.Fatalf("meta start: %v", err)
	}
	t.Cleanup(func() { _ = ms.Stop() })

	addr := v2FreeAddr(t)
	srv, err := kvserver.New(kvserver.Config{
		ID: addr, RaftAddr: addr, DataDir: t.TempDir(),
		ElectionTimeoutMin: 80 * time.Millisecond,
		ElectionTimeoutMax: 160 * time.Millisecond,
		HeartbeatInterval:  20 * time.Millisecond,
		CacheSize:          8 << 20, MemTableSize: 4 << 20,
		Logger: v2Logger(),
	})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Stop(sctx)
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && srv.Raft().Status().Role != "leader" {
		time.Sleep(20 * time.Millisecond)
	}
	if srv.Raft().Status().Role != "leader" {
		t.Fatal("no leader")
	}

	client, err := v2client.New(metaAddr, []string{addr})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	engine := sqlengine.New(v2client.NewSQLClient(client))

	exec := func(sql string) *sqlengine.Result {
		t.Helper()
		res, err := engine.Execute(ctx, sql)
		if err != nil {
			t.Fatalf("SQL %q: %v", sql, err)
		}
		return res
	}
	exec("CREATE TABLE users (id INT PRIMARY KEY, name TEXT)")
	exec("INSERT INTO users VALUES (1, 'alice')")
	exec("INSERT INTO users VALUES (2, 'bob')")

	res := exec("SELECT * FROM users")
	if len(res.Rows) != 2 || res.Rows[0][1] != "alice" {
		t.Fatalf("SQL select = %+v", res)
	}
	res = exec("SELECT name FROM users WHERE id = 2")
	if len(res.Rows) != 1 || res.Rows[0][0] != "bob" {
		t.Fatalf("SQL point select = %+v", res)
	}
	exec("CREATE INDEX idx_name ON users (name)")
	res = exec("SELECT id FROM users WHERE name = 'bob'")
	if len(res.Rows) != 1 || res.Rows[0][0] != int64(2) {
		t.Fatalf("SQL index select = %+v", res)
	}
	exec("UPDATE users SET name = 'carol' WHERE id = 1")
	res = exec("SELECT name FROM users WHERE id = 1")
	if len(res.Rows) != 1 || res.Rows[0][0] != "carol" {
		t.Fatalf("SQL update = %+v", res)
	}
	res = exec("SELECT id FROM users WHERE name = 'carol'")
	if len(res.Rows) != 1 || res.Rows[0][0] != int64(1) {
		t.Fatalf("SQL index after update = %+v", res)
	}
	exec("DELETE FROM users WHERE id = 2")
	res = exec("SELECT * FROM users")
	if len(res.Rows) != 1 {
		t.Fatalf("SQL delete = %+v", res)
	}
	exec("DROP TABLE users")
	if _, err := engine.Execute(ctx, "SELECT * FROM users"); err == nil {
		t.Fatal("SQL select after drop succeeded")
	}
}
