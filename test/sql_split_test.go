package test

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"distributed-db/internal/kvserver"
	"distributed-db/internal/meta"
	"distributed-db/internal/metaserver"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"
	"distributed-db/internal/sqlengine"
	"distributed-db/internal/v2client"

	"github.com/cespare/xxhash/v2"
)

// TestSQLSplitLifecycle exercises the SQL layer after a real range split on a
// three-node cluster: full scans, primary-key range scans, secondary-index
// scans, aggregates, ORDER BY, and cross-range UPDATE/DELETE all run against
// the same transaction and the v2client cross-range 2PC.
func TestSQLSplitLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	metaAddr := v2FreeAddr(t)
	addrs := []string{v2FreeAddr(t), v2FreeAddr(t), v2FreeAddr(t)}

	ranges := []meta.Range{{
		ID: 1, Start: nil, End: nil,
		Replicas: append([]string(nil), addrs...), Version: 1,
	}}
	ms, err := metaserver.New(metaserver.Config{Addr: metaAddr, Ranges: ranges, Logger: v2Logger()})
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	if err := ms.Start(ctx); err != nil {
		t.Fatalf("meta start: %v", err)
	}
	t.Cleanup(func() { _ = ms.Stop() })

	nodes := make([]*v2Node, 0, 3)
	for i, addr := range addrs {
		peers := make([]string, 0, 2)
		for j, p := range addrs {
			if i != j {
				peers = append(peers, p)
			}
		}
		cfg := kvserver.Config{
			ID: addr, RaftAddr: addr, DataDir: t.TempDir(), Peers: peers, RangeID: 1,
			ElectionTimeoutMin: 120 * time.Millisecond,
			ElectionTimeoutMax: 240 * time.Millisecond,
			HeartbeatInterval:  25 * time.Millisecond,
			CacheSize:          8 << 20, MemTableSize: 4 << 20,
			Logger: v2Logger(),
		}
		srv, err := kvserver.New(cfg)
		if err != nil {
			t.Fatalf("new %s: %v", addr, err)
		}
		if err := srv.Start(ctx); err != nil {
			t.Fatalf("start %s: %v", addr, err)
		}
		n := &v2Node{srv: srv, addr: addr, cfg: cfg}
		n.client = v2Dial(t, addr)
		nodes = append(nodes, n)
		t.Cleanup(func() {
			sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Stop(sctx)
		})
	}
	_ = v2WaitLeader(t, nodes)

	client, err := v2client.New(metaAddr, addrs)
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
	exec("CREATE TABLE users (id INT PRIMARY KEY, name TEXT, city TEXT)")
	for _, row := range []struct {
		id   int64
		name string
		city string
	}{
		{1, "u1", "msk"}, {2, "u2", "spb"}, {3, "u3", "msk"},
		{4, "u4", "spb"}, {5, "u5", "msk"}, {6, "u6", "spb"},
	} {
		exec(fmt.Sprintf("INSERT INTO users VALUES (%d, '%s', '%s')", row.id, row.name, row.city))
	}
	exec("CREATE INDEX idx_city ON users (city)")

	// Split between primary keys 3 and 4: the catalog and ids 1..3 stay in
	// range 1, ids 4..6 move to range 2.
	tableID := xxhash.Sum64String("users")
	rowSplit := make([]byte, 9+8)
	rowSplit[0] = 0x02
	binary.BigEndian.PutUint64(rowSplit[1:9], tableID)
	binary.BigEndian.PutUint64(rowSplit[9:], uint64(4)^(1<<63))
	split, err := ms.SplitRange(ctx, &pbv2.MetaSplitRangeRequest{RangeId: 1, SplitKey: rowSplit})
	if err != nil || !split.GetSuccess() {
		t.Fatalf("meta row split: %v %+v", err, split)
	}
	rightID := split.GetNewRangeId()
	waitRangeLeader(t, nodes, rightID)

	// CREATE INDEX must scan the table across both ranges and write index
	// entries that live in the right range.
	exec("CREATE INDEX idx_name ON users (name)")

	// Split the right range again inside the secondary-index keyspace, so
	// index scans must also cross a range boundary.
	idxID := xxhash.Sum64String("idx_city")
	indexSplit := make([]byte, 17)
	indexSplit[0] = 0x03
	binary.BigEndian.PutUint64(indexSplit[1:9], tableID)
	binary.BigEndian.PutUint64(indexSplit[9:17], idxID)
	var valueLen [4]byte
	binary.BigEndian.PutUint32(valueLen[:], 3)
	indexSplit = append(indexSplit, valueLen[:]...)
	indexSplit = append(indexSplit, []byte("spb")...)
	split, err = ms.SplitRange(ctx, &pbv2.MetaSplitRangeRequest{RangeId: rightID, SplitKey: indexSplit})
	if err != nil || !split.GetSuccess() {
		t.Fatalf("meta index split: %v %+v", err, split)
	}
	waitRangeLeader(t, nodes, split.GetNewRangeId())

	// The original client still has the pre-split descriptor cached for some
	// keys; ScanRanges refreshes through meta on the range-mismatch retry.
	res := exec("SELECT id FROM users")
	if len(res.Rows) != 6 {
		t.Fatalf("cross-range SELECT = %+v", res.Rows)
	}
	res = exec("SELECT id FROM users WHERE id BETWEEN 2 AND 5")
	if len(res.Rows) != 4 || res.Rows[0][0] != int64(2) || res.Rows[3][0] != int64(5) {
		t.Fatalf("cross-range PK range = %+v", res.Rows)
	}
	res = exec("SELECT id FROM users WHERE id <= 5")
	if len(res.Rows) != 5 || res.Rows[4][0] != int64(5) {
		t.Fatalf("cross-range inclusive PK bound = %+v", res.Rows)
	}
	res = exec("SELECT COUNT(*) FROM users")
	if len(res.Rows) != 1 || res.Rows[0][0] != int64(6) {
		t.Fatalf("cross-range COUNT = %+v", res.Rows)
	}
	res = exec("SELECT COUNT(*) FROM users GROUP BY city")
	if len(res.Rows) != 2 {
		t.Fatalf("cross-range GROUP BY = %+v", res.Rows)
	}
	res = exec("SELECT id FROM users WHERE city > 'msk'")
	if len(res.Rows) != 3 {
		t.Fatalf("cross-range index scan = %+v", res.Rows)
	}
	res = exec("SELECT id FROM users ORDER BY id DESC LIMIT 2")
	if len(res.Rows) != 2 || res.Rows[0][0] != int64(6) || res.Rows[1][0] != int64(5) {
		t.Fatalf("cross-range ORDER BY = %+v", res.Rows)
	}

	// Rows 4..6 live in range 2 while their idx_city entries live in range 3
	// (the value "zzz" sorts after the index split), so this mutation commits
	// through cross-range 2PC.
	res = exec("UPDATE users SET name = 'both', city = 'zzz' WHERE id >= 4")
	if res.RowsAffected != 3 {
		t.Fatalf("cross-range UPDATE affected = %d", res.RowsAffected)
	}
	res = exec("SELECT name FROM users WHERE id = 5")
	if len(res.Rows) != 1 || res.Rows[0][0] != "both" {
		t.Fatalf("UPDATE value = %+v", res.Rows)
	}
	res = exec("DELETE FROM users WHERE id BETWEEN 2 AND 5")
	if res.RowsAffected != 4 {
		t.Fatalf("cross-range DELETE affected = %d", res.RowsAffected)
	}
	res = exec("SELECT id FROM users")
	if len(res.Rows) != 2 || res.Rows[0][0] != int64(1) || res.Rows[1][0] != int64(6) {
		t.Fatalf("rows after cross-range DELETE = %+v", res.Rows)
	}
	res = exec("SELECT id FROM users WHERE city = 'msk'")
	if len(res.Rows) != 1 || res.Rows[0][0] != int64(1) {
		t.Fatalf("index after delete = %+v", res.Rows)
	}

	// INSERT after the split must choose the right range from meta and be
	// visible to scans and indexes.
	exec("INSERT INTO users VALUES (7, 'u7', 'msk')")
	res = exec("SELECT COUNT(*) FROM users")
	if res.Rows[0][0] != int64(3) {
		t.Fatalf("count after post-split insert = %+v", res.Rows)
	}
	res = exec("SELECT id FROM users WHERE city = 'msk'")
	if len(res.Rows) != 2 {
		t.Fatalf("index after post-split insert = %+v", res.Rows)
	}

	exec("DROP TABLE users")
	if _, err := engine.Execute(ctx, "SELECT * FROM users"); err == nil {
		t.Fatal("SELECT after DROP TABLE succeeded")
	}
}

func waitRangeLeader(t *testing.T, nodes []*v2Node, rangeID uint64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			if g, err := n.srv.GroupForID(rangeID); err == nil && g.Status().Role == raft.Leader {
				return
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("range %d did not elect a leader", rangeID)
}
