package sqlengine

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"

	"distributed-db/internal/sqlparser"

	"github.com/cespare/xxhash/v2"
)

// splitClient models a keyspace divided by one or more range boundaries. A
// plain Scan crossing any boundary fails like the KV server's range check,
// while ScanRanges concatenates the per-range slices.
type splitClient struct {
	base   *fakeClient
	splits [][]byte
}

func newSplitClient() *splitClient { return &splitClient{base: newFakeClient()} }

func (c *splitClient) Begin(ctx context.Context) (Txn, error) {
	tx, err := c.base.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &splitTxn{fakeTxn: tx.(*fakeTxn), splits: c.splits}, nil
}

type splitTxn struct {
	*fakeTxn
	splits [][]byte
}

// boundaryInside reports whether a split point lies strictly inside
// [start,end) (or at start when start == boundary is allowed only for a scan
// that begins there).
func (t *splitTxn) boundaryInside(start, end []byte) bool {
	for _, split := range t.splits {
		if bytes.Compare(start, split) < 0 && (end == nil || bytes.Compare(end, split) > 0) {
			return true
		}
	}
	return false
}

func (t *splitTxn) Scan(ctx context.Context, start, end []byte, limit int) ([]KV, error) {
	if t.boundaryInside(start, end) {
		return nil, errors.New("v2client: scan: range mismatch")
	}
	return t.fakeTxn.Scan(ctx, start, end, limit)
}

func (t *splitTxn) ScanRanges(ctx context.Context, start, end []byte, limit int) ([]KV, error) {
	if !t.boundaryInside(start, end) {
		return t.Scan(ctx, start, end, limit)
	}
	cursor := append([]byte(nil), start...)
	out := make([]KV, 0)
	for {
		if limit > 0 && len(out) >= limit {
			return out, nil
		}
		// Next boundary at or after cursor, capped by end.
		next := end
		for _, split := range t.splits {
			if bytes.Compare(split, cursor) > 0 && (next == nil || bytes.Compare(split, next) < 0) {
				next = split
			}
		}
		remaining := 0
		if limit > 0 {
			remaining = limit - len(out)
		}
		rows, err := t.Scan(ctx, cursor, next, remaining)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if next == nil || bytes.Equal(next, end) {
			return out, nil
		}
		cursor = append(cursor[:0], next...)
	}
}

func execRows(t *testing.T, e *Engine, sql string) [][]any {
	t.Helper()
	res, err := e.Execute(context.Background(), sql)
	if err != nil {
		t.Fatalf("Execute(%q): %v", sql, err)
	}
	return res.Rows
}

func TestSQLRangePredicates(t *testing.T) {
	e := New(newFakeClient())
	ctx := context.Background()
	exec(t, e, "CREATE TABLE nums (id INT PRIMARY KEY, val INT)")
	for i := int64(1); i <= 6; i++ {
		exec(t, e, fmt.Sprintf("INSERT INTO nums VALUES (%d, %d)", i, i*10))
	}
	cases := []struct {
		sql  string
		want []int64
	}{
		{"SELECT id FROM nums WHERE id > 4", []int64{5, 6}},
		{"SELECT id FROM nums WHERE id >= 4", []int64{4, 5, 6}},
		{"SELECT id FROM nums WHERE id < 3", []int64{1, 2}},
		{"SELECT id FROM nums WHERE id <= 3", []int64{1, 2, 3}},
		{"SELECT id FROM nums WHERE id BETWEEN 2 AND 5", []int64{2, 3, 4, 5}},
		{"SELECT id FROM nums WHERE id BETWEEN 5 AND 2", nil},
		{"SELECT id FROM nums WHERE id != 3", []int64{1, 2, 4, 5, 6}},
	}
	for _, tc := range cases {
		rows := execRows(t, e, tc.sql)
		got := make([]int64, 0, len(rows))
		for _, row := range rows {
			got = append(got, row[0].(int64))
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%q = %v, want %v", tc.sql, got, tc.want)
		}
	}
	// Bound literals must match the primary-key type.
	if _, err := e.Execute(ctx, "SELECT * FROM nums WHERE id > 'x'"); !errors.Is(err, ErrBadColumn) {
		t.Fatalf("text bound on INT pk = %v", err)
	}
}

