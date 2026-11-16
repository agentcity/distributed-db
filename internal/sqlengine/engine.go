package sqlengine

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"distributed-db/internal/sqlparser"

	"github.com/cespare/xxhash/v2"
)

var (
	// ErrNotFound is returned by a transaction when a key does not exist.
	ErrNotFound = errors.New("sqlengine: key not found")
	// ErrTableNotFound means the catalog has no such table.
	ErrTableNotFound = errors.New("sqlengine: table not found")
	// ErrTableExists means CREATE TABLE was called twice.
	ErrTableExists = errors.New("sqlengine: table already exists")
	// ErrRowNotFound means an UPDATE/DELETE target does not exist.
	ErrRowNotFound = errors.New("sqlengine: row not found")
	// ErrDuplicateKey means an INSERT would overwrite a primary key.
	ErrDuplicateKey = errors.New("sqlengine: duplicate primary key")
	// ErrBadColumn means a column is unknown or has the wrong type.
	ErrBadColumn = errors.New("sqlengine: bad column")
	// ErrUnsupported means the P0 engine does not implement this SQL feature.
	ErrUnsupported = errors.New("sqlengine: unsupported")
	// ErrIndexExists means CREATE INDEX reused a name.
	ErrIndexExists = errors.New("sqlengine: index already exists")
	// ErrIndexNotFound means DROP INDEX/SELECT referenced a missing index.
	ErrIndexNotFound = errors.New("sqlengine: index not found")
)

const (
	catalogPrefix byte = 0x01
	rowPrefix     byte = 0x02
	indexPrefix   byte = 0x03
)

// Engine executes the P0 SQL subset over a transactional KV client.
type Engine struct {
	client Client
}

// New returns an engine backed by client.
func New(client Client) *Engine { return &Engine{client: client} }

// Execute parses and executes exactly one SQL statement.
func (e *Engine) Execute(ctx context.Context, sql string) (*Result, error) {
	stmt, err := sqlparser.Parse(sql)
	if err != nil {
		return nil, err
	}
	return e.execute(ctx, stmt)
}

