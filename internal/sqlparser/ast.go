// Package sqlparser implements the small PostgreSQL-like SQL subset used by
// the v2 SQL layer: CREATE/DROP TABLE, INSERT, SELECT, UPDATE, DELETE with a
// primary-key equality WHERE clause. It is intentionally not a general parser.
package sqlparser

import "fmt"

// Statement is one parsed SQL statement.
type Statement interface{ isStatement() }

// ColumnDef describes one column in CREATE TABLE.
type ColumnDef struct {
	Name       string
	Type       string // INT or TEXT
	PrimaryKey bool
}

// CreateTable creates a table.
type CreateTable struct {
	Name    string
	Columns []ColumnDef
}

func (CreateTable) isStatement() {}

// DropTable drops a table.
type DropTable struct{ Name string }

func (DropTable) isStatement() {}

// Insert adds one row.
type Insert struct {
	Table   string
	Columns []string // optional; empty means schema order
	Values  []Value
}

func (Insert) isStatement() {}

// Select reads rows.
type Select struct {
	Table   string
	Columns []string // empty or ["*"] means all columns
	Where   *Condition
	Limit   int
}

func (Select) isStatement() {}

// Update changes columns of one row by primary key.
type Update struct {
	Table       string
	Assignments []Assignment
	Where       *Condition
}

func (Update) isStatement() {}

// Delete removes one row by primary key.
type Delete struct {
	Table string
	Where *Condition
}

func (Delete) isStatement() {}

// Condition is the supported WHERE form: column = literal.
type Condition struct {
	Column string
	Value  Value
}

// Assignment is one SET column = literal.
type Assignment struct {
	Column string
	Value  Value
}

// ValueKind distinguishes literals.
type ValueKind int

const (
	ValueInt ValueKind = iota + 1
	ValueText
)

// Value is a SQL literal used by the P0 subset.
type Value struct {
	Kind ValueKind
	Int  int64
	Str  string
}

// AsAny returns the Go value used in rows.
func (v Value) AsAny() any {
	if v.Kind == ValueText {
		return v.Str
	}
	return v.Int
}

// ParseError describes a syntax error and its byte offset.
type ParseError struct {
	Pos int
	Msg string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("sql: syntax error at byte %d: %s", e.Pos, e.Msg)
}

// Parse parses exactly one statement.
func Parse(sql string) (Statement, error) {
	return parse(sql)
}
