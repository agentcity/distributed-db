package test

// Opt-in Jepsen-style network-partition and clock-skew tests.
//
// These tests never change production code. Instead every KV node binds a
// private "real" address and the test publishes only proxy addresses:
//
//	clients ──> client proxy ──┐
//	                           ├──> node real address (KVService + RaftService)
//	peers   ──> link proxy  ───┘
//
// There is one client-facing proxy per node (used by v2client, status probes
// and the meta range descriptor) and one directed link proxy per ordered node
// pair i->j. Node i configures its Raft peers as the i->j link addresses, so the
// Raft transport dials the test proxy, never the peer directly. Blocking a link
// proxy closes its live connections and immediately closes every new one; the
// production binaries keep their real addresses and are completely unaware of
// the fault injection.
//
// The directed link topology makes asymmetric partitions possible: blocking
// i->j and j->i while leaving i->k and j->k open.
//
// Everything is gated by JEPSEN=1 and seeded by JEPSEN_SEED (default 1).
// Run with:
//
//	JEPSEN=1 go test ./test -run 'TestJepsenPartition|TestJepsenCrossRangePartition|TestJepsenClockSkew' -v -count=1
//
// Without JEPSEN=1 every test in this file skips.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"distributed-db/internal/kvserver"
	"distributed-db/internal/meta"
	"distributed-db/internal/metaserver"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/v2client"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ---------------------------------------------------------------------------
// Gate / seed
// ---------------------------------------------------------------------------

// jepGate skips unless JEPSEN=1 and returns the seed from JEPSEN_SEED.
func jepGate(t *testing.T) int64 {
	t.Helper()
	if os.Getenv("JEPSEN") != "1" {
		t.Skip("set JEPSEN=1 to run the opt-in partition/clock-skew Jepsen tests")
	}
	seed := int64(1)
	if s := os.Getenv("JEPSEN_SEED"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("jepsen: invalid JEPSEN_SEED %q: %v", s, err)
		}
		seed = v
	}
	return seed
}

// ---------------------------------------------------------------------------
// Loopback TCP proxy with a gate
// ---------------------------------------------------------------------------

type jepProxyConn struct {
	client   net.Conn // accepted from the test process
	upstream net.Conn // connected to the node's real address
}

// jepProxy forwards TCP traffic to target. When blocked, existing connections
// are closed and every new connection is accepted and immediately closed. The
// listener stays bound for the whole test, so healing is just a flag flip and
// always reuses the same address.
type jepProxy struct {
	name   string
	target string
	ln     net.Listener

	mu      sync.Mutex
	blocked bool
	closed  bool
	conns   map[*jepProxyConn]struct{}
	wg      sync.WaitGroup
}

func jepNewProxy(name, target string) (*jepProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("jepsen: proxy %s listen: %w", name, err)
	}
	p := &jepProxy{
		name:   name,
		target: target,
		ln:     ln,
		conns:  make(map[*jepProxyConn]struct{}),
	}
	p.wg.Add(1)
	go p.acceptLoop()
	return p, nil
}

func (p *jepProxy) addr() string { return p.ln.Addr().String() }

func (p *jepProxy) isBlocked() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.blocked
}

func (p *jepProxy) acceptLoop() {
	defer p.wg.Done()
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		if p.isBlocked() {
			_ = client.Close()
			continue
		}
		// The target is a loopback gRPC listener; a short timeout keeps a
		// dead node from piling up half-open proxy connections.
		upstream, err := net.DialTimeout("tcp", p.target, 500*time.Millisecond)
		if err != nil {
			_ = client.Close()
			continue
		}
		c := &jepProxyConn{client: client, upstream: upstream}
		if !p.track(c) {
			_ = client.Close()
			_ = upstream.Close()
			continue
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.pump(c)
		}()
	}
}

func (p *jepProxy) track(c *jepProxyConn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.blocked {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *jepProxy) untrack(c *jepProxyConn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
}

// pump copies both directions until either side closes, then closes both.
func (p *jepProxy) pump(c *jepProxyConn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(c.upstream, c.client)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(c.client, c.upstream)
		done <- struct{}{}
	}()
	<-done
	_ = c.client.Close()
	_ = c.upstream.Close()
	<-done
	p.untrack(c)
}

func (p *jepProxy) setBlocked(blocked bool) {
	p.mu.Lock()
	if p.blocked == blocked {
		p.mu.Unlock()
		return
	}
	p.blocked = blocked
	var drop []*jepProxyConn
	if blocked {
		for c := range p.conns {
			drop = append(drop, c)
		}
	}
	p.mu.Unlock()
	// Close outside of the lock: pump's untrack also takes the lock.
	for _, c := range drop {
		_ = c.client.Close()
		_ = c.upstream.Close()
	}
}

func (p *jepProxy) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.blocked = true
	var drop []*jepProxyConn
	for c := range p.conns {
		drop = append(drop, c)
	}
	p.mu.Unlock()
	_ = p.ln.Close()
	for _, c := range drop {
		_ = c.client.Close()
		_ = c.upstream.Close()
	}
	p.wg.Wait()
}

