package metaserver

import (
	"encoding/json"
	"fmt"

	"distributed-db/internal/meta"
	"distributed-db/internal/model"
)

// metaCommand is the JSON payload of a replicated meta command.
type metaCommand struct {
	Last   uint64       `json:"last,omitempty"`
	Ranges []meta.Range `json:"ranges,omitempty"`
}

// stateMachine applies replicated meta commands to the in-memory Cluster.
// The leader chooses timestamps; replicas only advance to that value, so the
// state machines stay deterministic.
type stateMachine struct {
	cluster     *meta.Cluster
	lastApplied uint64
}

type snapshotPayload struct {
	Index uint64     `json:"index"`
	State meta.State `json:"state"`
}

func (m *stateMachine) Apply(entry model.Entry) error {
	err := m.apply(entry)
	m.lastApplied = entry.Index
	return err
}

func (m *stateMachine) apply(entry model.Entry) error {
	switch entry.Type {
	case model.CommandNoop:
		return nil
	case model.CommandMetaAllocTS:
		var cmd metaCommand
		if err := json.Unmarshal(entry.Value, &cmd); err != nil {
			return fmt.Errorf("metaserver: decode alloc_ts: %w", err)
		}
		if cmd.Last == 0 {
			return fmt.Errorf("metaserver: alloc_ts with zero last")
		}
		m.cluster.AdvanceTS(cmd.Last)
		return nil
	case model.CommandMetaSetRange:
		var cmd metaCommand
		if err := json.Unmarshal(entry.Value, &cmd); err != nil {
			return fmt.Errorf("metaserver: decode set_ranges: %w", err)
		}
		return m.cluster.SetRanges(cmd.Ranges)
	default:
		return fmt.Errorf("metaserver: unknown command %q", entry.Type)
	}
}

// Snapshot implements raft.Snapshotter.
func (m *stateMachine) Snapshot() ([]byte, uint64, error) {
	payload := snapshotPayload{Index: m.lastApplied, State: m.cluster.Export()}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	return data, payload.Index, nil
}

// RestoreSnapshot implements raft.SnapshotRestorer. The applied index lives in
// Raft's snapshot header, not in the payload.
func (m *stateMachine) RestoreSnapshot(data []byte) error {
	var payload snapshotPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("metaserver: decode snapshot: %w", err)
	}
	if err := m.cluster.Restore(payload.State); err != nil {
		return err
	}
	m.lastApplied = payload.Index
	return nil
}
