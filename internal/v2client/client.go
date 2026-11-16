// Package v2client is the transactional client for the v2 KV service.
//
// It discovers the shard leader by polling KVService.Status, caches it for a
// short time, and exposes a small Begin/Set/Get/Commit/Rollback transaction
// API. TSO comes from the meta service and is fetched in batches. Range
// descriptors are cached briefly so a retry storm does not turn into a meta
// RPC storm; both caches are invalidated when the meta leader moves.
package v2client

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	pbv2 "distributed-db/internal/pb/v2"
)

var (
	// ErrNotFound is returned by Txn.Get when no visible value exists.
	ErrNotFound = errors.New("v2client: key not found")
	// ErrNotLeader is returned when every attempt landed on a non-leader.
	ErrNotLeader = errors.New("v2client: not leader")
	// ErrConflict is returned for lock/write conflicts.
	ErrConflict = errors.New("v2client: transaction conflict")
	// errClientClosed is returned after Close.
	errClientClosed = errors.New("v2client: closed")
)

const (
	leaderTTL      = time.Second
	maxAttempts    = 8
	backoff        = 50 * time.Millisecond
	defaultTimeout = 5 * time.Second
)

// Client is a transactional v2 KV client. It is safe for concurrent use.
type Client struct {
	// KV node connections and leader hints.
	mu      sync.Mutex
	clients map[string]pbv2.KVServiceClient
	conns   map[string]*grpc.ClientConn
	leaders map[uint64]leaderEntry // range ID -> cached leader
	closed  bool

	addrs []string

	// Meta service connection pool and leader discovery. Connections are
	// pooled by address so repeated failover does not create a new gRPC
	// connection per call.
	metaMu          sync.Mutex
	meta            metaAPI
	metaAddr        string
	metaSeeds       []string
	metaPool        map[string]*metaEntry
	metaDial        func(string) (*grpc.ClientConn, metaAPI, error)
	metaLeaderAddr  string
	metaLeaderUntil time.Time
	metaGen         uint64
	metaClosed      bool

	// TSO batch. All fields are guarded by tso.mu.
	tso             tsoBatcher
	tsBatchSize     int32
	tsBatchTTL      time.Duration
	tsIdleCheck     time.Duration
	tsValidateEvery uint64

	// Range descriptor cache.
	rangeMu  sync.Mutex
	ranges   map[uint64]*rangeEntry
	rangeSeq uint64
	rangeTTL time.Duration
	rangeCap int
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
	mc := pbv2.NewMetaServiceClient(cc)
	return &Client{
		meta:            mc,
		metaAddr:        metaAddr,
		metaSeeds:       []string{metaAddr},
		metaPool:        map[string]*metaEntry{metaAddr: {cc: cc, mc: mc}},
		metaDial:        dialMeta,
		addrs:           append([]string(nil), addrs...),
		clients:         make(map[string]pbv2.KVServiceClient),
		conns:           make(map[string]*grpc.ClientConn),
		leaders:         make(map[uint64]leaderEntry),
		tsBatchSize:     defaultTSOBatchSize,
		tsBatchTTL:      defaultTSOBatchTTL,
		tsIdleCheck:     defaultTSOIdleCheck,
		tsValidateEvery: defaultTSOValidateEvery,
		ranges:          make(map[uint64]*rangeEntry),
		rangeTTL:        defaultRangeTTL,
		rangeCap:        defaultRangeCacheMax,
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
	metaClosed := c.metaClosed
	c.metaClosed = true
	// Invalidating the generation makes any in-flight TSO/range cache entry
	// unusable after Close.
	c.metaGen++
	pool := c.metaPool
	c.metaPool = nil
	c.meta = nil
	c.metaMu.Unlock()
	c.clearRangeCache()

	var errs []error
	for _, cc := range conns {
		errs = append(errs, cc.Close())
	}
	if !metaClosed {
		for _, e := range pool {
			if e != nil && e.cc != nil {
				errs = append(errs, e.cc.Close())
			}
		}
	}
	return errors.Join(errs...)
}

func (c *Client) node(addr string) (pbv2.KVServiceClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errClientClosed
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

// discoverLeader probes the meta range's replicas (leader hint first) and
// returns the first node that reports leadership, together with the range
// descriptor version used for the probe.
func (c *Client) discoverLeader(ctx context.Context, key []byte) (string, uint64, uint64, error) {
	var candidates []string
	var rangeID, version uint64
	if r, err := c.rangeFor(ctx, key); err == nil {
		rangeID = r.GetId()
		version = r.GetVersion()
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
			return resp.GetLeader(), rangeID, version, nil
		}
		if resp.GetRole() == "leader" {
			c.setLeader(rangeID, addr)
			return addr, rangeID, version, nil
		}
	}
	if lastErr == nil {
		lastErr = ErrNotLeader
	}
	return "", rangeID, version, fmt.Errorf("v2client: discover leader: %w", lastErr)
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
	refreshedRange := false
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
			var rid, version uint64
			addr, rid, version, err = c.discoverLeader(ctx, key)
			if err != nil {
				lastErr = err
				continue
			}
			if rid != 0 {
				rangeID, rangeEpoch = rid, version
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
		switch msg {
		case "":
			return nil
		case "not leader":
			c.invalidateLeader(rangeID)
			lastErr = ErrNotLeader
			if hint != "" {
				c.setLeader(rangeID, hint)
			}
			continue
		case "range frozen":
			lastErr = ErrConflict
			continue
		case "stale epoch", "not hosted":
			// The cached descriptor is no longer valid (split/merge or the
			// range moved). Drop everything for this key and re-resolve it
			// through meta exactly once before retrying the RPC.
			c.invalidateLeader(rangeID)
			c.invalidateRangeForKey(rangeID, key)
			if refreshedRange {
				return fmt.Errorf("v2client: %s", msg)
			}
			refreshedRange = true
			nr, rerr := c.rangeForFresh(ctx, key)
			if rerr != nil {
				return fmt.Errorf("v2client: refresh range after %q: %w", msg, rerr)
			}
			rangeID, rangeEpoch = nr.GetId(), nr.GetVersion()
			lastErr = fmt.Errorf("%s", msg)
			// The meta round-trip already provided the retry opportunity, so
			// it does not consume the attempt budget.
			attempt--
			continue
		default:
			return fmt.Errorf("v2client: %s", msg)
		}
	}
	if lastErr == nil {
		lastErr = ErrNotLeader
	}
	return fmt.Errorf("v2client: %w", lastErr)
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

	// The whole transaction is in one range: apply prewrite+commit in one
	// Raft entry (1PC). commitTS is allocated before the single round.
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
	var commitMsg string
	if err := t.c.retry(ctx, primary, func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
		resp, err := cl.CommitSingle(ctx, &pbv2.CommitSingleRequest{
			StartTs: t.startTS, CommitTs: commitTS, Primary: primary, Mutations: mutations,
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
