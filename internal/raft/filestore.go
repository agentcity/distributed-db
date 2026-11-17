package raft

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"distributed-db/internal/model"
)

// WAL layout: a sequence of records. Each record is
//
//	4-byte big-endian magic ("RAFT")
//	4-byte big-endian payload length
//	4-byte big-endian CRC32 (IEEE) of the payload
//	payload (JSON-encoded walRecord)
//
// The magic, length and CRC let Load distinguish a torn tail (a crash in the
// middle of a write, or trailing garbage) from a complete record and drop only
// the incomplete suffix.
const (
	walFileName   = "wal.log"
	walFrameHdr   = 12
	walMagic      = 0x52414654 // "RAFT"
	maxWalPayload = 256 << 20

	// Snapshots live beside the WAL, never inside it: a snapshot payload can be
	// much larger than a log record and is written with its own temp+rename.
	snapshotDirName  = "snapshot"
	snapshotMetaName = "meta.json"
)

type walRecord struct {
	Type      string        `json:"type"` // "meta", "append", "truncate", "truncate_prefix", "membership"
	Term      uint64        `json:"term,omitempty"`
	VotedFor  string        `json:"voted_for,omitempty"`
	Entries   []model.Entry `json:"entries,omitempty"`
	FromIndex uint64        `json:"from_index,omitempty"`
	// Membership records. Voters includes the local node; Learners are
	// non-voting members. ConfigEpoch is the membership version; 0 marks a
	// record written by an older version (normalized to epoch 1 on load).
	Voters      []string `json:"voters,omitempty"`
	Learners    []string `json:"learners,omitempty"`
	ConfigEpoch uint64   `json:"config_epoch,omitempty"`
}

// snapshotMeta is the durable description of the current snapshot. The data
// file is versioned by index so a crash between writing the data and the meta
// leaves the previous meta pointing at an untouched file.
//
// Membership is the optional Raft membership envelope. It is absent from
// metadata written by older versions, which must keep loading as legacy
// snapshots.
type snapshotMeta struct {
	Index      uint64              `json:"index"`
	Term       uint64              `json:"term"`
	DataFile   string              `json:"data_file"`
	Size       int64               `json:"size"`
	CRC32      uint32              `json:"crc32"`
	Membership *SnapshotMembership `json:"membership,omitempty"`
}

// FileStore is an append-only WAL LogStore. Every mutation is fsynced before
// it updates the in-memory view or returns, so an acknowledged vote or entry
// survives a process crash.
//
// A FileStore is safe for concurrent use.
type FileStore struct {
	mu       sync.Mutex
	dir      string
	path     string
	f        *os.File
	size     int64
	closed   bool
	term     uint64
	votedFor string
	entries  []model.Entry
	// voters and learners are the last persisted Raft membership. A nil
	// voters slice means no membership record has ever been written.
	// configEpoch is the persisted membership version (0 for a legacy record).
	voters      []string
	learners    []string
	configEpoch uint64
	// prefix is the highest index removed by TruncatePrefix. Entries are
	// contiguous from prefix+1 (or empty).
	prefix   uint64
	snapMeta snapshotMeta
}

var errStoreClosed = errors.New("raft: log store closed")

// FileStore implements the base LogStore and the optional group-commit
// BatchAppender, streaming SnapshotStreamStore, membership and snapshot
// membership capabilities.
var (
	_ BatchAppender                 = (*FileStore)(nil)
	_ SnapshotStreamStore           = (*FileStore)(nil)
	_ SnapshotMembershipStreamStore = (*FileStore)(nil)
	_ MembershipStore               = (*FileStore)(nil)
	_ MembershipEpochStore          = (*FileStore)(nil)
)

// NewFileStore opens (creating if needed) the WAL in dir and replays it. A
// partial or corrupt final record is discarded and the file is truncated back
// to the last complete record.
func NewFileStore(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, errors.New("raft: empty log store directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("raft: create log directory: %w", err)
	}
	path := filepath.Join(dir, walFileName)

	_, statErr := os.Stat(path)
	created := errors.Is(statErr, os.ErrNotExist)

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("raft: open WAL: %w", err)
	}
	s := &FileStore{dir: dir, path: path, f: f}
	// Read the snapshot description before replaying the WAL: entries the
	// snapshot already covers are dropped, so Load starts after its index even
	// if the prefix-truncation record never made it to disk.
	meta, err := readSnapshotMeta(dir)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	s.snapMeta = meta
	s.prefix = meta.Index
	if err := s.replay(); err != nil {
		_ = f.Close()
		return nil, err
	}
	if created {
		if err := syncDir(dir); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("raft: sync log directory: %w", err)
		}
	}
	return s, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// replay rebuilds the in-memory state from the WAL and truncates an incomplete
