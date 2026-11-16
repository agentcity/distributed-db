// Package storage defines the durable ordered key/value engine used by the
// v2 KV product. The interface is deliberately small: batches, iterators and
// snapshots are the only primitives MVCC needs.
package storage

import "errors"

// ErrNotFound is returned by Get when the key does not exist.
var ErrNotFound = errors.New("storage: key not found")

// Options configures the LSM engine. Zero values mean sensible defaults.
type Options struct {
	CacheSize       int64  // block cache, bytes
	MemTableSize    int64  // write buffer, bytes
	MaxOpenFiles    int    // file descriptor cap
	Compression     string // "snappy", "zstd" or "none"
	SyncWrites      bool   // fsync on every Commit/Set/Delete
	WALBytesPerSync int    // fsync the WAL every N bytes (0 = default)
	BytesPerSync    int    // fsync SSTables every N bytes (0 = default)
}

// Stats is a cheap point-in-time view for metrics and the admin API.
type Stats struct {
	DiskSize     int64
	MemTableSize int64
	Reads        uint64
	Writes       uint64
}

// Engine is the durable ordered map. Keys and values are arbitrary bytes;
// lexicographic key order is the only ordering guarantee.
//
// Get returns ErrNotFound for a missing key. Returned slices are copies and
// remain valid after the call.
type Engine interface {
	Get(key []byte) ([]byte, error)
	Set(key, value []byte) error
	Delete(key []byte) error

	// NewBatch returns an atomic write batch. A batch must be closed even if
	// Commit is not called.
	NewBatch() Batch

	// NewIter returns an iterator over [start, end). A nil start means "from
	// the beginning"; a nil end means "to the end".
	NewIter(start, end []byte) (Iterator, error)

	// Snapshot returns a consistent read-only view of the database.
	Snapshot() (Snapshot, error)

	// Flush forces the memtable to stable storage. It is a durability aid,
	// not a requirement for normal writes.
	Flush() error

	Close() error
	Stats() Stats
}

// Batch is an atomic group of writes. Commit is all-or-nothing. Batch is not
// safe for concurrent use.
type Batch interface {
	Set(key, value []byte) error
	Delete(key []byte) error
	Commit() error
	Close() error
	Count() int
}

// Iterator walks keys in ascending order. Key and Value are valid only until
// the next call to Next; callers that need to keep them must copy.
type Iterator interface {
	Valid() bool
	Next()
	Key() []byte
	Value() []byte
	Error() error
	Close() error
}

// Snapshot is a read-only consistent view. It must be closed.
type Snapshot interface {
	Get(key []byte) ([]byte, error)
	NewIter(start, end []byte) (Iterator, error)
	Close() error
}
