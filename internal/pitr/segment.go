package pitr

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"time"

	"distributed-db/internal/model"
)

func encodeLogRecordPayload(r LogRecord) ([]byte, error) {
	if len(r.Entry.Type) > 1<<16-1 {
		return nil, fmt.Errorf("%w: command type is too long", ErrRecord)
	}
	key := []byte(r.Entry.Key)
	val := r.Entry.Value
	if uint64(len(key)) > 1<<32-1 || uint64(len(val)) > 1<<32-1 {
		return nil, fmt.Errorf("%w: record key or value is too long", ErrRecord)
	}
	typ := []byte(r.Entry.Type)
	out := make([]byte, 0, 8*3+2+4+4+len(typ)+len(key)+len(val))
	out = appendU64(out, r.Entry.Index)
	out = appendU64(out, r.Entry.Term)
	out = appendU64(out, r.TSO)
	out = appendU16(out, uint16(len(typ)))
	out = appendU32(out, uint32(len(key)))
	out = appendU32(out, uint32(len(val)))
	out = append(out, typ...)
	out = append(out, key...)
	out = append(out, val...)
	return out, nil
}

func decodeLogRecordPayload(payload []byte) (LogRecord, error) {
	off := 0
	index, err := readU64(payload, &off)
	if err != nil {
		return LogRecord{}, err
	}
	term, err := readU64(payload, &off)
	if err != nil {
		return LogRecord{}, err
	}
	tso, err := readU64(payload, &off)
	if err != nil {
		return LogRecord{}, err
	}
	typeLen, err := readU16(payload, &off)
	if err != nil {
		return LogRecord{}, err
	}
	keyLen, err := readU32(payload, &off)
	if err != nil {
		return LogRecord{}, err
	}
	valLen, err := readU32(payload, &off)
	if err != nil {
		return LogRecord{}, err
	}
	rest := uint64(len(payload) - off)
	if uint64(typeLen)+uint64(keyLen)+uint64(valLen) != rest {
		return LogRecord{}, fmt.Errorf("%w: record field lengths do not match payload (%d != %d)",
			ErrCorrupt, uint64(typeLen)+uint64(keyLen)+uint64(valLen), rest)
	}
	typ := string(payload[off : off+int(typeLen)])
	off += int(typeLen)
	key := string(payload[off : off+int(keyLen)])
	off += int(keyLen)
	val := append([]byte(nil), payload[off:off+int(valLen)]...)
	off += int(valLen)
	if off != len(payload) {
		return LogRecord{}, fmt.Errorf("%w: trailing record bytes", ErrCorrupt)
	}
	rec := LogRecord{
		TSO: tso,
		Entry: model.Entry{
			Term:  term,
			Index: index,
			Type:  model.CommandType(typ),
			Key:   key,
			Value: val,
		},
	}
	if err := rec.Validate(); err != nil {
		return LogRecord{}, err
	}
	return rec, nil
}

func encodeSegmentHeader(h SegmentHeader) ([]byte, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	cluster := []byte(h.ClusterID)
	prev := make([]byte, sha256.Size)
	if h.PrevSHA256 != "" {
		b, err := decodeSHA256(h.PrevSHA256)
		if err != nil {
			return nil, fmt.Errorf("%w: previous segment hash: %v", ErrCorrupt, err)
		}
		copy(prev, b)
	}
	body := make([]byte, 0, 2+len(cluster)+8*10+8+sha256.Size)
	body = appendU16(body, uint16(len(cluster)))
	body = append(body, cluster...)
	body = appendU64(body, h.RangeID)
	body = appendU64(body, h.Incarnation)
	body = appendU64(body, h.StartIndex)
	body = appendU64(body, h.EndIndex)
	body = appendU64(body, h.StartTerm)
	body = appendU64(body, h.EndTerm)
	body = appendU64(body, h.EntryCount)
	body = appendU64(body, h.MinTSO)
	body = appendU64(body, h.MaxTSO)
	body = appendU64(body, h.WriterEpoch)
	body = appendU64(body, h.PayloadBytes)
	body = appendU64(body, uint64(h.SealedAt.UnixNano()))
	body = append(body, prev...)
	if len(body) > maxHeaderBody {
		return nil, fmt.Errorf("%w: segment header body is %d bytes, max %d", ErrRecord, len(body), maxHeaderBody)
	}

	out := make([]byte, 0, segmentHeaderPref+len(body))
	out = append(out, segmentMagic...)
	out = appendU16(out, SegmentFormatVersion)
	out = appendU16(out, 0) // flags
	out = appendU32(out, uint32(len(body)))
	out = appendU32(out, crc32Checksum(body))
	out = append(out, body...)
	return out, nil
}

