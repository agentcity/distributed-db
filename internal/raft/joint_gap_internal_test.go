package raft

import (
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"distributed-db/internal/model"
)

// TestJointConfigEntryEpochGapAdoptsCommittedConfiguration covers recovery from a
// configuration entry whose epoch is more than one past the locally applied
// epoch. That happens when intermediate configuration entries were compacted
// away or when a replica fell behind and then received only a later committed
// entry. The committed entry is authoritative; rejecting it left the replica
// permanently on an old configuration and unable to apply later entries.
func TestJointConfigEntryEpochGapAdoptsCommittedConfiguration(t *testing.T) {
	newNode := func() *Node {
		return &Node{
			cfg:         Config{ID: "n1"},
			logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			voters:      []string{"n1", "n2"},
			learners:    map[string]bool{},
			configEpoch: 10,
		}
	}

	t.Run("final entry across a gap", func(t *testing.T) {
		n := newNode()
		value, err := EncodeConfigEntry(ConfigEntryData{
			Voters:   []string{"n1", "n2", "n3"},
			Epoch:    20,
			Learners: nil,
		})
		if err != nil {
			t.Fatalf("EncodeConfigEntry: %v", err)
		}
		changed, err := n.applyFinalConfigLocked(model.Entry{Type: CommandFinalConfig, Value: value})
		if err != nil {
			t.Fatalf("applyFinalConfigLocked(gap) = %v, want success", err)
		}
		if !changed {
			t.Fatal("applyFinalConfigLocked(gap) reported no change")
		}
		if n.isJointLocked() {
			t.Fatalf("configuration is joint after adopting a final entry: old=%v", n.votersOld)
		}
		if n.configEpoch != 20 {
			t.Fatalf("config epoch = %d, want 20", n.configEpoch)
		}
		if !reflect.DeepEqual(n.voters, []string{"n1", "n2", "n3"}) {
			t.Fatalf("voters = %v, want [n1 n2 n3]", n.voters)
		}
	})

	t.Run("joint entry across a gap", func(t *testing.T) {
		n := newNode()
		value, err := EncodeConfigEntry(ConfigEntryData{
			VotersOld: []string{"n1", "n2"},
			Voters:    []string{"n1", "n3"},
			Epoch:     20,
		})
		if err != nil {
			t.Fatalf("EncodeConfigEntry: %v", err)
		}
		changed, err := n.applyJointConfigLocked(model.Entry{Type: CommandJointConfig, Value: value})
		if err != nil {
			t.Fatalf("applyJointConfigLocked(gap) = %v, want success", err)
		}
		if !changed {
			t.Fatal("applyJointConfigLocked(gap) reported no change")
		}
		if !n.isJointLocked() || n.configEpoch != 20 ||
			!reflect.DeepEqual(n.votersOld, []string{"n1", "n2"}) ||
			!reflect.DeepEqual(n.voters, []string{"n1", "n3"}) {
			t.Fatalf("joint state = old %v new %v joint %v epoch %d",
				n.votersOld, n.voters, n.isJointLocked(), n.configEpoch)
		}
	})

	t.Run("stale entry is still rejected", func(t *testing.T) {
		n := newNode()
		value, err := EncodeConfigEntry(ConfigEntryData{
			Voters: []string{"n9"},
			Epoch:  10,
		})
		if err != nil {
			t.Fatalf("EncodeConfigEntry: %v", err)
		}
		if _, err := n.applyFinalConfigLocked(model.Entry{Type: CommandFinalConfig, Value: value}); !errors.Is(err, ErrStaleConfigChange) {
			t.Fatalf("applyFinalConfigLocked(stale) = %v, want ErrStaleConfigChange", err)
		}
		if n.configEpoch != 10 || !reflect.DeepEqual(n.voters, []string{"n1", "n2"}) {
			t.Fatalf("stale entry changed config to epoch %d voters %v", n.configEpoch, n.voters)
		}
	})
}
