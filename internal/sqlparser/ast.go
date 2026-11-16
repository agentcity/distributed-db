// Package sqlparser implements the small PostgreSQL-like SQL subset used by
// the v2 SQL layer: CREATE/DROP TABLE, CREATE/DROP INDEX, INSERT, SELECT,
// UPDATE and DELETE. WHERE accepts equality, inequality and range predicates
// (=, !=, <, <=, >, >=, BETWEEN) on one column. SELECT also supports a single
// ORDER BY column, LIMIT, aggregates, GROUP BY and one INNER JOIN with an
// equality ON predicate. It is intentionally not a general parser.
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

// CreateIndex creates a non-unique secondary index on one column.
type CreateIndex struct {
	Name   string
	Table  string
	Column string
}

func (CreateIndex) isStatement() {}

// DropIndex drops a secondary index.
type DropIndex struct {
	Name  string
	Table string
}

func (DropIndex) isStatement() {}

// Insert adds one row.
type Insert struct {
	Table   string
	Columns []string // optional; empty means schema order
	Values  []Value
}

func (Insert) isStatement() {}

// Aggregate is a single aggregate projection such as COUNT(*) or SUM(col).
type Aggregate struct {
	Func   string // COUNT, SUM, AVG, MIN, MAX
	Column string // empty for COUNT(*)
	Star   bool
}

// JoinClause describes the single supported INNER JOIN:
//
//	... FROM LeftTable [AS LeftAlias]
//	    JOIN RightTable [AS RightAlias]
//	    ON LeftColumn = RightColumn
//
// Column references keep the spelling from the input and may be qualified
// ("alias.column") or unqualified ("column").
type JoinClause struct {
	LeftTable   string
	RightTable  string
	LeftAlias   string // empty when the left table has no alias
	RightAlias  string // empty when the right table has no alias
	LeftColumn  string
	RightColumn string
}

// ComparisonOp is the comparison operator of a WHERE predicate. The zero
// value is equality so Conditions built before range predicates existed keep
// their meaning.
type ComparisonOp int

const (
	OpEQ ComparisonOp = iota
	OpNE
	OpLT
	OpLE
	OpGT
	OpGE
	OpBetween
)

// String returns the SQL spelling of the operator.
func (op ComparisonOp) String() string {
	switch op {
	case OpNE:
		return "!="
	case OpLT:
		return "<"
	case OpLE:
		return "<="
	case OpGT:
		return ">"
	case OpGE:
		return ">="
	case OpBetween:
		return "BETWEEN"
	default:
		return "="
	}
}

// IsRange reports whether op selects an open-ended or bounded value range.
func (op ComparisonOp) IsRange() bool {
	switch op {
	case OpLT, OpLE, OpGT, OpGE, OpBetween:
		return true
	}
	return false
}

// Select reads rows. ORDER BY supports a single column; LIMIT is applied after
// ordering. A zero Limit means no LIMIT clause.
type Select struct {
	Table     string
	Columns   []string // empty or ["*"] means all columns
	Aggregate *Aggregate
	Join      *JoinClause // nil for a single-table SELECT
	Where     *Condition
	GroupBy   string // empty means no grouping
	OrderBy   string // empty means no ORDER BY
	OrderDesc bool   // ORDER BY ... DESC
	Limit     int
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

// Condition is one supported WHERE predicate: column OP literal, or
// column BETWEEN low AND high. High is only set for OpBetween.
type Condition struct {
	Column string
	Op     ComparisonOp
	Value  Value
	High   Value
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
