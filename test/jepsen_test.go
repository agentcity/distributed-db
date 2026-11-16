package test

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"
	"time"

	"distributed-db/internal/kvserver"
	"distributed-db/internal/meta"
	"distributed-db/internal/metaserver"
	"distributed-db/internal/v2client"
)

// jepsenAck is one write that was acknowledged by a nil Txn.Commit error.
// Only acknowledged writes are checked by the safety invariant: a write that
// returned an error may or may not have committed and is deliberately not
// counted.
type jepsenAck struct {
	key   string
	value string
}

// jepsenRig owns the transactional client and the acknowledged write log used
// by TestJepsenKillRestart. It is not safe for concurrent use.
type jepsenRig struct {
	t        *testing.T
	metaAddr string
	addrs    []string
	client   *v2client.Client
	acked    []jepsenAck
	seq      int
}

func newJepsenRig(t *testing.T, metaAddr string, addrs []string) *jepsenRig {
	t.Helper()
	r := &jepsenRig{t: t, metaAddr: metaAddr, addrs: append([]string(nil), addrs...)}
	r.resetClient()
	t.Cleanup(r.close)
	return r
}

func (r *jepsenRig) close() {
	if r.client != nil {
		_ = r.client.Close()
		r.client = nil
	}
}

// resetClient replaces the v2 client. This is used after a node restart or a
// stale leader/range cache is suspected; v2client re-discovers the leader on
// its own, but a fresh client keeps the failover test out of cache TTL races.
func (r *jepsenRig) resetClient() {
	if r.client != nil {
		_ = r.client.Close()
		r.client = nil
	}
	c, err := v2client.New(r.metaAddr, r.addrs)
	if err != nil {
		r.t.Fatalf("jepsen: new v2 client: %v", err)
	}
	r.client = c
}

// putNext writes the next monotonically increasing key/value. A failed
// transaction is never added to acked; the whole operation is retried with a
// brand-new transaction, as required for the acknowledged-write invariant.
func (r *jepsenRig) putNext() {
	r.seq++
	key := fmt.Sprintf("jepsen-%06d", r.seq)
	value := fmt.Sprintf("value-%06d", r.seq)

	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			if attempt == 2 {
				r.resetClient()
			}
			time.Sleep(time.Duration(attempt) * 75 * time.Millisecond)
		}

		opCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		txn, err := r.client.Begin(opCtx)
		if err == nil {
			txn.Set([]byte(key), []byte(value))
			err = txn.Commit(opCtx)
		}
		cancel()
		if err == nil {
			r.acked = append(r.acked, jepsenAck{key: key, value: value})
			return
		}
		lastErr = err
		r.t.Logf("jepsen: write %s attempt %d/5 not acknowledged: %v", key, attempt+1, err)
	}
	r.t.Fatalf("jepsen: write %s never acknowledged after retries: %v", key, lastErr)
}

// get reads key through a fresh read transaction, retrying transient routing
// errors and replacing the client if the failure persists.
func (r *jepsenRig) get(key string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			if attempt == 3 {
				r.resetClient()
			}
			time.Sleep(time.Duration(attempt) * 50 * time.Millisecond)
		}

		opCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		txn, err := r.client.Begin(opCtx)
		var got []byte
		if err == nil {
			got, err = txn.Get(opCtx, []byte(key))
		}
		cancel()
		if err == nil {
			return string(got), nil
		}
		lastErr = err
	}
	return "", lastErr
}

// scanAcked reads every jepsen key in one consistent read transaction. It
// retries transient routing errors and replaces the client if they persist.
func (r *jepsenRig) scanAcked() (map[string]string, error) {
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			if attempt == 3 {
				r.resetClient()
			}
			time.Sleep(time.Duration(attempt) * 50 * time.Millisecond)
		}

		opCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		txn, err := r.client.Begin(opCtx)
		var rows []v2client.KV
		if err == nil {
			rows, err = txn.Scan(opCtx, []byte("jepsen-"), []byte("jepsen."), 0)
		}
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		got := make(map[string]string, len(rows))
		for _, row := range rows {
			got[string(row.Key)] = string(row.Value)
		}
		return got, nil
	}
	return nil, lastErr
}