// jepLinkServes probes a link proxy with a real gRPC Status call. The proxy
// forwards to a node's real address, which serves both KVService and
// RaftService, so a successful call proves the link is open; a blocked proxy
// closes the connection and the call fails before the deadline.
func jepLinkServes(p *jepProxy) bool {
	cc, err := grpc.NewClient(p.addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return false
	}
	defer func() { _ = cc.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	_, err = pbv2.NewKVServiceClient(cc).Status(ctx, &pbv2.StatusRequest{})
	return err == nil
}

// ---------------------------------------------------------------------------
// Cluster with per-link partition control
// ---------------------------------------------------------------------------

const jepNodeCount = 3

type jepRangeSpec struct {
	start []byte
	end   []byte
}

type jepCluster struct {
	t    *testing.T
	n    int
	meta string

	realAddrs   []string
	clientAddrs []string
	nodes       []*kvserver.Server
	clients     []pbv2.KVServiceClient

	links   map[[2]int]*jepProxy
	proxies []*jepProxy
	ms      *metaserver.Server
}

func jepStartCluster(t *testing.T, specs []jepRangeSpec) *jepCluster {
	t.Helper()
	return jepStartClusterN(t, specs, jepNodeCount)
}

// jepStartClusterN builds an n-replica cluster that hosts every range in specs
// on every node. All addresses published to clients, meta and peers are proxy
// addresses; the nodes listen on private real addresses.
func jepStartClusterN(t *testing.T, specs []jepRangeSpec, n int) *jepCluster {
	t.Helper()

	c := &jepCluster{
		t:       t,
		n:       n,
		meta:    v2FreeAddr(t),
		links:   make(map[[2]int]*jepProxy),
		clients: make([]pbv2.KVServiceClient, n),
		nodes:   make([]*kvserver.Server, n),
	}
	// Allocate the node data directories before registering the cluster
	// cleanup. t.Cleanup runs LIFO, so the engines must be stopped (cluster
	// cleanup) before t.TempDir removes the directories.
	dataDirs := make([]string, n)
	for i := 0; i < n; i++ {
		c.realAddrs = append(c.realAddrs, v2FreeAddr(t))
		dataDirs[i] = t.TempDir()
	}

	// Client-facing proxies are always open; they are how clients, status
	// probes and meta range descriptors reach a node.
	for i := 0; i < n; i++ {
		p, err := jepNewProxy(fmt.Sprintf("client-%d", i), c.realAddrs[i])
		if err != nil {
			t.Fatal(err)
		}
		c.proxies = append(c.proxies, p)
		c.clientAddrs = append(c.clientAddrs, p.addr())
	}
	// Directed raft link proxies: links[i][j] is the address node i uses for
	// its connection to node j.
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if i == j {
				continue
			}
			p, err := jepNewProxy(fmt.Sprintf("link-%d-to-%d", i, j), c.realAddrs[j])
			if err != nil {
				t.Fatal(err)
			}
			c.links[[2]int{i, j}] = p
			c.proxies = append(c.proxies, p)
		}
	}

	// Cleanup order: stop nodes, then proxies, then meta.
	t.Cleanup(func() {
		for _, srv := range c.nodes {
			if srv == nil {
				continue
			}
			stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = srv.Stop(stopCtx)
			cancel()
		}
		for _, p := range c.proxies {
			p.close()
		}
		if c.ms != nil {
			_ = c.ms.Stop()
		}
	})

	metaRanges := make([]meta.Range, 0, len(specs))
	for ri, sp := range specs {
		metaRanges = append(metaRanges, meta.Range{
			ID:       uint64(ri + 1),
			Start:    sp.start,
			End:      sp.end,
			Replicas: append([]string(nil), c.clientAddrs...),
			Version:  1,
		})
	}
	ms, err := metaserver.New(metaserver.Config{
		Addr:   c.meta,
		Ranges: metaRanges,
		Logger: v2Logger(),
	})
	if err != nil {
		t.Fatalf("jepsen: new meta: %v", err)
	}
	if err := ms.Start(context.Background()); err != nil {
		t.Fatalf("jepsen: start meta: %v", err)
	}
	c.ms = ms

	ctx := context.Background()
	for i := 0; i < n; i++ {
		// Voter identities are stable client-facing addresses. Raft traffic is
		// routed through per-source directed link proxies via PeerAddrs, so an
		// asymmetric partition does not require per-source voter IDs.
		peers := make([]string, 0, n-1)
		peerAddrs := make(map[string]string, n-1)
		for j := 0; j < n; j++ {
			if i != j {
				peers = append(peers, c.clientAddrs[j])
				peerAddrs[c.clientAddrs[j]] = c.links[[2]int{i, j}].addr()
			}
		}
		rcs := make([]kvserver.RangeConfig, 0, len(specs))
		for ri, sp := range specs {
			rcs = append(rcs, kvserver.RangeConfig{
				ID:    uint64(ri + 1),
				Start: sp.start,
				End:   sp.end,
				Peers: append([]string(nil), peers...),
			})
		}
		cfg := kvserver.Config{
			// The node ID must be a dialable address because Status leader
			// hints are fed back into v2client as dial targets.
			ID:        c.clientAddrs[i],
			RaftAddr:  c.realAddrs[i],
			DataDir:   dataDirs[i],
			Ranges:    rcs,
			PeerAddrs: peerAddrs,
			// Conservative timings: the test cluster shares a machine with
			// the rest of the suite and syncs every Raft batch, so aggressive
			// elections would produce spurious leader changes.
			ElectionTimeoutMin: 250 * time.Millisecond,
			ElectionTimeoutMax: 500 * time.Millisecond,
			HeartbeatInterval:  50 * time.Millisecond,
			CacheSize:          8 << 20,
			MemTableSize:       4 << 20,
			Logger:             v2Logger(),
		}
		srv, err := kvserver.New(cfg)
		if err != nil {
			t.Fatalf("jepsen: new node %d: %v", i, err)
		}
		if err := srv.Start(ctx); err != nil {
			t.Fatalf("jepsen: start node %d: %v", i, err)
		}
		c.nodes[i] = srv
		c.clients[i] = v2Dial(t, c.clientAddrs[i])
	}
	t.Logf("jepsen: cluster up meta=%s client=%v real=%v", c.meta, c.clientAddrs, c.realAddrs)
	return c
}

func (c *jepCluster) status(i int, rangeID uint64) *pbv2.StatusResponse {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	st, err := c.clients[i].Status(ctx, &pbv2.StatusRequest{RangeId: rangeID})
	if err != nil {
		return nil
	}
	return st
}

// waitLeader returns the node index whose rangeID replica is leader. exclude,
// when non-empty, must not be returned (used after isolating the old leader).
func (c *jepCluster) waitLeader(rangeID uint64, exclude string, timeout time.Duration) int {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		var b strings.Builder
		for i := 0; i < c.n; i++ {
			st := c.status(i, rangeID)
			if st == nil {
				fmt.Fprintf(&b, " %d=down", i)
				continue
			}
			fmt.Fprintf(&b, " %d=%s(term %d, leader %s)", i, st.GetRole(), st.GetTerm(), st.GetLeader())
			if strings.EqualFold(st.GetRole(), "leader") && c.clientAddrs[i] != exclude {
				return i
			}
		}
		last = b.String()
		time.Sleep(25 * time.Millisecond)
	}
	c.t.Fatalf("jepsen: no leader for range %d within %s (exclude=%q); last:%s",
		rangeID, timeout, exclude, last)
	return -1
}

// leaderNow returns the current leader index, or -1 if none is visible.
func (c *jepCluster) leaderNow(rangeID uint64) int {
	for i := 0; i < c.n; i++ {
		st := c.status(i, rangeID)
		if st != nil && strings.EqualFold(st.GetRole(), "leader") {
			return i
		}
	}
	return -1
}

