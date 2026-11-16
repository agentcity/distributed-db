// Package raftadapter adapts a single raft.Node to the generic multiraft.Group
// interface. The adapter keeps the multiraft host independent from Raft while
// letting a node host real groups.
package raftadapter

import (
	"context"
	"errors"

	"distributed-db/internal/model"
	"distributed-db/internal/multiraft"
	"distributed-db/internal/raft"
)

// Group is one raft.Node exposed as a multiraft.Group.
type Group struct {
	id   multiraft.GroupID
	node *raft.Node
}

// NewGroup wraps node. The node is owned by the Group: Stop stops it.
func NewGroup(id multiraft.GroupID, node *raft.Node) (*Group, error) {
	if node == nil {
		return nil, errors.New("raftadapter: nil node")
	}
	return &Group{id: id, node: node}, nil
}

// ID implements multiraft.Group.
func (g *Group) ID() multiraft.GroupID { return g.id }

// Propose implements multiraft.Group.
func (g *Group) Propose(ctx context.Context, cmd model.Entry) (uint64, error) {
	return g.node.Propose(ctx, cmd)
}

// Status implements multiraft.Group.
func (g *Group) Status() multiraft.Status {
	st := g.node.Status()
	return multiraft.Status{
		ID:          g.id,
		Leader:      st.Leader,
		Term:        st.Term,
		CommitIndex: st.CommitIndex,
		LastApplied: st.LastApplied,
	}
}

// Stop implements multiraft.Group.
func (g *Group) Stop() { g.node.Stop() }

// Node returns the underlying Raft node for status/snapshot/admin paths.
func (g *Group) Node() *raft.Node { return g.node }

// Factory creates Groups from a caller-provided constructor. The constructor
// owns LogStore/StateMachine/Transport wiring and must return a started or
// startable node.
type Factory struct {
	NewNode func(id multiraft.GroupID, peers []string) (*raft.Node, error)
}

// NewGroup implements multiraft.Factory.
func (f *Factory) NewGroup(id multiraft.GroupID, peers []string) (multiraft.Group, error) {
	if f == nil || f.NewNode == nil {
		return nil, errors.New("raftadapter: nil factory")
	}
	node, err := f.NewNode(id, peers)
	if err != nil {
		return nil, err
	}
	return NewGroup(id, node)
}

var _ multiraft.Group = (*Group)(nil)
var _ multiraft.Factory = (*Factory)(nil)
