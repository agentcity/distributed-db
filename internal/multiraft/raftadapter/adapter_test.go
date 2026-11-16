package raftadapter_test

import (
	"context"
	"testing"
	"time"

	"distributed-db/internal/model"
	"distributed-db/internal/multiraft"
	"distributed-db/internal/multiraft/raftadapter"
	"distributed-db/internal/raft"
	"distributed-db/internal/rafttest"
)

type nopSM struct{}

func (nopSM) Apply(model.Entry) error { return nil }

func TestRaftGroupThroughHost(t *testing.T) {
	store, err := raft.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	net := rafttest.NewNetwork()
	node, err := raft.New(raft.Config{
		ID: "n1", Peers: nil,
		ElectionTimeoutMin: 30 * time.Millisecond,
		ElectionTimeoutMax: 60 * time.Millisecond,
		HeartbeatInterval:  10 * time.Millisecond,
	}, store, nopSM{}, net.Transport("n1"))
	if err != nil {
		t.Fatalf("raft: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := node.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer node.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && node.Status().Role != raft.Leader {
		time.Sleep(10 * time.Millisecond)
	}
	if node.Status().Role != raft.Leader {
		t.Fatalf("single node did not become leader: %+v", node.Status())
	}

	host := multiraft.NewHost(&raftadapter.Factory{
		NewNode: func(id multiraft.GroupID, peers []string) (*raft.Node, error) {
			if id != 7 {
				t.Fatalf("factory id = %d, want 7", id)
			}
			return node, nil
		},
	})
	if err := host.Create(7, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := host.Propose(ctx, 7, model.Entry{Type: model.CommandNoop}); err != nil {
		t.Fatalf("propose: %v", err)
	}
	group, ok := host.Get(7)
	if !ok {
		t.Fatal("group 7 missing")
	}
	if group.Status().ID != 7 || group.Status().Leader != "n1" {
		t.Fatalf("group status = %+v", group.Status())
	}
	host.Stop()
	if node.Status().Role == raft.Leader {
		t.Fatal("node still leader after host stop")
	}
}