// verifyAckedScan checks every acknowledged write against one snapshot. A
// missing key or a mismatched value is a safety violation.
func (r *jepsenRig) verifyAckedScan() {
	r.t.Helper()
	got, err := r.scanAcked()
	if err != nil {
		r.t.Fatalf("jepsen: scan acknowledged keys: %v (acked writes: %d)", err, len(r.acked))
	}
	for _, a := range r.acked {
		v, ok := got[a.key]
		if !ok {
			r.t.Fatalf("jepsen: acknowledged key %s is missing (acked writes: %d)", a.key, len(r.acked))
		}
		if v != a.value {
			r.t.Fatalf("jepsen: acknowledged key %s has value %q, want %q", a.key, v, a.value)
		}
	}
	r.t.Logf("jepsen: verified %d acknowledged writes by scan", len(r.acked))
}

// verifyAcked checks every acknowledged write with an individual read
// transaction. It is used for the final check after all restarts.
func (r *jepsenRig) verifyAcked() {
	r.t.Helper()
	for _, a := range r.acked {
		got, err := r.get(a.key)
		if err != nil {
			r.t.Fatalf("jepsen: acknowledged key %s is not readable: %v (acked writes: %d)", a.key, err, len(r.acked))
		}
		if got != a.value {
			r.t.Fatalf("jepsen: acknowledged key %s has value %q, want %q", a.key, got, a.value)
		}
	}
	r.t.Logf("jepsen: verified %d acknowledged writes by get", len(r.acked))
}

