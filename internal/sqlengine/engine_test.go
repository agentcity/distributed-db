package sqlengine

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"testing"
)

type fakeClient struct {
	data map[string][]byte
}

func newFakeClient() *fakeClient { return &fakeClient{data: make(map[string][]byte)} }

func (c *fakeClient) Begin(context.Context) (Txn, error) {
	staged := make(map[string][]byte, len(c.data))
	for k, v := range c.data {
		staged[k] = append([]byte(nil), v...)
	}
	return &fakeTxn{base: c, data: staged}, nil
}

type fakeTxn struct {
	base *fakeClient
	data map[string][]byte
	done bool
}

func (t *fakeTxn) Get(_ context.Context, key []byte) ([]byte, error) {
	v, ok := t.data[string(key)]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

func (t *fakeTxn) Set(key, value []byte) { t.data[string(key)] = append([]byte(nil), value...) }
func (t *fakeTxn) Delete(key []byte)     { delete(t.data, string(key)) }

func (t *fakeTxn) Scan(_ context.Context, start, end []byte, limit int) ([]KV, error) {
	keys := make([]string, 0, len(t.data))
	for k := range t.data {
		kb := []byte(k)
		if bytes.Compare(kb, start) >= 0 && (end == nil || bytes.Compare(kb, end) < 0) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	out := make([]KV, 0, len(keys))
	for _, k := range keys {
		out = append(out, KV{Key: []byte(k), Value: append([]byte(nil), t.data[k]...)})
	}
	return out, nil
}

func (t *fakeTxn) Commit(context.Context) error {
	t.base.data = t.data
	t.done = true
	return nil
}

func (t *fakeTxn) Rollback(context.Context) error {
	t.done = true
	return nil
}

var _ Client = (*fakeClient)(nil)
var _ Txn = (*fakeTxn)(nil)

func exec(t *testing.T, e *Engine, sql string) *Result {
	t.Helper()
	res, err := e.Execute(context.Background(), sql)
	if err != nil {
		t.Fatalf("Execute(%q): %v", sql, err)
	}
	return res
}

func TestSQLP0Lifecycle(t *testing.T) {
	e := New(newFakeClient())
	ctx := context.Background()

	exec(t, e, "CREATE TABLE users (id INT PRIMARY KEY, name TEXT)")
	exec(t, e, "INSERT INTO users VALUES (1, 'alice')")
	exec(t, e, "INSERT INTO users VALUES (2, 'bob')")

	res := exec(t, e, "SELECT * FROM users")
	if len(res.Rows) != 2 || res.Columns[0] != "id" || res.Columns[1] != "name" {
		t.Fatalf("SELECT * = %+v", res)
	}
	if res.Rows[0][0] != int64(1) || res.Rows[0][1] != "alice" {
		t.Fatalf("row 0 = %#v", res.Rows[0])
	}

	res = exec(t, e, "SELECT name FROM users WHERE id = 2")
	if len(res.Rows) != 1 || res.Rows[0][0] != "bob" {
		t.Fatalf("point select = %+v", res)
	}

	res = exec(t, e, "SELECT id, name FROM users LIMIT 1")
	if len(res.Rows) != 1 {
		t.Fatalf("limit = %+v", res)
	}

	exec(t, e, "UPDATE users SET name = 'carol' WHERE id = 1")
	res = exec(t, e, "SELECT name FROM users WHERE id = 1")
	if len(res.Rows) != 1 || res.Rows[0][0] != "carol" {
		t.Fatalf("update = %+v", res)
	}

	exec(t, e, "DELETE FROM users WHERE id = 2")
	res = exec(t, e, "SELECT * FROM users")
	if len(res.Rows) != 1 {
		t.Fatalf("delete = %+v", res)
	}

	exec(t, e, "DROP TABLE users")
	if _, err := e.Execute(ctx, "SELECT * FROM users"); !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("select after drop = %v", err)
	}
}

func TestSQLP0Errors(t *testing.T) {
	e := New(newFakeClient())
	exec(t, e, "CREATE TABLE t (id INT PRIMARY KEY, v TEXT)")
	exec(t, e, "INSERT INTO t VALUES (1, 'a')")

	cases := []struct {
		sql string
		err error
	}{
		{"CREATE TABLE t (id INT PRIMARY KEY)", ErrTableExists},
		{"INSERT INTO t VALUES (1, 'b')", ErrDuplicateKey},
		{"INSERT INTO missing VALUES (1, 'b')", ErrTableNotFound},
		{"SELECT nope FROM t", ErrBadColumn},
		{"SELECT * FROM t WHERE v = 'a'", ErrUnsupported},
		{"UPDATE t SET v = 'b'", ErrUnsupported},
		{"DELETE FROM t", ErrUnsupported},
	}
	for _, tc := range cases {
		if _, err := e.Execute(context.Background(), tc.sql); !errors.Is(err, tc.err) {
			t.Errorf("%q: err = %v, want %v", tc.sql, err, tc.err)
		}
	}
}

func TestSQLTextPrimaryKeyOrder(t *testing.T) {
	e := New(newFakeClient())
	exec(t, e, "CREATE TABLE docs (name TEXT PRIMARY KEY, body TEXT)")
	exec(t, e, "INSERT INTO docs VALUES ('a', 'one')")
	exec(t, e, "INSERT INTO docs VALUES ('b', 'two')")
	exec(t, e, "INSERT INTO docs VALUES ('c', 'three')")
	res := exec(t, e, "SELECT name FROM docs")
	if len(res.Rows) != 3 || res.Rows[0][0] != "a" || res.Rows[2][0] != "c" {
		t.Fatalf("text pk order = %+v", res)
	}
}

func TestSQLSecondaryIndex(t *testing.T) {
	e := New(newFakeClient())
	exec(t, e, "CREATE TABLE users (id INT PRIMARY KEY, name TEXT, city TEXT)")
	exec(t, e, "INSERT INTO users VALUES (1, 'alice', 'msk')")
	exec(t, e, "INSERT INTO users VALUES (2, 'bob', 'spb')")
	exec(t, e, "INSERT INTO users VALUES (3, 'bob', 'msk')")
	exec(t, e, "CREATE INDEX idx_name ON users (name)")

	res := exec(t, e, "SELECT id FROM users WHERE name = 'bob'")
	if len(res.Rows) != 2 {
		t.Fatalf("index select = %+v", res)
	}

	exec(t, e, "INSERT INTO users VALUES (4, 'bob', 'kazan')")
	res = exec(t, e, "SELECT id FROM users WHERE name = 'bob'")
	if len(res.Rows) != 3 {
		t.Fatalf("index after insert = %+v", res)
	}

	exec(t, e, "UPDATE users SET name = 'carol' WHERE id = 2")
	res = exec(t, e, "SELECT id FROM users WHERE name = 'bob'")
	if len(res.Rows) != 2 {
		t.Fatalf("index after update = %+v", res)
	}
	res = exec(t, e, "SELECT id FROM users WHERE name = 'carol'")
	if len(res.Rows) != 1 || res.Rows[0][0] != int64(2) {
		t.Fatalf("new index value = %+v", res)
	}

	exec(t, e, "DELETE FROM users WHERE id = 4")
	res = exec(t, e, "SELECT id FROM users WHERE name = 'bob'")
	if len(res.Rows) != 1 {
		t.Fatalf("index after delete = %+v", res)
	}

	if _, err := e.Execute(context.Background(), "CREATE INDEX idx_name ON users (city)"); !errors.Is(err, ErrIndexExists) {
		t.Fatalf("duplicate index = %v", err)
	}
	if _, err := e.Execute(context.Background(), "CREATE INDEX idx_pk ON users (id)"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("index on PK = %v", err)
	}

	exec(t, e, "DROP INDEX idx_name ON users")
	if _, err := e.Execute(context.Background(), "SELECT id FROM users WHERE name = 'bob'"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("select after drop index = %v", err)
	}
}
