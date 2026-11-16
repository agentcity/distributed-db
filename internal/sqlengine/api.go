// Package sqlengine executes the small SQL subset on top of the transactional
// KV client. Tables are stored in the same KV keyspace:
//
//	catalog: 0x01 | tableName        -> JSON schema (version + approximate row count)
//	row:     0x02 | tableID(8 BE) | pk -> JSON row
//	index:   0x03 | tableID(8 BE) | indexID(8 BE) | len(4 BE) | value | pk
//
// The executor supports a single-column primary key (INT or TEXT), point and
// range predicates, secondary-index equality and range selection, ORDER BY /
// LIMIT pushdown, aggregates, and one INNER join with an equality predicate.
// A small cost-based planner picks the access path and the build side of the
// join; plans are cached by normalized SQL plus schema version.
package sqlengine

import "context"

// KV is one row returned by Txn.Scan.
type KV struct {
	Key   []byte
	Value []byte
}

// Txn is the subset of a transactional KV transaction used by the engine.
type Txn interface {
	Get(ctx context.Context, key []byte) ([]byte, error)
	Set(key, value []byte)
	Delete(key []byte)
	Scan(ctx context.Context, start, end []byte, limit int) ([]KV, error)
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// RangeScanner is an optional Txn extension. ScanRanges returns every visible
// row in [start, end) in key order even when the interval spans several
// ranges, applying limit globally (limit <= 0 means unlimited). A Txn that
// does not implement it is still supported but its Scan only covers the range
// that owns start.
type RangeScanner interface {
	ScanRanges(ctx context.Context, start, end []byte, limit int) ([]KV, error)
}

// Client begins transactions.
type Client interface {
	Begin(ctx context.Context) (Txn, error)
}

// Result is the JSON-friendly result of one statement.
type Result struct {
	Columns      []string `json:"columns,omitempty"`
	Rows         [][]any  `json:"rows,omitempty"`
	RowsAffected int      `json:"rows_affected,omitempty"`
	Message      string   `json:"message,omitempty"`
}

// Column describes a table column.
type Column struct {
	Name       string `json:"name"`
	Type       string `json:"type"` // INT or TEXT
	PrimaryKey bool   `json:"primary_key,omitempty"`
}

// Index describes a non-unique secondary index.
type Index struct {
	Name   string `json:"name"`
	Column string `json:"column"`
}

// Schema is the persisted table descriptor.
type Schema struct {
	ID       uint64   `json:"id"`
	Name     string   `json:"name"`
	Version  uint64   `json:"version"`
	RowCount uint64   `json:"row_count"`
	Columns  []Column `json:"columns"`
	Indexes  []Index  `json:"indexes,omitempty"`
}

// PrimaryKeyIndex returns the index of the primary-key column.
func (s *Schema) PrimaryKeyIndex() (int, bool) {
	for i := range s.Columns {
		if s.Columns[i].PrimaryKey {
			return i, true
		}
	}
	return 0, false
}