func (e *Engine) execute(ctx context.Context, stmt sqlparser.Statement) (*Result, error) {
	tx, err := e.client.Begin(ctx)
	if err != nil {
		return nil, err
	}
	// Best-effort rollback if Commit was not reached.
	defer func() { _ = tx.Rollback(ctx) }()

	var res *Result
	switch s := stmt.(type) {
	case *sqlparser.CreateTable:
		res, err = e.createTable(ctx, tx, *s)
	case *sqlparser.DropTable:
		res, err = e.dropTable(ctx, tx, *s)
	case *sqlparser.Insert:
		res, err = e.insert(ctx, tx, *s)
	case *sqlparser.Select:
		res, err = e.selectRows(ctx, tx, *s)
	case *sqlparser.Update:
		res, err = e.update(ctx, tx, *s)
	case *sqlparser.CreateIndex:
		res, err = e.createIndex(ctx, tx, *s)
	case *sqlparser.DropIndex:
		res, err = e.dropIndex(ctx, tx, *s)
	case *sqlparser.Delete:
		res, err = e.delete(ctx, tx, *s)
	case sqlparser.CreateTable:
		res, err = e.createTable(ctx, tx, s)
	case sqlparser.DropTable:
		res, err = e.dropTable(ctx, tx, s)
	case sqlparser.Insert:
		res, err = e.insert(ctx, tx, s)
	case sqlparser.Select:
		res, err = e.selectRows(ctx, tx, s)
	case sqlparser.Update:
		res, err = e.update(ctx, tx, s)
	case sqlparser.CreateIndex:
		res, err = e.createIndex(ctx, tx, s)
	case sqlparser.DropIndex:
		res, err = e.dropIndex(ctx, tx, s)
	case sqlparser.Delete:
		res, err = e.delete(ctx, tx, s)
	default:
		return nil, fmt.Errorf("%w: statement %T", ErrUnsupported, stmt)
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return res, nil
}

func (e *Engine) createTable(ctx context.Context, tx Txn, s sqlparser.CreateTable) (*Result, error) {
	if len(s.Columns) == 0 {
		return nil, fmt.Errorf("%w: table has no columns", ErrUnsupported)
	}
	seen := make(map[string]struct{}, len(s.Columns))
	pkCount := 0
	cols := make([]Column, 0, len(s.Columns))
	for _, c := range s.Columns {
		name := strings.ToLower(c.Name)
		if name == "" {
			return nil, fmt.Errorf("%w: empty column name", ErrBadColumn)
		}
		if _, ok := seen[name]; ok {
			return nil, fmt.Errorf("%w: duplicate column %q", ErrBadColumn, c.Name)
		}
		seen[name] = struct{}{}
		typ := strings.ToUpper(c.Type)
		if typ != "INT" && typ != "TEXT" {
			return nil, fmt.Errorf("%w: column %q has type %q", ErrBadColumn, c.Name, c.Type)
		}
		if c.PrimaryKey {
			pkCount++
		}
		cols = append(cols, Column{Name: name, Type: typ, PrimaryKey: c.PrimaryKey})
	}
	if pkCount != 1 {
		return nil, fmt.Errorf("%w: exactly one primary key is required", ErrBadColumn)
	}
	name := strings.ToLower(s.Name)
	if _, err := tx.Get(ctx, catalogKey(name)); err == nil {
		return nil, ErrTableExists
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	schema := Schema{ID: xxhash.Sum64String(name), Name: name, Columns: cols}
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	tx.Set(catalogKey(name), data)
	return &Result{Message: "table created"}, nil
}

func (e *Engine) dropTable(ctx context.Context, tx Txn, s sqlparser.DropTable) (*Result, error) {
	schema, err := e.loadSchema(ctx, tx, s.Name)
	if err != nil {
		return nil, err
	}
	for _, idx := range schema.Indexes {
		idxID := indexID(idx.Name)
		indexRows, err := tx.Scan(ctx, indexTablePrefix(schema.ID, idxID), indexTableEnd(schema.ID, idxID), 0)
		if err != nil {
			return nil, err
		}
		for _, row := range indexRows {
			tx.Delete(row.Key)
		}
	}
	rows, err := tx.Scan(ctx, tablePrefix(schema.ID), tablePrefixEnd(schema.ID), 0)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		tx.Delete(row.Key)
	}
	tx.Delete(catalogKey(schema.Name))
	return &Result{Message: "table dropped"}, nil
}

func (e *Engine) createIndex(ctx context.Context, tx Txn, s sqlparser.CreateIndex) (*Result, error) {
	schema, err := e.loadSchema(ctx, tx, s.Table)
	if err != nil {
		return nil, err
	}
	name := strings.ToLower(s.Name)
	for _, idx := range schema.Indexes {
		if idx.Name == name {
			return nil, ErrIndexExists
		}
	}
	colName := strings.ToLower(s.Column)
	idx, ok := columnIndex(schema, colName)
	if !ok {
		return nil, fmt.Errorf("%w: unknown column %q", ErrBadColumn, s.Column)
	}
	col := schema.Columns[idx]
	if col.PrimaryKey {
		return nil, fmt.Errorf("%w: index on primary key", ErrUnsupported)
	}
	rows, err := tx.Scan(ctx, tablePrefix(schema.ID), tablePrefixEnd(schema.ID), 0)
	if err != nil {
		return nil, err
	}
	idxID := indexID(name)
	for _, row := range rows {
		var values map[string]any
		if err := json.Unmarshal(row.Value, &values); err != nil {
			return nil, err
		}
		normalizeRow(schema, values)
		v, ok := values[col.Name]
		if !ok {
			continue
		}
		lit, err := anyToLiteral(col, v)
		if err != nil {
			return nil, err
		}
		encoded, err := encodeValue(col, lit)
		if err != nil {
			return nil, err
		}
		pk := row.Key[len(tablePrefix(schema.ID)):]
		tx.Set(indexEntryKey(schema.ID, idxID, encoded, pk), nil)
	}
	schema.Indexes = append(schema.Indexes, Index{Name: name, Column: col.Name})
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	tx.Set(catalogKey(schema.Name), data)
	return &Result{Message: "index created"}, nil
}

func (e *Engine) dropIndex(ctx context.Context, tx Txn, s sqlparser.DropIndex) (*Result, error) {
	schema, err := e.loadSchema(ctx, tx, s.Table)
	if err != nil {
		return nil, err
	}
	name := strings.ToLower(s.Name)
	pos := -1
	for i, idx := range schema.Indexes {
		if idx.Name == name {
			pos = i
			break
		}
	}
	if pos < 0 {
		return nil, ErrIndexNotFound
	}
	idxID := indexID(name)
	rows, err := tx.Scan(ctx, indexTablePrefix(schema.ID, idxID), indexTableEnd(schema.ID, idxID), 0)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		tx.Delete(row.Key)
	}
	schema.Indexes = append(schema.Indexes[:pos], schema.Indexes[pos+1:]...)
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	tx.Set(catalogKey(schema.Name), data)
	return &Result{Message: "index dropped"}, nil
}

