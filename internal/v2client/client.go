// Package v2client is the transactional client for the v2 KV service.
//
// It discovers the shard leader by polling KVService.Status, caches it for a
// short time, and exposes a small Begin/Set/Get/Commit/Rollback transaction
// API. TSO comes from the meta service. M1 has a single range, so there is no
// range cache yet; the interface is shaped so a range-aware router can be
// added without changing callers.
package v2client

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc/metadata"

	pbv2 "distributed-db/internal/pb/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	// ErrNotFound is returned by Txn.Get when no visible value exists.
	ErrNotFound = errors.New("v2client: key not found")
	// ErrNotLeader is returned when every attempt landed on a non-leader.
	ErrNotLeader = errors.New("v2client: not leader")
	// ErrConflict is returned for lock/write conflicts.
	ErrConflict = errors.New("v2client: transaction conflict")
)

const (
	leaderTTL      = time.Second
	maxAttempts    = 8
	backoff        = 50 * time.Millisecond
	defaultTimeout = 5 * time.Second
)

// Client is a transactional v2 KV client. It is safe for concurrent use.
type Client struct {
	meta      pbv2.MetaServiceClient
	metaCC    *grpc.ClientConn
	metaAddr  string
	metaSeeds []string
	addrs     []string

	metaMu          sync.Mutex
	metaConns       []*grpc.ClientConn
	metaLeaderAddr  string
	metaLeaderUntil time.Time

	mu      sync.Mutex
	clients map[string]pbv2.KVServiceClient
	conns   map[string]*grpc.ClientConn
	leaders map[uint64]leaderEntry // range ID -> cached leader
	closed  bool
}

type leaderEntry struct {
	addr  string
	until time.Time
}

// New connects to the meta service and one or more KV nodes. It does not dial
// KV nodes until the first operation.
func New(metaAddr string, addrs []string) (*Client, error) {
	if metaAddr == "" {
		return nil, errors.New("v2client: meta address is required")
	}
	if len(addrs) == 0 {
		return nil, errors.New("v2client: at least one node address is required")
	}
	cc, err := grpc.NewClient(metaAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("v2client: dial meta: %w", err)
	}
	return &Client{
		meta:      pbv2.NewMetaServiceClient(cc),
		metaCC:    cc,
		metaAddr:  metaAddr,
		metaSeeds: []string{metaAddr},
		metaConns: []*grpc.ClientConn{cc},
		addrs:     append([]string(nil), addrs...),
		clients:   make(map[string]pbv2.KVServiceClient),
		conns:     make(map[string]*grpc.ClientConn),
		leaders:   make(map[uint64]leaderEntry),
	}, nil
}

// Close closes all gRPC connections.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	conns := c.conns
	c.conns = nil
	c.clients = nil
	c.mu.Unlock()

	c.metaMu.Lock()
	metaConns := c.metaConns
	c.metaConns = nil
	c.metaCC = nil
	c.metaMu.Unlock()

	var errs []error
	for _, cc := range conns {
		errs = append(errs, cc.Close())
	}
	for _, cc := range metaConns {
		errs = append(errs, cc.Close())
	}
	return errors.Join(errs...)
}

func (c *Client) node(addr string) (pbv2.KVServiceClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("v2client: closed")
	}
	if cl, ok := c.clients[addr]; ok {
		return cl, nil
	}
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	cl := pbv2.NewKVServiceClient(cc)
	c.conns[addr] = cc
	c.clients[addr] = cl
	return cl, nil
}

func (c *Client) cachedLeader(rangeID uint64) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.leaders[rangeID]; ok && e.addr != "" && time.Now().Before(e.until) {
		return e.addr
	}
	return ""
}

func (c *Client) setLeader(rangeID uint64, addr string) {
	c.mu.Lock()
	c.leaders[rangeID] = leaderEntry{addr: addr, until: time.Now().Add(leaderTTL)}
	c.mu.Unlock()
}

func (c *Client) invalidateLeader(rangeID uint64) {
	c.mu.Lock()
	delete(c.leaders, rangeID)
	c.mu.Unlock()
}

// tryMetaSeeds re-discovers the leader from the original meta seed addresses.
// It is used when the cached leader is gone.
func (c *Client) tryMetaSeeds(ctx context.Context) (pbv2.MetaServiceClient, bool) {
	c.metaMu.Lock()
	seeds := append([]string(nil), c.metaSeeds...)
	c.metaMu.Unlock()
	for _, addr := range seeds {
		cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			continue
		}
		mc := pbv2.NewMetaServiceClient(cc)
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		resp, err := mc.Leader(probeCtx, &pbv2.LeaderRequest{})
		cancel()
		if err != nil {
			_ = cc.Close()
			continue
		}
		target, targetCC, targetMC := addr, cc, mc
		if leader := resp.GetLeader(); leader != "" && leader != addr {
			lcc, err := grpc.NewClient(leader, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				_ = cc.Close()
				continue
			}
			target, targetCC, targetMC = leader, lcc, pbv2.NewMetaServiceClient(lcc)
		}
		c.metaMu.Lock()
		c.meta = targetMC
		c.metaCC = targetCC
		c.metaAddr = target
		c.metaConns = append(c.metaConns, targetCC)
		c.metaLeaderAddr = target
		c.metaLeaderUntil = time.Now().Add(leaderTTL)
		c.metaMu.Unlock()
		if target != addr {
			_ = cc.Close()
		}
		return targetMC, true
	}
	return nil, false
}

