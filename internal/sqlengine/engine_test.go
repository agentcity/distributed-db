package sqlengine

import (
	"bytes"
	"context"
	"errors"
	"reflect"
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

func TestSQLMultiRowUpdateDeleteByIndex(t *testing.T) {
	e := New(newFakeClient())
	exec(t, e, "CREATE TABLE users (id INT PRIMARY KEY, name TEXT, city TEXT)")
	exec(t, e, "INSERT INTO users VALUES (1, 'a', 'msk')")
	exec(t, e, "INSERT INTO users VALUES (2, 'b', 'msk')")
	exec(t, e, "INSERT INTO users VALUES (3, 'c', 'spb')")
	exec(t, e, "CREATE INDEX idx_city ON users (city)")

	res := exec(t, e, "UPDATE users SET name = 'moscow' WHERE city = 'msk'")
	if res.RowsAffected != 2 {
		t.Fatalf("update affected = %d, want 2", res.RowsAffected)
	}
	res = exec(t, e, "SELECT name FROM users WHERE city = 'msk'")
	if len(res.Rows) != 2 || res.Rows[0][0] != "moscow" || res.Rows[1][0] != "moscow" {
		t.Fatalf("rows after update = %+v", res)
	}

	res = exec(t, e, "DELETE FROM users WHERE city = 'spb'")
	if res.RowsAffected != 1 {
		t.Fatalf("delete affected = %d, want 1", res.RowsAffected)
	}
	res = exec(t, e, "SELECT * FROM users")
	if len(res.Rows) != 2 {
		t.Fatalf("rows after delete = %+v", res)
	}
	res = exec(t, e, "DELETE FROM users WHERE city = 'spb'")
	if res.RowsAffected != 0 {
		t.Fatalf("second delete affected = %d, want 0", res.RowsAffected)
	}
}

func TestSQLAggregates(t *testing.T) {
	e := New(newFakeClient())
	exec(t, e, "CREATE TABLE t (id INT PRIMARY KEY, name TEXT, amount INT)")
	exec(t, e, "INSERT INTO t VALUES (1, 'a', 10)")
	exec(t, e, "INSERT INTO t VALUES (2, 'b', 20)")
	exec(t, e, "INSERT INTO t VALUES (3, 'c', 30)")

	cases := []struct {
		sql  string
		want any
	}{
		{"SELECT COUNT(*) FROM t", int64(3)},
		{"SELECT COUNT(name) FROM t", int64(3)},
		{"SELECT SUM(amount) FROM t", int64(60)},
		{"SELECT AVG(amount) FROM t", int64(20)},
		{"SELECT MIN(amount) FROM t", int64(10)},
		{"SELECT MAX(amount) FROM t", int64(30)},
		{"SELECT MIN(name) FROM t", "a"},
		{"SELECT MAX(name) FROM t", "c"},
		{"SELECT COUNT(*) FROM t WHERE id = 2", int64(1)},
	}
	for _, tc := range cases {
		res, err := e.Execute(context.Background(), tc.sql)
		if err != nil {
			t.Fatalf("%q: %v", tc.sql, err)
		}
		if len(res.Rows) != 1 || len(res.Rows[0]) != 1 || res.Rows[0][0] != tc.want {
			t.Fatalf("%q = %+v, want %v", tc.sql, res.Rows, tc.want)
		}
	}
}

func TestSQLGroupBy(t *testing.T) {
	e := New(newFakeClient())
	ctx := context.Background()

	exec(t, e, "CREATE TABLE sales (id INT PRIMARY KEY, region TEXT, amount INT)")
	exec(t, e, "INSERT INTO sales VALUES (1, 'eu', 10)")
	exec(t, e, "INSERT INTO sales VALUES (2, 'us', 20)")
	exec(t, e, "INSERT INTO sales VALUES (3, 'eu', 30)")
	exec(t, e, "INSERT INTO sales VALUES (4, 'us', 40)")
	exec(t, e, "INSERT INTO sales VALUES (5, 'ap', 5)")

	cases := []struct {
		name string
		sql  string
		cols []string
		rows [][]any
	}{
		{
			name: "text group count star",
			sql:  "SELECT COUNT(*) FROM sales GROUP BY region",
			cols: []string{"region", "count"},
			rows: [][]any{{"ap", int64(1)}, {"eu", int64(2)}, {"us", int64(2)}},
		},
		{
			name: "text group count column",
			sql:  "SELECT COUNT(amount) FROM sales GROUP BY region",
			cols: []string{"region", "count"},
			rows: [][]any{{"ap", int64(1)}, {"eu", int64(2)}, {"us", int64(2)}},
		},
		{
			name: "text group sum",
			sql:  "SELECT SUM(amount) FROM sales GROUP BY region",
			cols: []string{"region", "sum"},
			rows: [][]any{{"ap", int64(5)}, {"eu", int64(40)}, {"us", int64(60)}},
		},
		{
			name: "text group avg",
			sql:  "SELECT AVG(amount) FROM sales GROUP BY region",
			cols: []string{"region", "avg"},
			rows: [][]any{{"ap", int64(5)}, {"eu", int64(20)}, {"us", int64(30)}},
		},
		{
			name: "text group min",
			sql:  "SELECT MIN(amount) FROM sales GROUP BY region",
			cols: []string{"region", "min"},
			rows: [][]any{{"ap", int64(5)}, {"eu", int64(10)}, {"us", int64(20)}},
		},
		{
			name: "text group max",
			sql:  "SELECT MAX(amount) FROM sales GROUP BY region",
			cols: []string{"region", "max"},
			rows: [][]any{{"ap", int64(5)}, {"eu", int64(30)}, {"us", int64(40)}},
		},
		{
			name: "where filters before grouping",
			sql:  "SELECT COUNT(*) FROM sales WHERE id = 3 GROUP BY region",
			cols: []string{"region", "count"},
			rows: [][]any{{"eu", int64(1)}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := e.Execute(ctx, tc.sql)
			if err != nil {
				t.Fatalf("%q: %v", tc.sql, err)
			}
			if !reflect.DeepEqual(res.Columns, tc.cols) {
				t.Fatalf("%q columns = %v, want %v", tc.sql, res.Columns, tc.cols)
			}
			if !reflect.DeepEqual(res.Rows, tc.rows) {
				t.Fatalf("%q rows = %v, want %v", tc.sql, res.Rows, tc.rows)
			}
		})
	}

	exec(t, e, "CREATE TABLE metrics (id INT PRIMARY KEY, bucket INT, value INT)")
	exec(t, e, "INSERT INTO metrics VALUES (1, 2, 100)")
	exec(t, e, "INSERT INTO metrics VALUES (2, 1, 7)")
	exec(t, e, "INSERT INTO metrics VALUES (3, 2, 5)")
	exec(t, e, "INSERT INTO metrics VALUES (4, 1, 3)")

	intCases := []struct {
		name string
		sql  string
		cols []string
		rows [][]any
	}{
		{
			name: "int group count",
			sql:  "SELECT COUNT(*) FROM metrics GROUP BY bucket",
			cols: []string{"bucket", "count"},
			rows: [][]any{{int64(1), int64(2)}, {int64(2), int64(2)}},
		},
		{
			name: "int group sum sorted by key",
			sql:  "SELECT SUM(value) FROM metrics GROUP BY bucket",
			cols: []string{"bucket", "sum"},
			rows: [][]any{{int64(1), int64(10)}, {int64(2), int64(105)}},
		},
		{
			name: "int group avg",
			sql:  "SELECT AVG(value) FROM metrics GROUP BY bucket",
			cols: []string{"bucket", "avg"},
			rows: [][]any{{int64(1), int64(5)}, {int64(2), int64(52)}},
		},
		{
			name: "int group min",
			sql:  "SELECT MIN(value) FROM metrics GROUP BY bucket",
			cols: []string{"bucket", "min"},
			rows: [][]any{{int64(1), int64(3)}, {int64(2), int64(5)}},
		},
		{
			name: "int group max",
			sql:  "SELECT MAX(value) FROM metrics GROUP BY bucket",
			cols: []string{"bucket", "max"},
			rows: [][]any{{int64(1), int64(7)}, {int64(2), int64(100)}},
		},
	}
	for _, tc := range intCases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := e.Execute(ctx, tc.sql)
			if err != nil {
				t.Fatalf("%q: %v", tc.sql, err)
			}
			if !reflect.DeepEqual(res.Columns, tc.cols) {
				t.Fatalf("%q columns = %v, want %v", tc.sql, res.Columns, tc.cols)
			}
			if !reflect.DeepEqual(res.Rows, tc.rows) {
				t.Fatalf("%q rows = %v, want %v", tc.sql, res.Rows, tc.rows)
			}
		})
	}

	exec(t, e, "CREATE TABLE empty_sales (id INT PRIMARY KEY, region TEXT, amount INT)")
	for _, sql := range []string{
		"SELECT COUNT(*) FROM empty_sales GROUP BY region",
		"SELECT SUM(amount) FROM empty_sales GROUP BY region",
		"SELECT MIN(region) FROM empty_sales GROUP BY region",
	} {
		res, err := e.Execute(ctx, sql)
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		if len(res.Columns) != 2 || len(res.Rows) != 0 {
			t.Fatalf("%q = %+v, want two columns and no rows", sql, res)
		}
	}

	errorCases := []struct {
		sql string
		err error
	}{
		{"SELECT COUNT(*) FROM sales GROUP BY nope", ErrBadColumn},
		{"SELECT SUM(nope) FROM sales GROUP BY region", ErrBadColumn},
		{"SELECT SUM(region) FROM sales GROUP BY region", ErrBadColumn},
	}
	for _, tc := range errorCases {
		if _, err := e.Execute(ctx, tc.sql); !errors.Is(err, tc.err) {
			t.Errorf("%q: err = %v, want %v", tc.sql, err, tc.err)
		}
	}
}

