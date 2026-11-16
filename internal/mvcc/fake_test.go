package mvcc

import (
	"bytes"
	"errors"
	"sort"
	"sync"

	"distributed-db/internal/storage"
)

// fakeEngine is a test-only in-memory implementation of storage.Engine. It
// exists because internal/storage currently only contains api.go; when
// storage.Open lands, the tests can be switched to a temp Pebble database.
// Production code never imports or uses this type.
type fakeEngine struct {
	mu     sync.RWMutex
	data   map[string][]byte
	closed bool

	// failNextBatchCommit makes the next batch Commit fail before applying
	// anything, for exercising the prewrite cleanup path.
	failNextBatchCommit error
}

var errFakeClosed = errors.New("fake storage: closed")

func newFakeEngine() *fakeEngine {
	return &fakeEngine{data: make(map[string][]byte)}
}

// reopen simulates closing and reopening the same persistent database: it
// returns a new engine with a copy of the current contents.
func (e *fakeEngine) reopen() *fakeEngine {
	e.mu.RLock()
	defer e.mu.RUnlock()
	data := make(map[string][]byte, len(e.data))
	for key, value := range e.data {
		data[key] = append([]byte(nil), value...)
	}
	return &fakeEngine{data: data}
}

func (e *fakeEngine) Get(key []byte) ([]byte, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	value, ok := e.data[string(key)]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return append([]byte(nil), value...), nil
}

func (e *fakeEngine) Set(key, value []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errFakeClosed
	}
	e.data[string(key)] = append([]byte(nil), value...)
	return nil
}

func (e *fakeEngine) Delete(key []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errFakeClosed
	}
	delete(e.data, string(key))
	return nil
}

func (e *fakeEngine) NewBatch() storage.Batch {
	return &fakeBatch{engine: e}
}

func (e *fakeEngine) NewIter(start, end []byte) (storage.Iterator, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return newFakeIter(e.data, start, end), nil
}

func (e *fakeEngine) Snapshot() (storage.Snapshot, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	data := make(map[string][]byte, len(e.data))
	for key, value := range e.data {
		data[key] = append([]byte(nil), value...)
	}
	return &fakeSnapshot{data: data}, nil
}

func (e *fakeEngine) Flush() error { return nil }

func (e *fakeEngine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	return nil
}

func (e *fakeEngine) Stats() storage.Stats {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var size int64
	for key, value := range e.data {
		size += int64(len(key) + len(value))
	}
	return storage.Stats{DiskSize: size, MemTableSize: size}
}

type fakeOp struct {
	key    []byte
	value  []byte
	delete bool
}

type fakeBatch struct {
	engine *fakeEngine
	ops    []fakeOp
	closed bool
}

func (b *fakeBatch) Set(key, value []byte) error {
	if b.closed {
		return errFakeClosed
	}
	b.ops = append(b.ops, fakeOp{
		key:   append([]byte(nil), key...),
		value: append([]byte(nil), value...),
	})
	return nil
}

func (b *fakeBatch) Delete(key []byte) error {
	if b.closed {
		return errFakeClosed
	}
	b.ops = append(b.ops, fakeOp{key: append([]byte(nil), key...), delete: true})
	return nil
}

func (b *fakeBatch) Commit() error {
	if b.closed {
		return errFakeClosed
	}
	b.engine.mu.Lock()
	defer b.engine.mu.Unlock()
	if b.engine.closed {
		return errFakeClosed
	}
	if b.engine.failNextBatchCommit != nil {
		err := b.engine.failNextBatchCommit
		b.engine.failNextBatchCommit = nil
		return err
	}
	for _, op := range b.ops {
		if op.delete {
			delete(b.engine.data, string(op.key))
		} else {
			b.engine.data[string(op.key)] = append([]byte(nil), op.value...)
		}
	}
	b.closed = true
	return nil
}

func (b *fakeBatch) Close() error {
	b.closed = true
	return nil
}

func (b *fakeBatch) Count() int { return len(b.ops) }

type fakeEntry struct {
	key   []byte
	value []byte
}

type fakeIter struct {
	entries []fakeEntry
	index   int
	closed  bool
}

func newFakeIter(data map[string][]byte, start, end []byte) *fakeIter {
	keys := make([]string, 0, len(data))
	for key := range data {
		if start != nil && bytes.Compare([]byte(key), start) < 0 {
			continue
		}
		if end != nil && bytes.Compare([]byte(key), end) >= 0 {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]fakeEntry, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, fakeEntry{
			key:   []byte(key),
			value: append([]byte(nil), data[key]...),
		})
	}
	return &fakeIter{entries: entries}
}

func (it *fakeIter) Valid() bool {
	return !it.closed && it.index >= 0 && it.index < len(it.entries)
}

func (it *fakeIter) Next() {
	if it.index < len(it.entries) {
		it.index++
	}
}

func (it *fakeIter) Key() []byte {
	if !it.Valid() {
		return nil
	}
	return it.entries[it.index].key
}

func (it *fakeIter) Value() []byte {
	if !it.Valid() {
		return nil
	}
	return it.entries[it.index].value
}

func (it *fakeIter) Error() error { return nil }

func (it *fakeIter) Close() error {
	it.closed = true
	return nil
}

type fakeSnapshot struct {
	data   map[string][]byte
	closed bool
}

func (s *fakeSnapshot) Get(key []byte) ([]byte, error) {
	value, ok := s.data[string(key)]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return append([]byte(nil), value...), nil
}

func (s *fakeSnapshot) NewIter(start, end []byte) (storage.Iterator, error) {
	return newFakeIter(s.data, start, end), nil
}

func (s *fakeSnapshot) Close() error {
	s.closed = true
	return nil
}