// decodeSegmentHeader decodes one header from data and returns the number of
// bytes consumed.
func decodeSegmentHeader(data []byte) (SegmentHeader, int, error) {
	if len(data) < segmentHeaderPref {
		return SegmentHeader{}, 0, fmt.Errorf("%w: segment header is truncated", ErrCorrupt)
	}
	if string(data[:8]) != segmentMagic {
		return SegmentHeader{}, 0, fmt.Errorf("%w: bad segment magic", ErrCorrupt)
	}
	off := 8
	version, err := readU16(data, &off)
	if err != nil {
		return SegmentHeader{}, 0, err
	}
	if version != SegmentFormatVersion {
		return SegmentHeader{}, 0, fmt.Errorf("%w: segment version %d, want %d",
			ErrCorrupt, version, SegmentFormatVersion)
	}
	flags, err := readU16(data, &off)
	if err != nil {
		return SegmentHeader{}, 0, err
	}
	if flags != 0 {
		return SegmentHeader{}, 0, fmt.Errorf("%w: unsupported segment flags %#x", ErrCorrupt, flags)
	}
	bodyLen, err := readU32(data, &off)
	if err != nil {
		return SegmentHeader{}, 0, err
	}
	if bodyLen == 0 || bodyLen > maxHeaderBody {
		return SegmentHeader{}, 0, fmt.Errorf("%w: invalid segment header length %d", ErrCorrupt, bodyLen)
	}
	wantCRC, err := readU32(data, &off)
	if err != nil {
		return SegmentHeader{}, 0, err
	}
	total := off + int(bodyLen)
	if total > len(data) {
		return SegmentHeader{}, 0, fmt.Errorf("%w: segment header is truncated", ErrCorrupt)
	}
	body := data[off:total]
	if got := crc32Checksum(body); got != wantCRC {
		return SegmentHeader{}, 0, fmt.Errorf("%w: segment header checksum %#x, want %#x", ErrCorrupt, got, wantCRC)
	}
	boff := 0
	clusterLen, err := readU16(body, &boff)
	if err != nil {
		return SegmentHeader{}, 0, err
	}
	if clusterLen == 0 || int(clusterLen) > len(body)-boff {
		return SegmentHeader{}, 0, fmt.Errorf("%w: invalid cluster id length", ErrCorrupt)
	}
	cluster := string(body[boff : boff+int(clusterLen)])
	boff += int(clusterLen)
	var fields [12]uint64
	for i := range fields {
		v, err := readU64(body, &boff)
		if err != nil {
			return SegmentHeader{}, 0, err
		}
		fields[i] = v
	}
	if len(body)-boff != sha256.Size {
		return SegmentHeader{}, 0, fmt.Errorf("%w: segment header has %d trailing bytes", ErrCorrupt, len(body)-boff)
	}
	prevBytes := body[boff : boff+sha256.Size]
	prev := hex.EncodeToString(prevBytes)
	if bytes.Equal(prevBytes, make([]byte, sha256.Size)) {
		prev = ""
	}
	sealedNano := int64(fields[11])
	if sealedNano <= 0 {
		return SegmentHeader{}, 0, fmt.Errorf("%w: invalid seal time %d", ErrCorrupt, sealedNano)
	}
	h := SegmentHeader{
		ClusterID:    cluster,
		RangeID:      fields[0],
		Incarnation:  fields[1],
		StartIndex:   fields[2],
		EndIndex:     fields[3],
		StartTerm:    fields[4],
		EndTerm:      fields[5],
		EntryCount:   fields[6],
		MinTSO:       fields[7],
		MaxTSO:       fields[8],
		WriterEpoch:  fields[9],
		PayloadBytes: fields[10],
		SealedAt:     time.Unix(0, sealedNano).UTC(),
		PrevSHA256:   prev,
	}
	if err := h.Validate(); err != nil {
		return SegmentHeader{}, 0, err
	}
	return h, total, nil
}

