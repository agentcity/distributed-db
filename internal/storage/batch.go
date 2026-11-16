package storage

import (
	"errors"

	"github.com/cockroachdb/pebble"
)

var (
	errBatchClosed    = errors.New("storage: batch closed")
	errBatchCommitted = errors.New("storage: batch already committed")
)

// batch wraps a Pebble batch. Pebble batches are atomic when committed and
// become unusable after Close; this wrapper adds idempotent Close, a stable
// mutation count and clear errors for use after Close/Commit.
//
// As documented by the Batch interface, a batch is not safe for concurrent
// use. Commit is nevertheless coordinated with the engine lifecycle so that it
// cannot race with Close.
type batch struct {
	db  *db
	pb  *pebble.Batch
	ops int

	closed    bool
	committed bool
}

// Set stages key/value in the batch. The bytes are copied by Pebble.
func (b *batch) Set(key, value []byte) error {
	if b.closed {
		return errBatchClosed
	}
	if b.committed {
		return errBatchCommitted
	}
	if b.pb == nil {
		return errClosed
	}
	if err := b.pb.Set(key, value, nil); err != nil {
		return err
	}
	b.ops++
	return nil
}

// Delete stages a deletion in the batch.
func (b *batch) Delete(key []byte) error {
	if b.closed {
		return errBatchClosed
	}
	if b.committed {
		return errBatchCommitted
	}
	if b.pb == nil {
		return errClosed
	}
	if err := b.pb.Delete(key, nil); err != nil {
		return err
	}
	b.ops++
	return nil
}

// Commit atomically applies the batch. Durability is governed by the engine's
// SyncWrites option.
func (b *batch) Commit() error {
	if b.closed {
		return errBatchClosed
	}
	if b.committed {
		return errBatchCommitted
	}
	if b.pb == nil {
		return errClosed
	}

	d := b.db
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return errClosed
	}
	if err := b.pb.Commit(d.writeOpts); err != nil {
		return err
	}
	b.committed = true
	d.writes.Add(uint64(b.ops))
	return nil
}

// Close releases the batch. It is idempotent and safe to call after Commit.
func (b *batch) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	if b.pb == nil {
		return nil
	}
	return b.pb.Close()
}

// Count reports the number of mutations staged in the batch.
func (b *batch) Count() int {
	return b.ops
}