// waitStableLeader waits for a leader that keeps the same term for a full
// election window. A freshly started Raft group can briefly show a leader that
// is then replaced by a higher-term candidate; choosing that transient leader
// as a fault target would make the scenario nondeterministic.
func (c *jepCluster) waitStableLeader(rangeID uint64, timeout time.Duration) int {
	c.t.Helper()
	const stableFor = 600 * time.Millisecond
	deadline := time.Now().Add(timeout)
	candidate, term := -1, uint64(0)
	var since time.Time
	for time.Now().Before(deadline) {
		idx := c.leaderNow(rangeID)
		if idx < 0 {
			candidate = -1
			time.Sleep(25 * time.Millisecond)
			continue
		}
		st := c.status(idx, rangeID)
		if st == nil {
			candidate = -1
			time.Sleep(25 * time.Millisecond)
			continue
		}
		if idx != candidate || st.GetTerm() != term {
			candidate, term, since = idx, st.GetTerm(), time.Now()
		} else if time.Since(since) >= stableFor {
			return idx
		}
		time.Sleep(25 * time.Millisecond)
	}
	c.t.Fatalf("jepsen: no stable leader for range %d within %s", rangeID, timeout)
	return -1
}

// maxCommit returns the highest commit/apply index for rangeID across nodes.
func (c *jepCluster) maxCommit(rangeID uint64) uint64 {
	var target uint64
	for i := 0; i < c.n; i++ {
		st := c.status(i, rangeID)
		if st == nil {
			continue
		}
		if st.GetCommitIndex() > target {
			target = st.GetCommitIndex()
		}
		if st.GetLastApplied() > target {
			target = st.GetLastApplied()
		}
	}
	return target
}

