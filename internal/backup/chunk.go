package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"distributed-db/internal/v2client"
)

// Chunk file layout, all integers big-endian:
//
//	magic        [8]byte  "KVBCHNK1"
//	version      uint32   (1)
//	range_id     uint64
//	key_count    uint32
//	payload_bytes uint64   sum of len(key)+len(value)
//	records...   repeated: uvarint key_len, key, uvarint value_len, value
//	digest       [32]byte sha256 of every preceding byte
//
// The digest is both stored in the file and recorded in the manifest. A chunk
// is only accepted when both copies agree, so a truncated or bit-flipped file
// fails before any key is restored.
const (
	chunkMagic       = "KVBCHNK1"
	chunkVersion     = 1
	chunkHeaderSize  = 8 + 4 + 8 + 4 + 8
	chunkTrailerSize = sha256.Size
)

type chunkData struct {
	rangeID      uint64
	rows         []v2client.KV
	payloadBytes int64
	digest       []byte
	sizeBytes    int64
}

// chunkRelPath returns the slash-separated path of chunk index inside a
// backup directory.
func chunkRelPath(index int) string {
	return fmt.Sprintf("%s/%09d.chunk", ChunksDir, index)
}

// writeChunkFile writes rows as one immutable chunk and returns its manifest
// descriptor. The caller must ensure rows are non-empty and belong to rangeID.
func writeChunkFile(dir string, index int, rangeID uint64, rows []v2client.KV) (Chunk, error) {
	if len(rows) == 0 {
		return Chunk{}, errors.New("backup: refusing to write an empty chunk")
	}
	var payload int64
	for i := range rows {
		if i > 0 && bytes.Compare(rows[i-1].Key, rows[i].Key) >= 0 {
			return Chunk{}, fmt.Errorf("backup: chunk rows are not strictly ascending at %q", rows[i].Key)
		}
		payload += int64(len(rows[i].Key) + len(rows[i].Value))
	}

	rel := chunkRelPath(index)
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Chunk{}, fmt.Errorf("backup: create chunk dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return Chunk{}, fmt.Errorf("backup: create chunk %s: %w", path, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(path)
		}
	}()

	h := sha256.New()
	mw := io.MultiWriter(f, h)
	header := make([]byte, chunkHeaderSize)
	copy(header[0:8], chunkMagic)
	binary.BigEndian.PutUint32(header[8:12], chunkVersion)
	binary.BigEndian.PutUint64(header[12:20], rangeID)
	binary.BigEndian.PutUint32(header[20:24], uint32(len(rows)))
	binary.BigEndian.PutUint64(header[24:32], uint64(payload))
	if _, err := mw.Write(header); err != nil {
		return Chunk{}, fmt.Errorf("backup: write chunk header: %w", err)
	}
	var scratch [binary.MaxVarintLen64]byte
	for _, kv := range rows {
		n := binary.PutUvarint(scratch[:], uint64(len(kv.Key)))
		if _, err := mw.Write(scratch[:n]); err != nil {
			return Chunk{}, fmt.Errorf("backup: write chunk key length: %w", err)
		}
		if _, err := mw.Write(kv.Key); err != nil {
			return Chunk{}, fmt.Errorf("backup: write chunk key: %w", err)
		}
		n = binary.PutUvarint(scratch[:], uint64(len(kv.Value)))
		if _, err := mw.Write(scratch[:n]); err != nil {
			return Chunk{}, fmt.Errorf("backup: write chunk value length: %w", err)
		}
		if _, err := mw.Write(kv.Value); err != nil {
			return Chunk{}, fmt.Errorf("backup: write chunk value: %w", err)
		}
	}
	digest := h.Sum(nil)
	if _, err := f.Write(digest); err != nil {
		return Chunk{}, fmt.Errorf("backup: write chunk digest: %w", err)
	}
	if err := f.Sync(); err != nil {
		return Chunk{}, fmt.Errorf("backup: sync chunk: %w", err)
	}
	if err := f.Close(); err != nil {
		return Chunk{}, fmt.Errorf("backup: close chunk: %w", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return Chunk{}, fmt.Errorf("backup: stat chunk: %w", err)
	}
	ok = true
	return Chunk{
		Index: index, RangeID: rangeID, File: rel,
		KeyCount: len(rows), PayloadBytes: payload, SizeBytes: st.Size(),
		FirstKey: append([]byte(nil), rows[0].Key...),
		LastKey:  append([]byte(nil), rows[len(rows)-1].Key...),
		SHA256:   fmt.Sprintf("%x", digest),
	}, nil
}

