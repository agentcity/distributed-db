package test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"distributed-db/internal/kvserver"
	"distributed-db/internal/metaserver"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// snapshotStreamEntry is the on-wire NDJSON record written by the kvserver
// state machine. encoding/json decodes the base64 "k"/"v" fields into bytes.
type snapshotStreamEntry struct {
	Key   []byte `json:"k"`
	Value []byte `json:"v"`
	End   bool   `json:"end"`
}

// followerSnapshotHasKV takes a state-machine snapshot on node and reports
// whether it contains key=value. It lets an end-to-end test inspect the data a
// restarted follower restored without needing that follower to be leader.
func followerSnapshotHasKV(t *testing.T, node *raft.Node, key, value []byte) bool {
	t.Helper()
	snap, err := node.Snapshot()
	if err != nil {
		t.Fatalf("follower Snapshot: %v", err)
	}
	if snap.Index == 0 {
		return false
	}
	body := snap.Data
	if i := bytes.IndexByte(body, '\n'); i >= 0 {
		var header struct {
			Comp string `json:"comp"`
		}
		if err := json.Unmarshal(body[:i], &header); err != nil {
			t.Fatalf("decode snapshot header: %v", err)
		}
		body = body[i+1:]
		if header.Comp == "gzip" {
			zr, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				t.Fatalf("snapshot gzip: %v", err)
			}
			defer zr.Close()
			body, err = io.ReadAll(zr)
			if err != nil {
				t.Fatalf("snapshot gzip body: %v", err)
			}
		}
	}
	for _, line := range bytes.Split(body, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var entry snapshotStreamEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decode follower snapshot record: %v", err)
		}
		if entry.End {
			continue
		}
		// MVCC keys are '<column family> | user key | be64(version)'; the data
		// record for the key carries the transaction value.
		if len(entry.Key) > 1+8 && bytes.Equal(entry.Key[1:len(entry.Key)-8], key) && bytes.Equal(entry.Value, value) {
			return true
		}
	}
	return false
}