// joinFixture builds the users/orders data set used by the join tests.
//
//	users:  (1, alice) (2, bob) (3, carol)
//	orders: (10, user 1, 100) (11, user 1, 150) (12, user 2, 200) (13, user 4, 50)
func joinFixture(t *testing.T) *Engine {
	t.Helper()
	e := New(newFakeClient())
	exec(t, e, "CREATE TABLE users (id INT PRIMARY KEY, name TEXT)")
	exec(t, e, "INSERT INTO users VALUES (1, 'alice')")
	exec(t, e, "INSERT INTO users VALUES (2, 'bob')")
	exec(t, e, "INSERT INTO users VALUES (3, 'carol')")
	exec(t, e, "CREATE TABLE orders (id INT PRIMARY KEY, user_id INT, total INT)")
	exec(t, e, "INSERT INTO orders VALUES (10, 1, 100)")
	exec(t, e, "INSERT INTO orders VALUES (11, 1, 150)")
	exec(t, e, "INSERT INTO orders VALUES (12, 2, 200)")
	exec(t, e, "INSERT INTO orders VALUES (13, 4, 50)")
	exec(t, e, "CREATE TABLE empty_orders (id INT PRIMARY KEY, user_id INT)")
	exec(t, e, "CREATE TABLE codes (id INT PRIMARY KEY, code TEXT)")
	exec(t, e, "CREATE TABLE nicknames (id INT PRIMARY KEY, nick TEXT)")
	exec(t, e, "INSERT INTO nicknames VALUES (1, 'alice')")
	exec(t, e, "INSERT INTO nicknames VALUES (2, 'bob')")
	return e
}