func TestSQLTextRangePredicates(t *testing.T) {
	e := New(newFakeClient())
	exec(t, e, "CREATE TABLE docs (name TEXT PRIMARY KEY, body TEXT)")
	for _, v := range []string{"a", "ab", "b", "ba", "c"} {
		exec(t, e, fmt.Sprintf("INSERT INTO docs VALUES ('%s', 'x')", v))
	}
	cases := []struct {
		sql  string
		want []string
	}{
		{"SELECT name FROM docs WHERE name > 'a'", []string{"ab", "b", "ba", "c"}},
		{"SELECT name FROM docs WHERE name >= 'b'", []string{"b", "ba", "c"}},
		{"SELECT name FROM docs WHERE name < 'b'", []string{"a", "ab"}},
		{"SELECT name FROM docs WHERE name <= 'b'", []string{"a", "ab", "b"}},
		{"SELECT name FROM docs WHERE name BETWEEN 'ab' AND 'ba'", []string{"ab", "b", "ba"}},
	}
	for _, tc := range cases {
		rows := execRows(t, e, tc.sql)
		got := make([]string, 0, len(rows))
		for _, row := range rows {
			got = append(got, row[0].(string))
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%q = %v, want %v", tc.sql, got, tc.want)
		}
	}
}

func TestSQLPlannerAccessPaths(t *testing.T) {
	e := New(newFakeClient())
	ctx := context.Background()
	exec(t, e, "CREATE TABLE users (id INT PRIMARY KEY, age INT, name TEXT, city TEXT)")
	for i := int64(1); i <= 40; i++ {
		exec(t, e, fmt.Sprintf("INSERT INTO users VALUES (%d, %d, 'n%d', 'c%d')", i, i, i, i%3))
	}
	exec(t, e, "CREATE INDEX idx_age ON users (age)")
	exec(t, e, "CREATE INDEX idx_city ON users (city)")

	cases := []struct {
		sql    string
		access AccessKind
		index  string
	}{
		{"SELECT * FROM users", AccessTableScan, ""},
		{"SELECT * FROM users WHERE id = 7", AccessPKPoint, ""},
		{"SELECT * FROM users WHERE id > 7", AccessPKRange, ""},
		{"SELECT * FROM users WHERE id BETWEEN 3 AND 9", AccessPKRange, ""},
		{"SELECT * FROM users WHERE age = 7", AccessIndexPoint, "idx_age"},
		{"SELECT * FROM users WHERE age > 7", AccessIndexRange, "idx_age"},
		{"SELECT * FROM users WHERE age BETWEEN 3 AND 9", AccessIndexRange, "idx_age"},
		{"SELECT * FROM users WHERE name = 'n7'", AccessTableScan, ""},
		{"SELECT * FROM users WHERE city > 'c1'", AccessIndexFull, "idx_city"},
	}
	for _, tc := range cases {
		plan, err := e.Explain(ctx, tc.sql)
		if err != nil {
			// No index on name: fall back to an error for unsupported WHERE.
			if tc.access == AccessTableScan {
				continue
			}
			t.Fatalf("%q: %v", tc.sql, err)
		}
		if plan.Access != tc.access || plan.Index != tc.index {
			t.Errorf("%q access = %s/%q, want %s/%q", tc.sql, plan.Access, plan.Index, tc.access, tc.index)
		}
	}

	// The indexed INT range clips the index; the TEXT range scans it.
	plan, err := e.Explain(ctx, "SELECT * FROM users WHERE age > 7")
	if err != nil {
		t.Fatal(err)
	}
	if plan.EstimatedRows >= 40 {
		t.Errorf("index range estimate = %d, want below full table", plan.EstimatedRows)
	}
	if !plan.Ordered || plan.Sort {
		t.Errorf("index range should be ordered, got ordered=%v sort=%v", plan.Ordered, plan.Sort)
	}
}