func (e *Engine) insert(ctx context.Context, tx Txn, s sqlparser.Insert) (*Result, error) {
	schema, err := e.loadSchema(ctx, tx, s.Table)
	if err != nil {
		return nil, err
	}
	values := make(map[string]sqlparser.Value, len(schema.Columns))
	if len(s.Columns) == 0 {
		if len(s.Values) != len(schema.Columns) {
			return nil, fmt.Errorf("%w: got %d values for %d columns", ErrBadColumn, len(s.Values), len(schema.Columns))
		}
		for i, col := range schema.Columns {
			values[col.Name] = s.Values[i]
		}
	} else {
		if len(s.Columns) != len(s.Values) {
			return nil, fmt.Errorf("%w: got %d values for %d columns", ErrBadColumn, len(s.Values), len(s.Columns))
		}
		for i, name := range s.Columns {
			name = strings.ToLower(name)
			if _, ok := columnIndex(schema, name); !ok {
				return nil, fmt.Errorf("%w: unknown column %q", ErrBadColumn, name)
			}
			values[name] = s.Values[i]
		}
	}

	pkIdx, ok := schema.PrimaryKeyIndex()
	if !ok {
		return nil, fmt.Errorf("%w: table has no primary key", ErrBadColumn)
	}
	pkCol := schema.Columns[pkIdx]
	pkVal, ok := values[pkCol.Name]
	if !ok {
		return nil, fmt.Errorf("%w: primary key %q is required", ErrBadColumn, pkCol.Name)
	}
	pk, err := encodePK(pkCol, pkVal)
	if err != nil {
		return nil, err
	}
	key := rowKey(schema.ID, pk)
	if _, err := tx.Get(ctx, key); err == nil {
		return nil, ErrDuplicateKey
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	row := make(map[string]any, len(schema.Columns))
	for _, col := range schema.Columns {
		v, ok := values[col.Name]
		if !ok {
			switch col.Type {
			case "INT":
				row[col.Name] = int64(0)
			case "TEXT":
				row[col.Name] = ""
			}
			continue
		}
		if err := checkLiteralType(col, v); err != nil {
			return nil, err
		}
		row[col.Name] = v.AsAny()
	}
	data, err := json.Marshal(row)
	if err != nil {
		return nil, err
	}
	tx.Set(key, data)
	for _, idx := range schema.Indexes {
		ci, _ := columnIndex(schema, idx.Column)
		col := schema.Columns[ci]
		lit, err := anyToLiteral(col, row[idx.Column])
		if err != nil {
			return nil, err
		}
		encoded, err := encodeValue(col, lit)
		if err != nil {
			return nil, err
		}
		tx.Set(indexEntryKey(schema.ID, indexID(idx.Name), encoded, pk), nil)
	}
	return &Result{RowsAffected: 1, Message: "row inserted"}, nil
}

func (e *Engine) selectRows(ctx context.Context, tx Txn, s sqlparser.Select) (*Result, error) {
	schema, err := e.loadSchema(ctx, tx, s.Table)
	if err != nil {
		return nil, err
	}
	if s.Join != nil {
		return e.selectJoin(ctx, tx, schema, s)
	}
	if s.Aggregate != nil {
		return e.aggregate(ctx, tx, schema, s)
	}
	cols, err := selectColumns(schema, s.Columns)
	if err != nil {
		return nil, err
	}
	rows, err := e.readRows(ctx, tx, schema, s.Where, s.Limit)
	if err != nil {
		return nil, err
	}
	out := make([][]any, 0, len(rows))
	for _, row := range rows {
		line := make([]any, 0, len(cols))
		for _, col := range cols {
			line = append(line, row[col.Name])
		}
		out = append(out, line)
	}
	names := make([]string, 0, len(cols))
	for _, col := range cols {
		names = append(names, col.Name)
	}
	return &Result{Columns: names, Rows: out}, nil
}

// Bounds on the in-memory nested-loop join. The P0 planner never pushes the
// join down, so it scans both tables and hashes the right side.
const (
	maxJoinScanRows   = 100_000
	maxJoinResultRows = 1_000_000
)

const (
	joinSideLeft = iota
	joinSideRight
)

// joinProjection is one output column of a join and the side it comes from.
type joinProjection struct {
	side   int
	column Column
	name   string
}

// joinCondition is a WHERE column = literal resolved against one join side.
type joinCondition struct {
	side   int
	column Column
	value  sqlparser.Value
}

// selectJoin executes one INNER JOIN with an equality predicate and an
// optional single-column WHERE condition, followed by LIMIT. Both inputs are
// scanned in primary-key order, so output order is deterministic: left rows in
// key order, and for each left row its right matches in key order.
func (e *Engine) selectJoin(ctx context.Context, tx Txn, leftSchema *Schema, s sqlparser.Select) (*Result, error) {
	if s.Aggregate != nil {
		return nil, fmt.Errorf("%w: aggregates with JOIN", ErrUnsupported)
	}
	if s.GroupBy != "" {
		return nil, fmt.Errorf("%w: GROUP BY with JOIN", ErrUnsupported)
	}
	join := s.Join
	if join == nil {
		return nil, fmt.Errorf("%w: missing JOIN clause", ErrUnsupported)
	}
	if s.Limit < 0 {
		return nil, fmt.Errorf("%w: negative LIMIT", ErrBadColumn)
	}
	rightSchema, err := e.loadSchema(ctx, tx, join.RightTable)
	if err != nil {
		return nil, err
	}

	leftAlias := joinQualifier(join.LeftAlias, leftSchema.Name)
	rightAlias := joinQualifier(join.RightAlias, rightSchema.Name)

	leftJoinCol, err := resolveJoinSideColumn(join.LeftColumn, leftSchema, leftAlias, "left")
	if err != nil {
		return nil, err
	}
	rightJoinCol, err := resolveJoinSideColumn(join.RightColumn, rightSchema, rightAlias, "right")
	if err != nil {
		return nil, err
	}
	if leftJoinCol.Type != rightJoinCol.Type {
		return nil, fmt.Errorf("%w: JOIN columns %q and %q have different types", ErrBadColumn, join.LeftColumn, join.RightColumn)
	}

	projections, err := joinProjections(s.Columns, leftSchema, rightSchema, leftAlias, rightAlias)
	if err != nil {
		return nil, err
	}
	var where *joinCondition
	if s.Where != nil {
		where, err = resolveJoinCondition(*s.Where, leftSchema, rightSchema, leftAlias, rightAlias)
		if err != nil {
			return nil, err
		}
	}

	// Scan both sides bounded; the extra row lets us detect an oversized input.
	leftRows, err := e.readRows(ctx, tx, leftSchema, nil, maxJoinScanRows+1)
	if err != nil {
		return nil, err
	}
	if len(leftRows) > maxJoinScanRows {
		return nil, fmt.Errorf("%w: JOIN input table %q exceeds %d rows", ErrUnsupported, leftSchema.Name, maxJoinScanRows)
	}
	rightRows, err := e.readRows(ctx, tx, rightSchema, nil, maxJoinScanRows+1)
	if err != nil {
		return nil, err
	}
	if len(rightRows) > maxJoinScanRows {
		return nil, fmt.Errorf("%w: JOIN input table %q exceeds %d rows", ErrUnsupported, rightSchema.Name, maxJoinScanRows)
	}

	// Hash the right side by join value, preserving scan order per value.
	rightByKey := make(map[any][]map[string]any, len(rightRows))
	for _, row := range rightRows {
		key, err := joinKey(rightJoinCol, row)
		if err != nil {
			return nil, err
		}
		rightByKey[key] = append(rightByKey[key], row)
	}

	names := make([]string, len(projections))
	for i, p := range projections {
		names[i] = p.name
	}
	out := make([][]any, 0)
	for _, leftRow := range leftRows {
		key, err := joinKey(leftJoinCol, leftRow)
		if err != nil {
			return nil, err
		}
		for _, rightRow := range rightByKey[key] {
			if where != nil {
				ok, err := where.matches(leftRow, rightRow)
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
			}
			if s.Limit > 0 && len(out) == s.Limit {
				return &Result{Columns: names, Rows: out}, nil
			}
			line := make([]any, len(projections))
			for i, p := range projections {
				if p.side == joinSideLeft {
					line[i] = leftRow[p.column.Name]
				} else {
					line[i] = rightRow[p.column.Name]
				}
			}
			out = append(out, line)
			if len(out) > maxJoinResultRows {
				return nil, fmt.Errorf("%w: JOIN produces more than %d rows", ErrUnsupported, maxJoinResultRows)
			}
		}
	}
	return &Result{Columns: names, Rows: out}, nil
}

// joinQualifier returns the lower-case qualifier used for one join side.
func joinQualifier(alias, table string) string {
	if alias != "" {
		return strings.ToLower(alias)
	}
	return strings.ToLower(table)
}

// splitColumnRef splits "qualifier.column" into its parts.
func splitColumnRef(ref string) (qualifier, column string, qualified bool) {
	pos := strings.IndexByte(ref, '.')
	if pos < 0 {
		return "", ref, false
	}
	return ref[:pos], ref[pos+1:], true
}

// resolveJoinSideColumn resolves an ON operand against the given side. A
// qualified operand must name that side's alias (or the table name when it
// has no alias); an unqualified operand is looked up in that side's schema.
func resolveJoinSideColumn(ref string, schema *Schema, alias, side string) (Column, error) {
	qualifier, name, qualified := splitColumnRef(ref)
	if qualified && !strings.EqualFold(qualifier, alias) {
		return Column{}, fmt.Errorf("%w: column %q is not from the %s table", ErrBadColumn, ref, side)
	}
	idx, ok := columnIndex(schema, strings.ToLower(name))
	if !ok {
		return Column{}, fmt.Errorf("%w: unknown column %q", ErrBadColumn, ref)
	}
	return schema.Columns[idx], nil
}

// resolveJoinColumn resolves a SELECT or WHERE column reference against either
// join side. An unqualified name that exists on both sides is ambiguous.
func resolveJoinColumn(ref string, left, right *Schema, leftAlias, rightAlias string) (int, Column, error) {
	qualifier, name, qualified := splitColumnRef(ref)
	column := strings.ToLower(name)
	if qualified {
		switch {
		case strings.EqualFold(qualifier, leftAlias):
			idx, ok := columnIndex(left, column)
			if !ok {
				return 0, Column{}, fmt.Errorf("%w: unknown column %q", ErrBadColumn, ref)
			}
			return joinSideLeft, left.Columns[idx], nil
		case strings.EqualFold(qualifier, rightAlias):
			idx, ok := columnIndex(right, column)
			if !ok {
				return 0, Column{}, fmt.Errorf("%w: unknown column %q", ErrBadColumn, ref)
			}
			return joinSideRight, right.Columns[idx], nil
		default:
			return 0, Column{}, fmt.Errorf("%w: unknown table qualifier %q in column %q", ErrBadColumn, qualifier, ref)
		}
	}
	leftIdx, leftOK := columnIndex(left, column)
	rightIdx, rightOK := columnIndex(right, column)
	switch {
	case leftOK && rightOK:
		return 0, Column{}, fmt.Errorf("%w: ambiguous column %q", ErrBadColumn, ref)
	case leftOK:
		return joinSideLeft, left.Columns[leftIdx], nil
	case rightOK:
		return joinSideRight, right.Columns[rightIdx], nil
	default:
		return 0, Column{}, fmt.Errorf("%w: unknown column %q", ErrBadColumn, ref)
	}
}

// joinProjections resolves the SELECT list. "*" expands to every left column
// followed by every right column, with qualified names; explicit columns keep
// their unqualified column name.
func joinProjections(refs []string, left, right *Schema, leftAlias, rightAlias string) ([]joinProjection, error) {
	if len(refs) == 0 || (len(refs) == 1 && refs[0] == "*") {
		out := make([]joinProjection, 0, len(left.Columns)+len(right.Columns))
		for _, col := range left.Columns {
			out = append(out, joinProjection{side: joinSideLeft, column: col, name: leftAlias + "." + col.Name})
		}
		for _, col := range right.Columns {
			out = append(out, joinProjection{side: joinSideRight, column: col, name: rightAlias + "." + col.Name})
		}
		return out, nil
	}
	out := make([]joinProjection, 0, len(refs))
	for _, ref := range refs {
		side, col, err := resolveJoinColumn(ref, left, right, leftAlias, rightAlias)
		if err != nil {
			return nil, err
		}
		out = append(out, joinProjection{side: side, column: col, name: col.Name})
	}
	return out, nil
}

// resolveJoinCondition resolves a WHERE column = literal for the join output.
func resolveJoinCondition(cond sqlparser.Condition, left, right *Schema, leftAlias, rightAlias string) (*joinCondition, error) {
	side, col, err := resolveJoinColumn(cond.Column, left, right, leftAlias, rightAlias)
	if err != nil {
		return nil, err
	}
	if err := checkLiteralType(col, cond.Value); err != nil {
		return nil, err
	}
	return &joinCondition{side: side, column: col, value: cond.Value}, nil
}

// matches reports whether the joined row satisfies the WHERE condition.
func (c *joinCondition) matches(leftRow, rightRow map[string]any) (bool, error) {
	row := leftRow
	if c.side == joinSideRight {
		row = rightRow
	}
	lit, err := anyToLiteral(c.column, row[c.column.Name])
	if err != nil {
		return false, err
	}
	return lit.AsAny() == c.value.AsAny(), nil
}

// joinKey returns the comparable hash key for a row's join column.
func joinKey(col Column, row map[string]any) (any, error) {
	lit, err := anyToLiteral(col, row[col.Name])
	if err != nil {
		return nil, err
	}
	return lit.AsAny(), nil
}

func (e *Engine) aggregate(ctx context.Context, tx Txn, schema *Schema, s sqlparser.Select) (*Result, error) {
	if s.GroupBy != "" {
		return e.aggregateGroups(ctx, tx, schema, s)
	}
	rows, err := e.readRows(ctx, tx, schema, s.Where, 0)
	if err != nil {
		return nil, err
	}
	fn := strings.ToUpper(s.Aggregate.Func)
	name := strings.ToLower(fn)
	if s.Aggregate.Star {
		if fn != "COUNT" {
			return nil, fmt.Errorf("%w: %s(*)", ErrUnsupported, fn)
		}
		return &Result{Columns: []string{name}, Rows: [][]any{{int64(len(rows))}}}, nil
	}
	ci, ok := columnIndex(schema, strings.ToLower(s.Aggregate.Column))
	if !ok {
		return nil, fmt.Errorf("%w: unknown column %q", ErrBadColumn, s.Aggregate.Column)
	}
	col := schema.Columns[ci]
	if (fn == "SUM" || fn == "AVG") && col.Type != "INT" {
		return nil, fmt.Errorf("%w: %s requires INT", ErrBadColumn, fn)
	}
	if fn == "COUNT" {
		return &Result{Columns: []string{name}, Rows: [][]any{{int64(len(rows))}}}, nil
	}
	if len(rows) == 0 {
		return &Result{Columns: []string{name}, Rows: [][]any{{nil}}}, nil
	}
	switch fn {
	case "SUM", "AVG":
		var sum int64
		for _, row := range rows {
			n, _ := row[col.Name].(int64)
			sum += n
		}
		if fn == "AVG" {
			return &Result{Columns: []string{name}, Rows: [][]any{{sum / int64(len(rows))}}}, nil
		}
		return &Result{Columns: []string{name}, Rows: [][]any{{sum}}}, nil
	case "MIN", "MAX":
		if col.Type == "INT" {
			best, _ := rows[0][col.Name].(int64)
			for _, row := range rows[1:] {
				n, _ := row[col.Name].(int64)
				if (fn == "MIN" && n < best) || (fn == "MAX" && n > best) {
					best = n
				}
			}
			return &Result{Columns: []string{name}, Rows: [][]any{{best}}}, nil
		}
		best, _ := rows[0][col.Name].(string)
		for _, row := range rows[1:] {
			v, _ := row[col.Name].(string)
			if (fn == "MIN" && v < best) || (fn == "MAX" && v > best) {
				best = v
			}
		}
		return &Result{Columns: []string{name}, Rows: [][]any{{best}}}, nil
	default:
		return nil, fmt.Errorf("%w: aggregate %s", ErrUnsupported, fn)
	}
}

// aggregateGroups executes an aggregate with GROUP BY: one output row per
// distinct group key, sorted by that key.
func (e *Engine) aggregateGroups(ctx context.Context, tx Txn, schema *Schema, s sqlparser.Select) (*Result, error) {
	gi, ok := columnIndex(schema, strings.ToLower(s.GroupBy))
	if !ok {
		return nil, fmt.Errorf("%w: unknown group column %q", ErrBadColumn, s.GroupBy)
	}
	groupCol := schema.Columns[gi]

	fn := strings.ToUpper(s.Aggregate.Func)
	name := strings.ToLower(fn)
	var aggCol Column
	if s.Aggregate.Star {
		if fn != "COUNT" {
			return nil, fmt.Errorf("%w: %s(*)", ErrUnsupported, fn)
		}
	} else {
		ai, ok := columnIndex(schema, strings.ToLower(s.Aggregate.Column))
		if !ok {
			return nil, fmt.Errorf("%w: unknown column %q", ErrBadColumn, s.Aggregate.Column)
		}
		aggCol = schema.Columns[ai]
		if (fn == "SUM" || fn == "AVG") && aggCol.Type != "INT" {
			return nil, fmt.Errorf("%w: %s requires INT", ErrBadColumn, fn)
		}
		switch fn {
		case "COUNT", "SUM", "AVG", "MIN", "MAX":
		default:
			return nil, fmt.Errorf("%w: aggregate %s", ErrUnsupported, fn)
		}
	}

	rows, err := e.readRows(ctx, tx, schema, s.Where, 0)
	if err != nil {
		return nil, err
	}
	cols := []string{groupCol.Name, name}
	if len(rows) == 0 {
		return &Result{Columns: cols, Rows: [][]any{}}, nil
	}

	type bucket struct {
		key  any
		rows []map[string]any
	}
	buckets := make([]bucket, 0)
	byKey := make(map[any]int)
	for _, row := range rows {
		lit, err := anyToLiteral(groupCol, row[groupCol.Name])
		if err != nil {
			return nil, err
		}
		key := lit.AsAny()
		pos, ok := byKey[key]
		if !ok {
			pos = len(buckets)
			byKey[key] = pos
			buckets = append(buckets, bucket{key: key})
		}
		buckets[pos].rows = append(buckets[pos].rows, row)
	}
	sort.Slice(buckets, func(i, j int) bool {
		if groupCol.Type == "INT" {
			return buckets[i].key.(int64) < buckets[j].key.(int64)
		}
		return buckets[i].key.(string) < buckets[j].key.(string)
	})

	out := make([][]any, 0, len(buckets))
	for _, b := range buckets {
		v, err := aggregateGroupValue(fn, aggCol, b.rows)
		if err != nil {
			return nil, err
		}
		out = append(out, []any{b.key, v})
	}
	return &Result{Columns: cols, Rows: out}, nil
}

// aggregateGroupValue computes one aggregate over the rows of a single group.
// Every group has at least one row.
func aggregateGroupValue(fn string, col Column, rows []map[string]any) (any, error) {
	switch fn {
	case "COUNT":
		return int64(len(rows)), nil
	case "SUM", "AVG":
		var sum int64
		for _, row := range rows {
			n, _ := row[col.Name].(int64)
			sum += n
		}
		if fn == "AVG" {
			return sum / int64(len(rows)), nil
		}
		return sum, nil
	case "MIN", "MAX":
		if col.Type == "INT" {
			best, _ := rows[0][col.Name].(int64)
			for _, row := range rows[1:] {
				n, _ := row[col.Name].(int64)
				if (fn == "MIN" && n < best) || (fn == "MAX" && n > best) {
					best = n
				}
			}
			return best, nil
		}
		best, _ := rows[0][col.Name].(string)
		for _, row := range rows[1:] {
			v, _ := row[col.Name].(string)
			if (fn == "MIN" && v < best) || (fn == "MAX" && v > best) {
				best = v
			}
		}
		return best, nil
	default:
		return nil, fmt.Errorf("%w: aggregate %s", ErrUnsupported, fn)
	}
}

func (e *Engine) update(ctx context.Context, tx Txn, s sqlparser.Update) (*Result, error) {
	schema, err := e.loadSchema(ctx, tx, s.Table)
	if err != nil {
		return nil, err
	}
	if s.Where == nil {
		return nil, fmt.Errorf("%w: UPDATE requires WHERE", ErrUnsupported)
	}
	if len(s.Assignments) == 0 {
		return nil, fmt.Errorf("%w: SET list is empty", ErrBadColumn)
	}
	pks, err := e.conditionPKs(ctx, tx, schema, s.Where)
	if err != nil {
		return nil, err
	}
	pkIdx, _ := schema.PrimaryKeyIndex()
	affected := 0
	for _, pk := range pks {
		key := rowKey(schema.ID, pk)
		raw, err := tx.Get(ctx, key)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var row map[string]any
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, err
		}
		normalizeRow(schema, row)
		oldRow := make(map[string]any, len(row))
		for k, v := range row {
			oldRow[k] = v
		}
		for _, as := range s.Assignments {
			name := strings.ToLower(as.Column)
			idx, ok := columnIndex(schema, name)
			if !ok {
				return nil, fmt.Errorf("%w: unknown column %q", ErrBadColumn, name)
			}
			if idx == pkIdx {
				return nil, fmt.Errorf("%w: primary key cannot be updated", ErrUnsupported)
			}
			if err := checkLiteralType(schema.Columns[idx], as.Value); err != nil {
				return nil, err
			}
			row[name] = as.Value.AsAny()
		}
		if err := e.deleteIndexEntries(tx, schema, oldRow, pk); err != nil {
			return nil, err
		}
		data, err := json.Marshal(row)
		if err != nil {
			return nil, err
		}
		tx.Set(key, data)
		if err := e.putIndexEntries(tx, schema, row, pk); err != nil {
			return nil, err
		}
		affected++
	}
	return &Result{RowsAffected: affected, Message: "rows updated"}, nil
}

