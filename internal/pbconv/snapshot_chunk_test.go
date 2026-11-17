package pbconv

import (
	"reflect"
	"testing"

	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestInstallSnapshotChunkRequestRoundTrip(t *testing.T) {
	in := &raft.InstallSnapshotChunkRequest{
		Term:         9,
		LeaderID:     "n1",
		Index:        42,
		SnapshotTerm: 7,
		Offset:       1 << 20,
		Data:         []byte("chunk-payload"),
		Done:         false,
		Size:         1234,
		CRC32:        0xdeadbeef,
		ChunkCRC32:   0x12345678,
		Membership:   &raft.SnapshotMembership{Voters: []string{"a:1"}, ConfigEpoch: 3},
	}
	pb := InstallSnapshotChunkRequestToPB(in)
	if got := pb.GetChunkCrc32(); got != in.ChunkCRC32 {
		t.Fatalf("ChunkCrc32 = %08x, want %08x", got, in.ChunkCRC32)
	}
	out := InstallSnapshotChunkRequestFromPB(pb)
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip mismatch:\n in: %+v\nout: %+v", in, out)
	}
}

func TestInstallSnapshotChunkRequestNil(t *testing.T) {
	if InstallSnapshotChunkRequestToPB(nil) != nil || InstallSnapshotChunkRequestFromPB(nil) != nil {
		t.Fatal("nil chunk request must stay nil")
	}
	if InstallSnapshotChunkResponseToPB(nil) != nil || InstallSnapshotChunkResponseFromPB(nil) != nil {
		t.Fatal("nil chunk response must stay nil")
	}
}

func TestInstallSnapshotChunkResponseRoundTrip(t *testing.T) {
	in := &raft.InstallSnapshotChunkResponse{
		Term: 5, Success: true, LastIndex: 99, Error: "e", NextOffset: 1234,
	}
	out := InstallSnapshotChunkResponseFromPB(InstallSnapshotChunkResponseToPB(in))
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip mismatch: %+v != %+v", in, out)
	}
}

// TestInstallSnapshotChunkCRCIsWireAdditive pins the wire representation of
// the new field: chunk_crc32 is field number 12 with varint wire type, and a
// request from an old sender (field absent) decodes as zero, which the
// receiver treats as "not supplied".
func TestInstallSnapshotChunkCRCIsWireAdditive(t *testing.T) {
	legacy := &pbv2.InstallSnapshotChunkRequest{
		Term:         1,
		LeaderId:     "n1",
		Index:        2,
		SnapshotTerm: 1,
		Offset:       0,
		Data:         []byte("abc"),
		Size:         3,
		Crc32:        0x01020304,
	}
	legacyWire, err := proto.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy request: %v", err)
	}
	if _, ok := varintField(legacyWire, 12); ok {
		t.Fatal("legacy request unexpectedly carries field 12")
	}
	if got := InstallSnapshotChunkRequestFromPB(legacy).ChunkCRC32; got != 0 {
		t.Fatalf("legacy ChunkCRC32 = %08x, want 0", got)
	}

	withCRC := proto.Clone(legacy).(*pbv2.InstallSnapshotChunkRequest)
	withCRC.ChunkCrc32 = 0x89abcdef
	wire, err := proto.Marshal(withCRC)
	if err != nil {
		t.Fatalf("marshal request with chunk crc: %v", err)
	}
	got, ok := varintField(wire, 12)
	if !ok {
		t.Fatal("field 12 not found in encoded request")
	}
	if got != uint64(withCRC.ChunkCrc32) {
		t.Fatalf("field 12 = %#x, want %#x", got, withCRC.ChunkCrc32)
	}
	var decoded pbv2.InstallSnapshotChunkRequest
	if err := proto.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("unmarshal request with chunk crc: %v", err)
	}
	if decoded.GetChunkCrc32() != withCRC.ChunkCrc32 {
		t.Fatalf("decoded ChunkCrc32 = %08x, want %08x", decoded.GetChunkCrc32(), withCRC.ChunkCrc32)
	}
}

// varintField walks a protobuf wire payload and returns the value of the first
// varint field with the requested number.
func varintField(wire []byte, want protowire.Number) (uint64, bool) {
	for len(wire) > 0 {
		num, typ, n := protowire.ConsumeTag(wire)
		if n < 0 {
			return 0, false
		}
		wire = wire[n:]
		switch typ {
		case protowire.VarintType:
			v, m := protowire.ConsumeVarint(wire)
			if m < 0 {
				return 0, false
			}
			wire = wire[m:]
			if num == want {
				return v, true
			}
		case protowire.Fixed32Type:
			_, m := protowire.ConsumeFixed32(wire)
			if m < 0 {
				return 0, false
			}
			wire = wire[m:]
		case protowire.Fixed64Type:
			_, m := protowire.ConsumeFixed64(wire)
			if m < 0 {
				return 0, false
			}
			wire = wire[m:]
		case protowire.BytesType:
			_, m := protowire.ConsumeBytes(wire)
			if m < 0 {
				return 0, false
			}
			wire = wire[m:]
		default:
			return 0, false
		}
	}
	return 0, false
}
