package pbconv

import (
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"
)

// InstallSnapshotChunkRequestFromPB converts a streamed snapshot chunk from
// the wire model to the Raft model. RangeID is transport routing metadata and
// is intentionally not part of the Raft request.
func InstallSnapshotChunkRequestFromPB(req *pbv2.InstallSnapshotChunkRequest) *raft.InstallSnapshotChunkRequest {
	if req == nil {
		return nil
	}
	return &raft.InstallSnapshotChunkRequest{
		Term:         req.GetTerm(),
		LeaderID:     req.GetLeaderId(),
		Index:        req.GetIndex(),
		SnapshotTerm: req.GetSnapshotTerm(),
		Offset:       req.GetOffset(),
		Data:         req.GetData(),
		Done:         req.GetDone(),
		Size:         req.GetSize(),
		CRC32:        req.GetCrc32(),
		ChunkCRC32:   req.GetChunkCrc32(),
		Membership:   SnapshotMembershipFromPB(req.GetMembership()),
	}
}

// InstallSnapshotChunkRequestToPB converts a Raft snapshot chunk to the wire
// model. RangeID is left zero because the Raft request does not carry it; the
// transport fills it in before sending.
func InstallSnapshotChunkRequestToPB(req *raft.InstallSnapshotChunkRequest) *pbv2.InstallSnapshotChunkRequest {
	if req == nil {
		return nil
	}
	return &pbv2.InstallSnapshotChunkRequest{
		Term:         req.Term,
		LeaderId:     req.LeaderID,
		Index:        req.Index,
		SnapshotTerm: req.SnapshotTerm,
		Offset:       req.Offset,
		Data:         req.Data,
		Done:         req.Done,
		Size:         req.Size,
		Crc32:        req.CRC32,
		ChunkCrc32:   req.ChunkCRC32,
		Membership:   SnapshotMembershipToPB(req.Membership),
	}
}

// InstallSnapshotChunkResponseFromPB converts a chunk response from the wire
// model to the Raft model.
func InstallSnapshotChunkResponseFromPB(resp *pbv2.InstallSnapshotChunkResponse) *raft.InstallSnapshotChunkResponse {
	if resp == nil {
		return nil
	}
	return &raft.InstallSnapshotChunkResponse{
		Term:       resp.GetTerm(),
		Success:    resp.GetSuccess(),
		LastIndex:  resp.GetLastIndex(),
		Error:      resp.GetError(),
		NextOffset: resp.GetNextOffset(),
	}
}

// InstallSnapshotChunkResponseToPB converts a Raft chunk response to the wire
// model.
func InstallSnapshotChunkResponseToPB(resp *raft.InstallSnapshotChunkResponse) *pbv2.InstallSnapshotChunkResponse {
	if resp == nil {
		return nil
	}
	return &pbv2.InstallSnapshotChunkResponse{
		Term:       resp.Term,
		Success:    resp.Success,
		LastIndex:  resp.LastIndex,
		Error:      resp.Error,
		NextOffset: resp.NextOffset,
	}
}
