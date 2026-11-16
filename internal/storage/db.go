package storage

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/pebble"
)

// errClosed is returned when an operation is attempted on an engine that has
// already been closed. Pebble panics on some post-Close operations, so the
// engine tracks its lifecycle itself and fails with an ordinary error instead.
var errClosed = errors.New("storage: engine closed")

var (
	_ Engine   = (*db)(nil)
	_ Batch    = (*batch)(nil)
	_ Iterator = (*iterator)(nil)
	_ Iterator = emptyIterator{}
	_ Snapshot = (*snapshot)(nil)
)

// db is the Pebble-backed Engine implementation.
//
// A single RWMutex guards the engine lifecycle: ordinary operations take a
// read lock (so they still run concurrently with each other) while Close takes
// the write lock. This keeps Close from racing with in-flight Pebble calls,
// which Pebble does not permit.
type db struct {
	pdb        *pebble.DB
	cache      *pebble.Cache // non-nil only when this engine created it
	writeOpts  *pebble.WriteOptions
	syncWrites bool

	mu     sync.RWMutex
	closed bool

	reads  atomic.Uint64
	writes atomic.Uint64
}

// Open opens (creating if necessary) a Pebble database in dir.
//
// Options are translated to Pebble as follows:
//
//   - CacheSize > 0 creates a block cache with pebble.NewCache; the cache is
//     released when the engine is closed. Zero leaves Pebble's default cache.
//   - MemTableSize, MaxOpenFiles, WALBytesPerSync and BytesPerSync are passed
//     through when greater than zero; otherwise Pebble's defaults apply.
//   - Compression accepts "snappy" (the default), "zstd" or "none"
//     (case-insensitive) and is applied to every LSM level.
//   - SyncWrites selects pebble.Sync for all writes and batch commits. When
//     false, pebble.NoSync is used: writes are still atomic and ordered, but a
//     process or machine crash may lose the most recent writes. The data is
//     still durable across a clean Close/reopen because Close flushes.
func Open(dir string, opts Options) (Engine, error) {
	if dir == "" {
		return nil, errors.New("storage: empty directory")
	}
	compression, err := parseCompression(opts.Compression)
	if err != nil {
		return nil, err
	}

	popt := &pebble.Options{}
	// A single level entry is enough: Pebble applies the last level's options
	// to all subsequent levels. This also selects the compression for all
	// levels while leaving block/filter defaults untouched.
	popt.Levels = []pebble.LevelOptions{{Compression: compression}}

	var cache *pebble.Cache
	if opts.CacheSize > 0 {
		cache = pebble.NewCache(opts.CacheSize)
		popt.Cache = cache
	}
	if opts.MemTableSize > 0 {
		popt.MemTableSize = uint64(opts.MemTableSize)
	}
	if opts.MaxOpenFiles > 0 {
		popt.MaxOpenFiles = opts.MaxOpenFiles
	}
	if opts.WALBytesPerSync > 0 {
		popt.WALBytesPerSync = opts.WALBytesPerSync
	}
	if opts.BytesPerSync > 0 {
		popt.BytesPerSync = opts.BytesPerSync
	}

	pdb, err := pebble.Open(dir, popt)
	if err != nil {
		// pebble.Open did not take ownership of a cache we created.
		if cache != nil {
			cache.Unref()
		}
		return nil, fmt.Errorf("storage: open %q: %w", dir, err)
	}

	writeOpts := pebble.NoSync
	if opts.SyncWrites {
		writeOpts = pebble.Sync
	}
	return &db{
		pdb:        pdb,
		cache:      cache,
		writeOpts:  writeOpts,
		syncWrites: opts.SyncWrites,
	}, nil
}

// parseCompression maps the string form used by Options to a Pebble
// compression. The empty string selects Pebble's default (snappy).
func parseCompression(s string) (pebble.Compression, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "snappy":
		return pebble.SnappyCompression, nil
	case "zstd":
		return pebble.ZstdCompression, nil
	case "none":
		return pebble.NoCompression, nil
	default:
		return pebble.NoCompression, fmt.Errorf("storage: unknown compression %q (want snappy, zstd or none)", s)
	}
}

// Get returns a copy of the value stored under key. A missing key yields
// ErrNotFound; an existing key with a zero-length value is not an error.
func (d *db) Get(key []byte) ([]byte, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return nil, errClosed
	}
	d.reads.Add(1)
	return getCopy(d.pdb, key)
}

// Set stores value under key.
func (d *db) Set(key, value []byte) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return errClosed
	}
	if err := d.pdb.Set(key, value, d.writeOpts); err != nil {
		return err
	}
	d.writes.Add(1)
	return nil
}

// Delete removes key. Deleting a missing key is not an error.
func (d *db) Delete(key []byte) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return errClosed
	}
	if err := d.pdb.Delete(key, d.writeOpts); err != nil {
		return err
	}
	d.writes.Add(1)
	return nil
}

// NewBatch returns an atomic write batch. The batch must be closed even when
// it is never committed.
func (d *db) NewBatch() Batch {
	d.mu.RLock()
	defer d.mu.RUnlock()
	// NewBatch panics on a closed DB. Callers that race with Close still get a
	// non-nil Batch whose operations fail cleanly instead of panicking.
	b := &batch{db: d}
	if d.closed {
		b.closed = true
		return b
	}
	b.pb = d.pdb.NewBatch()
	return b
}

// NewIter returns an iterator over [start, end) in ascending key order.
func (d *db) NewIter(start, end []byte) (Iterator, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return nil, errClosed
	}
	return newIterator(d.pdb.NewIter, start, end)
}

// Snapshot returns an independent, consistent read-only view of the engine.
func (d *db) Snapshot() (Snapshot, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return nil, errClosed
	}
	return &snapshot{db: d, snap: d.pdb.NewSnapshot()}, nil
}

// Flush forces the current memtable to stable storage.
func (d *db) Flush() error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return errClosed
	}
	return d.pdb.Flush()
}

// Close flushes pending writes and closes the database. It is idempotent: the
// second and subsequent calls return nil.
func (d *db) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true

	var errs []error
	if err := d.pdb.Flush(); err != nil && !errors.Is(err, pebble.ErrClosed) {
		errs = append(errs, fmt.Errorf("storage: flush: %w", err))
	}
	if err := d.pdb.Close(); err != nil {
		errs = append(errs, fmt.Errorf("storage: close: %w", err))
	}
	if d.cache != nil {
		d.cache.Unref()
		d.cache = nil
	}
	return errors.Join(errs...)
}

// getter is implemented by *pebble.DB and *pebble.Snapshot.
type getter interface {
	Get(key []byte) ([]byte, io.Closer, error)
}

// getCopy reads key and returns an independent copy of the value. Pebble
// values are only valid until the closer is released, hence the copy.
func getCopy(g getter, key []byte) ([]byte, error) {
	value, closer, err := g.Get(key)
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	out := make([]byte, len(value))
	copy(out, value)
	if cerr := closer.Close(); cerr != nil {
		return nil, cerr
	}
	return out, nil
}