func (e *Engine) delete(ctx context.Context, tx Txn, s sqlparser.Delete) (*Result, error) {
	schema, err := e.loadSchema(ctx, tx, s.Table)
	if err != nil {
		return nil, err
	}
	if s.Where == nil {
		return nil, fmt.Errorf("%w: DELETE requires WHERE", ErrUnsupported)
	}
	pks, err := e.conditionPKs(ctx, tx, schema, s.Where)
	if err != nil {
		return nil, err
	}
	affected := 0
	for _, pk := range pks {
		key := rowKey(schema.ID, pk)
		raw, err := tx.Get(ctx, key)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var row map[string]any
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, err
		}
		normalizeRow(schema, row)
		if err := e.deleteIndexEntries(tx, schema, row, pk); err != nil {
			return nil, err
		}
		tx.Delete(key)
		affected++
	}
	return &Result{RowsAffected: affected, Message: "rows deleted"}, nil
}

// conditionPKs returns the primary keys matching the supported WHERE form:
// primary key equality or an indexed-column equality.
func (e *Engine) conditionPKs(ctx context.Context, tx Txn, schema *Schema, where *sqlparser.Condition) ([][]byte, error) {
	pkIdx, _ := schema.PrimaryKeyIndex()
	pkCol := schema.Columns[pkIdx]
	if strings.EqualFold(where.Column, pkCol.Name) {
		pk, err := encodePK(pkCol, where.Value)
		if err != nil {
			return nil, err
		}
		return [][]byte{pk}, nil
	}
	column := strings.ToLower(where.Column)
	ci, ok := columnIndex(schema, column)
	if !ok {
		return nil, fmt.Errorf("%w: unknown column %q", ErrBadColumn, where.Column)
	}
	idx, ok := indexForColumn(schema, column)
	if !ok {
		return nil, fmt.Errorf("%w: WHERE must use the primary key or an indexed column", ErrUnsupported)
	}
	encoded, err := encodeValue(schema.Columns[ci], where.Value)
	if err != nil {
		return nil, err
	}
	start := indexValueKey(schema.ID, indexID(idx.Name), encoded)
	end := indexValueEnd(schema.ID, indexID(idx.Name), encoded)
	rows, err := tx.Scan(ctx, start, end, 0)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(rows))
	for _, row := range rows {
		pk := append([]byte(nil), row.Key[len(start):]...)
		out = append(out, pk)
	}
	return out, nil
}