func TestSQLOrderByAndLimit(t *testing.T) {
	e := New(newFakeClient())
	exec(t, e, "CREATE TABLE t (id INT PRIMARY KEY, age INT)")
	for _, pair := range [][2]int64{{1, 30}, {2, 10}, {3, 20}} {
		exec(t, e, fmt.Sprintf("INSERT INTO t VALUES (%d, %d)", pair[0], pair[1]))
	}
	exec(t, e, "CREATE INDEX idx_age ON t (age)")

	rows := execRows(t, e, "SELECT id FROM t ORDER BY age")
	if fmt.Sprint(rows) != "[[2] [3] [1]]" {
		t.Fatalf("order by indexed asc = %v", rows)
	}
	rows = execRows(t, e, "SELECT id FROM t ORDER BY age DESC")
	if fmt.Sprint(rows) != "[[1] [3] [2]]" {
		t.Fatalf("order by indexed desc = %v", rows)
	}
	rows = execRows(t, e, "SELECT id FROM t ORDER BY id DESC LIMIT 2")
	if fmt.Sprint(rows) != "[[3] [2]]" {
		t.Fatalf("order by pk desc limit = %v", rows)
	}
	rows = execRows(t, e, "SELECT id FROM t ORDER BY id LIMIT 2")
	if fmt.Sprint(rows) != "[[1] [2]]" {
		t.Fatalf("order by pk asc limit = %v", rows)
	}
	// LIMIT is pushed for an ordered primary-key scan but not when sorting.
	plan, err := e.Explain(context.Background(), "SELECT id FROM t ORDER BY age DESC LIMIT 1")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Sort || plan.LimitPushed {
		t.Fatalf("descending index order plan = %+v, want sort without pushdown", plan)
	}
	plan, err = e.Explain(context.Background(), "SELECT id FROM t ORDER BY id LIMIT 2")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Sort || !plan.LimitPushed {
		t.Fatalf("ascending primary-key plan = %+v, want limit pushdown", plan)
	}

	// A primary-key range scans slightly past GT/LE boundaries to include all
	// versions, so LIMIT must be applied after filtering.
	rows = execRows(t, e, "SELECT id FROM t WHERE id > 1 LIMIT 1")
	if fmt.Sprint(rows) != "[[2]]" {
		t.Fatalf("gt limit = %v", rows)
	}
	// An index access path is ordered by age, not by the primary key, so
	// ORDER BY id needs a sort.
	rows = execRows(t, e, "SELECT id FROM t WHERE age > 0 ORDER BY id LIMIT 1")
	if fmt.Sprint(rows) != "[[1]]" {
		t.Fatalf("index scan ordered by pk = %v", rows)
	}
	// ORDER BY the indexed range column is already satisfied by the index.
	plan, err = e.Explain(context.Background(), "SELECT id FROM t WHERE age > 0 ORDER BY age LIMIT 1")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Access != AccessIndexRange || plan.Sort || !plan.LimitPushed {
		t.Fatalf("index range order plan = %+v", plan)
	}
	rows = execRows(t, e, "SELECT id FROM t WHERE age > 0 ORDER BY age LIMIT 1")
	if fmt.Sprint(rows) != "[[2]]" {
		t.Fatalf("index ordered limit = %v", rows)
	}
}