// metaClient returns a client connected to the current meta leader. A follower
// answers Leader with the current leader, so callers can start from any meta
// node and get redirected without a separate discovery service.
func (c *Client) metaClient(ctx context.Context) (pbv2.MetaServiceClient, error) {
	c.metaMu.Lock()
	mc := c.meta
	addr := c.metaAddr
	cached := c.metaLeaderAddr
	until := c.metaLeaderUntil
	c.metaMu.Unlock()
	if cached != "" && cached == addr && time.Now().Before(until) {
		return mc, nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, time.Second)
	resp, err := mc.Leader(probeCtx, &pbv2.LeaderRequest{})
	cancel()
	if err != nil {
		if seeded, ok := c.tryMetaSeeds(ctx); ok {
			return seeded, nil
		}
		return mc, nil
	}
	leader := resp.GetLeader()
	if leader == "" || leader == addr {
		c.metaMu.Lock()
		c.metaLeaderAddr = addr
		c.metaLeaderUntil = time.Now().Add(leaderTTL)
		c.metaMu.Unlock()
		return mc, nil
	}
	cc, err := grpc.NewClient(leader, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return mc, nil
	}
	next := pbv2.NewMetaServiceClient(cc)
	c.metaMu.Lock()
	c.meta = next
	c.metaCC = cc
	c.metaAddr = leader
	c.metaConns = append(c.metaConns, cc)
	c.metaLeaderAddr = leader
	c.metaLeaderUntil = time.Now().Add(leaderTTL)
	c.metaMu.Unlock()
	return next, nil
}

func (c *Client) invalidateMetaLeader() {
	c.metaMu.Lock()
	c.metaLeaderAddr = ""
	c.metaLeaderUntil = time.Time{}
	c.metaMu.Unlock()
}

// rangeFor asks the meta service which range owns key. A meta failure is not
// fatal: the client falls back to probing every configured node.
func (c *Client) rangeFor(ctx context.Context, key []byte) (*pbv2.RangeInfo, error) {
	probeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	mc, err := c.metaClient(ctx)
	if err != nil {
		return nil, err
	}
	r, err := mc.GetRange(probeCtx, &pbv2.GetRangeRequest{Key: key})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// discoverLeader probes the meta range's replicas (leader hint first) and
// returns the first node that reports leadership.
func (c *Client) discoverLeader(ctx context.Context, key []byte) (string, uint64, error) {
	var candidates []string
	var rangeID uint64
	if r, err := c.rangeFor(ctx, key); err == nil {
		rangeID = r.GetId()
		if r.GetLeader() != "" {
			candidates = append(candidates, r.GetLeader())
		}
		candidates = append(candidates, r.GetReplicas()...)
	}
	candidates = append(candidates, c.addrs...)

	seen := make(map[string]struct{}, len(candidates))
	var lastErr error
	for _, addr := range candidates {
		if addr == "" {
			continue
		}
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		cl, err := c.node(addr)
		if err != nil {
			lastErr = err
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		resp, err := cl.Status(probeCtx, &pbv2.StatusRequest{RangeId: rangeID})
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.GetLeader() != "" {
			c.setLeader(rangeID, resp.GetLeader())
			return resp.GetLeader(), rangeID, nil
		}
		if resp.GetRole() == "leader" {
			c.setLeader(rangeID, addr)
			return addr, rangeID, nil
		}
	}
	if lastErr == nil {
		lastErr = ErrNotLeader
	}
	return "", rangeID, fmt.Errorf("v2client: discover leader: %w", lastErr)
}

type callFunc func(ctx context.Context, cl pbv2.KVServiceClient) (leaderHint, errMsg string, err error)

func (c *Client) retry(ctx context.Context, key []byte, call callFunc) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	var rangeID, rangeEpoch uint64
	if r, err := c.rangeFor(ctx, key); err == nil {
		rangeID = r.GetId()
		rangeEpoch = r.GetVersion()
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * backoff):
			}
		}
		addr := c.cachedLeader(rangeID)
		if addr == "" {
			var err error
			addr, _, err = c.discoverLeader(ctx, key)
			if err != nil {
				lastErr = err
				continue
			}
		}
		cl, err := c.node(addr)
		if err != nil {
			lastErr = err
			continue
		}
		callCtx := ctx
		if rangeEpoch != 0 {
			callCtx = metadata.AppendToOutgoingContext(ctx, "x-range-epoch", strconv.FormatUint(rangeEpoch, 10))
		}
		hint, msg, err := call(callCtx, cl)
		if err != nil {
			lastErr = err
			c.invalidateLeader(rangeID)
			continue
		}
		if msg == "not leader" {
			c.invalidateLeader(rangeID)
			lastErr = ErrNotLeader
			if hint != "" {
				c.setLeader(rangeID, hint)
			}
			continue
		}
		if msg == "range frozen" {
			lastErr = ErrConflict
			continue
		}
		if msg != "" {
			return fmt.Errorf("v2client: %s", msg)
		}
		return nil
	}
	if lastErr == nil {
		lastErr = ErrNotLeader
	}
	return fmt.Errorf("v2client: %w", lastErr)
}

