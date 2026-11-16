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
	"sync"
	"time"

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
	maxAttempts    = 4
	backoff        = 20 * time.Millisecond
	defaultTimeout = 5 * time.Second
)

// Client is a transactional v2 KV client. It is safe for concurrent use.
type Client struct {
	meta   pbv2.MetaServiceClient
	metaCC *grpc.ClientConn
	addrs  []string

	mu          sync.Mutex
	clients     map[string]pbv2.KVServiceClient
	conns       map[string]*grpc.ClientConn
	leader      string
	leaderUntil time.Time
	closed      bool
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
		meta:    pbv2.NewMetaServiceClient(cc),
		metaCC:  cc,
		addrs:   append([]string(nil), addrs...),
		clients: make(map[string]pbv2.KVServiceClient),
		conns:   make(map[string]*grpc.ClientConn),
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
	metaCC := c.metaCC
	c.conns = nil
	c.clients = nil
	c.metaCC = nil
	c.mu.Unlock()
	var errs []error
	for _, cc := range conns {
		errs = append(errs, cc.Close())
	}
	if metaCC != nil {
		errs = append(errs, metaCC.Close())
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

func (c *Client) cachedLeader() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.leader != "" && time.Now().Before(c.leaderUntil) {
		return c.leader
	}
	return ""
}

func (c *Client) setLeader(addr string) {
	c.mu.Lock()
	c.leader = addr
	c.leaderUntil = time.Now().Add(leaderTTL)
	c.mu.Unlock()
}

func (c *Client) invalidateLeader() {
	c.mu.Lock()
	c.leader = ""
	c.leaderUntil = time.Time{}
	c.mu.Unlock()
}

// discoverLeader asks every node for Status and returns the first leader.
func (c *Client) discoverLeader(ctx context.Context) (string, error) {
	var lastErr error
	for _, addr := range c.addrs {
		cl, err := c.node(addr)
		if err != nil {
			lastErr = err
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		resp, err := cl.Status(probeCtx, &pbv2.StatusRequest{})
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.GetLeader() != "" {
			c.setLeader(resp.GetLeader())
			return resp.GetLeader(), nil
		}
		if resp.GetRole() == "leader" {
			c.setLeader(addr)
			return addr, nil
		}
	}
	if lastErr == nil {
		lastErr = ErrNotLeader
	}
	return "", fmt.Errorf("v2client: discover leader: %w", lastErr)
}

type callFunc func(ctx context.Context, cl pbv2.KVServiceClient) (leaderHint, errMsg string, err error)

func (c *Client) retry(ctx context.Context, call callFunc) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
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
		addr := c.cachedLeader()
		if addr == "" {
			var err error
			addr, err = c.discoverLeader(ctx)
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
		hint, msg, err := call(ctx, cl)
		if err != nil {
			lastErr = err
			c.invalidateLeader()
			continue
		}
		if msg == "not leader" {
			c.invalidateLeader()
			lastErr = ErrNotLeader
			if hint != "" {
				c.setLeader(hint)
			}
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
	resp, err := c.meta.AllocTS(ctx, &pbv2.AllocTSRequest{Count: 1})
	if err != nil {
		return 0, err
	}
	return resp.GetStart(), nil
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
	err := t.c.retry(ctx, func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
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

	var prewriteMsg string
	if err := t.c.retry(ctx, func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
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
	if err := t.c.retry(ctx, func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
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
	if err := t.c.retry(ctx, func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
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