// tail so future appends are not appended after garbage.
func (s *FileStore) replay() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("raft: read WAL: %w", err)
	}
	off := 0
	for off < len(data) {
		// A torn header or payload can only be the last thing in the file.
		if len(data)-off < walFrameHdr {
			break
		}
		if binary.BigEndian.Uint32(data[off:off+4]) != walMagic {
			// Trailing garbage (or a torn record whose header never landed).
			break
		}
		plen := binary.BigEndian.Uint32(data[off+4 : off+8])
		wantCRC := binary.BigEndian.Uint32(data[off+8 : off+12])
		if uint64(plen) > maxWalPayload {
			// A garbage length can only come from a torn tail; no valid
			// record can be this large.
			break
		}
		end := off + walFrameHdr + int(plen)
		if end > len(data) {
			break
		}
		payload := data[off+walFrameHdr : end]
		var rec walRecord
		bad := crc32.ChecksumIEEE(payload) != wantCRC || json.Unmarshal(payload, &rec) != nil
		if bad {
			if end == len(data) {
				// Corrupt final record: drop it.
				break
			}
			return fmt.Errorf("raft: corrupt WAL record at offset %d", off)
		}
		if err := s.applyRecord(rec); err != nil {
			return err
		}
		off = end
	}
	if off < len(data) {
		if err := os.Truncate(s.path, int64(off)); err != nil {
			return fmt.Errorf("raft: truncate torn WAL tail: %w", err)
		}
		if err := s.f.Sync(); err != nil {
			return fmt.Errorf("raft: sync WAL after tail truncation: %w", err)
		}
	}
	s.size = int64(off)
	if _, err := s.f.Seek(s.size, io.SeekStart); err != nil {
		return fmt.Errorf("raft: seek WAL: %w", err)
	}
	return nil
}

func (s *FileStore) applyRecord(rec walRecord) error {
	switch rec.Type {
	case "meta":
		s.term = rec.Term
		s.votedFor = rec.VotedFor
	case "append":
		for _, e := range rec.Entries {
			if e.Index <= s.prefix {
				// Covered by a snapshot or a prefix truncation that is
				// applied while replaying.
				continue
			}
			if e.Index != s.nextIndexLocked() {
				return fmt.Errorf("raft: WAL append index %d after %d", e.Index, s.lastIndexLocked())
			}
			s.entries = append(s.entries, cloneEntry(e))
		}
	case "truncate":
		if rec.FromIndex == 0 {
			return errors.New("raft: WAL truncate from index 0")
		}
		s.dropFromLocked(rec.FromIndex)
	case "truncate_prefix":
		if rec.FromIndex == 0 {
			return errors.New("raft: WAL truncate prefix from index 0")
		}
		if rec.FromIndex > s.prefix {
			s.prefix = rec.FromIndex
			s.dropThroughLocked(rec.FromIndex)
		}
	case "membership":
		// A legacy WAL has no membership record and keeps working: the node
		// then initializes from its static configuration. A record without a
		// config epoch was written by an older version; epoch 0 is normalized
		// to the initial epoch (1) by the node.
		if len(rec.Voters) == 0 {
			return errors.New("raft: WAL membership record has no voters")
		}
		s.voters = append([]string(nil), rec.Voters...)
		s.learners = append([]string(nil), rec.Learners...)
		s.configEpoch = rec.ConfigEpoch
	default:
		return fmt.Errorf("raft: unknown WAL record type %q", rec.Type)
	}
	return nil
}

// lastIndexLocked returns the index of the last log entry (or the compacted
// prefix when no entries remain).
func (s *FileStore) lastIndexLocked() uint64 {
	if len(s.entries) == 0 {
		return s.prefix
	}
	return s.entries[len(s.entries)-1].Index
}

func (s *FileStore) nextIndexLocked() uint64 {
	return s.lastIndexLocked() + 1
}

func (s *FileStore) dropFromLocked(fromIndex uint64) {
	if fromIndex <= s.prefix {
		s.entries = nil
		return
	}
	keep := make([]model.Entry, 0, len(s.entries))
	for _, e := range s.entries {
		if e.Index < fromIndex {
			keep = append(keep, e)
		}
	}
	s.entries = keep
}