// AllocTS reserves one timestamp from the meta service.
func (c *Client) AllocTS(ctx context.Context) (uint64, error) {
	var lastErr error
	deadline := time.Now().Add(defaultTimeout)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		mc, err := c.metaClient(ctx)
		if err != nil {
			lastErr = err
			continue
		}
		resp, err := mc.AllocTS(ctx, &pbv2.AllocTSRequest{Count: 1})
		if err == nil {
			return resp.GetStart(), nil
		}
		lastErr = err
		c.invalidateMetaLeader()
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return 0, fmt.Errorf("v2client: alloc ts: %w", lastErr)
}

// KV is one row returned by Txn.Scan.
type KV struct {
	Key   []byte
	Value []byte
}

// CheckTxnStatus inspects the lock on key and reports committed/rolled-back/
// live/expired status. startTS may be zero when the caller wants the lock's
// own timestamp.
func (c *Client) CheckTxnStatus(ctx context.Context, key []byte, startTS uint64) (*pbv2.CheckTxnStatusResponse, error) {
	var out *pbv2.CheckTxnStatusResponse
	if err := c.retry(ctx, key, func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
		resp, err := cl.CheckTxnStatus(ctx, &pbv2.CheckTxnStatusRequest{Key: key, StartTs: startTS})
		if err != nil {
			return "", "", err
		}
		out = resp
		return resp.GetLeader(), resp.GetError(), nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// ResolveLock commits (commitTS > 0) or rolls back (commitTS == 0) a
// transaction's keys.
func (c *Client) ResolveLock(ctx context.Context, keys [][]byte, startTS, commitTS uint64) error {
	if len(keys) == 0 {
		return errors.New("v2client: ResolveLock requires keys")
	}
	return c.retry(ctx, keys[0], func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
		resp, err := cl.ResolveLock(ctx, &pbv2.ResolveLockRequest{Keys: keys, StartTs: startTS, CommitTs: commitTS})
		if err != nil {
			return "", "", err
		}
		return resp.GetLeader(), resp.GetError(), nil
	})
}

// Txn is a single optimistic transaction. It is not safe for concurrent use.
type Txn struct {
	c       *Client
	startTS uint64
	order   []string
	writes  map[string]*pbv2.Mutation
	done    bool
}

// Begin allocates a start timestamp and returns a transaction.
func (c *Client) Begin(ctx context.Context) (*Txn, error) {
	ts, err := c.AllocTS(ctx)
	if err != nil {
		return nil, err
	}
	return &Txn{c: c, startTS: ts, writes: make(map[string]*pbv2.Mutation)}, nil
}

// StartTS returns the transaction's start timestamp.
func (t *Txn) StartTS() uint64 { return t.startTS }

// Set stages a put.
func (t *Txn) Set(key, value []byte) {
	k := string(key)
	if _, ok := t.writes[k]; !ok {
		t.order = append(t.order, k)
	}
	t.writes[k] = &pbv2.Mutation{Key: append([]byte(nil), key...), Value: append([]byte(nil), value...)}
}

// Delete stages a delete.
func (t *Txn) Delete(key []byte) {
	k := string(key)
	if _, ok := t.writes[k]; !ok {
		t.order = append(t.order, k)
	}
	t.writes[k] = &pbv2.Mutation{Key: append([]byte(nil), key...), Delete: true}
}

// Get reads through the transaction snapshot: staged writes first, then the
// committed value at startTS.
func (t *Txn) Get(ctx context.Context, key []byte) ([]byte, error) {
	if m, ok := t.writes[string(key)]; ok {
		if m.GetDelete() {
			return nil, ErrNotFound
		}
		return append([]byte(nil), m.GetValue()...), nil
	}
	var out []byte
	var found bool
	var msg string
	err := t.c.retry(ctx, key, func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
		resp, err := cl.Get(ctx, &pbv2.GetRequest{Key: key, Ts: t.startTS})
		if err != nil {
			return "", "", err
		}
		out, found, msg = resp.GetValue(), resp.GetFound(), resp.GetError()
		return resp.GetLeader(), resp.GetError(), nil
	})
	if err != nil {
		return nil, err
	}
	if msg != "" {
		return nil, fmt.Errorf("v2client: get: %s", msg)
	}
	if !found {
		return nil, ErrNotFound
	}
	return out, nil
}

