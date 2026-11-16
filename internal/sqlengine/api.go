// Package sqlengine executes the P0 SQL subset on top of the transactional KV
// client. Tables are stored in the same KV keyspace:
//
//	catalog: 0x01 | tableName        -> JSON schema
//	row:     0x02 | tableID(8 BE) | pk -> JSON row
//
// P0 supports a single-column primary key (INT or TEXT), point lookups by
// primary key and full-table scans. It deliberately has no joins, secondary
// indexes or SQL planner.
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

// Schema is the persisted table descriptor.
type Schema struct {
	ID      uint64   `json:"id"`
	Name    string   `json:"name"`
	Columns []Column `json:"columns"`
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
