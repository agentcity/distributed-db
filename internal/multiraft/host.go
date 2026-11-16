// Package multiraft hosts many independent Raft groups inside one process.
//
// A Host owns the process-wide group registry, while a Factory is responsible
// for constructing the group itself (for example, an adapter around the
// package's Raft node). The Host deliberately has no knowledge of Raft: it only
// enforces lifecycle and routing rules, which keeps it usable with either a
// real factory or a test double.
package multiraft

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"distributed-db/internal/model"
)

// GroupID identifies one Raft group within a Host.
type GroupID uint64

// Status is a point-in-time view of a group's Raft state.
type Status struct {
	ID          GroupID
	Leader      string
	Term        uint64
	CommitIndex uint64
	LastApplied uint64
}

// Group is one Raft replication group managed by a Host.
type Group interface {
	ID() GroupID
	Propose(ctx context.Context, cmd model.Entry) (uint64, error)
	Status() Status
	Stop()
}

// Factory creates the concrete Group for an ID and its initial peer set.
type Factory interface {
	NewGroup(id GroupID, peers []string) (Group, error)
}

var (
	// ErrGroupNotFound is returned when an operation names an ID that is not
	// registered with the Host.
	ErrGroupNotFound = errors.New("multiraft: group not found")

	// ErrGroupExists is returned by Create when the ID is already registered.
	ErrGroupExists = errors.New("multiraft: group already exists")

	// ErrDuplicateGroup is an alias for ErrGroupExists.
	ErrDuplicateGroup = ErrGroupExists

	// ErrHostStopped is returned by Create after Stop has been called.
	ErrHostStopped = errors.New("multiraft: host is stopped")
)

var errNilFactory = errors.New("multiraft: nil factory")

// groupSlot keeps the group and guarantees that Stop is invoked at most once,
// even when Remove and Host.Stop race with each other.
type groupSlot struct {
	group    Group
	stopOnce sync.Once
}

func (s *groupSlot) stop() {
	s.stopOnce.Do(s.group.Stop)
}

// Host is a concurrent-safe registry of Raft groups. The zero value is not
// usable; call NewHost.
type Host struct {
	mu      sync.RWMutex
	factory Factory
	groups  map[GroupID]*groupSlot
	stopped bool
}

// NewHost returns an empty Host that uses f to create groups. A nil factory is
// permitted at construction time; Create then returns an error.
func NewHost(f Factory) *Host {
	return &Host{
		factory: f,
		groups:  make(map[GroupID]*groupSlot),
	}
}

// Create registers a new group under id. It returns ErrGroupExists if id is
// already registered and ErrHostStopped if the host has been stopped.
//
// NewGroup is called while the host lock is held. That serialises concurrent
// creates of the same ID and makes Stop's "no creation after stop" rule
// atomic; factories are therefore expected to be fast constructors and must
// not call back into the Host.
func (h *Host) Create(id GroupID, peers []string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.stopped {
		return ErrHostStopped
	}
	if _, ok := h.groups[id]; ok {
		return ErrGroupExists
	}
	if h.factory == nil {
		return errNilFactory
	}

	// Copy peers so the caller cannot mutate a slice that the factory may
	// retain.
	group, err := h.factory.NewGroup(id, append([]string(nil), peers...))
	if err != nil {
		return fmt.Errorf("multiraft: create group %d: %w", id, err)
	}
	if group == nil {
		return fmt.Errorf("multiraft: create group %d: factory returned nil group", id)
	}

	h.groups[id] = &groupSlot{group: group}
	return nil
}

// Get returns the group registered under id.
func (h *Host) Get(id GroupID) (Group, bool) {
	h.mu.RLock()
	slot, ok := h.groups[id]
	h.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return slot.group, true
}

// Propose routes cmd to the group identified by id. The context is honoured
// before dispatch and is passed to the group.
//
// A Propose that already obtained the group may overlap a concurrent Remove or
// Stop; in that case it is up to the group's Propose implementation to fail
// once the group has been stopped.
func (h *Host) Propose(ctx context.Context, id GroupID, cmd model.Entry) (uint64, error) {
	h.mu.RLock()
	slot, ok := h.groups[id]
	h.mu.RUnlock()
	if !ok {
		return 0, ErrGroupNotFound
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return slot.group.Propose(ctx, cmd)
}

// Remove unregisters id and stops its group. It returns ErrGroupNotFound if the
// ID is not registered.
func (h *Host) Remove(id GroupID) error {
	h.mu.Lock()
	slot, ok := h.groups[id]
	if ok {
		delete(h.groups, id)
	}
	h.mu.Unlock()

	if !ok {
		return ErrGroupNotFound
	}
	slot.stop()
	return nil
}

// Stop stops every registered group and makes the Host unusable for Create.
// Stop is idempotent and safe to call concurrently.
func (h *Host) Stop() {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return
	}
	h.stopped = true
	groups := h.groups
	h.groups = make(map[GroupID]*groupSlot)
	h.mu.Unlock()

	for _, slot := range groups {
		slot.stop()
	}
}