func TestSQLPlanCacheHitAndInvalidation(t *testing.T) {
	e := New(newFakeClient())
	ctx := context.Background()
	exec(t, e, "CREATE TABLE t (id INT PRIMARY KEY, val INT)")
	for i := int64(1); i <= 5; i++ {
		exec(t, e, fmt.Sprintf("INSERT INTO t VALUES (%d, %d)", i, i))
	}

	// Same normalized statement with different literals should hit.
	exec(t, e, "SELECT * FROM t WHERE id > 1")
	exec(t, e, "SELECT * FROM t WHERE id > 2")
	stats := e.PlanCacheStats()
	if stats.Hits == 0 {
		t.Fatalf("plan cache stats after second statement = %+v, want a hit", stats)
	}
	if stats.Size == 0 {
		t.Fatalf("plan cache is empty after execution")
	}

	plan, err := e.Explain(ctx, "SELECT * FROM t WHERE id > 3")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.CacheHit {
		t.Fatalf("Explain plan = %+v, want cache hit", plan)
	}

	before := e.PlanCacheStats()
	exec(t, e, "CREATE INDEX idx_val ON t (val)")
	after := e.PlanCacheStats()
	if after.Invalidations <= before.Invalidations {
		t.Fatalf("invalidations = %d, want > %d after DDL", after.Invalidations, before.Invalidations)
	}
	if after.Size != 0 {
		t.Fatalf("plan cache size = %d after DDL, want 0", after.Size)
	}
	plan, err = e.Explain(ctx, "SELECT * FROM t WHERE val = 3")
	if err != nil {
		t.Fatal(err)
	}
	if plan.CacheHit {
		t.Fatalf("plan after DDL = %+v, want cache miss", plan)
	}
	if plan.Access != AccessIndexPoint || plan.Index != "idx_val" {
		t.Fatalf("plan after create index = %+v", plan)
	}
}

func TestSQLJoinOrderChoice(t *testing.T) {
	e := New(newFakeClient())
	ctx := context.Background()
	exec(t, e, "CREATE TABLE small (id INT PRIMARY KEY, v INT)")
	exec(t, e, "CREATE TABLE big (id INT PRIMARY KEY, v INT)")
	exec(t, e, "INSERT INTO small VALUES (1, 10)")
	exec(t, e, "INSERT INTO small VALUES (2, 20)")
	for i := int64(1); i <= 20; i++ {
		exec(t, e, fmt.Sprintf("INSERT INTO big VALUES (%d, %d)", i, i%3*10))
	}

	plan, err := e.Explain(ctx, "SELECT small.id, big.id FROM small JOIN big ON small.v = big.v")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Access != AccessHashJoin || plan.Join == nil {
		t.Fatalf("join plan = %+v", plan)
	}
	if plan.Join.BuildSide != "left" || plan.Join.BuildTable != "small" {
		t.Fatalf("join build = %+v, want the 2-row table", plan.Join)
	}
	if plan.Join.LeftRows >= plan.Join.RightRows {
		t.Fatalf("join estimates = %+v, want left smaller", plan.Join)
	}

	plan, err = e.Explain(ctx, "SELECT big.id, small.id FROM big JOIN small ON big.v = small.v")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Join.BuildSide != "right" || plan.Join.BuildTable != "small" {
		t.Fatalf("reversed join build = %+v, want right/small", plan.Join)
	}

	// Build-left output must still be left-key ordered.
	exec(t, e, "CREATE TABLE users2 (id INT PRIMARY KEY, name TEXT)")
	exec(t, e, "INSERT INTO users2 VALUES (1, 'one')")
	exec(t, e, "INSERT INTO users2 VALUES (2, 'two')")
	exec(t, e, "CREATE TABLE events (id INT PRIMARY KEY, user_id INT)")
	exec(t, e, "INSERT INTO events VALUES (10, 2)")
	exec(t, e, "INSERT INTO events VALUES (11, 1)")
	exec(t, e, "INSERT INTO events VALUES (12, 1)")
	rows := execRows(t, e, "SELECT events.id FROM users2 JOIN events ON users2.id = events.user_id")
	if fmt.Sprint(rows) != "[[11] [12] [10]]" {
		t.Fatalf("build-left join order = %v, want left-key order", rows)
	}
}

