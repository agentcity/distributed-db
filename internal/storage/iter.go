package storage

import (
	"bytes"
	"sync"

	"github.com/cockroachdb/pebble"
)

// iterator wraps a Pebble iterator. Key and Value are borrowed from Pebble and
// remain valid only until the next positioning call, matching the Iterator
// contract. Bounds are enforced by Pebble (UpperBound is exclusive).
type iterator struct {
	it     *pebble.Iterator
	closed bool
}

// newIterator builds an iterator over [start, end) and positions it on the
// first key. A nil start means "from the beginning"; a nil end means "to the
// end". Empty non-nil bounds are treated as ordinary bounds, so an end of
// []byte{} describes an empty range (not "unbounded").
func newIterator(
	create func(*pebble.IterOptions) (*pebble.Iterator, error),
	start, end []byte,
) (Iterator, error) {
	opts, ok := boundOptions(start, end)
	if !ok {
		return emptyIterator{}, nil
	}
	it, err := create(opts)
	if err != nil {
		return nil, err
	}
	w := &iterator{it: it}
	it.First()
	return w, nil
}

// boundOptions validates the [start,end) range and converts it to Pebble
// iterator bounds. It reports false when the range is provably empty.
func boundOptions(start, end []byte) (*pebble.IterOptions, bool) {
	if end != nil && len(end) == 0 {
		// Pebble treats a nil/empty upper bound as "no bound"; an explicitly
		// empty end therefore has to be handled here.
		return nil, false
	}
	if start != nil && end != nil && bytes.Compare(start, end) >= 0 {
		return nil, false
	}
	opts := &pebble.IterOptions{}
	if len(start) > 0 {
		opts.LowerBound = start
	}
	if len(end) > 0 {
		opts.UpperBound = end
	}
	return opts, true
}

func (i *iterator) Valid() bool {
	return !i.closed && i.it.Valid()
}

func (i *iterator) Next() {
	if i.closed {
		return
	}
	i.it.Next()
}

func (i *iterator) Key() []byte {
	if !i.Valid() {
		return nil
	}
	return i.it.Key()
}

func (i *iterator) Value() []byte {
	if !i.Valid() {
		return nil
	}
	return i.it.Value()
}

func (i *iterator) Error() error {
	if i.closed {
		return nil
	}
	return i.it.Error()
}

func (i *iterator) Close() error {
	if i.closed {
		return nil
	}
	i.closed = true
	return i.it.Close()
}

// emptyIterator represents an empty [start,end) range without touching Pebble.
type emptyIterator struct{}

func (emptyIterator) Valid() bool   { return false }
func (emptyIterator) Next()         {}
func (emptyIterator) Key() []byte   { return nil }
func (emptyIterator) Value() []byte { return nil }
func (emptyIterator) Error() error  { return nil }
func (emptyIterator) Close() error  { return nil }

// snapshot is an independent point-in-time view. A snapshot pins its sequence
// number in Pebble, so later writes are invisible to it and its iterators.
type snapshot struct {
	db   *db
	snap *pebble.Snapshot

	mu     sync.RWMutex
	closed bool
}

func (s *snapshot) Get(key []byte) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errClosed
	}
	d := s.db
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return nil, errClosed
	}
	d.reads.Add(1)
	return getCopy(s.snap, key)
}

func (s *snapshot) NewIter(start, end []byte) (Iterator, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errClosed
	}
	d := s.db
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return nil, errClosed
	}
	return newIterator(s.snap.NewIter, start, end)
}

func (s *snapshot) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	d := s.db
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		// Pebble does not support closing a snapshot after the DB itself is
		// closed, so leave the (now irrelevant) snapshot to Pebble.
		return nil
	}
	return s.snap.Close()
}