func (s *FileStore) dropThroughLocked(index uint64) {
	keep := make([]model.Entry, 0, len(s.entries))
	for _, e := range s.entries {
		if e.Index > index {
			keep = append(keep, e)
		}
	}
	s.entries = keep
}

// Load returns the durable state. The returned entries are copies.
func (s *FileStore) Load() (PersistentState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return PersistentState{}, errStoreClosed
	}
	return PersistentState{
		Term:     s.term,
		VotedFor: s.votedFor,
		Entries:  cloneEntries(s.entries),
	}, nil
}

// SaveMeta durably records the current term and vote.
func (s *FileStore) SaveMeta(term uint64, votedFor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errStoreClosed
	}
	if err := s.writeRecord(walRecord{Type: "meta", Term: term, VotedFor: votedFor}); err != nil {
		return err
	}
	s.term = term
	s.votedFor = votedFor
	return nil
}

// SaveMembership durably records the current voter and learner sets. Voters
// must be non-empty and include the local node; callers validate that before
// calling. The record is independent of the log, so membership survives a
// restart even after the config entries that produced it were compacted.
//
// SaveMembership writes a legacy-compatible record with no config epoch; use
// SaveMembershipEpoch to persist the epoch as well.
func (s *FileStore) SaveMembership(voters, learners []string) error {
	return s.SaveMembershipEpoch(voters, learners, 0)
}

// SaveMembershipEpoch durably records the voter/learner sets together with the
// monotonic membership config epoch. An epoch of 0 is written as a legacy
// record.
func (s *FileStore) SaveMembershipEpoch(voters, learners []string, epoch uint64) error {
	if len(voters) == 0 {
		return errors.New("raft: membership must have at least one voter")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errStoreClosed
	}
	v := append([]string(nil), voters...)
	l := append([]string(nil), learners...)
	if err := s.writeRecord(walRecord{Type: "membership", Voters: v, Learners: l, ConfigEpoch: epoch}); err != nil {
		return err
	}
	s.voters = v
	s.learners = l
	s.configEpoch = epoch
	return nil
}

// LoadMembership returns the persisted voter and learner sets. A store that
// has never saved membership (including a WAL written by an older version)
// returns nil, nil, nil.
func (s *FileStore) LoadMembership() ([]string, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, errStoreClosed
	}
	if len(s.voters) == 0 {
		return nil, nil, nil
	}
	return append([]string(nil), s.voters...), append([]string(nil), s.learners...), nil
}

// LoadMembershipEpoch returns the persisted voter/learner sets and their config
// epoch. A legacy record has epoch 0; a store with no record returns nil, nil,
// 0, nil.
func (s *FileStore) LoadMembershipEpoch() ([]string, []string, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, 0, errStoreClosed
	}
	if len(s.voters) == 0 {
		return nil, nil, 0, nil
	}
	return append([]string(nil), s.voters...), append([]string(nil), s.learners...), s.configEpoch, nil
}

// Append durably appends contiguous entries. It is atomic with respect to the
// in-memory view: on error nothing is added there.
func (s *FileStore) Append(entries ...model.Entry) error {
	return s.append(entries)
}

// AppendBatch durably appends a contiguous batch of entries as a single WAL
// record: one write and one fsync cover the whole batch. It is atomic with
// respect to both the durable and the in-memory log: on error neither is
// changed. It implements the optional BatchAppender capability used for group
// commit.
func (s *FileStore) AppendBatch(entries []model.Entry) error {
	return s.append(entries)
}

func (s *FileStore) append(entries []model.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errStoreClosed
	}
	next := s.nextIndexLocked()
	for i, e := range entries {
		if e.Index != next+uint64(i) {
			return fmt.Errorf("raft: append index %d, want %d", e.Index, next+uint64(i))
		}
	}
	copied := cloneEntries(entries)
	if err := s.writeRecord(walRecord{Type: "append", Entries: copied}); err != nil {
		return err
	}
	s.entries = append(s.entries, copied...)
	return nil
}

// Truncate durably removes entries with Index >= fromIndex. Truncating at or
// beyond the end of the log is a no-op.
func (s *FileStore) Truncate(fromIndex uint64) error {
	if fromIndex == 0 {
		return errors.New("raft: truncate from index 0")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errStoreClosed
	}
	if fromIndex > s.lastIndexLocked() {
		return nil
	}
	if err := s.writeRecord(walRecord{Type: "truncate", FromIndex: fromIndex}); err != nil {
		return err
	}
	s.dropFromLocked(fromIndex)
	return nil
}