func TestSQLSplitTableLifecycle(t *testing.T) {
	client := newSplitClient()
	e := New(client)
	ctx := context.Background()
	exec(t, e, "CREATE TABLE users (id INT PRIMARY KEY, name TEXT, city TEXT)")
	for i := int64(1); i <= 6; i++ {
		exec(t, e, fmt.Sprintf("INSERT INTO users VALUES (%d, 'u%d', '%s')", i, i, []string{"msk", "spb", "msk", "spb", "msk", "spb"}[i-1]))
	}
	exec(t, e, "CREATE INDEX idx_city ON users (city)")

	// Split between ids 3 and 4. Rows 1..3 and the catalog stay left; rows
	// 4..6 live right. A second boundary inside the secondary-index keyspace
	// makes index scans span ranges too.
	schemaID := xxhash.Sum64String("users")
	var pk [8]byte
	binary.BigEndian.PutUint64(pk[:], uint64(4)^(1<<63))
	rowSplit := rowKey(schemaID, pk[:])
	idxID := indexID("idx_city")
	encodedSPB, err := encodeValue(Column{Name: "city", Type: "TEXT"}, sqlparser.Value{Kind: sqlparser.ValueText, Str: "spb"})
	if err != nil {
		t.Fatal(err)
	}
	indexSplit := indexValueKey(schemaID, idxID, encodedSPB)
	client.splits = [][]byte{rowSplit, indexSplit}
	// The index was created before the split above; rebuild it after the
	// split to exercise cross-range CREATE INDEX scanning as well.
	exec(t, e, "DROP INDEX idx_city ON users")
	exec(t, e, "CREATE INDEX idx_city ON users (city)")

	if rows := execRows(t, e, "SELECT id FROM users"); len(rows) != 6 {
		t.Fatalf("cross-range SELECT = %v", rows)
	}
	if rows := execRows(t, e, "SELECT COUNT(*) FROM users"); len(rows) != 1 || rows[0][0] != int64(6) {
		t.Fatalf("cross-range COUNT = %v", rows)
	}
	if rows := execRows(t, e, "SELECT COUNT(*) FROM users WHERE id > 3"); len(rows) != 1 || rows[0][0] != int64(3) {
		t.Fatalf("cross-range filtered COUNT = %v", rows)
	}
	if rows := execRows(t, e, "SELECT SUM(id) FROM users WHERE id BETWEEN 2 AND 5"); len(rows) != 1 || rows[0][0] != int64(14) {
		t.Fatalf("cross-range filtered SUM = %v", rows)
	}
	if rows := execRows(t, e, "SELECT id FROM users WHERE id BETWEEN 2 AND 5"); len(rows) != 4 {
		t.Fatalf("cross-range range scan = %v", rows)
	}
	if rows := execRows(t, e, "SELECT id FROM users WHERE city = 'spb'"); len(rows) != 3 {
		t.Fatalf("cross-range index scan = %v", rows)
	}

	// UPDATE spans both ranges (and updates the catalog row count on the
	// left) in a single transaction.
	res := exec(t, e, "UPDATE users SET name = 'both' WHERE id >= 2")
	if res.RowsAffected != 5 {
		t.Fatalf("cross-range UPDATE affected = %d", res.RowsAffected)
	}
	if rows := execRows(t, e, "SELECT COUNT(*) FROM users WHERE city = 'msk'"); rows[0][0] != int64(3) {
		t.Fatalf("cross-range UPDATE index count = %v", rows)
	}
	if rows := execRows(t, e, "SELECT name FROM users WHERE id = 2"); rows[0][0] != "both" {
		t.Fatalf("cross-range UPDATE value = %v", rows)
	}

	// DELETE by indexed predicate across both ranges.
	res = exec(t, e, "DELETE FROM users WHERE city = 'spb'")
	if res.RowsAffected != 3 {
		t.Fatalf("cross-range DELETE affected = %d", res.RowsAffected)
	}
	if rows := execRows(t, e, "SELECT COUNT(*) FROM users"); rows[0][0] != int64(3) {
		t.Fatalf("rows after cross-range DELETE = %v", rows)
	}

	// DROP TABLE must also walk both ranges.
	exec(t, e, "DROP TABLE users")
	if _, err := e.Execute(ctx, "SELECT * FROM users"); !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("select after drop = %v", err)
	}
}