// encodeSegment produces a complete segment file. It returns the bytes and
// their lowercase hex SHA-256. The supplied header must already describe
// recs exactly; a mismatch is a programming error and is rejected.
func encodeSegment(h SegmentHeader, recs []LogRecord) ([]byte, string, error) {
	if len(recs) == 0 {
		return nil, "", fmt.Errorf("%w: empty segment", ErrRecord)
	}
	if err := h.Validate(); err != nil {
		return nil, "", err
	}
	if uint64(len(recs)) != h.EntryCount {
		return nil, "", fmt.Errorf("%w: header entry count %d, records %d", ErrRecord, h.EntryCount, len(recs))
	}
	if recs[0].Entry.Index != h.StartIndex || recs[len(recs)-1].Entry.Index != h.EndIndex {
		return nil, "", fmt.Errorf("%w: header index range does not match records", ErrRecord)
	}
	if recs[0].Entry.Term != h.StartTerm || recs[len(recs)-1].Entry.Term != h.EndTerm {
		return nil, "", fmt.Errorf("%w: header term range does not match records", ErrRecord)
	}
	var payloadBytes uint64
	minTSO, maxTSO := recs[0].TSO, recs[0].TSO
	prevTerm := recs[0].Entry.Term
	frames := make([]byte, 0, 64*1024)
	for i, rec := range recs {
		if err := rec.Validate(); err != nil {
			return nil, "", err
		}
		if rec.Entry.Index != h.StartIndex+uint64(i) {
			return nil, "", fmt.Errorf("%w: record %d has index %d, want %d",
				ErrRecord, i, rec.Entry.Index, h.StartIndex+uint64(i))
		}
		if rec.Entry.Term < prevTerm {
			return nil, "", fmt.Errorf("%w: raft term regresses at index %d", ErrRecord, rec.Entry.Index)
		}
		prevTerm = rec.Entry.Term
		if rec.TSO < minTSO {
			minTSO = rec.TSO
		}
		if rec.TSO > maxTSO {
			maxTSO = rec.TSO
		}
		payload, err := encodeLogRecordPayload(rec)
		if err != nil {
			return nil, "", err
		}
		if len(payload) > maxRecordPayload {
			return nil, "", fmt.Errorf("%w: record payload %d exceeds %d", ErrRecord, len(payload), maxRecordPayload)
		}
		frames = appendU32(frames, uint32(len(payload)))
		frames = appendU32(frames, crc32Checksum(payload))
		frames = append(frames, payload...)
		payloadBytes += uint64(len(payload))
	}
	if payloadBytes != h.PayloadBytes {
		return nil, "", fmt.Errorf("%w: header payload bytes %d, records %d", ErrRecord, h.PayloadBytes, payloadBytes)
	}
	if minTSO != h.MinTSO || maxTSO != h.MaxTSO {
		return nil, "", fmt.Errorf("%w: header TSO bounds do not match records", ErrRecord)
	}
	header, err := encodeSegmentHeader(h)
	if err != nil {
		return nil, "", err
	}
	body := make([]byte, 0, len(header)+len(frames))
	body = append(body, header...)
	body = append(body, frames...)
	bodySum := sha256.Sum256(body)
	out := make([]byte, 0, len(body)+segmentFooterLen)
	out = append(out, body...)
	out = append(out, segmentFooterMagic...)
	out = append(out, bodySum[:]...)
	if len(out) > maxSegmentBytes {
		return nil, "", fmt.Errorf("%w: segment size %d exceeds %d", ErrRecord, len(out), maxSegmentBytes)
	}
	fileSum := sha256.Sum256(out)
	return out, hex.EncodeToString(fileSum[:]), nil
}

