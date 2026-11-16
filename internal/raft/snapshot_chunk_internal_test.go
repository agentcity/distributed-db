package raft

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"distributed-db/internal/model"
)

// internalSnapshotSM is the smallest StateMachine needed by these white-box
// tests: partial chunk uploads never reach Apply.
type internalSnapshotSM struct{}

func (internalSnapshotSM) Apply(model.Entry) error { return nil }

type internalSnapshotTransport struct{}

func (internalSnapshotTransport) RequestVote(context.Context, string, *RequestVoteRequest) (*RequestVoteResponse, error) {
	return nil, nil
}

func (internalSnapshotTransport) AppendEntries(context.Context, string, *AppendEntriesRequest) (*AppendEntriesResponse, error) {
	return nil, nil
}

func newInternalChunkNode(t *testing.T) (*Node, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	cfg := Config{
		ID:                 "n1",
		DataDir:            dir,
		ElectionTimeoutMin: 40 * time.Millisecond,
		ElectionTimeoutMax: 80 * time.Millisecond,
		HeartbeatInterval:  10 * time.Millisecond,
	}
	node, err := New(cfg, store, internalSnapshotSM{}, internalSnapshotTransport{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		node.Stop()
		_ = store.Close()
	})
	return node, dir
}

func requireChunkAccepted(t *testing.T, node *Node, req *InstallSnapshotChunkRequest) {
	t.Helper()
	resp, err := node.HandleInstallSnapshotChunk(context.Background(), req)
	if err != nil {
		t.Fatalf("HandleInstallSnapshotChunk: %v", err)
	}
	if !resp.Success {
		t.Fatalf("chunk rejected: %q", resp.Error)
	}
}

func TestInstallSnapshotChunkStaleUploadsAreRemoved(t *testing.T) {
	node, _ := newInternalChunkNode(t)
	node.snapshotChunkTimeout = 10 * time.Millisecond
	requireChunkAccepted(t, node, &InstallSnapshotChunkRequest{
		Term: 1, LeaderID: "leader", Index: 9, SnapshotTerm: 1,
		Offset: 0, Data: []byte("partial"), Size: 100,
	})

	node.snapshotRecvMu.Lock()
	if got := len(node.snapshotRecv); got != 1 {
		node.snapshotRecvMu.Unlock()
		t.Fatalf("active uploads = %d, want 1", got)
	}
	recv := node.snapshotRecv["leader"]
	path := recv.path
	node.snapshotRecvMu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		node.snapshotRecvMu.Lock()
		got = len(node.snapshotRecv)
		node.snapshotRecvMu.Unlock()
		if got == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	node.snapshotRecvMu.Lock()
	got = len(node.snapshotRecv)
	node.snapshotRecvMu.Unlock()
	if got != 0 {
		t.Fatalf("stale uploads after timer = %d, want 0", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale upload file %q still exists: %v", filepath.Base(path), err)
	}
}

func TestInstallSnapshotChunkUploadMapIsBounded(t *testing.T) {
	node, _ := newInternalChunkNode(t)
	node.snapshotChunkTimeout = time.Hour
	for i := 0; i < 6; i++ {
		requireChunkAccepted(t, node, &InstallSnapshotChunkRequest{
			Term: 1, LeaderID: fmt.Sprintf("leader-%d", i), Index: 9, SnapshotTerm: 1,
			Offset: 0, Data: []byte("partial"), Size: 100,
		})
	}
	node.snapshotRecvMu.Lock()
	got := len(node.snapshotRecv)
	node.snapshotRecvMu.Unlock()
	if got > maxSnapshotChunkStreams {
		t.Fatalf("active uploads = %d, want <= %d", got, maxSnapshotChunkStreams)
	}
}
