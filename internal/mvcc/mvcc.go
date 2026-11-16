// Package mvcc implements a versioned key/value store with Percolator-style
// optimistic transactions on top of internal/storage.
//
// # Key layout
//
//	data:  'd' | userKey | bigEndian(commitTS, 8)
//	lock:  'l' | userKey
//	write: 'w' | userKey | bigEndian(commitTS, 8)
//
// A data version is visible only when a matching write record exists. A write
// record is one of put, delete or rollback. Prewrite stores a lock (including
// its value, kind, primary key and TTL) plus a provisional data version at
// startTS; Commit copies the value from the lock to commitTS, writes the write
// record and removes the lock. Rollback removes the lock and writes a rollback
// record at startTS, which makes any provisional data invisible.
//
// # Lock resolution
//
// Resolving a lock (looking at the primary lock, then committing or rolling
// back the secondary) is intentionally deferred to a later milestone. Until
// then reads that hit a lock with startTS <= read timestamp return ErrLocked;
// callers are expected to retry or roll back. GC treats locks below its safe
// point as abandoned, but does not attempt to resolve them.
//
// # Timestamps
//
// startTS and commitTS are supplied by the caller (normally a TSO); this
// package only requires commitTS > startTS and that no two transactions reuse
// a timestamp. It never reads a wall clock.
package mvcc

import (
	"encoding/binary"
	"errors"
	"fmt"

	"distributed-db/internal/storage"
)

// Key space prefixes. They are chosen so that each column family is contiguous
// and can be scanned with a single iterator range.
const (
	dataCF  byte = 'd'
	lockCF  byte = 'l'
	writeCF byte = 'w'

	timestampLen = 8

	// maxTimestamp is the largest encoded timestamp.
	maxTimestamp uint64 = 1<<64 - 1
)

// Lock/write value format versions. They allow the record encodings to evolve
// without silently misinterpreting old records.
const (
	lockValueVersion  byte = 1
	writeValueVersion byte = 1
)

// Record kinds shared by lock and write records.
const (
	kindPut      byte = 1
	kindDelete   byte = 2
	kindRollback byte = 3
)

// defaultLockTTLMillis is stored in every lock record. It is informational for
// now: lock resolution (which will enforce it) is a later milestone, and GC
// uses its safe point rather than wall-clock time to identify abandoned locks.
const defaultLockTTLMillis uint64 = 3000

// lockValueHeaderLen is version + kind + startTS + TTL + primary length.
const lockValueHeaderLen = 1 + 1 + timestampLen + 8 + 4

// writeValueLen is version + kind + startTS.
const writeValueLen = 1 + 1 + timestampLen

// Sentinel errors returned by Store. They are returned unwrapped so callers
// can compare with == as well as errors.Is.
var (
	// ErrKeyNotFound means no visible value exists at the requested timestamp.
	ErrKeyNotFound = errors.New("mvcc: key not found")
	// ErrLocked means another transaction holds a lock that blocks the read or
	// write. Lock resolution is deferred to a later milestone.
	ErrLocked = errors.New("mvcc: key is locked")
	// ErrWriteConflict means the key was committed at a timestamp later than
	// the caller's startTS.
	ErrWriteConflict = errors.New("mvcc: write conflict")
	// ErrAlreadyCommitted means the transaction (or key) was already committed,
	// so it can no longer be rolled back or committed at a different timestamp.
	ErrAlreadyCommitted = errors.New("mvcc: transaction already committed")
)

// KV is a user-visible key/value pair returned by Scan. The byte slices are
// owned by the caller.
type KV struct {
	Key   []byte
	Value []byte
}

// Mutation is one write in a transaction. Delete ignores Value.
type Mutation struct {
	Key    []byte
	Value  []byte
	Delete bool
}

// Store is a versioned view over a storage.Engine. It is safe for concurrent
// use as long as the underlying engine is.
type Store struct {
	engine storage.Engine
}

// New returns a Store backed by engine. The store does not own the engine and
// does not close it.
func New(engine storage.Engine) *Store {
	return &Store{engine: engine}
}

// writeRecord is the decoded value of a write column entry.
type writeRecord struct {
	kind    byte
	startTS uint64
}

// lockRecord is the decoded value of a lock column entry.
type lockRecord struct {
	startTS   uint64
	kind      byte
	ttlMillis uint64
	primary   []byte
	value     []byte
}

// makeVersionedKey builds '<cf> | userKey | be64(version>'.
func makeVersionedKey(cf byte, userKey []byte, version uint64) []byte {
	key := make([]byte, 1+len(userKey)+timestampLen)
	key[0] = cf
	copy(key[1:], userKey)
	binary.BigEndian.PutUint64(key[1+len(userKey):], version)
	return key
}

func dataKey(userKey []byte, commitTS uint64) []byte {
	return makeVersionedKey(dataCF, userKey, commitTS)
}