// Scan reads a range at the transaction snapshot. It is routed by start key.
func (t *Txn) Scan(ctx context.Context, start, end []byte, limit int) ([]KV, error) {
	var rows []KV
	var msg string
	if err := t.c.retry(ctx, start, func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
		resp, err := cl.Scan(ctx, &pbv2.ScanRequest{Start: start, End: end, Ts: t.startTS, Limit: int32(limit)})
		if err != nil {
			return "", "", err
		}
		msg = resp.GetError()
		rows = rows[:0]
		for _, kv := range resp.GetKvs() {
			rows = append(rows, KV{Key: kv.GetKey(), Value: kv.GetValue()})
		}
		return resp.GetLeader(), resp.GetError(), nil
	}); err != nil {
		return nil, err
	}
	if msg != "" {
		return nil, fmt.Errorf("v2client: scan: %s", msg)
	}
	return rows, nil
}

// Commit prewrites all staged mutations and commits them.
func (t *Txn) Commit(ctx context.Context) error {
	if t.done {
		return errors.New("v2client: transaction is finished")
	}
	if len(t.writes) == 0 {
		t.done = true
		return nil
	}
	keys := make([]string, 0, len(t.order))
	for _, k := range t.order {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	primary := []byte(keys[0])
	mutations := make([]*pbv2.Mutation, 0, len(keys))
	for _, k := range keys {
		mutations = append(mutations, t.writes[k])
	}

	// M2 supports a single range per transaction. Verify before writing.
	ri, err := t.c.rangeFor(ctx, primary)
	if err != nil {
		return err
	}
	for _, k := range keys {
		other, err := t.c.rangeFor(ctx, []byte(k))
		if err != nil {
			return err
		}
		if other.GetId() != ri.GetId() {
			return fmt.Errorf("v2client: cross-range transaction %d != %d", ri.GetId(), other.GetId())
		}
	}

	var prewriteMsg string
	if err := t.c.retry(ctx, primary, func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
		resp, err := cl.Prewrite(ctx, &pbv2.PrewriteRequest{
			StartTs: t.startTS, Primary: primary, Mutations: mutations,
		})
		if err != nil {
			return "", "", err
		}
		prewriteMsg = resp.GetError()
		return resp.GetLeader(), resp.GetError(), nil
	}); err != nil {
		return err
	}
	if prewriteMsg != "" {
		return fmt.Errorf("v2client: prewrite: %s", prewriteMsg)
	}

	commitTS, err := t.c.AllocTS(ctx)
	if err != nil {
		return err
	}
	for commitTS <= t.startTS {
		commitTS, err = t.c.AllocTS(ctx)
		if err != nil {
			return err
		}
	}
	keyBytes := make([][]byte, 0, len(keys))
	for _, k := range keys {
		keyBytes = append(keyBytes, []byte(k))
	}
	var commitMsg string
	if err := t.c.retry(ctx, primary, func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
		resp, err := cl.Commit(ctx, &pbv2.CommitRequest{
			StartTs: t.startTS, CommitTs: commitTS, Keys: keyBytes,
		})
		if err != nil {
			return "", "", err
		}
		commitMsg = resp.GetError()
		return resp.GetLeader(), resp.GetError(), nil
	}); err != nil {
		return err
	}
	if commitMsg != "" {
		return fmt.Errorf("v2client: commit: %s", commitMsg)
	}
	t.done = true
	return nil
}

// Rollback aborts the transaction. It is idempotent.
func (t *Txn) Rollback(ctx context.Context) error {
	if t.done || len(t.writes) == 0 {
		t.done = true
		return nil
	}
	keys := make([][]byte, 0, len(t.order))
	for _, k := range t.order {
		keys = append(keys, []byte(k))
	}
	var msg string
	if err := t.c.retry(ctx, keys[0], func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
		resp, err := cl.Rollback(ctx, &pbv2.RollbackRequest{StartTs: t.startTS, Keys: keys})
		if err != nil {
			return "", "", err
		}
		msg = resp.GetError()
		return resp.GetLeader(), resp.GetError(), nil
	}); err != nil {
		return err
	}
	if msg != "" {
		return fmt.Errorf("v2client: rollback: %s", msg)
	}
	t.done = true
	return nil
}