// readSegmentFile opens and fully validates one segment before invoking sink
// for any record. The complete-file SHA-256 and footer body hash are checked
// first, then each framed record's CRC32C. Sink may be nil.
func readSegmentFile(ctx context.Context, dir, clusterID string, info SegmentInfo, sink func(LogRecord) error) (*SegmentHeader, error) {
	p, err := segmentFilePath(dir, info)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %w: segment %s", ErrGap, ErrMissing, info.Path)
		}
		return nil, fmt.Errorf("%w: read segment %s: %v", ErrCorrupt, info.Path, err)
	}
	if int64(len(data)) != info.SizeBytes {
		return nil, fmt.Errorf("%w: segment %s size %d, manifest says %d",
			ErrCorrupt, info.Path, len(data), info.SizeBytes)
	}
	if len(data) > maxSegmentBytes {
		return nil, fmt.Errorf("%w: segment %s is too large", ErrCorrupt, info.Path)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != info.SHA256 {
		return nil, fmt.Errorf("%w: segment %s full-file sha256 does not match manifest", ErrCorrupt, info.Path)
	}
	if len(data) < segmentHeaderPref+segmentFooterLen {
		return nil, fmt.Errorf("%w: segment %s is truncated", ErrCorrupt, info.Path)
	}
	footerStart := len(data) - segmentFooterLen
	if string(data[footerStart:footerStart+8]) != segmentFooterMagic {
		return nil, fmt.Errorf("%w: segment %s has a bad footer", ErrCorrupt, info.Path)
	}
	bodySum := sha256.Sum256(data[:footerStart])
	if string(bodySum[:]) != string(data[footerStart+8:]) {
		return nil, fmt.Errorf("%w: segment %s body checksum does not match footer", ErrCorrupt, info.Path)
	}
	h, headerLen, err := decodeSegmentHeader(data[:footerStart])
	if err != nil {
		return nil, err
	}
	if headerLen > footerStart {
		return nil, fmt.Errorf("%w: segment %s header overruns body", ErrCorrupt, info.Path)
	}
	if err := matchHeaderInfo(h, clusterID, info); err != nil {
		return nil, err
	}
	pos := headerLen
	expectedIndex := h.StartIndex
	firstTerm := uint64(0)
	prevTerm := h.StartTerm
	var count, payloadBytes uint64
	minTSO, maxTSO := h.MinTSO, h.MaxTSO
	first := true
	for count < h.EntryCount {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if pos+8 > footerStart {
			return nil, fmt.Errorf("%w: segment %s record %d frame is truncated", ErrCorrupt, info.Path, count)
		}
		length := readU32At(data, pos)
		wantCRC := readU32At(data, pos+4)
		pos += 8
		if length == 0 || length > maxRecordPayload {
			return nil, fmt.Errorf("%w: segment %s record %d has length %d", ErrCorrupt, info.Path, count, length)
		}
		if uint64(pos)+uint64(length) > uint64(footerStart) {
			return nil, fmt.Errorf("%w: segment %s record %d overruns body", ErrCorrupt, info.Path, count)
		}
		payload := data[pos : pos+int(length)]
		pos += int(length)
		if got := crc32Checksum(payload); got != wantCRC {
			return nil, fmt.Errorf("%w: segment %s record %d crc32c %#x, want %#x",
				ErrCorrupt, info.Path, count, got, wantCRC)
		}
		rec, err := decodeLogRecordPayload(payload)
		if err != nil {
			return nil, fmt.Errorf("segment %s record %d: %w", info.Path, count, err)
		}
		if rec.Entry.Index != expectedIndex {
			return nil, fmt.Errorf("%w: %w: segment %s has index %d, want %d",
				ErrCorrupt, ErrGap, info.Path, rec.Entry.Index, expectedIndex)
		}
		if rec.Entry.Term < prevTerm {
			return nil, fmt.Errorf("%w: segment %s term regresses at index %d", ErrCorrupt, info.Path, rec.Entry.Index)
		}
		if rec.TSO < h.MinTSO || rec.TSO > h.MaxTSO {
			return nil, fmt.Errorf("%w: segment %s record %d tso %d outside [%d,%d]",
				ErrCorrupt, info.Path, count, rec.TSO, h.MinTSO, h.MaxTSO)
		}
		if first {
			firstTerm = rec.Entry.Term
			minTSO, maxTSO = rec.TSO, rec.TSO
			first = false
		} else {
			if rec.TSO < minTSO {
				minTSO = rec.TSO
			}
			if rec.TSO > maxTSO {
				maxTSO = rec.TSO
			}
		}
		if sink != nil {
			if err := sink(rec); err != nil {
				return nil, err
			}
		}
		payloadBytes += uint64(len(payload))
		prevTerm = rec.Entry.Term
		expectedIndex++
		count++
	}
	if pos != footerStart {
		return nil, fmt.Errorf("%w: segment %s has %d trailing record bytes", ErrCorrupt, info.Path, footerStart-pos)
	}
	if count != h.EntryCount || payloadBytes != h.PayloadBytes {
		return nil, fmt.Errorf("%w: segment %s record count/payload does not match header", ErrCorrupt, info.Path)
	}
	if first || minTSO != h.MinTSO || maxTSO != h.MaxTSO {
		return nil, fmt.Errorf("%w: segment %s tso bounds do not match header", ErrCorrupt, info.Path)
	}
	if h.StartTerm != firstTerm || h.EndTerm != prevTerm {
		return nil, fmt.Errorf("%w: segment %s term bounds do not match records", ErrCorrupt, info.Path)
	}
	return &h, nil
}