func writeKey(userKey []byte, commitTS uint64) []byte {
	return makeVersionedKey(writeCF, userKey, commitTS)
}

func lockKey(userKey []byte) []byte {
	key := make([]byte, 1+len(userKey))
	key[0] = lockCF
	copy(key[1:], userKey)
	return key
}

// versionedRange returns the iterator range covering all versions of exactly
// one user key in cf. The end bound is exclusive but may still include data
// keys of longer user keys (the encoding has no length prefix); callers filter
// with decodeVersionedKey and bytes.Equal.
func versionedRange(cf byte, userKey []byte) (start, end []byte) {
	start = makeVersionedKey(cf, userKey, 0)
	end = makeVersionedKey(cf, userKey, maxTimestamp)
	end = append(end, 0)
	return start, end
}

// decodeVersionedKey splits a data/write column key into user key and version.
func decodeVersionedKey(key []byte) (userKey []byte, version uint64, ok bool) {
	if len(key) < 1+timestampLen {
		return nil, 0, false
	}
	userLen := len(key) - 1 - timestampLen
	userKey = key[1 : 1+userLen]
	version = binary.BigEndian.Uint64(key[1+userLen:])
	return userKey, version, true
}

// encodeWrite serialises a write record value.
func encodeWrite(rec writeRecord) []byte {
	value := make([]byte, writeValueLen)
	value[0] = writeValueVersion
	value[1] = rec.kind
	binary.BigEndian.PutUint64(value[2:], rec.startTS)
	return value
}

// decodeWrite parses a write record value.
func decodeWrite(value []byte) (writeRecord, error) {
	if len(value) != writeValueLen {
		return writeRecord{}, fmt.Errorf("mvcc: corrupt write record: %d bytes", len(value))
	}
	if value[0] != writeValueVersion {
		return writeRecord{}, fmt.Errorf("mvcc: unsupported write record version %d", value[0])
	}
	kind := value[1]
	switch kind {
	case kindPut, kindDelete, kindRollback:
	default:
		return writeRecord{}, fmt.Errorf("mvcc: unknown write kind %d", kind)
	}
	return writeRecord{
		kind:    kind,
		startTS: binary.BigEndian.Uint64(value[2:]),
	}, nil
}

// encodeLock serialises a lock record value.
func encodeLock(rec lockRecord) ([]byte, error) {
	if uint64(len(rec.primary)) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("mvcc: primary key too large: %d bytes", len(rec.primary))
	}
	value := make([]byte, lockValueHeaderLen+len(rec.primary)+len(rec.value))
	value[0] = lockValueVersion
	value[1] = rec.kind
	binary.BigEndian.PutUint64(value[2:], rec.startTS)
	binary.BigEndian.PutUint64(value[10:], rec.ttlMillis)
	binary.BigEndian.PutUint32(value[18:], uint32(len(rec.primary)))
	copy(value[lockValueHeaderLen:], rec.primary)
	copy(value[lockValueHeaderLen+len(rec.primary):], rec.value)
	return value, nil
}

// decodeLock parses a lock record value.
func decodeLock(value []byte) (lockRecord, error) {
	if len(value) < lockValueHeaderLen {
		return lockRecord{}, fmt.Errorf("mvcc: corrupt lock record: %d bytes", len(value))
	}
	if value[0] != lockValueVersion {
		return lockRecord{}, fmt.Errorf("mvcc: unsupported lock record version %d", value[0])
	}
	kind := value[1]
	switch kind {
	case kindPut, kindDelete:
	default:
		return lockRecord{}, fmt.Errorf("mvcc: unknown lock kind %d", kind)
	}
	primaryLen := int(binary.BigEndian.Uint32(value[18:22]))
	if primaryLen > len(value)-lockValueHeaderLen {
		return lockRecord{}, fmt.Errorf("mvcc: corrupt lock primary length %d", primaryLen)
	}
	return lockRecord{
		startTS:   binary.BigEndian.Uint64(value[2:]),
		kind:      kind,
		ttlMillis: binary.BigEndian.Uint64(value[10:]),
		primary:   value[lockValueHeaderLen : lockValueHeaderLen+primaryLen],
		value:     value[lockValueHeaderLen+primaryLen:],
	}, nil
}

// normalizeKeys validates and deduplicates a key list. Empty keys are rejected
// because they make the versioned encoding ambiguous.
func normalizeKeys(keys [][]byte) ([][]byte, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("mvcc: at least one key is required")
	}
	out := make([][]byte, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if len(key) == 0 {
			return nil, fmt.Errorf("mvcc: empty key")
		}
		if _, dup := seen[string(key)]; dup {
			continue
		}
		seen[string(key)] = struct{}{}
		out = append(out, key)
	}
	return out, nil
}