// TruncatePrefix durably removes all entries with Index <= index. The snapshot
// that covers the removed prefix must have been saved first: truncating beyond
// the durable snapshot would make the log unloadable, so it is rejected.
func (s *FileStore) TruncatePrefix(index uint64) error {
	if index == 0 {
		return errors.New("raft: truncate prefix from index 0")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errStoreClosed
	}
	if index <= s.prefix {
		return nil
	}
	if index > s.snapMeta.Index {
		return fmt.Errorf("raft: truncate prefix %d beyond snapshot index %d", index, s.snapMeta.Index)
	}
	if err := s.writeRecord(walRecord{Type: "truncate_prefix", FromIndex: index}); err != nil {
		return err
	}
	s.prefix = index
	s.dropThroughLocked(index)
	return nil
}

// SaveSnapshot durably stores snapshot data and metadata under the store's
// snapshot/ directory. The snapshot's optional membership envelope is stored in
// the metadata, so a node that restarts from this snapshot recovers both the
// data and the voter/learner configuration.
func (s *FileStore) SaveSnapshot(snapshot Snapshot) error {
	return s.SaveSnapshotFromReaderWithMembership(snapshot.Index, snapshot.Term, snapshot.Membership, bytes.NewReader(snapshot.Data))
}

// SaveSnapshotFromReader durably stores a snapshot whose payload is read from
// r. It writes a legacy snapshot without membership metadata; use
// SaveSnapshotFromReaderWithMembership to persist membership atomically with
// the payload.
func (s *FileStore) SaveSnapshotFromReader(index, term uint64, r io.Reader) error {
	return s.SaveSnapshotFromReaderWithMembership(index, term, nil, r)
}

