package storage

import (
	"bytes"
	"sync/atomic"
)

// Prefix returns a range-scoped view of engine that stores every key under
// prefix. Keys passed through the returned Engine and its batches and
// iterators are user keys: the wrapper adds prefix before touching the shared
// engine and strips it again on the way out. Values are passed through
// unchanged.
//
// The wrapper is intended for hosting many logical ranges in one physical
// Pebble engine. Callers must use disjoint prefixes: Prefix only isolates keys
// that do not start with another wrapper's prefix. (A prefix that is a byte
// prefix of another prefix shares the longer prefix's keys.) The KV server
// uses a fixed-width range prefix, so its prefixes are always disjoint.
//
// Close on the returned Engine is a no-op: the shared engine has one owner,
// which closes the real engine. Batch.Close and Snapshot.Close still release
// their own resources. Flush affects the shared engine, not just this prefix,
// because Pebble has no per-prefix flush.
//
// Stats is implemented by scanning this prefix; see its method documentation
// for the cost.
func Prefix(engine Engine, prefix []byte) Engine {
	if engine == nil {
		panic("storage: Prefix called with a nil engine")
	}
	p := append([]byte(nil), prefix...)
	return &prefixedEngine{
		engine:    engine,
		prefix:    p,
		prefixEnd: prefixEnd(p),
	}
}

// prefixEnd returns the smallest key greater than every key that starts with
// prefix, or nil when no such key exists (prefix is empty or all 0xFF bytes).
//
// The result is computed by incrementing the last byte that is not 0xFF and
// truncating the rest: prefix "a\xff" ends at "b" and prefix "a\x00" ends at
// "a\x01". This is exact for arbitrary bytes, including 0x00 and 0xFF.
func prefixEnd(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] != 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}

var (
	_ Engine   = (*prefixedEngine)(nil)
	_ Batch    = (*prefixedBatch)(nil)
	_ Iterator = (*prefixedIterator)(nil)
	_ Snapshot = (*prefixedSnapshot)(nil)
)

// prefixedEngine is a range-scoped Engine view over a shared engine.
//
// Reads and Writes are tracked per prefix so Stats remains range-local; they
// count calls made through this wrapper (including committed batch mutations),
// not the shared engine's global counters.
type prefixedEngine struct {
	engine    Engine
	prefix    []byte
	prefixEnd []byte

	reads  atomic.Uint64
	writes atomic.Uint64
}

// fullKey returns a newly allocated prefix+key. The allocation is required
// because callers (and Pebble) may retain or mutate the slice they pass in;
// returning a subslice of a reused buffer would break that contract.
func (p *prefixedEngine) fullKey(key []byte) []byte {
	out := make([]byte, len(p.prefix)+len(key))
	copy(out, p.prefix)
	copy(out[len(p.prefix):], key)
	return out
}

// lowerBound maps an inclusive user-key bound to the shared engine. A nil user
// start means "from the beginning of this prefix" rather than "from the
// beginning of the shared engine".
func (p *prefixedEngine) lowerBound(start []byte) []byte {
	if start == nil {
		if len(p.prefix) == 0 {
			return nil
		}
		return p.prefix
	}
	return p.fullKey(start)
}

// upperBound maps an exclusive user-key bound to the shared engine. A nil user
// end means "up to this prefix's end", not "to the end of the shared engine".
// An explicitly empty non-nil end stays an empty bound (making the range
// empty), preserving the base engine's contract.
func (p *prefixedEngine) upperBound(end []byte) []byte {
	if end == nil {
		return p.prefixEnd
	}
	return p.fullKey(end)
}

func (p *prefixedEngine) iterBounds(start, end []byte) (lower, upper []byte) {
	return p.lowerBound(start), p.upperBound(end)
}

// Get returns the value stored under key in this prefix.
func (p *prefixedEngine) Get(key []byte) ([]byte, error) {
	p.reads.Add(1)
	return p.engine.Get(p.fullKey(key))
}

// Set stores value under key in this prefix.
func (p *prefixedEngine) Set(key, value []byte) error {
	if err := p.engine.Set(p.fullKey(key), value); err != nil {
		return err
	}
	p.writes.Add(1)
	return nil
}

// Delete removes key from this prefix. Deleting a missing key is not an error.
func (p *prefixedEngine) Delete(key []byte) error {
	if err := p.engine.Delete(p.fullKey(key)); err != nil {
		return err
	}
	p.writes.Add(1)
	return nil
}

// NewBatch returns an atomic batch whose keys are scoped to this prefix.
func (p *prefixedEngine) NewBatch() Batch {
	return &prefixedBatch{engine: p, batch: p.engine.NewBatch()}
}

// NewIter returns an iterator over [start,end) in this prefix. The underlying
// iterator is bounded to [prefix+start, prefix+end) (or prefix/nil for
// unbounded ends), and the wrapper strips the prefix from returned keys.
func (p *prefixedEngine) NewIter(start, end []byte) (Iterator, error) {
	lower, upper := p.iterBounds(start, end)
	it, err := p.engine.NewIter(lower, upper)
	if err != nil {
		return nil, err
	}
	return newPrefixedIterator(it, p.prefix), nil
}

