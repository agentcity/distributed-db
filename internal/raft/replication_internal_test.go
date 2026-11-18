package raft

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"distributed-db/internal/model"
)

// snapshotBoundaryTransport models a follower that kept a divergent log suffix
// after the leader's snapshot index. An AppendEntries probe at or above the
// suffix conflicts with a ConflictIndex equal to the snapshot index (the first
// index of the suffix's term is represented by the snapshot itself). A probe at
// exactly the snapshot boundary succeeds and replaces the suffix.
type snapshotBoundaryTransport struct {
	mu          sync.Mutex
	appends     []*AppendEntriesRequest
	installs    int
	snapshotIdx uint64
	lastIdx     uint64
}

func (t *snapshotBoundaryTransport) RequestVote(context.Context, string, *RequestVoteRequest) (*RequestVoteResponse, error) {
	return nil, errors.New("not used")
}

func (t *snapshotBoundaryTransport) AppendEntries(_ context.Context, _ string, req *AppendEntriesRequest) (*AppendEntriesResponse, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.appends = append(t.appends, req)
	if req.PrevLogIndex >= t.snapshotIdx+1 {
		return &AppendEntriesResponse{Term: req.Term, Success: false, ConflictIndex: t.snapshotIdx}, nil
	}
	return &AppendEntriesResponse{
		Term:       req.Term,
		Success:    true,
		MatchIndex: t.lastIdx,
	}, nil
}

func (t *snapshotBoundaryTransport) InstallSnapshot(_ context.Context, _ string, req *InstallSnapshotRequest) (*InstallSnapshotResponse, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.installs++
	return &InstallSnapshotResponse{Term: req.Term, Success: true, LastIndex: t.lastIdx}, nil
}

// TestAppendEntriesConflictAtSnapshotBoundaryProbesAppendEntries covers the
// catch-up deadlock where a follower's conflict index equals the leader's
// snapshot index. Repeating InstallSnapshot cannot make progress for such a
// follower (it already covers the snapshot, and reports its longer divergent
// suffix as the last index), so the leader must probe with AppendEntries at the
// snapshot boundary instead.
func TestAppendEntriesConflictAtSnapshotBoundaryProbesAppendEntries(t *testing.T) {
	const (
		snapshotIndex = 10
		lastIndex     = 20
	)
	tr := &snapshotBoundaryTransport{snapshotIdx: snapshotIndex, lastIdx: lastIndex}

	n := &Node{
		cfg:         Config{ID: "n1"},
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		tr:          tr,
		term:        2,
		role:        Leader,
		voters:      []string{"n1", "n2"},
		learners:    map[string]bool{},
		configEpoch: 1,
		snapshot:    Snapshot{Index: snapshotIndex, Term: 1},
		lastApplied: snapshotIndex,
		commitIndex: lastIndex,
		nextIndex:   map[string]uint64{"n2": lastIndex + 1},
		matchIndex:  map[string]uint64{"n2": 0},
		lastAck:     map[string]time.Time{},
	}
	for i := uint64(snapshotIndex) + 1; i <= lastIndex; i++ {
		n.entries = append(n.entries, model.Entry{Term: 2, Index: i, Type: model.CommandNoop})
	}

	// The follower reports a conflict at the snapshot index while its log
	// reaches lastIndex. The leader must not answer with InstallSnapshot.
	if retry := n.sendAppendEntries(context.Background(), "n2", n.term); !retry {
		t.Fatal("conflict was not retried")
	}
	if got := n.nextIndex["n2"]; got != snapshotIndex+1 {
		t.Fatalf("nextIndex after boundary conflict = %d, want %d", got, snapshotIndex+1)
	}
	if retry := n.sendAppendEntries(context.Background(), "n2", n.term); retry {
		t.Fatal("boundary probe reported more entries to send after reaching the leader last index")
	}
	tr.mu.Lock()
	installs := tr.installs
	appends := append([]*AppendEntriesRequest(nil), tr.appends...)
	tr.mu.Unlock()
	if installs != 0 {
		t.Fatalf("leader sent %d InstallSnapshot requests; want an AppendEntries probe first", installs)
	}
	if len(appends) != 2 {
		t.Fatalf("AppendEntries calls = %d, want 2", len(appends))
	}
	probe := appends[1]
	if probe.PrevLogIndex != snapshotIndex {
		t.Fatalf("probe PrevLogIndex = %d, want %d", probe.PrevLogIndex, snapshotIndex)
	}
	if len(probe.Entries) != lastIndex-snapshotIndex {
		t.Fatalf("probe entries = %d, want %d", len(probe.Entries), lastIndex-snapshotIndex)
	}
	if got := n.nextIndex["n2"]; got != lastIndex+1 {
		t.Fatalf("nextIndex after probe = %d, want %d", got, lastIndex+1)
	}
}