// readChunkFile reads and fully validates one chunk file. It verifies the
// trailer digest, all framing, the record count and ascending key order.
func readChunkFile(path string) (chunkData, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return chunkData{}, fmt.Errorf("%w: read chunk %s: %v", ErrCorrupt, path, err)
	}
	if len(data) < chunkHeaderSize+chunkTrailerSize {
		return chunkData{}, fmt.Errorf("%w: chunk %s is truncated", ErrCorrupt, path)
	}
	body := data[:len(data)-chunkTrailerSize]
	trailer := data[len(data)-chunkTrailerSize:]
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:], trailer) {
		return chunkData{}, fmt.Errorf("%w: chunk %s digest mismatch", ErrCorrupt, path)
	}
	if string(body[0:8]) != chunkMagic {
		return chunkData{}, fmt.Errorf("%w: chunk %s has bad magic", ErrCorrupt, path)
	}
	version := binary.BigEndian.Uint32(body[8:12])
	if version != chunkVersion {
		return chunkData{}, fmt.Errorf("%w: chunk %s version %d", ErrCorrupt, path, version)
	}
	rangeID := binary.BigEndian.Uint64(body[12:20])
	keyCount := binary.BigEndian.Uint32(body[20:24])
	payload := binary.BigEndian.Uint64(body[24:32])
	if keyCount == 0 {
		return chunkData{}, fmt.Errorf("%w: chunk %s has no keys", ErrCorrupt, path)
	}
	// Every record needs at least two length bytes, so a wildly larger count
	// cannot be genuine and must not drive a huge allocation.
	if uint64(keyCount) > uint64(len(body)-chunkHeaderSize)/2 {
		return chunkData{}, fmt.Errorf("%w: chunk %s key count %d is implausible", ErrCorrupt, path, keyCount)
	}

	r := bytes.NewReader(body[chunkHeaderSize:])
	rows := make([]v2client.KV, 0, keyCount)
	var prevKey []byte
	var gotPayload uint64
	for i := uint32(0); i < keyCount; i++ {
		key, err := readChunkBytes(r)
		if err != nil {
			return chunkData{}, fmt.Errorf("%w: chunk %s record %d key: %v", ErrCorrupt, path, i, err)
		}
		value, err := readChunkBytes(r)
		if err != nil {
			return chunkData{}, fmt.Errorf("%w: chunk %s record %d value: %v", ErrCorrupt, path, i, err)
		}
		if prevKey != nil && bytes.Compare(prevKey, key) >= 0 {
			return chunkData{}, fmt.Errorf("%w: chunk %s keys are not strictly ascending", ErrCorrupt, path)
		}
		prevKey = key
		gotPayload += uint64(len(key) + len(value))
		rows = append(rows, v2client.KV{Key: key, Value: value})
	}
	if r.Len() != 0 {
		return chunkData{}, fmt.Errorf("%w: chunk %s has trailing bytes", ErrCorrupt, path)
	}
	if gotPayload != payload {
		return chunkData{}, fmt.Errorf("%w: chunk %s payload %d, want %d", ErrCorrupt, path, gotPayload, payload)
	}
	return chunkData{
		rangeID: rangeID, rows: rows, payloadBytes: int64(payload),
		digest: sum[:], sizeBytes: int64(len(data)),
	}, nil
}

func readChunkBytes(r *bytes.Reader) ([]byte, error) {
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	if n > uint64(r.Len()) {
		return nil, fmt.Errorf("length %d exceeds remaining %d bytes", n, r.Len())
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