func (e *Engine) deleteIndexEntries(tx Txn, schema *Schema, row map[string]any, pk []byte) error {
	for _, idx := range schema.Indexes {
		ci, _ := columnIndex(schema, idx.Column)
		col := schema.Columns[ci]
		lit, err := anyToLiteral(col, row[idx.Column])
		if err != nil {
			return err
		}
		encoded, err := encodeValue(col, lit)
		if err != nil {
			return err
		}
		tx.Delete(indexEntryKey(schema.ID, indexID(idx.Name), encoded, pk))
	}
	return nil
}

func (e *Engine) putIndexEntries(tx Txn, schema *Schema, row map[string]any, pk []byte) error {
	for _, idx := range schema.Indexes {
		ci, _ := columnIndex(schema, idx.Column)
		col := schema.Columns[ci]
		lit, err := anyToLiteral(col, row[idx.Column])
		if err != nil {
			return err
		}
		encoded, err := encodeValue(col, lit)
		if err != nil {
			return err
		}
		tx.Set(indexEntryKey(schema.ID, indexID(idx.Name), encoded, pk), nil)
	}
	return nil
}

func (e *Engine) readRows(ctx context.Context, tx Txn, schema *Schema, where *sqlparser.Condition, limit int) ([]map[string]any, error) {
	if limit < 0 {
		return nil, fmt.Errorf("%w: negative LIMIT", ErrBadColumn)
	}
	pkIdx, _ := schema.PrimaryKeyIndex()
	pkCol := schema.Columns[pkIdx]
	if where != nil {
		if strings.EqualFold(where.Column, pkCol.Name) {
			pk, err := encodePK(pkCol, where.Value)
			if err != nil {
				return nil, err
			}
			raw, err := tx.Get(ctx, rowKey(schema.ID, pk))
			if errors.Is(err, ErrNotFound) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			var row map[string]any
			if err := json.Unmarshal(raw, &row); err != nil {
				return nil, err
			}
			normalizeRow(schema, row)
			return []map[string]any{row}, nil
		}

		column := strings.ToLower(where.Column)
		ci, ok := columnIndex(schema, column)
		if !ok {
			return nil, fmt.Errorf("%w: unknown column %q", ErrBadColumn, where.Column)
		}
		idx, ok := indexForColumn(schema, column)
		if !ok {
			return nil, fmt.Errorf("%w: WHERE must use the primary key or an indexed column", ErrUnsupported)
		}
		col := schema.Columns[ci]
		encoded, err := encodeValue(col, where.Value)
		if err != nil {
			return nil, err
		}
		idxID := indexID(idx.Name)
		start := indexValueKey(schema.ID, idxID, encoded)
		end := indexValueEnd(schema.ID, idxID, encoded)
		indexRows, err := tx.Scan(ctx, start, end, limit)
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(indexRows))
		valueKeyLen := len(start)
		for _, ir := range indexRows {
			pk := ir.Key[valueKeyLen:]
			raw, err := tx.Get(ctx, rowKey(schema.ID, pk))
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			var row map[string]any
			if err := json.Unmarshal(raw, &row); err != nil {
				return nil, err
			}
			normalizeRow(schema, row)
			out = append(out, row)
		}
		return out, nil
	}

	rows, err := tx.Scan(ctx, tablePrefix(schema.ID), tablePrefixEnd(schema.ID), limit)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		var row map[string]any
		if err := json.Unmarshal(r.Value, &row); err != nil {
			return nil, err
		}
		normalizeRow(schema, row)
		out = append(out, row)
	}
	return out, nil
}