func TestNormalizeIgnoresLiterals(t *testing.T) {
	a, err := sqlparser.Parse("SELECT * FROM t WHERE id > 1 ORDER BY id DESC LIMIT 3")
	if err != nil {
		t.Fatal(err)
	}
	b, err := sqlparser.Parse("select * from T where ID > 999 order by Id desc limit 3")
	if err != nil {
		t.Fatal(err)
	}
	if sqlparser.Normalize(a) != sqlparser.Normalize(b) {
		t.Fatalf("normalized forms differ:\n%q\n%q", sqlparser.Normalize(a), sqlparser.Normalize(b))
	}
	c, err := sqlparser.Parse("SELECT * FROM t WHERE id BETWEEN 1 AND 2")
	if err != nil {
		t.Fatal(err)
	}
	if got := sqlparser.Normalize(c); got != "SELECT * FROM t WHERE id BETWEEN ? AND ?" {
		t.Fatalf("between normalization = %q", got)
	}
}

func TestSQLIndexRangeUpdateDelete(t *testing.T) {
	e := New(newFakeClient())
	exec(t, e, "CREATE TABLE scores (id INT PRIMARY KEY, score INT, name TEXT)")
	for i := int64(1); i <= 10; i++ {
		exec(t, e, fmt.Sprintf("INSERT INTO scores VALUES (%d, %d, 'n%d')", i, i*10, i))
	}
	exec(t, e, "CREATE INDEX idx_score ON scores (score)")

	res := exec(t, e, "UPDATE scores SET name = 'high' WHERE score >= 60")
	if res.RowsAffected != 5 {
		t.Fatalf("index range UPDATE affected = %d", res.RowsAffected)
	}
	rows := execRows(t, e, "SELECT COUNT(*) FROM scores WHERE score >= 60")
	if rows[0][0] != int64(5) {
		t.Fatalf("count after index range update = %v", rows)
	}
	res = exec(t, e, "DELETE FROM scores WHERE score BETWEEN 20 AND 50")
	if res.RowsAffected != 4 {
		t.Fatalf("index range DELETE affected = %d", res.RowsAffected)
	}
	rows = execRows(t, e, "SELECT id FROM scores WHERE score >= 20")
	if len(rows) != 5 {
		t.Fatalf("rows after index range delete = %v", rows)
	}
	// The index no longer returns deleted values.
	rows = execRows(t, e, "SELECT id FROM scores WHERE score = 30")
	if len(rows) != 0 {
		t.Fatalf("deleted index entry still visible = %v", rows)
	}
}

func TestSQLGroupByOrderAndLimit(t *testing.T) {
	e := New(newFakeClient())
	exec(t, e, "CREATE TABLE sales (id INT PRIMARY KEY, region TEXT, amount INT)")
	exec(t, e, "INSERT INTO sales VALUES (1, 'eu', 10)")
	exec(t, e, "INSERT INTO sales VALUES (2, 'us', 20)")
	exec(t, e, "INSERT INTO sales VALUES (3, 'eu', 30)")
	res := exec(t, e, "SELECT COUNT(*) FROM sales GROUP BY region ORDER BY region DESC LIMIT 1")
	if len(res.Rows) != 1 || res.Rows[0][0] != "us" || res.Rows[0][1] != int64(1) {
		t.Fatalf("group by order/limit = %+v", res.Rows)
	}
	if _, err := e.Execute(context.Background(), "SELECT COUNT(*) FROM sales GROUP BY region ORDER BY amount"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("group by invalid order = %v", err)
	}
	if _, err := e.Execute(context.Background(), "SELECT COUNT(*) FROM sales ORDER BY id"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("global aggregate order = %v", err)
	}
}