func TestJepsenKillRestart(t *testing.T) {
	if os.Getenv("JEPSEN") == "" {
		t.Skip("set JEPSEN=1")
	}

	seed := int64(1)
	if s := os.Getenv("JEPSEN_SEED"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("jepsen: invalid JEPSEN_SEED %q: %v", s, err)
		}
		seed = v
	}
	rng := rand.New(rand.NewSource(seed))

	ctx := context.Background()
	started := time.Now()

	metaAddr := v2FreeAddr(t)
	addrs := []string{v2FreeAddr(t), v2FreeAddr(t), v2FreeAddr(t)}

	// The metaserver is a standalone (non-replicated) process for this test:
	// only the KV replicas are killed. The explicit single range makes leader
	// discovery exercise the replicas from the meta range descriptor.
	ms, err := metaserver.New(metaserver.Config{
		Addr: metaAddr,
		Ranges: []meta.Range{{
			ID:       1,
			Start:    nil,
			End:      nil,
			Replicas: append([]string(nil), addrs...),
			Version:  1,
		}},
		Logger: v2Logger(),
	})
	if err != nil {
		t.Fatalf("jepsen: new meta: %v", err)
	}
	if err := ms.Start(ctx); err != nil {
		t.Fatalf("jepsen: start meta: %v", err)
	}
	t.Cleanup(func() { _ = ms.Stop() })

	stopServer := func(srv *kvserver.Server) error {
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return srv.Stop(stopCtx)
	}

	nodes := make([]*v2Node, 0, len(addrs))
	for i, addr := range addrs {
		peers := make([]string, 0, len(addrs)-1)
		for j, p := range addrs {
			if i != j {
				peers = append(peers, p)
			}
		}
		cfg := kvserver.Config{
			ID:                 addr,
			RaftAddr:           addr,
			DataDir:            t.TempDir(),
			Peers:              peers,
			RangeID:            1,
			ElectionTimeoutMin: 100 * time.Millisecond,
			ElectionTimeoutMax: 200 * time.Millisecond,
			HeartbeatInterval:  20 * time.Millisecond,
			CacheSize:          8 << 20,
			MemTableSize:       4 << 20,
			Logger:             v2Logger(),
		}
		srv, err := kvserver.New(cfg)
		if err != nil {
			t.Fatalf("jepsen: new node %s: %v", addr, err)
		}
		if err := srv.Start(ctx); err != nil {
			t.Fatalf("jepsen: start node %s: %v", addr, err)
		}
		n := &v2Node{srv: srv, addr: addr, cfg: cfg, client: v2Dial(t, addr)}
		nodes = append(nodes, n)
		t.Cleanup(func() { _ = stopServer(srv) })
	}

	// Discover the initial leader through Status rather than assuming an ID.
	v2WaitLeader(t, nodes)

	rig := newJepsenRig(t, metaAddr, addrs)

	// maxIndex is the highest commit/apply index observed on the surviving
	// quorum after writing while the victim was down. The restarted replica
	// must reach it before the safety check.
	maxIndex := func(group []*v2Node) uint64 {
		var target uint64
		for _, n := range group {
			st := v2Status(t, n.client)
			if st == nil {
				continue
			}
			if st.CommitIndex > target {
				target = st.CommitIndex
			}
			if st.LastApplied > target {
				target = st.LastApplied
			}
		}
		return target
	}

	waitCaughtUp := func(n *v2Node, target uint64) {
		t.Helper()
		deadline := time.Now().Add(6 * time.Second)
		var lastApplied uint64
		var reachable bool
		for time.Now().Before(deadline) {
			st := v2Status(t, n.client)
			if st != nil {
				reachable = true
				lastApplied = st.LastApplied
				if st.LastApplied >= target {
					return
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("jepsen: restarted node %s did not catch up to index %d (reachable=%v last_applied=%d)",
			n.addr, target, reachable, lastApplied)
	}

	const rounds = 20
	running := append([]*v2Node(nil), nodes...)
	t.Logf("jepsen: seed=%d rounds=%d nodes=%v meta=%s", seed, rounds, addrs, metaAddr)

	for round := 0; round < rounds; round++ {
		// Keep a leader before writing; this discovers leadership via Status.
		leader := v2WaitLeader(t, running)

		// Write on the healthy quorum before the fault.
		for n := 1 + rng.Intn(2); n > 0; n-- {
			rig.putNext()
		}

		// Pick one victim. Half the time target the discovered leader so the
		// test exercises real leader failover as well as follower loss. Never
		// stop more than one node: quorum (2/3) is preserved.
		victim := running[rng.Intn(len(running))]
		if rng.Intn(2) == 0 {
			victim = leader
		}

		if err := stopServer(victim.srv); err != nil {
			t.Fatalf("jepsen: stop node %s: %v", victim.addr, err)
		}
		rest := make([]*v2Node, 0, len(running)-1)
		for _, n := range running {
			if n != victim {
				rest = append(rest, n)
			}
		}
		if len(rest) < 2 {
			t.Fatalf("jepsen: refusing to continue without quorum: %d survivors", len(rest))
		}

		// Wait for the surviving quorum's leader, then write into the fault.
		v2WaitLeader(t, rest)
		for n := 2 + rng.Intn(2); n > 0; n-- {
			rig.putNext()
		}
		target := maxIndex(rest)

		// Restart the same node at the same address with the same DataDir.
		restarted, err := kvserver.New(victim.cfg)
		if err != nil {
			t.Fatalf("jepsen: restart node %s: %v", victim.addr, err)
		}
		if err := restarted.Start(ctx); err != nil {
			t.Fatalf("jepsen: start restarted node %s: %v", victim.addr, err)
		}
		victim.srv = restarted
		victim.client = v2Dial(t, victim.addr)
		t.Cleanup(func() { _ = stopServer(restarted) })

		// A fresh client avoids any stale leader cache from before the fault.
		rig.resetClient()
		waitCaughtUp(victim, target)

		// The invariant is checked after every restart, not only at the end.
		rig.verifyAckedScan()

		running = append(rest, victim)
	}

	// Final check with a freshly discovered leader and a fresh client.
	rig.resetClient()
	v2WaitLeader(t, running)
	rig.verifyAcked()

	if got := len(rig.acked); got < 3*rounds {
		t.Fatalf("jepsen: only %d acknowledged writes for %d rounds, want at least %d", got, rounds, 3*rounds)
	}
	t.Logf("jepsen: completed %d rounds in %s with %d acknowledged writes (seed=%d)",
		rounds, time.Since(started).Round(time.Millisecond), len(rig.acked), seed)
}
