package kvserver

import (
	"context"
	"errors"
	"testing"

	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"

	"google.golang.org/grpc"
)

// chunkCaptureClient is a unary-only gRPC client that records the request the
// transport sends and returns a fixed response.
type chunkCaptureClient struct {
	got  *pbv2.InstallSnapshotChunkRequest
	resp *pbv2.InstallSnapshotChunkResponse
}

func (c *chunkCaptureClient) Invoke(_ context.Context, _ string, args, reply any, _ ...grpc.CallOption) error {
	req, ok := args.(*pbv2.InstallSnapshotChunkRequest)
	if !ok {
		return errors.New("kvserver test: unexpected request type")
	}
	c.got = req
	out, ok := reply.(*pbv2.InstallSnapshotChunkResponse)
	if !ok {
		return errors.New("kvserver test: unexpected response type")
	}
	if c.resp != nil {
		out.Term = c.resp.Term
		out.Success = c.resp.Success
		out.LastIndex = c.resp.LastIndex
		out.Error = c.resp.Error
		out.NextOffset = c.resp.NextOffset
	}
	return nil
}

func (c *chunkCaptureClient) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, errors.New("kvserver test: streaming is not supported")
}

// TestPeerTransportInstallSnapshotChunkCarriesChunkCRC checks that the chunk
// transport maps the per-chunk checksum onto the wire request and carries the
// response fields back to Raft.
func TestPeerTransportInstallSnapshotChunkCarriesChunkCRC(t *testing.T) {
	tr := newPeerTransport(nil, 7, nil, nil)
	fake := &chunkCaptureClient{resp: &pbv2.InstallSnapshotChunkResponse{
		Term: 4, Success: true, LastIndex: 99, NextOffset: 12,
	}}
	tr.clients["peer"] = pbv2.NewRaftServiceClient(fake)

	in := &raft.InstallSnapshotChunkRequest{
		Term:         4,
		LeaderID:     "leader",
		Index:        42,
		SnapshotTerm: 3,
		Offset:       5,
		Data:         []byte("hello"),
		Size:         10,
		CRC32:        0xdeadbeef,
		ChunkCRC32:   0x12345678,
		Membership:   &raft.SnapshotMembership{Voters: []string{"a:1"}},
	}
	resp, err := tr.InstallSnapshotChunk(context.Background(), "peer", in)
	if err != nil {
		t.Fatalf("InstallSnapshotChunk: %v", err)
	}
	if fake.got == nil {
		t.Fatal("transport sent no request")
	}
	if got := fake.got.GetRangeId(); got != 7 {
		t.Fatalf("range_id = %d, want 7", got)
	}
	if got := fake.got.GetChunkCrc32(); got != in.ChunkCRC32 {
		t.Fatalf("chunk_crc32 = %08x, want %08x", got, in.ChunkCRC32)
	}
	if got := fake.got.GetCrc32(); got != in.CRC32 {
		t.Fatalf("crc32 = %08x, want total %08x", got, in.CRC32)
	}
	if got := fake.got.GetData(); string(got) != string(in.Data) {
		t.Fatalf("data = %q, want %q", got, in.Data)
	}
	if got := fake.got.GetMembership().GetVoters(); len(got) != 1 || got[0] != "a:1" {
		t.Fatalf("membership voters = %v, want [a:1]", got)
	}
	if resp.Term != 4 || !resp.Success || resp.LastIndex != 99 || resp.NextOffset != 12 {
		t.Fatalf("response = %+v, want term 4 success last 99 next 12", resp)
	}
}