func TestSQLJoin(t *testing.T) {
	e := joinFixture(t)
	cases := []struct {
		name string
		sql  string
		cols []string
		rows [][]any
	}{
		{
			name: "basic join without aliases",
			sql:  "SELECT users.name, orders.total FROM users JOIN orders ON users.id = orders.user_id",
			cols: []string{"name", "total"},
			rows: [][]any{{"alice", int64(100)}, {"alice", int64(150)}, {"bob", int64(200)}},
		},
		{
			name: "aliases",
			sql:  "SELECT u.name, o.total FROM users AS u JOIN orders AS o ON u.id = o.user_id",
			cols: []string{"name", "total"},
			rows: [][]any{{"alice", int64(100)}, {"alice", int64(150)}, {"bob", int64(200)}},
		},
		{
			name: "inner join keyword",
			sql:  "SELECT u.name, o.total FROM users AS u INNER JOIN orders AS o ON u.id = o.user_id",
			cols: []string{"name", "total"},
			rows: [][]any{{"alice", int64(100)}, {"alice", int64(150)}, {"bob", int64(200)}},
		},
		{
			name: "select star qualified left then right",
			sql:  "SELECT * FROM users AS u JOIN orders AS o ON u.id = o.user_id",
			cols: []string{"u.id", "u.name", "o.id", "o.user_id", "o.total"},
			rows: [][]any{
				{int64(1), "alice", int64(10), int64(1), int64(100)},
				{int64(1), "alice", int64(11), int64(1), int64(150)},
				{int64(2), "bob", int64(12), int64(2), int64(200)},
			},
		},
		{
			name: "select star without aliases uses table names",
			sql:  "SELECT * FROM users JOIN orders ON users.id = orders.user_id",
			cols: []string{"users.id", "users.name", "orders.id", "orders.user_id", "orders.total"},
			rows: [][]any{
				{int64(1), "alice", int64(10), int64(1), int64(100)},
				{int64(1), "alice", int64(11), int64(1), int64(150)},
				{int64(2), "bob", int64(12), int64(2), int64(200)},
			},
		},
		{
			name: "explicit unqualified columns",
			sql:  "SELECT name, total FROM users JOIN orders ON users.id = orders.user_id",
			cols: []string{"name", "total"},
			rows: [][]any{{"alice", int64(100)}, {"alice", int64(150)}, {"bob", int64(200)}},
		},
		{
			name: "where qualified right column",
			sql:  "SELECT u.name FROM users AS u JOIN orders AS o ON u.id = o.user_id WHERE o.total = 150",
			cols: []string{"name"},
			rows: [][]any{{"alice"}},
		},
		{
			name: "where unqualified unique column",
			sql:  "SELECT u.name FROM users AS u JOIN orders AS o ON u.id = o.user_id WHERE total = 200",
			cols: []string{"name"},
			rows: [][]any{{"bob"}},
		},
		{
			name: "where qualified left column",
			sql:  "SELECT u.name, o.total FROM users AS u JOIN orders AS o ON u.id = o.user_id WHERE u.id = 1",
			cols: []string{"name", "total"},
			rows: [][]any{{"alice", int64(100)}, {"alice", int64(150)}},
		},
		{
			name: "where no matches",
			sql:  "SELECT u.name FROM users AS u JOIN orders AS o ON u.id = o.user_id WHERE o.total = 999",
			cols: []string{"name"},
			rows: [][]any{},
		},
		{
			name: "limit after join",
			sql:  "SELECT u.name FROM users AS u JOIN orders AS o ON u.id = o.user_id LIMIT 2",
			cols: []string{"name"},
			rows: [][]any{{"alice"}, {"alice"}},
		},
		{
			name: "join against empty table",
			sql:  "SELECT * FROM users AS u JOIN empty_orders AS e ON u.id = e.user_id",
			cols: []string{"u.id", "u.name", "e.id", "e.user_id"},
			rows: [][]any{},
		},
		{
			name: "self join with aliases",
			sql:  "SELECT a.name, b.name FROM users AS a JOIN users AS b ON a.id = b.id",
			cols: []string{"name", "name"},
			rows: [][]any{{"alice", "alice"}, {"bob", "bob"}, {"carol", "carol"}},
		},
		{
			name: "join on text columns",
			sql:  "SELECT u.name, n.id FROM users AS u JOIN nicknames AS n ON u.name = n.nick",
			cols: []string{"name", "id"},
			rows: [][]any{{"alice", int64(1)}, {"bob", int64(2)}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := e.Execute(context.Background(), tc.sql)
			if err != nil {
				t.Fatalf("%q: %v", tc.sql, err)
			}
			if !reflect.DeepEqual(res.Columns, tc.cols) {
				t.Fatalf("%q columns = %v, want %v", tc.sql, res.Columns, tc.cols)
			}
			if !reflect.DeepEqual(res.Rows, tc.rows) {
				t.Fatalf("%q rows = %v, want %v", tc.sql, res.Rows, tc.rows)
			}
		})
	}
}