func (e *Engine) loadSchema(ctx context.Context, tx Txn, table string) (*Schema, error) {
	name := strings.ToLower(table)
	raw, err := tx.Get(ctx, catalogKey(name))
	if errors.Is(err, ErrNotFound) {
		return nil, ErrTableNotFound
	}
	if err != nil {
		return nil, err
	}
	var schema Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	return &schema, nil
}

func catalogKey(table string) []byte {
	key := make([]byte, 1+len(table))
	key[0] = catalogPrefix
	copy(key[1:], table)
	return key
}

func tablePrefix(id uint64) []byte {
	prefix := make([]byte, 9)
	prefix[0] = rowPrefix
	binary.BigEndian.PutUint64(prefix[1:], id)
	return prefix
}

func tablePrefixEnd(id uint64) []byte {
	prefix := tablePrefix(id)
	end := make([]byte, len(prefix)+16)
	copy(end, prefix)
	for i := len(prefix); i < len(end); i++ {
		end[i] = 0xff
	}
	return end
}

func rowKey(id uint64, pk []byte) []byte {
	key := tablePrefix(id)
	return append(key, pk...)
}

func encodePK(col Column, v sqlparser.Value) ([]byte, error) {
	if err := checkLiteralType(col, v); err != nil {
		return nil, err
	}
	return encodeValue(col, v)
}