// SaveSnapshotFromReaderWithMembership durably stores a snapshot whose payload
// is read from r, together with its optional membership envelope. The payload
// is streamed to a temporary file in snapshot/ while its size and IEEE CRC-32
// are computed, then fsynced and renamed into a data file versioned by index.
// Only after that does the meta file get atomically replaced, which is the
// commit point: a reader or write error, or a crash before the meta rename,
// leaves the previous snapshot untouched.
//
// A snapshot that is not newer than the current one is ignored without
// consuming r, mirroring SaveSnapshot. A membership envelope is validated
// before anything is written, and a snapshot whose config epoch is older than
// the stored snapshot's epoch is rejected rather than committing a stale
// configuration. The data file and meta file are the only persistent state;
// stale data and .tmp files are removed best-effort after a successful commit.
// SaveSnapshotFromReaderWithMembership never holds the whole payload in memory.
func (s *FileStore) SaveSnapshotFromReaderWithMembership(index, term uint64, membership *SnapshotMembership, r io.Reader) error {
	if index == 0 {
		return errors.New("raft: snapshot index must be positive")
	}
	if r == nil {
		return errors.New("raft: nil snapshot reader")
	}
	normalized, err := normalizeSnapshotMembership(membership)
	if err != nil {
		return err
	}
	membership = normalized
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errStoreClosed
	}
	if index <= s.snapMeta.Index {
		return nil
	}
	if membership != nil && s.snapMeta.Membership != nil &&
		membership.ConfigEpoch < s.snapMeta.Membership.ConfigEpoch {
		return fmt.Errorf("%w: snapshot epoch %d, stored epoch %d",
			ErrStaleSnapshotMembership, membership.ConfigEpoch, s.snapMeta.Membership.ConfigEpoch)
	}

	dir, err := s.snapshotDirLocked()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, fmt.Sprintf("data-%d-*.tmp", index))
	if err != nil {
		return fmt.Errorf("raft: create snapshot temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		// After a successful rename this is a no-op: the temporary name no
		// longer exists.
		_ = os.Remove(tmpName)
	}()

	hasher := crc32.NewIEEE()
	size, err := io.Copy(io.MultiWriter(tmp, hasher), r)
	if err != nil {
		return fmt.Errorf("raft: stream snapshot data: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("raft: chmod snapshot data: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("raft: sync snapshot data: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("raft: close snapshot data: %w", err)
	}

	dataName := fmt.Sprintf("data-%d.bin", index)
	dataPath := filepath.Join(dir, dataName)
	if err := os.Rename(tmpName, dataPath); err != nil {
		return fmt.Errorf("raft: commit snapshot data: %w", err)
	}
	// The rename is durable before the meta file can point at it.
	if err := syncDir(dir); err != nil {
		_ = os.Remove(dataPath)
		return fmt.Errorf("raft: sync snapshot directory: %w", err)
	}

	meta := snapshotMeta{
		Index:      index,
		Term:       term,
		DataFile:   dataName,
		Size:       size,
		CRC32:      hasher.Sum32(),
		Membership: cloneMembership(membership),
	}
	payload, err := json.Marshal(meta)
	if err != nil {
		_ = os.Remove(dataPath)
		return fmt.Errorf("raft: marshal snapshot metadata: %w", err)
	}
	metaPath := filepath.Join(dir, snapshotMetaName)
	if err := writeFileAtomic(metaPath, payload); err != nil {
		// The meta file is the commit point, so the newly written data file is
		// unreferenced and can be removed; the old snapshot still loads.
		_ = os.Remove(dataPath)
		return fmt.Errorf("raft: write snapshot metadata: %w", err)
	}
	s.snapMeta = meta
	s.cleanupSupersededSnapshotFiles(dataName)
	return nil
}

// OpenSnapshot returns the metadata of the current snapshot and a reader over
// its data. The caller owns the reader and must close it. When no snapshot has
// been saved it returns the zero SnapshotMeta, a nil reader and a nil error.
//
// OpenSnapshot does not read or checksum the payload; the returned Size and
// CRC32 let a caller streaming the data (for example to a peer in chunks)
// verify it as it goes. On Unix the reader stays valid even if a newer
// snapshot replaces this one and the old file is cleaned up.
func (s *FileStore) OpenSnapshot() (SnapshotMeta, io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return SnapshotMeta{}, nil, errStoreClosed
	}
	meta, f, err := s.openSnapshotLocked()
	if err != nil {
		return SnapshotMeta{}, nil, err
	}
	if meta.Index == 0 {
		return SnapshotMeta{}, nil, nil
	}
	return SnapshotMeta{
		Index:      meta.Index,
		Term:       meta.Term,
		Size:       meta.Size,
		CRC32:      meta.CRC32,
		File:       filepath.Join(snapshotDirName, meta.DataFile),
		Membership: cloneMembership(meta.Membership),
	}, f, nil
}

// snapshotDirLocked returns the snapshot directory, creating it and syncing
// its parent when needed. The caller holds s.mu.
func (s *FileStore) snapshotDirLocked() (string, error) {
	dir := filepath.Join(s.dir, snapshotDirName)
	_, err := os.Stat(dir)
	created := errors.Is(err, os.ErrNotExist)
	if err != nil && !created {
		return "", fmt.Errorf("raft: stat snapshot directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("raft: create snapshot directory: %w", err)
	}
	if created {
		if err := syncDir(s.dir); err != nil {
			return "", fmt.Errorf("raft: sync log directory: %w", err)
		}
	}
	return dir, nil
}

// cleanupSupersededSnapshotFiles removes data files other than keepDataName
// and any leftover temporary files. Stale files are harmless (openSnapshot
// only follows meta), so failures are ignored.
func (s *FileStore) cleanupSupersededSnapshotFiles(keepDataName string) {
	dir := filepath.Join(s.dir, snapshotDirName)
	des, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, de := range des {
		if de.IsDir() || de.Name() == snapshotMetaName || de.Name() == keepDataName {
			continue
		}
		if strings.HasPrefix(de.Name(), "data-") || strings.HasSuffix(de.Name(), ".tmp") {
			_ = os.Remove(filepath.Join(dir, de.Name()))
		}
	}
}

// LoadSnapshot returns the durable snapshot. When none has ever been saved it
// returns the zero Snapshot and a nil error.
func (s *FileStore) LoadSnapshot() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Snapshot{}, errStoreClosed
	}
	return s.loadSnapshotLocked()
}

func (s *FileStore) loadSnapshotLocked() (Snapshot, error) {
	meta, f, err := s.openSnapshotLocked()
	if err != nil {
		return Snapshot{}, err
	}
	if meta.Index == 0 {
		return Snapshot{}, nil
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return Snapshot{}, fmt.Errorf("raft: read snapshot data: %w", err)
	}
	if int64(len(data)) != meta.Size || crc32.ChecksumIEEE(data) != meta.CRC32 {
		return Snapshot{}, fmt.Errorf("raft: snapshot data at index %d is corrupt", meta.Index)
	}
	return Snapshot{
		Index:      meta.Index,
		Term:       meta.Term,
		Data:       data,
		Membership: cloneMembership(meta.Membership),
	}, nil
}