func TestSQLJoinErrors(t *testing.T) {
	e := joinFixture(t)
	ctx := context.Background()
	cases := []struct {
		sql string
		err error
	}{
		{"SELECT u.nope FROM users AS u JOIN orders AS o ON u.id = o.user_id", ErrBadColumn},
		{"SELECT x.name FROM users AS u JOIN orders AS o ON u.id = o.user_id", ErrBadColumn},
		{"SELECT id FROM users JOIN orders ON users.id = orders.user_id", ErrBadColumn},
		{"SELECT users.name FROM users JOIN orders ON users.id = orders.user_id WHERE id = 1", ErrBadColumn},
		{"SELECT * FROM users JOIN orders ON users.id = orders.nope", ErrBadColumn},
		{"SELECT * FROM users JOIN orders ON users.id = users.id", ErrBadColumn},
		{"SELECT * FROM users AS u JOIN codes AS c ON u.id = c.code", ErrBadColumn},
		{"SELECT * FROM users AS u JOIN orders AS o ON u.id = o.user_id WHERE u.name = 1", ErrBadColumn},
		{"SELECT COUNT(*) FROM users AS u JOIN orders AS o ON u.id = o.user_id", ErrUnsupported},
		{"SELECT * FROM missing JOIN orders ON missing.id = orders.user_id", ErrTableNotFound},
	}
	for _, tc := range cases {
		if _, err := e.Execute(ctx, tc.sql); !errors.Is(err, tc.err) {
			t.Errorf("%q: err = %v, want %v", tc.sql, err, tc.err)
		}
	}

	// Parse-level join limits are reported as syntax errors.
	for _, sql := range []string{
		"SELECT * FROM users JOIN orders ON users.id != orders.user_id",
		"SELECT * FROM users JOIN orders ON users.id = orders.user_id JOIN users ON users.id = orders.user_id",
		"SELECT * FROM users AS x JOIN orders AS x ON x.id = x.id",
		"SELECT * FROM users JOIN users ON users.id = users.id",
	} {
		if _, err := e.Execute(ctx, sql); err == nil {
			t.Errorf("%q: expected error", sql)
		}
	}
}