// Snapshot returns a consistent read-only view scoped to this prefix.
func (p *prefixedEngine) Snapshot() (Snapshot, error) {
	snap, err := p.engine.Snapshot()
	if err != nil {
		return nil, err
	}
	return &prefixedSnapshot{engine: p, snap: snap}, nil
}

// Flush flushes the shared engine's memtable. Pebble cannot flush a prefix.
func (p *prefixedEngine) Flush() error { return p.engine.Flush() }

// Close is a no-op: the shared engine's owner closes the real engine. This
// keeps a range group's shutdown from tearing down storage used by other
// ranges in the process.
func (p *prefixedEngine) Close() error { return nil }

// Stats returns a range-local approximate size.
//
// DiskSize is the sum of len(storedKey)+len(value) over every record under
// this prefix as of the scan. The stored key includes the shared prefix, so
// the number approximates the bytes this range contributes to the shared
// engine (tombstones and LSM/SST overhead are not included). Computing it
// performs a full prefix scan and is therefore O(records in the range); it is
// intended for periodic stats/auto-split calls, not a hot path.
//
// MemTableSize is always 0: the shared memtable holds every range's writes and
// cannot be attributed to one prefix. Reads and Writes are the per-wrapper
// operation counters described on prefixedEngine.
func (p *prefixedEngine) Stats() Stats {
	st := Stats{
		Reads:  p.reads.Load(),
		Writes: p.writes.Load(),
	}
	it, err := p.engine.NewIter(p.lowerBound(nil), p.prefixEnd)
	if err != nil {
		return st
	}
	defer it.Close()
	for ; it.Valid(); it.Next() {
		st.DiskSize += int64(len(it.Key()) + len(it.Value()))
	}
	return st
}

// prefixedBatch adds the prefix to staged keys. Atomicity is inherited from
// the shared engine's batch.
type prefixedBatch struct {
	engine *prefixedEngine
	batch  Batch
}

func (b *prefixedBatch) Set(key, value []byte) error {
	return b.batch.Set(b.engine.fullKey(key), value)
}

func (b *prefixedBatch) Delete(key []byte) error {
	return b.batch.Delete(b.engine.fullKey(key))
}

func (b *prefixedBatch) Commit() error {
	if err := b.batch.Commit(); err != nil {
		return err
	}
	b.engine.writes.Add(uint64(b.batch.Count()))
	return nil
}

func (b *prefixedBatch) Close() error { return b.batch.Close() }

func (b *prefixedBatch) Count() int { return b.batch.Count() }

// prefixedIterator wraps an iterator over a bounded prefix and exposes user
// keys. Keys whose bytes lie outside the prefix are skipped defensively; with
// the bounds computed by prefixedEngine they should not occur.
type prefixedIterator struct {
	it     Iterator
	prefix []byte
	closed bool
}

func newPrefixedIterator(it Iterator, prefix []byte) *prefixedIterator {
	w := &prefixedIterator{it: it, prefix: prefix}
	w.skipOutside()
	return w
}

func (i *prefixedIterator) skipOutside() {
	for i.it.Valid() && !bytes.HasPrefix(i.it.Key(), i.prefix) {
		i.it.Next()
	}
}

func (i *prefixedIterator) Valid() bool {
	return !i.closed && i.it.Valid()
}

func (i *prefixedIterator) Next() {
	if i.closed {
		return
	}
	i.it.Next()
	i.skipOutside()
}

func (i *prefixedIterator) Key() []byte {
	if !i.Valid() {
		return nil
	}
	key := i.it.Key()
	if len(key) < len(i.prefix) {
		return nil
	}
	return key[len(i.prefix):]
}

func (i *prefixedIterator) Value() []byte {
	if !i.Valid() {
		return nil
	}
	return i.it.Value()
}

func (i *prefixedIterator) Error() error {
	if i.closed {
		return nil
	}
	return i.it.Error()
}

func (i *prefixedIterator) Close() error {
	if i.closed {
		return nil
	}
	i.closed = true
	return i.it.Close()
}

// prefixedSnapshot scopes a consistent snapshot to one prefix. Closing it
// releases the underlying snapshot but never closes the shared engine.
type prefixedSnapshot struct {
	engine *prefixedEngine
	snap   Snapshot
}

func (s *prefixedSnapshot) Get(key []byte) ([]byte, error) {
	s.engine.reads.Add(1)
	return s.snap.Get(s.engine.fullKey(key))
}

func (s *prefixedSnapshot) NewIter(start, end []byte) (Iterator, error) {
	lower, upper := s.engine.iterBounds(start, end)
	it, err := s.snap.NewIter(lower, upper)
	if err != nil {
		return nil, err
	}
	return newPrefixedIterator(it, s.engine.prefix), nil
}

func (s *prefixedSnapshot) Close() error { return s.snap.Close() }