// TestV2SnapshotChunkedCatchUp forces a real streamed snapshot on a 3-node
// cluster, restarts a lagging follower after compaction and verifies that it
// catches up through chunked InstallSnapshot without a whole-snapshot []byte
// on either side of the transfer.
func TestV2SnapshotChunkedCatchUp(t *testing.T) {
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
	metaCC, err := grpc.NewClient(metaAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("meta dial: %v", err)
	}
	t.Cleanup(func() { _ = metaCC.Close() })
	metaClient := pbv2.NewMetaServiceClient(metaCC)

	addrs := []string{v2FreeAddr(t), v2FreeAddr(t), v2FreeAddr(t)}
	nodes := make([]*v2Node, 0, len(addrs))
	for i, addr := range addrs {
		peers := make([]string, 0, len(addrs)-1)
		for j, p := range addrs {
			if i != j {
				peers = append(peers, p)
			}
		}
		cfg := kvserver.Config{
			ID: addr, RaftAddr: addr, DataDir: t.TempDir(), Peers: peers,
			// Large snapshot payloads plus the race detector can delay a
			// heartbeat; generous timers keep leadership stable while writing.
			ElectionTimeoutMin: 500 * time.Millisecond,
			ElectionTimeoutMax: time.Second,
			HeartbeatInterval:  100 * time.Millisecond,
			CacheSize:          8 << 20, MemTableSize: 8 << 20,
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
			stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Stop(stopCtx)
		})
	}

	leader := v2WaitLeader(t, nodes)
	var follower, other *v2Node
	for _, n := range nodes {
		if n == leader {
			continue
		}
		if follower == nil {
			follower = n
		} else {
			other = n
		}
	}
	if follower == nil || other == nil {
		t.Fatal("expected two followers")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := follower.srv.Stop(stopCtx); err != nil {
		t.Fatalf("stop follower: %v", err)
	}

	type kvWrite struct {
		key   []byte
		value []byte
		ts    uint64
	}
	put := func(key string, size int) kvWrite {
		t.Helper()
		value := make([]byte, size)
		var seed [8]byte
		for off, ctr := 0, uint64(0); off < size; ctr++ {
			binary.LittleEndian.PutUint64(seed[:], ctr)
			sum := sha256.Sum256(seed[:])
			off += copy(value[off:], sum[:])
		}
		var lastErr string
		for attempt := 0; attempt < 3; attempt++ {
			// A slow CI runner can elect a new leader between v2WaitLeader and
			// the next write. Re-resolve the leader and retry the transaction
			// instead of failing the snapshot scenario on a transient
			// "not leader" response.
			leader = v2WaitLeader(t, nodes)
			r, err := metaClient.AllocTS(ctx, &pbv2.AllocTSRequest{Count: 2})
			if err != nil {
				t.Fatalf("ts: %v", err)
			}
			pr, err := leader.client.Prewrite(ctx, &pbv2.PrewriteRequest{
				StartTs: r.Start, Primary: []byte(key),
				Mutations: []*pbv2.Mutation{{Key: []byte(key), Value: value}},
			})
			if err == nil && pr.GetSuccess() {
				cr, cerr := leader.client.Commit(ctx, &pbv2.CommitRequest{
					StartTs: r.Start, CommitTs: r.Start + 1, Keys: [][]byte{[]byte(key)},
				})
				if cerr == nil && cr.GetSuccess() {
					return kvWrite{key: []byte(key), value: value, ts: r.Start + 1}
				}
				lastErr = fmt.Sprintf("commit: %v %+v", cerr, cr)
			} else {
				lastErr = fmt.Sprintf("prewrite: %v %+v", err, pr)
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("write %s after retries: %s", key, lastErr)
		return kvWrite{}
	}

	// Four ~450 KiB values make the snapshot stream comfortably larger than
	// one 1 MiB chunk, so the transfer needs several InstallSnapshotChunk RPCs.
	writes := make([]kvWrite, 0, 4)
	for i := 0; i < 4; i++ {
		writes = append(writes, put("chunk-key-"+string(rune('a'+i)), 400<<10))
	}

	meta, err := leader.srv.Raft().SnapshotToStore()
	if err != nil {
		t.Fatalf("SnapshotToStore: %v", err)
	}
	if meta.Index == 0 {
		t.Fatal("streamed snapshot index is zero")
	}
	if meta.Size <= raft.SnapshotChunkSize {
		t.Fatalf("snapshot size %d does not exceed one chunk of %d", meta.Size, raft.SnapshotChunkSize)
	}

	// Restart the lagging follower from its old log. The leader has compacted
	// the prefix, so catch-up must install the snapshot.
	restarted, err := kvserver.New(follower.cfg)
	if err != nil {
		t.Fatalf("restart follower: %v", err)
	}
	if err := restarted.Start(ctx); err != nil {
		t.Fatalf("start restarted follower: %v", err)
	}
	restartedNode := &v2Node{srv: restarted, addr: follower.addr, cfg: follower.cfg}
	restartedNode.client = v2Dial(t, follower.addr)
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = restarted.Stop(sctx)
	})

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st := v2Status(t, restartedNode.client)
		if st != nil && st.LastApplied >= meta.Index {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st := v2Status(t, restartedNode.client); st == nil || st.LastApplied < meta.Index {
		t.Fatalf("follower did not catch up through chunked snapshot: %+v (snapshot index %d)", st, meta.Index)
	}

	// Inspect the restarted follower's own state machine: the restored data is
	// there even before it becomes leader.
	for _, w := range writes {
		if !followerSnapshotHasKV(t, restarted.Raft(), w.key, w.value) {
			t.Fatalf("restarted follower snapshot is missing %q", w.key)
		}
	}

	// Kill the old leader and read the committed values from the surviving
	// cluster; the restarted follower plus the other follower retain quorum.
	if err := leader.srv.Stop(stopCtx); err != nil {
		t.Fatalf("stop leader: %v", err)
	}
	newLeader := v2WaitLeader(t, []*v2Node{restartedNode, other})
	for _, w := range writes {
		gr, err := newLeader.client.Get(ctx, &pbv2.GetRequest{Key: w.key, Ts: w.ts})
		if err != nil {
			t.Fatalf("get %q after chunked catch-up: %v", w.key, err)
		}
		if !gr.GetFound() || !bytes.Equal(gr.GetValue(), w.value) {
			t.Fatalf("chunked catch-up lost %q: found=%v bytes=%d err=%q",
				w.key, gr.GetFound(), len(gr.GetValue()), gr.GetError())
		}
	}
}