// waitApplied waits until node i has applied at least target for rangeID.
func (c *jepCluster) waitApplied(i int, rangeID uint64, target uint64, timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	var last uint64
	for time.Now().Before(deadline) {
		st := c.status(i, rangeID)
		if st != nil {
			last = st.GetLastApplied()
			if last >= target {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	c.t.Fatalf("jepsen: node %d range %d did not catch up to %d within %s (last_applied=%d)",
		i, rangeID, target, timeout, last)
}

// onLeader retries a low-level call until a current leader accepts it. It is
// used for admin/probe calls that bypass v2client leader discovery. A call that
// returns "not leader" was never proposed, so retrying it is safe.
func (c *jepCluster) onLeader(rangeID uint64, what string, fn func(cl pbv2.KVServiceClient) (done bool, msg string)) {
	c.t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	var lastMsg string
	for {
		if idx := c.leaderNow(rangeID); idx >= 0 {
			done, msg := fn(c.clients[idx])
			if done {
				return
			}
			lastMsg = msg
			if msg != "not leader" {
				c.t.Fatalf("jepsen: %s: %s", what, msg)
			}
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("jepsen: %s: no leader accepted the call (%s)", what, lastMsg)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (c *jepCluster) isolateNode(i int) {
	for key, p := range c.links {
		if key[0] == i || key[1] == i {
			p.setBlocked(true)
		}
	}
}

// assertIsolated proves every Raft link touching i is really blocked. The
// client-facing proxy is deliberately not checked: it stays open so status and
// heal observation still work.
func (c *jepCluster) assertIsolated(i int) {
	c.t.Helper()
	for j := 0; j < c.n; j++ {
		if j == i {
			continue
		}
		if jepLinkServes(c.links[[2]int{i, j}]) || jepLinkServes(c.links[[2]int{j, i}]) {
			c.t.Fatalf("jepsen: node %d is still connected to node %d after isolation", i, j)
		}
	}
}

func (c *jepCluster) blockPair(a, b int) {
	c.links[[2]int{a, b}].setBlocked(true)
	c.links[[2]int{b, a}].setBlocked(true)
}

func (c *jepCluster) heal() {
	for _, p := range c.links {
		p.setBlocked(false)
	}
}

// roleOf returns the lower-cased role for node i and rangeID, or "" if the
// node is unreachable.
func (c *jepCluster) roleOf(i int, rangeID uint64) string {
	st := c.status(i, rangeID)
	if st == nil {
		return ""
	}
	return strings.ToLower(st.GetRole())
}

func jepIsLeader(role string) bool { return role == "leader" }

// ---------------------------------------------------------------------------
// Client rig: acknowledged writes and retrying reads
// ---------------------------------------------------------------------------

type jepAck struct {
	key   string
	value string
}

type jepRig struct {
	t      *testing.T
	meta   string
	addrs  []string
	client *v2client.Client
	acked  []jepAck
	seq    int
	prefix string
}

func jepNewRig(t *testing.T, metaAddr string, addrs []string, prefix string) *jepRig {
	t.Helper()
	r := &jepRig{t: t, meta: metaAddr, addrs: append([]string(nil), addrs...), prefix: prefix}
	r.reset()
	t.Cleanup(r.close)
	return r
}

func (r *jepRig) close() {
	if r.client != nil {
		_ = r.client.Close()
		r.client = nil
	}
}

func (r *jepRig) reset() {
	if r.client != nil {
		_ = r.client.Close()
		r.client = nil
	}
	c, err := v2client.New(r.meta, r.addrs)
	if err != nil {
		r.t.Fatalf("jepsen: new v2 client: %v", err)
	}
	r.client = c
}

func (r *jepRig) allocTS(timeout time.Duration) (uint64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return r.client.AllocTS(ctx)
}

// put retries a fixed key/value until it is acknowledged or budget expires.
// Only a nil Commit error counts as acknowledged.
func (r *jepRig) put(key, value string, timeout, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	var lastErr error
	for {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		txn, err := r.client.Begin(ctx)
		if err == nil {
			txn.Set([]byte(key), []byte(value))
			err = txn.Commit(ctx)
		}
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return lastErr
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// write writes a fresh key and records it in acked on success.
func (r *jepRig) write(timeout, budget time.Duration) (jepAck, bool) {
	key := fmt.Sprintf("%s-%04d", r.prefix, r.seq+1)
	value := fmt.Sprintf("v-%04d", r.seq+1)
	r.seq++
	if err := r.put(key, value, timeout, budget); err != nil {
		r.t.Logf("jepsen: write %s not acknowledged within %s: %v", key, budget, err)
		return jepAck{}, false
	}
	a := jepAck{key: key, value: value}
	r.acked = append(r.acked, a)
	return a, true
}

// get reads key through a fresh snapshot. found=false with nil error means the
// key does not exist.
func (r *jepRig) get(key string, timeout time.Duration) (value string, found bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	txn, err := r.client.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	got, err := txn.Get(ctx, []byte(key))
	_ = txn.Rollback(ctx)
	if errors.Is(err, v2client.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(got), true, nil
}

// waitValue waits until key reads back as want.
func (r *jepRig) waitValue(key, want string, timeout time.Duration) {
	r.t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	var lastErr error
	for time.Now().Before(deadline) {
		got, found, err := r.get(key, 800*time.Millisecond)
		if err == nil && found && got == want {
			return
		}
		last, lastErr = got, err
		time.Sleep(40 * time.Millisecond)
	}
	r.t.Fatalf("jepsen: key %s did not converge to %q within %s (last=%q, err=%v)", key, want, timeout, last, lastErr)
}

// verifyAcked waits, per acknowledged write, until the healed cluster returns
// the acknowledged value. This is the "no acknowledged write loss" invariant.
func (r *jepRig) verifyAcked(perKey time.Duration) {
	r.t.Helper()
	for _, a := range r.acked {
		r.waitValue(a.key, a.value, perKey)
	}
	r.t.Logf("jepsen: verified %d acknowledged writes after heal", len(r.acked))
}

// ---------------------------------------------------------------------------
// Scenario 1: minority follower isolation + heal
// ---------------------------------------------------------------------------

func TestJepsenPartitionMinorityFollower(t *testing.T) {
	seed := jepGate(t)
	started := time.Now()
	rng := rand.New(rand.NewSource(seed))

	c := jepStartCluster(t, []jepRangeSpec{{}})
	leader := c.waitStableLeader(1, 8*time.Second)
	rig := jepNewRig(t, c.meta, c.clientAddrs, fmt.Sprintf("jepsen-min-%d", seed))

	for i := 0; i < 3; i++ {
		if _, ok := rig.write(2*time.Second, 5*time.Second); !ok {
			t.Fatalf("jepsen: healthy quorum did not acknowledge write %d", i)
		}
	}

	// Pick a follower, never the leader: the cluster must retain a 2/3 quorum.
	var followers []int
	for i := 0; i < c.n; i++ {
		if i != leader && c.roleOf(i, 1) == "follower" {
			followers = append(followers, i)
		}
	}
	if len(followers) == 0 {
		t.Fatalf("jepsen: no follower to isolate (leader=%d)", leader)
	}
	victim := followers[rng.Intn(len(followers))]
	c.isolateNode(victim)
	c.assertIsolated(victim)
	t.Logf("jepsen: isolated follower %d (%s); leader %d stays with quorum", victim, c.clientAddrs[victim], leader)

	// The isolated replica must not accept writes: it can never reach the
	// majority needed to commit one. Give it one election window to lose the
	// lease in case it was promoted between picking the follower and closing
	// the links; an isolated replica may not remain leader.
	leaderDeadline := time.Now().Add(2 * time.Second)
	for jepIsLeader(c.roleOf(victim, 1)) && time.Now().Before(leaderDeadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if role := c.roleOf(victim, 1); jepIsLeader(role) {
		t.Fatalf("jepsen: isolated replica %d remained leader without quorum", victim)
	}
	probeKey := fmt.Sprintf("%s-stale-probe", rig.prefix)
	probeResp, probeErr := jepProbeWrite(c.clients[victim], probeKey, "should-not-commit")
	if probeErr == nil && probeResp.GetSuccess() {
		t.Fatalf("jepsen: isolated follower acknowledged a write without quorum")
	}
	t.Logf("jepsen: isolated follower rejected the direct write (err=%v, msg=%q)", probeErr, probeResp.GetError())

	// The majority (leader + other follower) still serves writes.
	for i := 0; i < 3; i++ {
		if _, ok := rig.write(2*time.Second, 5*time.Second); !ok {
			t.Fatalf("jepsen: quorum lost write %d while only a follower was isolated", i)
		}
	}

	// Heal and require the isolated replica to catch up to the quorum.
	c.heal()
	target := c.maxCommit(1)
	c.waitApplied(victim, 1, target, 8*time.Second)
	last, ok := rig.write(2*time.Second, 5*time.Second)
	if !ok {
		t.Fatalf("jepsen: post-heal write was not acknowledged")
	}
	c.waitApplied(victim, 1, c.maxCommit(1), 8*time.Second)
	if role := c.roleOf(victim, 1); role == "" {
		t.Fatalf("jepsen: healed follower is unreachable")
	}

	rig.verifyAcked(4 * time.Second)
	t.Logf("jepsen: minority follower scenario passed in %s with %d acked writes (seed=%d, last=%s)",
		time.Since(started).Round(time.Millisecond), len(rig.acked), seed, last.key)
}

// ---------------------------------------------------------------------------
// Scenario 2: leader isolation, new election, stale-leader safety
// ---------------------------------------------------------------------------

func TestJepsenPartitionLeaderIsolation(t *testing.T) {
	seed := jepGate(t)
	started := time.Now()

	c := jepStartCluster(t, []jepRangeSpec{{}})
	oldLeader := c.waitStableLeader(1, 8*time.Second)
	before := c.status(oldLeader, 1)
	if before == nil {
		t.Fatalf("jepsen: leader status unavailable")
	}
	oldTerm := before.GetTerm()
	rig := jepNewRig(t, c.meta, c.clientAddrs, fmt.Sprintf("jepsen-lead-%d", seed))

	for i := 0; i < 3; i++ {
		if _, ok := rig.write(2*time.Second, 5*time.Second); !ok {
			t.Fatalf("jepsen: pre-partition write %d was not acknowledged", i)
		}
	}

	c.isolateNode(oldLeader)
	c.assertIsolated(oldLeader)
	if st := c.status(oldLeader, 1); st != nil {
		t.Logf("jepsen: isolated old leader %d (%s, term %d, role %s)",
			oldLeader, c.clientAddrs[oldLeader], st.GetTerm(), st.GetRole())
	}

	// Probe the old leader immediately: it may still believe it is leader
	// until its quorum lease expires. A stale leader must never acknowledge a
	// write without a quorum. The write may hang until the RPC deadline or be
	// rejected once the lease expires; a nil Success would be a violation.
	staleKey := fmt.Sprintf("%s-stale-leader", rig.prefix)
	probeResp, probeErr := jepProbeWrite(c.clients[oldLeader], staleKey, "must-not-commit")
	if probeErr == nil && probeResp.GetSuccess() {
		t.Fatalf("jepsen: isolated stale leader acknowledged a write without quorum")
	}
	t.Logf("jepsen: stale leader rejected the direct write (err=%v, msg=%q)", probeErr, probeResp.GetError())

	newLeader := c.waitLeader(1, c.clientAddrs[oldLeader], 6*time.Second)
	newStatus := c.status(newLeader, 1)
	if newStatus == nil {
		t.Fatalf("jepsen: new leader %d status unavailable", newLeader)
	}
	if newStatus.GetTerm() <= oldTerm {
		t.Fatalf("jepsen: new leader term %d did not advance past old term %d", newStatus.GetTerm(), oldTerm)
	}
	t.Logf("jepsen: elected new leader %d (%s, term %d)", newLeader, c.clientAddrs[newLeader], newStatus.GetTerm())

	// Writes must still be acknowledged through the newly elected leader even
	// while the old leader is partitioned. A fresh client avoids the cached
	// leader hint for the old term.
	rig.reset()
	for i := 0; i < 3; i++ {
		if _, ok := rig.write(1500*time.Millisecond, 8*time.Second); !ok {
			t.Fatalf("jepsen: new leader did not acknowledge write %d during isolation", i)
		}
	}

	c.heal()
	// The old leader must step down and catch up.
	deadline := time.Now().Add(8 * time.Second)
	for {
		role := c.roleOf(oldLeader, 1)
		if role != "" && role != "leader" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("jepsen: old leader did not step down after heal (role=%q)", role)
		}
		time.Sleep(25 * time.Millisecond)
	}
	target := c.maxCommit(1)
	c.waitApplied(oldLeader, 1, target, 8*time.Second)

	// The acknowledged-write invariant spans the whole run: everything acked
	// before, during, and after the fault must still be readable.
	rig.reset()
	rig.verifyAcked(4 * time.Second)
	t.Logf("jepsen: leader isolation scenario passed in %s with %d acked writes (seed=%d)",
		time.Since(started).Round(time.Millisecond), len(rig.acked), seed)
}

// ---------------------------------------------------------------------------
// Scenario 3: asymmetric partition A<->B blocked, A<->C and B<->C open
// ---------------------------------------------------------------------------

func TestJepsenPartitionAsymmetric(t *testing.T) {
	seed := jepGate(t)
	started := time.Now()

	c := jepStartCluster(t, []jepRangeSpec{{}})
	leader := c.waitStableLeader(1, 8*time.Second)
	rig := jepNewRig(t, c.meta, c.clientAddrs, fmt.Sprintf("jepsen-asym-%d", seed))

	others := make([]int, 0, 2)
	for i := 0; i < c.n; i++ {
		if i != leader {
			others = append(others, i)
		}
	}
	a, b, cc := leader, others[0], others[1]

	for i := 0; i < 2; i++ {
		if _, ok := rig.write(2*time.Second, 5*time.Second); !ok {
			t.Fatalf("jepsen: pre-partition write %d was not acknowledged", i)
		}
	}

	c.blockPair(a, b)
	t.Logf("jepsen: asymmetric partition: %d<->%d blocked, %d<->%d open", a, b, a, cc)

	// Verify the fault injector itself: both directions of A<->B must be
	// blocked and the A<->C / B<->C links must still serve traffic.
	if jepLinkServes(c.links[[2]int{a, b}]) || jepLinkServes(c.links[[2]int{b, a}]) {
		t.Fatalf("jepsen: A<->B link is not blocked")
	}
	for _, pair := range [][2]int{{a, cc}, {cc, a}, {b, cc}, {cc, b}} {
		if !jepLinkServes(c.links[pair]) {
			t.Fatalf("jepsen: link %d->%d should stay open", pair[0], pair[1])
		}
	}

	// Asymmetric partitions can cause term churn; writes may fail transiently.
	// Liveness is not asserted for every write, but the cluster must make
	// progress on at least one side of the partition.
	ackedDuring := 0
	for i := 0; i < 4; i++ {
		if _, ok := rig.write(1200*time.Millisecond, 5*time.Second); ok {
			ackedDuring++
		}
	}
	if ackedDuring == 0 {
		t.Fatalf("jepsen: no write made progress during the asymmetric partition")
	}
	t.Logf("jepsen: %d/4 writes acknowledged during the asymmetric partition", ackedDuring)

	c.heal()
	c.waitStableLeader(1, 8*time.Second)
	// All three replicas must converge and every acknowledged write must be
	// present, including those written while the link was broken.
	target := c.maxCommit(1)
	for i := 0; i < c.n; i++ {
		c.waitApplied(i, 1, target, 8*time.Second)
	}
	rig.reset()
	rig.verifyAcked(4 * time.Second)
	for i := 0; i < c.n; i++ {
		if role := c.roleOf(i, 1); role == "" {
			t.Fatalf("jepsen: node %d unreachable after asymmetric heal", i)
		}
	}
	t.Logf("jepsen: asymmetric scenario passed in %s with %d acked writes (seed=%d)",
		time.Since(started).Round(time.Millisecond), len(rig.acked), seed)
}

// ---------------------------------------------------------------------------
// Scenario 4: cross-range 2PC atomicity across a minority partition
// ---------------------------------------------------------------------------

type jepPairAck struct {
	aKey, aValue string
	zKey, zValue string
}

func TestJepsenCrossRangePartition(t *testing.T) {
	seed := jepGate(t)
	started := time.Now()

	// Two ranges, both replicated 3x: [nil,"m") and ["m",nil). Range 1 gets
	// keys starting with "a-", range 2 gets keys starting with "z-".
	c := jepStartCluster(t, []jepRangeSpec{
		{end: []byte("m")},
		{start: []byte("m")},
	})
	c.waitStableLeader(1, 8*time.Second)
	c.waitStableLeader(2, 8*time.Second)

	rig := jepNewRig(t, c.meta, c.clientAddrs, fmt.Sprintf("jepsen-x-%d", seed))
	var pairs []jepPairAck

	seedPair := func(i int, value string) (string, string) {
		aKey := fmt.Sprintf("a-jepsen-%d-%02d", seed, i)
		zKey := fmt.Sprintf("z-jepsen-%d-%02d", seed, i)
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		txn, err := rig.client.Begin(ctx)
		if err != nil {
			t.Fatalf("jepsen: begin cross-range seed %d: %v", i, err)
		}
		txn.Set([]byte(aKey), []byte(value))
		txn.Set([]byte(zKey), []byte(value))
		if err := txn.Commit(ctx); err != nil {
			t.Fatalf("jepsen: seed cross-range pair %d: %v", i, err)
		}
		return aKey, zKey
	}

	for i := 0; i < 4; i++ {
		value := fmt.Sprintf("seed-%02d", i)
		aKey, zKey := seedPair(i, value)
		pairs = append(pairs, jepPairAck{aKey: aKey, aValue: value, zKey: zKey, zValue: value})
	}
	// Cross-range seed writes were acknowledged above; verify both sides are
	// visible at one snapshot before injecting the fault.
	for _, p := range pairs {
		if err := jepWaitPair(rig, p.aKey, p.zKey, p.aValue, p.zValue, 4*time.Second); err != nil {
			t.Fatalf("jepsen: seeded pair not visible: %v", err)
		}
	}

	// Isolate a replica that is a follower in both ranges. Both ranges keep a
	// 2/3 quorum, so cross-range transactions must still be able to commit;
	// this exercises 2PC while a minority replica is unreachable.
	victim := -1
	for i := 0; i < c.n; i++ {
		r1, r2 := c.roleOf(i, 1), c.roleOf(i, 2)
		if r1 == "follower" && r2 == "follower" {
			victim = i
			break
		}
	}
	if victim < 0 {
		victim = 0
		for i := 0; i < c.n; i++ {
			if c.roleOf(i, 1) != "leader" {
				victim = i
				break
			}
		}
	}
	c.isolateNode(victim)
	c.assertIsolated(victim)
	t.Logf("jepsen: cross-range minority isolation of node %d", victim)

	for i := 4; i < 7; i++ {
		value := fmt.Sprintf("during-%02d", i)
		aKey, zKey := seedPair(i, value)
		pairs = append(pairs, jepPairAck{aKey: aKey, aValue: value, zKey: zKey, zValue: value})
	}

	c.heal()
	c.waitStableLeader(1, 8*time.Second)
	c.waitStableLeader(2, 8*time.Second)
	c.waitApplied(victim, 1, c.maxCommit(1), 8*time.Second)
	c.waitApplied(victim, 2, c.maxCommit(2), 8*time.Second)

	// Invariant: every acknowledged cross-range transaction is fully visible
	// on both sides after heal. Reading both keys at one start timestamp makes
	// one-sided visibility a hard failure.
	for _, p := range pairs {
		if err := jepWaitPair(rig, p.aKey, p.zKey, p.aValue, p.zValue, 6*time.Second); err != nil {
			t.Fatalf("jepsen: acknowledged cross-range pair partially visible: %v", err)
		}
	}

	// Invariant: a cross-range transaction that fails before commit (here the
	// secondary prewrite hits a foreign lock) must leave neither side visible.
	baseA, baseZ := fmt.Sprintf("a-jepsen-%d-conflict", seed), fmt.Sprintf("z-jepsen-%d-conflict", seed)
	if err := rig.put(baseA, "base-a", 2*time.Second, 4*time.Second); err != nil {
		t.Fatalf("jepsen: seed conflict key A: %v", err)
	}
	if err := rig.put(baseZ, "base-z", 2*time.Second, 4*time.Second); err != nil {
		t.Fatalf("jepsen: seed conflict key Z: %v", err)
	}

	// Hold a foreign lock on the secondary key with a direct low-level prewrite.
	lockTS, err := rig.allocTS(2 * time.Second)
	if err != nil {
		t.Fatalf("jepsen: alloc lock ts: %v", err)
	}
	c.onLeader(2, "hold secondary lock", func(cl pbv2.KVServiceClient) (bool, string) {
		resp, err := cl.Prewrite(context.Background(), &pbv2.PrewriteRequest{
			StartTs: lockTS,
			Primary: []byte(baseZ),
			Mutations: []*pbv2.Mutation{{
				Key:   []byte(baseZ),
				Value: []byte("held-by-other-txn"),
			}},
		})
		if err != nil {
			return false, err.Error()
		}
		return resp.GetSuccess() && resp.GetError() == "", resp.GetError()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	badTxn, err := rig.client.Begin(ctx)
	if err != nil {
		cancel()
		t.Fatalf("jepsen: begin conflicting cross-range txn: %v", err)
	}
	badTxn.Set([]byte(baseA), []byte("must-not-commit-a"))
	badTxn.Set([]byte(baseZ), []byte("must-not-commit-z"))
	badErr := badTxn.Commit(ctx)
	cancel()
	if badErr == nil {
		t.Fatalf("jepsen: conflicting cross-range transaction unexpectedly committed")
	}
	t.Logf("jepsen: conflicting cross-range transaction rejected: %v", badErr)

	// Release the foreign lock and require both sides to remain at their base
	// values ("neither visible").
	c.onLeader(2, "release secondary lock", func(cl pbv2.KVServiceClient) (bool, string) {
		resp, err := cl.Rollback(context.Background(), &pbv2.RollbackRequest{
			StartTs: lockTS,
			Keys:    [][]byte{[]byte(baseZ)},
		})
		if err != nil {
			return false, err.Error()
		}
		return resp.GetSuccess() && resp.GetError() == "", resp.GetError()
	})
	if err := jepWaitPair(rig, baseA, baseZ, "base-a", "base-z", 6*time.Second); err != nil {
		t.Fatalf("jepsen: failed cross-range txn left partial visibility: %v", err)
	}

	// And a healthy cross-range transaction after all the churn still works.
	postA, postZ := seedPair(99, "post-heal")
	if err := jepWaitPair(rig, postA, postZ, "post-heal", "post-heal", 6*time.Second); err != nil {
		t.Fatalf("jepsen: post-heal cross-range transaction not visible: %v", err)
	}

	// Invariant: a crash point after the primary range committed but before
	// the secondary range committed must end with BOTH sides visible, never
	// one-sided. The state is created deterministically with low-level RPCs
	// while a minority replica is partitioned:
	//
	//	1. prewrite the secondary key (range 2) with a primary key in range 1;
	//	2. commit the primary range only;
	//	3. let the secondary lock outlive its TTL while a replica is down,
	//	   which is when a local-only resolver would wrongly roll it back;
	//	4. heal and read both keys through a client, which resolves the
	//	   secondary lock by asking the primary range.
	partialA := fmt.Sprintf("a-jepsen-%d-partial", seed)
	partialZ := fmt.Sprintf("z-jepsen-%d-partial", seed)
	l1, l2 := c.leaderNow(1), c.leaderNow(2)
	partialVictim := -1
	for i := 0; i < c.n; i++ {
		if i != l1 && i != l2 {
			partialVictim = i
			break
		}
	}
	if partialVictim < 0 {
		partialVictim = (l1 + 1) % c.n
	}
	c.isolateNode(partialVictim)
	c.assertIsolated(partialVictim)
	c.waitStableLeader(1, 8*time.Second)
	c.waitStableLeader(2, 8*time.Second)

	startTS, err := rig.allocTS(2 * time.Second)
	if err != nil {
		t.Fatalf("jepsen: alloc partial startTS: %v", err)
	}
	commitTS, err := rig.allocTS(2 * time.Second)
	if err != nil {
		t.Fatalf("jepsen: alloc partial commitTS: %v", err)
	}
	if commitTS <= startTS {
		t.Fatalf("jepsen: partial commitTS %d <= startTS %d", commitTS, startTS)
	}
	c.onLeader(2, "prewrite secondary of partial txn", func(cl pbv2.KVServiceClient) (bool, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		resp, err := cl.Prewrite(ctx, &pbv2.PrewriteRequest{
			StartTs: startTS,
			Primary: []byte(partialA),
			Mutations: []*pbv2.Mutation{{
				Key:   []byte(partialZ),
				Value: []byte("partial-right"),
			}},
		})
		if err != nil {
			return false, err.Error()
		}
		return resp.GetSuccess() && resp.GetError() == "", resp.GetError()
	})
	c.onLeader(1, "commit primary of partial txn", func(cl pbv2.KVServiceClient) (bool, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		resp, err := cl.CommitSingle(ctx, &pbv2.CommitSingleRequest{
			StartTs:  startTS,
			CommitTs: commitTS,
			Primary:  []byte(partialA),
			Mutations: []*pbv2.Mutation{{
				Key:   []byte(partialA),
				Value: []byte("partial-left"),
			}},
		})
		if err != nil {
			return false, err.Error()
		}
		return resp.GetSuccess() && resp.GetError() == "", resp.GetError()
	})
	t.Logf("jepsen: created crash point after primary commit (a=%s committed, z=%s locked)", partialA, partialZ)

	// 3s lock TTL + one resolver tick. The background resolver must leave the
	// cross-range secondary lock alone; resolving it is the client's job.
	time.Sleep(3500 * time.Millisecond)

	c.heal()
	c.waitStableLeader(1, 8*time.Second)
	c.waitStableLeader(2, 8*time.Second)
	if err := jepWaitPair(rig, partialA, partialZ, "partial-left", "partial-right", 15*time.Second); err != nil {
		t.Fatalf("jepsen: crash point after primary commit became partially visible: %v", err)
	}
	t.Logf("jepsen: crash-point pair recovered atomically after heal")

	t.Logf("jepsen: cross-range scenario passed in %s with %d pairs (seed=%d)",
		time.Since(started).Round(time.Millisecond), len(pairs), seed)
}

// jepWaitPair reads both keys inside one transaction (same snapshot) and waits
// until both carry the wanted values. A missing or one-sided value is an
// atomicity violation.
func jepWaitPair(rig *jepRig, aKey, zKey, wantA, wantZ string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		txn, err := rig.client.Begin(ctx)
		if err != nil {
			cancel()
			last = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		gotA, errA := txn.Get(ctx, []byte(aKey))
		gotZ, errZ := txn.Get(ctx, []byte(zKey))
		_ = txn.Rollback(ctx)
		cancel()
		if errA == nil && errZ == nil && string(gotA) == wantA && string(gotZ) == wantZ {
			return nil
		}
		last = fmt.Errorf("a=%q/%v z=%q/%v", gotA, errA, gotZ, errZ)
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("pair (%s,%s) not (%q,%q) within %s: last %v", aKey, zKey, wantA, wantZ, timeout, last)
}

// ---------------------------------------------------------------------------
// Scenario 5: clock skew
// ---------------------------------------------------------------------------

// jepProbeWrite sends one low-level CommitSingle to a specific node. The caller
// treats a nil error with Success=true as an acknowledged write. The timestamps
// only need to be internally consistent: commitTS > startTS.
func jepProbeWrite(cl pbv2.KVServiceClient, key, value string) (*pbv2.CommitSingleResponse, error) {
	now := uint64(time.Now().UnixMilli()) << 18
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	return cl.CommitSingle(ctx, &pbv2.CommitSingleRequest{
		StartTs:  now,
		CommitTs: now + 1,
		Primary:  []byte(key),
		Mutations: []*pbv2.Mutation{{
			Key:   []byte(key),
			Value: []byte(value),
		}},
	})
}

func jepDirectGet(t *testing.T, cl pbv2.KVServiceClient, key string, ts uint64) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := cl.Get(ctx, &pbv2.GetRequest{Key: []byte(key), Ts: ts})
	if err != nil {
		t.Fatalf("jepsen: direct get %s at ts %d: %v", key, ts, err)
	}
	if msg := resp.GetError(); msg != "" {
		t.Fatalf("jepsen: direct get %s at ts %d: %s", key, ts, msg)
	}
	return string(resp.GetValue()), resp.GetFound()
}

func jepDirectCommitSingle(t *testing.T, cl pbv2.KVServiceClient, key, value string, startTS, commitTS uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := cl.CommitSingle(ctx, &pbv2.CommitSingleRequest{
		StartTs:  startTS,
		CommitTs: commitTS,
		Primary:  []byte(key),
		Mutations: []*pbv2.Mutation{{
			Key:   []byte(key),
			Value: []byte(value),
		}},
	})
	if err != nil {
		t.Fatalf("jepsen: direct commit %s: %v", key, err)
	}
	if msg := resp.GetError(); msg != "" {
		t.Fatalf("jepsen: direct commit %s: %s", key, msg)
	}
	if !resp.GetSuccess() {
		t.Fatalf("jepsen: direct commit %s not acknowledged", key)
	}
}

// TestJepsenClockSkew simulates client clock skew through the public KV API.
//
// LIMITATION: production has no injectable TSO/wall-clock (meta.Cluster reads
// time.Now directly and kvserver has no clock hook), so the server's own clock
// cannot be skewed from an external test. Instead the test supplies timestamps
// itself: a "fast" client with a startTS one hour in the future, a "stale"
// client holding an old snapshot, and an ordered pair of commits with explicit
// commitTS values. It asserts timestamp monotonicity and safety (no early
// visibility, no stale overwrite), which is what a skewed client can violate;
// it does not assert liveness for a permanently fast client.
func TestJepsenClockSkew(t *testing.T) {
	seed := jepGate(t)
	started := time.Now()

	// A single node keeps this scenario focused on timestamp semantics: there
	// is no failover to confuse a direct timestamp probe with "not leader".
	c := jepStartClusterN(t, []jepRangeSpec{{}}, 1)
	leader := c.waitStableLeader(1, 8*time.Second)
	cl := c.clients[leader]
	rig := jepNewRig(t, c.meta, c.clientAddrs, fmt.Sprintf("jepsen-skew-%d", seed))

	// 1) TSO monotonicity: sequential allocations strictly increase and stay
	// close to the wall clock.
	var last uint64
	for i := 0; i < 64; i++ {
		ts, err := rig.allocTS(2 * time.Second)
		if err != nil {
			t.Fatalf("jepsen: alloc ts %d: %v", i, err)
		}
		if ts <= last {
			t.Fatalf("jepsen: TSO went backwards at %d: %d <= %d", i, ts, last)
		}
		last = ts
	}
	if phys := int64(last >> 18); phys < time.Now().UnixMilli()-10000 || phys > time.Now().UnixMilli()+10000 {
		t.Fatalf("jepsen: TSO physical component %d is not close to wall clock %d", phys, time.Now().UnixMilli())
	}

	// Concurrent allocations must be unique, and a later sequential allocation
	// must sort after all of them.
	const concurrent = 32
	tsCh := make(chan uint64, concurrent)
	var wg sync.WaitGroup
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ts, err := rig.allocTS(3 * time.Second)
			if err != nil {
				tsCh <- 0
				return
			}
			tsCh <- ts
		}()
	}
	wg.Wait()
	close(tsCh)
	seen := make(map[uint64]struct{}, concurrent)
	var maxConcurrent uint64
	for ts := range tsCh {
		if ts == 0 {
			t.Fatalf("jepsen: concurrent TSO allocation failed")
		}
		if _, dup := seen[ts]; dup {
			t.Fatalf("jepsen: duplicate TSO %d", ts)
		}
		seen[ts] = struct{}{}
		if ts > maxConcurrent {
			maxConcurrent = ts
		}
	}
	after, err := rig.allocTS(2 * time.Second)
	if err != nil {
		t.Fatalf("jepsen: alloc ts after concurrency: %v", err)
	}
	if after <= maxConcurrent {
		t.Fatalf("jepsen: TSO not monotonic across concurrency: %d <= %d", after, maxConcurrent)
	}

	// 2) Snapshot isolation / monotonic reads with a stale snapshot: a txn
	// begun before a newer commit must keep seeing the old value.
	snapKey := fmt.Sprintf("%s-snap", rig.prefix)
	if err := rig.put(snapKey, "v1", 2*time.Second, 5*time.Second); err != nil {
		t.Fatalf("jepsen: write v1: %v", err)
	}
	oldCtx, oldCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer oldCancel()
	oldTxn, err := rig.client.Begin(oldCtx)
	if err != nil {
		t.Fatalf("jepsen: begin stale snapshot: %v", err)
	}
	if err := rig.put(snapKey, "v2", 2*time.Second, 5*time.Second); err != nil {
		t.Fatalf("jepsen: write v2: %v", err)
	}
	for i := 0; i < 2; i++ {
		got, err := oldTxn.Get(oldCtx, []byte(snapKey))
		if err != nil {
			t.Fatalf("jepsen: stale snapshot read %d: %v", i, err)
		}
		if string(got) != "v1" {
			t.Fatalf("jepsen: stale snapshot read %d = %q, want v1 (monotonic snapshot violated)", i, got)
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = oldTxn.Rollback(oldCtx)
	rig.waitValue(snapKey, "v2", 4*time.Second)

	// 3) A stale client that tries to write after a newer commit must get a
	// conflict and must not overwrite the newer value.
	staleCtx, staleCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer staleCancel()
	staleTxn, err := rig.client.Begin(staleCtx)
	if err != nil {
		t.Fatalf("jepsen: begin stale writer: %v", err)
	}
	if err := rig.put(snapKey, "v3", 2*time.Second, 5*time.Second); err != nil {
		t.Fatalf("jepsen: write v3: %v", err)
	}
	staleTxn.Set([]byte(snapKey), []byte("stale-overwrite"))
	if err := staleTxn.Commit(staleCtx); err == nil {
		t.Fatalf("jepsen: stale writer overwrote a newer commit")
	}
	rig.waitValue(snapKey, "v3", 4*time.Second)

	// 4) A "fast clock" client writes at now+1h through the public API. A
	// normal-clock reader must not see the future version early, and a reader
	// at the future timestamp must see it exactly (no loss).
	tsNow, err := rig.allocTS(2 * time.Second)
	if err != nil {
		t.Fatalf("jepsen: alloc now ts: %v", err)
	}
	futureStart := tsNow + (uint64(time.Hour/time.Millisecond) << 18)
	futureCommit := futureStart + 1
	futureKey := fmt.Sprintf("%s-future", rig.prefix)
	jepDirectCommitSingle(t, cl, futureKey, "fast-clock", futureStart, futureCommit)

	if _, found := jepDirectGet(t, cl, futureKey, tsNow); found {
		t.Fatalf("jepsen: future-timestamp write became visible to a present-day reader")
	}
	if _, found := jepDirectGet(t, cl, futureKey, futureCommit-1); found {
		t.Fatalf("jepsen: future-timestamp write visible one tick before its commitTS")
	}
	if got, found := jepDirectGet(t, cl, futureKey, futureCommit); !found || got != "fast-clock" {
		t.Fatalf("jepsen: future-timestamp write not visible at its commitTS: got=%q found=%v", got, found)
	}
	if got, found := jepDirectGet(t, cl, futureKey, futureCommit+1000); !found || got != "fast-clock" {
		t.Fatalf("jepsen: future-timestamp write vanished after commitTS: got=%q found=%v", got, found)
	}

	// A normal-clock write to a different key still works; the future
	// timestamp is not a global poison pill.
	afterKey := fmt.Sprintf("%s-after-fast", rig.prefix)
	if err := rig.put(afterKey, "normal-clock", 2*time.Second, 5*time.Second); err != nil {
		t.Fatalf("jepsen: normal write after future-timestamp commit: %v", err)
	}

	// 5) Explicit ordered commit timestamps: as the read timestamp advances,
	// the visible version must advance and never regress.
	orderedKey := fmt.Sprintf("%s-ordered", rig.prefix)
	s1, err := rig.allocTS(2 * time.Second)
	if err != nil {
		t.Fatalf("jepsen: alloc s1: %v", err)
	}
	c1, err := rig.allocTS(2 * time.Second)
	if err != nil {
		t.Fatalf("jepsen: alloc c1: %v", err)
	}
	s2, err := rig.allocTS(2 * time.Second)
	if err != nil {
		t.Fatalf("jepsen: alloc s2: %v", err)
	}
	c2, err := rig.allocTS(2 * time.Second)
	if err != nil {
		t.Fatalf("jepsen: alloc c2: %v", err)
	}
	if !(s1 < c1 && c1 < s2 && s2 < c2) {
		t.Fatalf("jepsen: TSO order not strict: s1=%d c1=%d s2=%d c2=%d", s1, c1, s2, c2)
	}
	jepDirectCommitSingle(t, cl, orderedKey, "one", s1, c1)
	jepDirectCommitSingle(t, cl, orderedKey, "two", s2, c2)

	for _, tc := range []struct {
		ts    uint64
		want  string
		found bool
	}{
		{ts: c1, want: "one", found: true},
		{ts: c2 - 1, want: "one", found: true},
		{ts: c2, want: "two", found: true},
		{ts: futureCommit, want: "two", found: true},
	} {
		got, found := jepDirectGet(t, cl, orderedKey, tc.ts)
		if found != tc.found || got != tc.want {
			t.Fatalf("jepsen: read at ts %d = (%q,%v), want (%q,%v)", tc.ts, got, found, tc.want, tc.found)
		}
	}

	t.Logf("jepsen: clock-skew scenario passed in %s (seed=%d); limitation: client-supplied timestamps only, server clock is not injectable",
		time.Since(started).Round(time.Millisecond), seed)
}