func matchHeaderInfo(h SegmentHeader, clusterID string, info SegmentInfo) error {
	if h.ClusterID != clusterID {
		return fmt.Errorf("%w: segment %s cluster %q, manifest cluster %q",
			ErrCorrupt, info.Path, h.ClusterID, clusterID)
	}
	if h.RangeID != info.RangeID ||
		h.StartIndex != info.StartIndex ||
		h.EndIndex != info.EndIndex ||
		h.StartTerm != info.StartTerm ||
		h.EndTerm != info.EndTerm ||
		h.EntryCount != info.EntryCount ||
		h.MinTSO != info.MinTSO ||
		h.MaxTSO != info.MaxTSO ||
		h.WriterEpoch != info.WriterEpoch ||
		h.PayloadBytes != info.PayloadBytes ||
		h.SealedAt.UnixNano() != info.SealedAt.UnixNano() ||
		h.PrevSHA256 != info.PrevSHA256 {
		return fmt.Errorf("%w: segment %s header does not match manifest descriptor", ErrCorrupt, info.Path)
	}
	return nil
}

func segmentFilePath(dir string, info SegmentInfo) (string, error) {
	if err := validateSegmentPath(info.Path, info); err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.FromSlash(info.Path)), nil
}

func readU32At(b []byte, off int) uint32 {
	return uint32(b[off])<<24 | uint32(b[off+1])<<16 | uint32(b[off+2])<<8 | uint32(b[off+3])
}

func crc32Checksum(b []byte) uint32 {
	// crc32cTable is immutable after initialization, so no archive state is
	// shared between Writer/Reader instances.
	return crc32.Checksum(b, crc32cTable)
}
