package test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"distributed-db/internal/v2client"
)

// TestV2CommitBatch exercises CommitBatch against a real three-node cluster.
// Each transaction is a single-key 1PC transaction; the batch makes them run
// concurrently but is explicitly not atomic. The second subtest proves that one
// conflicting transaction does not affect its siblings and that its error stays
// at its own input index.
func TestV2CommitBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	c := newSpeedCluster(t)
	ctx := context.Background()
	speedWarmup(t, c.client)

	t.Run("all succeed and visible", func(t *testing.T) {
		const n = 24
		keys := make([]string, n)
		txns := make([]*v2client.Txn, n)
		for i := range txns {
			txn, err := c.client.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin(%d): %v", i, err)
			}
			keys[i] = batchTestKey("batch-all", i)
			txn.Set([]byte(keys[i]), []byte("batch-value"))
			txns[i] = txn
		}

		errs := c.client.CommitBatch(ctx, txns)
		for i, err := range errs {
			if err != nil {
				t.Fatalf("CommitBatch slot %d = %v, want nil", i, err)
			}
		}

		readTxn, err := c.client.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin for read: %v", err)
		}
		rows, err := readTxn.Scan(ctx, []byte("batch-all/"), []byte("batch-all0"), 0)
		if err != nil {
			t.Fatalf("Scan after batch commit: %v", err)
		}
		got := make(map[string]string, len(rows))
		for _, kv := range rows {
			got[string(kv.Key)] = string(kv.Value)
		}
		for i, key := range keys {
			v, ok := got[key]
			if !ok {
				t.Fatalf("key %d (%q) is not visible after batch commit", i, key)
			}
			if v != "batch-value" {
				t.Fatalf("key %d (%q) = %q, want batch-value", i, key, v)
			}
		}
	})

	t.Run("conflict is isolated", func(t *testing.T) {
		const conflictKey = "batch-conflict/key"
		loser, err := c.client.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin loser: %v", err)
		}
		loser.Set([]byte(conflictKey), []byte("loser"))

		winner, err := c.client.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin winner: %v", err)
		}
		winner.Set([]byte(conflictKey), []byte("winner"))
		if err := winner.Commit(ctx); err != nil {
			t.Fatalf("Commit winner: %v", err)
		}

		good := func(key string) *v2client.Txn {
			txn, err := c.client.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin good %q: %v", key, err)
			}
			txn.Set([]byte(key), []byte("good"))
			return txn
		}
		goodKeys := []string{"batch-conflict/good-0", "batch-conflict/good-1"}
		txns := []*v2client.Txn{good(goodKeys[0]), loser, good(goodKeys[1])}

		errs := c.client.CommitBatch(ctx, txns)
		if errs[0] != nil {
			t.Fatalf("slot 0 = %v, want nil", errs[0])
		}
		if !errors.Is(errs[1], v2client.ErrConflict) {
			t.Fatalf("slot 1 = %v, want ErrConflict", errs[1])
		}
		if errs[2] != nil {
			t.Fatalf("slot 2 = %v, want nil (a conflict must not abort siblings)", errs[2])
		}

		readTxn, err := c.client.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin for read: %v", err)
		}
		for _, key := range goodKeys {
			v, err := readTxn.Get(ctx, []byte(key))
			if err != nil {
				t.Fatalf("Get(%q) after batch: %v", key, err)
			}
			if string(v) != "good" {
				t.Fatalf("Get(%q) = %q, want good", key, v)
			}
		}
		v, err := readTxn.Get(ctx, []byte(conflictKey))
		if err != nil {
			t.Fatalf("Get(conflict key): %v", err)
		}
		if string(v) != "winner" {
			t.Fatalf("conflict key = %q, want winner (loser must not overwrite it)", v)
		}
	})
}

func batchTestKey(prefix string, i int) string {
	return fmt.Sprintf("%s/%03d", prefix, i)
}
