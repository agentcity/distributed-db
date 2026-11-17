// Package pbconv converts between Raft types and the protobuf wire model.
package pbconv

import (
	"distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"
)

// SnapshotMembershipToPB converts a Raft snapshot membership envelope.
func SnapshotMembershipToPB(m *raft.SnapshotMembership) *pbv2.SnapshotMembership {
	if m == nil {
		return nil
	}
	return &pbv2.SnapshotMembership{
		Voters:       append([]string(nil), m.Voters...),
		Learners:     append([]string(nil), m.Learners...),
		ConfigEpoch:  m.ConfigEpoch,
		VotersOld:    append([]string(nil), m.VotersOld...),
		AutoFinalize: m.AutoFinalize,
	}
}

// SnapshotMembershipFromPB converts a protobuf membership envelope.
func SnapshotMembershipFromPB(m *pbv2.SnapshotMembership) *raft.SnapshotMembership {
	if m == nil {
		return nil
	}
	return &raft.SnapshotMembership{
		Voters:       append([]string(nil), m.GetVoters()...),
		Learners:     append([]string(nil), m.GetLearners()...),
		ConfigEpoch:  m.GetConfigEpoch(),
		VotersOld:    append([]string(nil), m.GetVotersOld()...),
		AutoFinalize: m.GetAutoFinalize(),
	}
}
