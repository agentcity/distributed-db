package main

import (
	"bytes"
	"errors"
	"sync"
)

// ErrNotFound is returned by Engine.Get when the key does not exist.
var ErrNotFound = errors.New("bench: key not found")

// KV is a single key/value pair returned by Engine.Scan.
type KV struct {
	Key   []byte
	Value []byte
}

// Engine is the point-operation surface exercised by the benchmark harness.
//
// The in-memory MemEngine below implements it today; a future server adapter
// (for example one backed by distributed-db/internal/client) can implement the
// same interface without changing the workload generator or the metrics code.
//
// Implementations must be safe for concurrent use. Get and Scan return values
// that remain valid after the call, and Put must not retain the caller's
// slices. Delete is idempotent.
type Engine interface {
	Get(key []byte) ([]byte, error)
	Put(key, value []byte) error
	Delete(key []byte) error

	// Scan returns key/value pairs with start <= key < end in ascending key
	// order. A nil start means "from the beginning" and a nil end means "to
	// the end". limit <= 0 means no limit.
	Scan(start, end []byte, limit int) ([]KV, error)
}

// MemEngine is an in-memory ordered key/value store. It is a treap whose
// priorities are derived deterministically from the keys, so lookups and
// scans are O(log n) expected and the shape does not depend on insertion
// order. A single RWMutex guards the tree; individual point operations are
// short, and readers run concurrently.
//
// It exists only to make the harness runnable without a cluster. It makes no
// durability or replication claims.
type MemEngine struct {
	mu    sync.RWMutex
	root  *memNode
	count int
}

type memNode struct {
	key   []byte
	value []byte
	prio  uint64
	left  *memNode
	right *memNode
}

// NewMemEngine returns an empty in-memory engine.
func NewMemEngine() *MemEngine {
	return &MemEngine{}
}

// Len reports the number of live keys.
func (e *MemEngine) Len() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.count
}

// Get implements Engine.
func (e *MemEngine) Get(key []byte) ([]byte, error) {
	e.mu.RLock()
	n := e.root
	for n != nil {
		switch cmp := bytes.Compare(key, n.key); {
		case cmp == 0:
			value := cloneBytes(n.value)
			e.mu.RUnlock()
			return value, nil
		case cmp < 0:
			n = n.left
		default:
			n = n.right
		}
	}
	e.mu.RUnlock()
	return nil, ErrNotFound
}

// Put implements Engine.
func (e *MemEngine) Put(key, value []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	added := false
	e.root, added = setNode(e.root, key, value, priority(key))
	if added {
		e.count++
	}
	return nil
}

// Delete implements Engine. Deleting a missing key is a no-op.
func (e *MemEngine) Delete(key []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	deleted := false
	e.root, deleted = deleteNode(e.root, key)
	if deleted {
		e.count--
	}
	return nil
}

// Scan implements Engine.
func (e *MemEngine) Scan(start, end []byte, limit int) ([]KV, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	out := make([]KV, 0)
	if limit <= 0 || limit > e.count {
		out = make([]KV, 0, e.count)
	} else {
		out = make([]KV, 0, limit)
	}
	scanNode(e.root, start, end, limit, &out)
	return out, nil
}

func scanNode(n *memNode, start, end []byte, limit int, out *[]KV) bool {
	if n == nil {
		return false
	}
	// Left subtree is entirely below n.key; only visit it if it can contain
	// keys >= start.
	if start == nil || bytes.Compare(n.key, start) > 0 {
		if scanNode(n.left, start, end, limit, out) {
			return true
		}
	}
	if start == nil || bytes.Compare(n.key, start) >= 0 {
		if end == nil || bytes.Compare(n.key, end) < 0 {
			*out = append(*out, KV{Key: cloneBytes(n.key), Value: cloneBytes(n.value)})
			if limit > 0 && len(*out) >= limit {
				return true
			}
		}
	}
	// Everything in the right subtree is >= n.key; when n.key is already at
	// or past end there is nothing more to visit anywhere to the right.
	if end != nil && bytes.Compare(n.key, end) >= 0 {
		return false
	}
	return scanNode(n.right, start, end, limit, out)
}

func setNode(n *memNode, key, value []byte, prio uint64) (*memNode, bool) {
	if n == nil {
		return &memNode{key: cloneBytes(key), value: cloneBytes(value), prio: prio}, true
	}
	added := false
	switch cmp := bytes.Compare(key, n.key); {
	case cmp == 0:
		n.value = cloneBytes(value)
		return n, false
	case cmp < 0:
		n.left, added = setNode(n.left, key, value, prio)
		if higher(n.left, n) {
			n = rotateRight(n)
		}
	default:
		n.right, added = setNode(n.right, key, value, prio)
		if higher(n.right, n) {
			n = rotateLeft(n)
		}
	}
	return n, added
}

func deleteNode(n *memNode, key []byte) (*memNode, bool) {
	if n == nil {
		return nil, false
	}
	deleted := false
	switch cmp := bytes.Compare(key, n.key); {
	case cmp < 0:
		n.left, deleted = deleteNode(n.left, key)
		return n, deleted
	case cmp > 0:
		n.right, deleted = deleteNode(n.right, key)
		return n, deleted
	default:
		return mergeNodes(n.left, n.right), true
	}
}

// mergeNodes joins two treaps whose key ranges do not overlap. It is the
// standard treap delete: the higher-priority root becomes the new root.
func mergeNodes(a, b *memNode) *memNode {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if higher(a, b) {
		a.right = mergeNodes(a.right, b)
		return a
	}
	b.left = mergeNodes(a, b.left)
	return b
}

// higher reports whether a should sit above b in the treap. Priorities are
// unique in practice, but the key comparison makes the heap order total if
// two keys hash to the same value.
func higher(a, b *memNode) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	if a.prio != b.prio {
		return a.prio < b.prio
	}
	return bytes.Compare(a.key, b.key) < 0
}

func rotateRight(n *memNode) *memNode {
	left := n.left
	n.left = left.right
	left.right = n
	return left
}

func rotateLeft(n *memNode) *memNode {
	right := n.right
	n.right = right.left
	right.left = n
	return right
}

// priority derives a stable pseudo-random priority from a key using FNV-1a.
// Deriving it from the key (rather than from insertion order) keeps the tree
// shape deterministic across runs and concurrency levels.
func priority(key []byte) uint64 {
	const (
		offset64 = uint64(14695981039346656037)
		prime64  = uint64(1099511628211)
	)
	h := offset64
	for _, b := range key {
		h ^= uint64(b)
		h *= prime64
	}
	return h
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