func checkLiteralType(col Column, v sqlparser.Value) error {
	switch col.Type {
	case "INT":
		if v.Kind != sqlparser.ValueInt {
			return fmt.Errorf("%w: column %q expects INT", ErrBadColumn, col.Name)
		}
	case "TEXT":
		if v.Kind != sqlparser.ValueText {
			return fmt.Errorf("%w: column %q expects TEXT", ErrBadColumn, col.Name)
		}
	default:
		return fmt.Errorf("%w: type %q", ErrUnsupported, col.Type)
	}
	return nil
}

func indexID(name string) uint64 { return xxhash.Sum64String(name) }

func indexTablePrefix(tableID, idxID uint64) []byte {
	prefix := make([]byte, 17)
	prefix[0] = indexPrefix
	binary.BigEndian.PutUint64(prefix[1:], tableID)
	binary.BigEndian.PutUint64(prefix[9:], idxID)
	return prefix
}

func indexTableEnd(tableID, idxID uint64) []byte {
	prefix := indexTablePrefix(tableID, idxID)
	end := make([]byte, len(prefix)+16)
	copy(end, prefix)
	for i := len(prefix); i < len(end); i++ {
		end[i] = 0xff
	}
	return end
}

func indexValueKey(tableID, idxID uint64, value []byte) []byte {
	key := indexTablePrefix(tableID, idxID)
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(value)))
	key = append(key, n[:]...)
	return append(key, value...)
}