// openSnapshotLocked opens the data file described by the current metadata.
// It returns a zero meta and a nil file when no snapshot exists. The caller
// holds s.mu or otherwise owns the returned file.
func (s *FileStore) openSnapshotLocked() (snapshotMeta, *os.File, error) {
	meta := s.snapMeta
	if meta.Index == 0 {
		return snapshotMeta{}, nil, nil
	}
	path := filepath.Join(s.dir, snapshotDirName, meta.DataFile)
	f, err := os.Open(path)
	if err != nil {
		return snapshotMeta{}, nil, fmt.Errorf("raft: open snapshot data: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return snapshotMeta{}, nil, fmt.Errorf("raft: stat snapshot data: %w", err)
	}
	if info.Size() != meta.Size {
		_ = f.Close()
		return snapshotMeta{}, nil, fmt.Errorf("raft: snapshot data at index %d is corrupt: size %d, want %d", meta.Index, info.Size(), meta.Size)
	}
	return meta, f, nil
}

// readSnapshotMeta loads and validates the snapshot metadata. A missing file
// (or directory) means the store simply has no snapshot.
func readSnapshotMeta(dir string) (snapshotMeta, error) {
	path := filepath.Join(dir, snapshotDirName, snapshotMetaName)
	payload, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return snapshotMeta{}, nil
		}
		return snapshotMeta{}, fmt.Errorf("raft: read snapshot metadata: %w", err)
	}
	var meta snapshotMeta
	if err := json.Unmarshal(payload, &meta); err != nil {
		return snapshotMeta{}, fmt.Errorf("raft: parse snapshot metadata: %w", err)
	}
	if meta.Index == 0 {
		return snapshotMeta{}, errors.New("raft: snapshot metadata has index 0")
	}
	if meta.DataFile == "" || filepath.Base(meta.DataFile) != meta.DataFile {
		return snapshotMeta{}, fmt.Errorf("raft: invalid snapshot data file %q", meta.DataFile)
	}
	if meta.Size < 0 {
		return snapshotMeta{}, fmt.Errorf("raft: invalid snapshot size %d", meta.Size)
	}
	normalized, err := normalizeSnapshotMembership(meta.Membership)
	if err != nil {
		return snapshotMeta{}, fmt.Errorf("raft: snapshot metadata: %w", err)
	}
	meta.Membership = normalized
	return meta, nil
}

// writeFileAtomic writes b to a sibling temporary file, fsyncs it, renames it
// over path and fsyncs the containing directory.
func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := writeFull(f, b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	return nil
}

// Close syncs and closes the WAL. Close is idempotent. FileStore is unusable
// after Close.
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.f == nil {
		return nil
	}
	err := s.f.Sync()
	if cerr := s.f.Close(); err == nil {
		err = cerr
	}
	s.f = nil
	return err
}

// writeRecord appends one framed record and fsyncs it. On failure it rolls the
// file back to the previously known-good size so a torn write cannot poison
// later appends.
func (s *FileStore) writeRecord(rec walRecord) error {
	frame, err := encodeWalRecord(rec)
	if err != nil {
		return err
	}
	start := s.size
	if err := writeFull(s.f, frame); err != nil {
		s.rollback(start)
		return fmt.Errorf("raft: write WAL: %w", err)
	}
	if err := s.f.Sync(); err != nil {
		s.rollback(start)
		return fmt.Errorf("raft: sync WAL: %w", err)
	}
	s.size = start + int64(len(frame))
	return nil
}

func encodeWalRecord(rec walRecord) ([]byte, error) {
	payload, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("raft: marshal WAL record: %w", err)
	}
	if len(payload) > maxWalPayload {
		return nil, fmt.Errorf("raft: WAL record too large (%d bytes)", len(payload))
	}
	frame := make([]byte, walFrameHdr+len(payload))
	binary.BigEndian.PutUint32(frame[:4], walMagic)
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(payload))
	copy(frame[walFrameHdr:], payload)
	return frame, nil
}

func (s *FileStore) rollback(size int64) {
	_ = s.f.Truncate(size)
	_, _ = s.f.Seek(size, io.SeekStart)
	s.size = size
	_ = s.f.Sync()
}

func writeFull(f *os.File, b []byte) error {
	for len(b) > 0 {
		n, err := f.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
