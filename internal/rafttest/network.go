// Package rafttest provides an in-memory Raft network for tests. Links can be
// partitioned one-way or fully healed, and every RPC observes the current link
// state without holding a lock while the peer handler runs.
package rafttest

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"distributed-db/internal/raft"
)

var (
	// ErrBlocked is returned when a partition or drop rule blocks a link.
	ErrBlocked = errors.New("rafttest: link blocked")
	// ErrUnknownNode is returned when the destination is not registered.
	ErrUnknownNode = errors.New("rafttest: unknown node")
)

// Handler is the receiving half of a Raft node.
type Handler interface {
	HandleRequestVote(ctx context.Context, req *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error)
	HandleAppendEntries(ctx context.Context, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error)
}

// Network routes RPCs between registered nodes.
type Network struct {
	mu       sync.RWMutex
	handlers map[string]Handler
	blocked  map[string]map[string]bool
	dropped  map[string]map[string]bool
}

// NewNetwork returns an empty, ready-to-use network.
func NewNetwork() *Network {
	return &Network{
		handlers: make(map[string]Handler),
		blocked:  make(map[string]map[string]bool),
		dropped:  make(map[string]map[string]bool),
	}
}

// Register installs (or replaces) the handler for id.
func (n *Network) Register(id string, h Handler) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.handlers[id] = h
}

// Unregister removes id from the network. Calls to a removed node fail with
// ErrUnknownNode.
func (n *Network) Unregister(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.handlers, id)
	delete(n.blocked, id)
	delete(n.dropped, id)
	for other := range n.blocked {
		delete(n.blocked[other], id)
	}
	for other := range n.dropped {
		delete(n.dropped[other], id)
	}
}

// Transport returns the transport that sends on behalf of id.
func (n *Network) Transport(id string) raft.Transport {
	return &transport{net: n, from: id}
}

// Partition blocks every link from id to the listed peers (one-way). With no
// peers it clears id's outgoing blocks. It does not affect links in the
// opposite direction; use Heal to clear both directions.
func (n *Network) Partition(id string, blocked ...string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(blocked) == 0 {
		delete(n.blocked, id)
		return
	}
	m := make(map[string]bool, len(blocked))
	for _, p := range blocked {
		if p != "" {
			m[p] = true
		}
	}
	n.blocked[id] = m
}

// Heal clears all partition and drop rules involving id in either direction.
func (n *Network) Heal(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.blocked, id)
	delete(n.dropped, id)
	for other := range n.blocked {
		delete(n.blocked[other], id)
	}
	for other := range n.dropped {
		delete(n.dropped[other], id)
	}
}

// Drop controls a single directed link from -> to. It is optional fine-grained
// control on top of Partition.
func (n *Network) Drop(from, to string, drop bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if drop {
		m := n.dropped[from]
		if m == nil {
			m = make(map[string]bool)
			n.dropped[from] = m
		}
		m[to] = true
		return
	}
	delete(n.dropped[from], to)
	if len(n.dropped[from]) == 0 {
		delete(n.dropped, from)
	}
}

// handler resolves the destination under the lock, then releases it before
// invoking user code so handlers may call back into the network safely.
func (n *Network) handler(from, to string) (Handler, error) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.blocked[from][to] || n.dropped[from][to] {
		return nil, fmt.Errorf("%w: %s -> %s", ErrBlocked, from, to)
	}
	h, ok := n.handlers[to]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownNode, to)
	}
	return h, nil
}

type transport struct {
	net  *Network
	from string
}

func (t *transport) RequestVote(ctx context.Context, peer string, req *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h, err := t.net.handler(t.from, peer)
	if err != nil {
		return nil, err
	}
	return h.HandleRequestVote(ctx, req)
}

func (t *transport) AppendEntries(ctx context.Context, peer string, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h, err := t.net.handler(t.from, peer)
	if err != nil {
		return nil, err
	}
	return h.HandleAppendEntries(ctx, req)
}