func indexEntryKey(tableID, idxID uint64, value, pk []byte) []byte {
	key := indexValueKey(tableID, idxID, value)
	return append(key, pk...)
}

func indexValueEnd(tableID, idxID uint64, value []byte) []byte {
	start := indexValueKey(tableID, idxID, value)
	end := make([]byte, len(start)+16)
	copy(end, start)
	for i := len(start); i < len(end); i++ {
		end[i] = 0xff
	}
	return end
}

func indexForColumn(s *Schema, column string) (Index, bool) {
	for _, idx := range s.Indexes {
		if idx.Column == column {
			return idx, true
		}
	}
	return Index{}, false
}

func encodeValue(col Column, v sqlparser.Value) ([]byte, error) {
	switch col.Type {
	case "INT":
		if v.Kind != sqlparser.ValueInt {
			return nil, fmt.Errorf("%w: column %q expects INT", ErrBadColumn, col.Name)
		}
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(v.Int)^(1<<63))
		return buf[:], nil
	case "TEXT":
		if v.Kind != sqlparser.ValueText {
			return nil, fmt.Errorf("%w: column %q expects TEXT", ErrBadColumn, col.Name)
		}
		return []byte(v.Str), nil
	default:
		return nil, fmt.Errorf("%w: type %q", ErrUnsupported, col.Type)
	}
}

func anyToLiteral(col Column, v any) (sqlparser.Value, error) {
	switch col.Type {
	case "INT":
		switch n := v.(type) {
		case int64:
			return sqlparser.Value{Kind: sqlparser.ValueInt, Int: n}, nil
		case float64:
			return sqlparser.Value{Kind: sqlparser.ValueInt, Int: int64(n)}, nil
		default:
			return sqlparser.Value{}, fmt.Errorf("%w: column %q is not INT", ErrBadColumn, col.Name)
		}
	case "TEXT":
		s, ok := v.(string)
		if !ok {
			return sqlparser.Value{}, fmt.Errorf("%w: column %q is not TEXT", ErrBadColumn, col.Name)
		}
		return sqlparser.Value{Kind: sqlparser.ValueText, Str: s}, nil
	default:
		return sqlparser.Value{}, fmt.Errorf("%w: type %q", ErrUnsupported, col.Type)
	}
}

func columnIndex(s *Schema, name string) (int, bool) {
	for i := range s.Columns {
		if s.Columns[i].Name == name {
			return i, true
		}
	}
	return 0, false
}

func selectColumns(s *Schema, names []string) ([]Column, error) {
	if len(names) == 0 || (len(names) == 1 && names[0] == "*") {
		return s.Columns, nil
	}
	out := make([]Column, 0, len(names))
	for _, name := range names {
		idx, ok := columnIndex(s, strings.ToLower(name))
		if !ok {
			return nil, fmt.Errorf("%w: unknown column %q", ErrBadColumn, name)
		}
		out = append(out, s.Columns[idx])
	}
	return out, nil
}

func normalizeRow(s *Schema, row map[string]any) {
	for _, col := range s.Columns {
		v, ok := row[col.Name]
		if !ok {
			continue
		}
		if col.Type == "INT" {
			switch n := v.(type) {
			case float64:
				row[col.Name] = int64(n)
			case int:
				row[col.Name] = int64(n)
			}
		}
	}
}
