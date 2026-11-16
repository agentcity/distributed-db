package sqlengine

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

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

// Engine executes the SQL subset over a transactional KV client.
type Engine struct {
	client Client

	// Plan cache: normalized SQL + schema version -> chosen plan.
	planMu            sync.Mutex
	plans             map[string]*Plan
	planHits          int64
	planMisses        int64
	planInvalidations int64
}

// New returns an engine backed by client.
func New(client Client) *Engine {
	return &Engine{client: client, plans: make(map[string]*Plan)}
}

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
	// Schema-changing statements make every cached plan stale. Schema version
	// keys already prevent reuse; clearing the map also reclaims memory and
	// makes the invalidation observable through PlanCacheStats.
	switch stmt.(type) {
	case *sqlparser.CreateTable, *sqlparser.DropTable, *sqlparser.CreateIndex, *sqlparser.DropIndex,
		sqlparser.CreateTable, sqlparser.DropTable, sqlparser.CreateIndex, sqlparser.DropIndex:
		e.invalidatePlans()
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
	schema := Schema{ID: xxhash.Sum64String(name), Name: name, Version: 1, Columns: cols}
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
		indexRows, err := scanRows(ctx, tx, indexTablePrefix(schema.ID, idxID), indexTableEnd(schema.ID, idxID), 0)
		if err != nil {
			return nil, err
		}
		for _, row := range indexRows {
			tx.Delete(row.Key)
		}
	}
	rows, err := scanRows(ctx, tx, tablePrefix(schema.ID), tablePrefixEnd(schema.ID), 0)
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
	rows, err := scanRows(ctx, tx, tablePrefix(schema.ID), tablePrefixEnd(schema.ID), 0)
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
	bumpSchemaVersion(schema)
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
	rows, err := scanRows(ctx, tx, indexTablePrefix(schema.ID, idxID), indexTableEnd(schema.ID, idxID), 0)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		tx.Delete(row.Key)
	}
	schema.Indexes = append(schema.Indexes[:pos], schema.Indexes[pos+1:]...)
	bumpSchemaVersion(schema)
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
	if err := e.saveStats(tx, schema, 1); err != nil {
		return nil, err
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
	plan, err := e.planFor(&s, schema)
	if err != nil {
		return nil, err
	}
	rows, err := e.readRowsWithPlan(ctx, tx, schema, plan, s.Where, s.Limit)
	if err != nil {
		return nil, err
	}
	if plan.Sort {
		col, ok := resolveTableColumn(schema, plan.OrderBy)
		if !ok {
			return nil, fmt.Errorf("%w: unknown column %q in ORDER BY", ErrBadColumn, plan.OrderBy)
		}
		if err := sortRows(schema, rows, col, plan.OrderDesc); err != nil {
			return nil, err
		}
	}
	if s.Limit > 0 && len(rows) > s.Limit {
		rows = rows[:s.Limit]
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

// joinOutputRow is one joined row plus its sort keys, used when the hash-build
// side is the left table and the left-driven order must be restored.
type joinOutputRow struct {
	line    []any
	leftPK  sqlparser.Value
	rightPK sqlparser.Value
}

// joinProjection is one output column of a join and the side it comes from.
type joinProjection struct {
	side   int
	column Column
	name   string
}

// joinCondition is a WHERE predicate resolved against one join side.
type joinCondition struct {
	side   int
	column Column
	op     sqlparser.ComparisonOp
	value  sqlparser.Value
	high   sqlparser.Value
}

// selectJoin executes one INNER JOIN with an equality predicate and an
// optional single-column WHERE condition, followed by LIMIT. The planner
// chooses the smaller estimated input as the hash-build side, but the output
// is kept in left-primary-key order (then right-primary-key order) so results
// stay deterministic regardless of the chosen join order.
func (e *Engine) selectJoin(ctx context.Context, tx Txn, leftSchema *Schema, s sqlparser.Select) (*Result, error) {
	if s.Aggregate != nil {
		return nil, fmt.Errorf("%w: aggregates with JOIN", ErrUnsupported)
	}
	if s.GroupBy != "" {
		return nil, fmt.Errorf("%w: GROUP BY with JOIN", ErrUnsupported)
	}
	if s.OrderBy != "" {
		return nil, fmt.Errorf("%w: ORDER BY with JOIN", ErrUnsupported)
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

	plan, err := e.planFor(&s, leftSchema, rightSchema)
	if err != nil {
		return nil, err
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

	// Push the WHERE filter into the side it references before hashing. This
	// is what makes the join-order estimate and the build side meaningful.
	if where != nil {
		switch where.side {
		case joinSideLeft:
			leftRows, err = filterJoinRows(leftRows, where)
		case joinSideRight:
			rightRows, err = filterJoinRows(rightRows, where)
		}
		if err != nil {
			return nil, err
		}
	}

	buildLeft := plan.Join != nil && plan.Join.BuildSide == "left"
	probeRows := leftRows
	buildRows := rightRows
	probeCol := leftJoinCol
	buildCol := rightJoinCol
	if buildLeft {
		probeRows, buildRows = rightRows, leftRows
		probeCol, buildCol = rightJoinCol, leftJoinCol
	}

	// Hash the build side by join value, preserving scan order per value.
	buildByKey := make(map[any][]map[string]any, len(buildRows))
	for _, row := range buildRows {
		key, err := joinKey(buildCol, row)
		if err != nil {
			return nil, err
		}
		buildByKey[key] = append(buildByKey[key], row)
	}

	// Hash-build on the left makes output follow the probe (right) table; sort
	// by left PK then right PK afterwards to restore left-driven order.
	needSort := buildLeft
	collected := make([]joinOutputRow, 0)
	names := make([]string, len(projections))
	for i, p := range projections {
		names[i] = p.name
	}

	for _, probeRow := range probeRows {
		key, err := joinKey(probeCol, probeRow)
		if err != nil {
			return nil, err
		}
		for _, buildRow := range buildByKey[key] {
			leftRow, rightRow := probeRow, buildRow
			if buildLeft {
				leftRow, rightRow = buildRow, probeRow
			}
			if where != nil {
				ok, err := where.matches(leftRow, rightRow)
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
			}
			if !needSort && s.Limit > 0 && len(collected) == s.Limit {
				return &Result{Columns: names, Rows: joinLines(collected)}, nil
			}
			line := make([]any, len(projections))
			for i, p := range projections {
				if p.side == joinSideLeft {
					line[i] = leftRow[p.column.Name]
				} else {
					line[i] = rightRow[p.column.Name]
				}
			}
			item := joinOutputRow{line: line}
			if needSort {
				item.leftPK, err = rowPrimaryKey(leftSchema, leftRow)
				if err != nil {
					return nil, err
				}
				item.rightPK, err = rowPrimaryKey(rightSchema, rightRow)
				if err != nil {
					return nil, err
				}
			}
			collected = append(collected, item)
			if len(collected) > maxJoinResultRows {
				return nil, fmt.Errorf("%w: JOIN produces more than %d rows", ErrUnsupported, maxJoinResultRows)
			}
		}
	}

	if needSort {
		leftPKCol, _ := leftSchema.PrimaryKeyIndex()
		rightPKCol, _ := rightSchema.PrimaryKeyIndex()
		sort.SliceStable(collected, func(i, j int) bool {
			c := compareLiteral(leftSchema.Columns[leftPKCol], collected[i].leftPK, collected[j].leftPK)
			if c != 0 {
				return c < 0
			}
			return compareLiteral(rightSchema.Columns[rightPKCol], collected[i].rightPK, collected[j].rightPK) < 0
		})
	}
	if s.Limit > 0 && len(collected) > s.Limit {
		collected = collected[:s.Limit]
	}
	return &Result{Columns: names, Rows: joinLines(collected)}, nil
}

func joinLines(items []joinOutputRow) [][]any {
	out := make([][]any, len(items))
	for i := range items {
		out[i] = items[i].line
	}
	return out
}

// filterJoinRows keeps rows matching a resolved single-column WHERE
// predicate. It runs before hashing so filtered input cardinality drives the
// plan.
func filterJoinRows(rows []map[string]any, c *joinCondition) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		ok, err := c.matchesRow(row)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, row)
		}
	}
	return out, nil
}

// rowPrimaryKey returns the primary-key value of one row.
func rowPrimaryKey(schema *Schema, row map[string]any) (sqlparser.Value, error) {
	idx, ok := schema.PrimaryKeyIndex()
	if !ok {
		return sqlparser.Value{}, fmt.Errorf("%w: table has no primary key", ErrBadColumn)
	}
	return anyToLiteral(schema.Columns[idx], row[schema.Columns[idx].Name])
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
	if err := checkConditionTypes(col, &cond); err != nil {
		return nil, err
	}
	return &joinCondition{
		side: side, column: col, op: cond.Op,
		value: cond.Value, high: cond.High,
	}, nil
}

// matches reports whether the joined row satisfies the WHERE condition.
func (c *joinCondition) matches(leftRow, rightRow map[string]any) (bool, error) {
	row := leftRow
	if c.side == joinSideRight {
		row = rightRow
	}
	return c.matchesRow(row)
}

// matchesRow evaluates the predicate against one row from the predicate side.
func (c *joinCondition) matchesRow(row map[string]any) (bool, error) {
	lit, err := anyToLiteral(c.column, row[c.column.Name])
	if err != nil {
		return false, err
	}
	if c.op == sqlparser.OpBetween {
		return compareLiteral(c.column, lit, c.value) >= 0 && compareLiteral(c.column, lit, c.high) <= 0, nil
	}
	cmp := compareLiteral(c.column, lit, c.value)
	switch c.op {
	case sqlparser.OpEQ:
		return cmp == 0, nil
	case sqlparser.OpNE:
		return cmp != 0, nil
	case sqlparser.OpLT:
		return cmp < 0, nil
	case sqlparser.OpLE:
		return cmp <= 0, nil
	case sqlparser.OpGT:
		return cmp > 0, nil
	case sqlparser.OpGE:
		return cmp >= 0, nil
	default:
		return false, nil
	}
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
	if s.OrderBy != "" {
		return nil, fmt.Errorf("%w: ORDER BY with a global aggregate", ErrUnsupported)
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
	if s.OrderBy != "" {
		if !strings.EqualFold(s.OrderBy, groupCol.Name) {
			return nil, fmt.Errorf("%w: ORDER BY %q with GROUP BY %q", ErrUnsupported, s.OrderBy, groupCol.Name)
		}
		if s.OrderDesc {
			for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if s.Limit > 0 && len(out) > s.Limit {
		out = out[:s.Limit]
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
	plan, err := e.planFor(&s, schema)
	if err != nil {
		return nil, err
	}
	pks, err := e.conditionPKs(ctx, tx, schema, plan, s.Where)
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
		ok, err := conditionMatches(schema, s.Where, row)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
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
	plan, err := e.planFor(&s, schema)
	if err != nil {
		return nil, err
	}
	pks, err := e.conditionPKs(ctx, tx, schema, plan, s.Where)
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
		ok, err := conditionMatches(schema, s.Where, row)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if err := e.deleteIndexEntries(tx, schema, row, pk); err != nil {
			return nil, err
		}
		tx.Delete(key)
		affected++
	}
	if affected > 0 {
		if err := e.saveStats(tx, schema, -int64(affected)); err != nil {
			return nil, err
		}
	}
	return &Result{RowsAffected: affected, Message: "rows deleted"}, nil
}

// conditionPKs returns the primary keys selected by the planner's access
// path. Callers re-check the full predicate against each row, so access paths
// that can only over-select (TEXT index ranges, !=) stay correct.
func (e *Engine) conditionPKs(ctx context.Context, tx Txn, schema *Schema, plan *Plan, where *sqlparser.Condition) ([][]byte, error) {
	switch plan.Access {
	case AccessPKPoint:
		col, ok := resolveTableColumn(schema, where.Column)
		if !ok {
			return nil, fmt.Errorf("%w: unknown column %q", ErrBadColumn, where.Column)
		}
		pk, err := encodePK(col, where.Value)
		if err != nil {
			return nil, err
		}
		return [][]byte{pk}, nil
	case AccessPKRange:
		start, end, err := rowRangeBounds(schema, where)
		if err != nil {
			return nil, err
		}
		rows, err := scanRows(ctx, tx, start, end, 0)
		if err != nil {
			return nil, err
		}
		prefixLen := len(tablePrefix(schema.ID))
		out := make([][]byte, 0, len(rows))
		for _, row := range rows {
			if len(row.Key) < prefixLen {
				continue
			}
			out = append(out, append([]byte(nil), row.Key[prefixLen:]...))
		}
		return out, nil
	case AccessTableScan:
		rows, err := scanRows(ctx, tx, tablePrefix(schema.ID), tablePrefixEnd(schema.ID), 0)
		if err != nil {
			return nil, err
		}
		prefixLen := len(tablePrefix(schema.ID))
		out := make([][]byte, 0, len(rows))
		for _, row := range rows {
			if len(row.Key) < prefixLen {
				continue
			}
			out = append(out, append([]byte(nil), row.Key[prefixLen:]...))
		}
		return out, nil
	case AccessIndexPoint, AccessIndexRange, AccessIndexFull:
		start, end, err := indexRangeBounds(schema, plan.Index, where)
		if err != nil {
			return nil, err
		}
		rows, err := scanRows(ctx, tx, start, end, 0)
		if err != nil {
			return nil, err
		}
		out := make([][]byte, 0, len(rows))
		for _, row := range rows {
			pk, ok := indexEntryPK(row.Key)
			if ok {
				out = append(out, pk)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: access path %s", ErrUnsupported, plan.Access)
	}
}

// inclusiveUserKeyEnd returns an exclusive scan bound that includes every
// version of userKey. MVCC stores userKey||commitTS, so appending a single zero
// byte is not enough: a version suffix starting with zero would sort after the
// bound. Eight 0xff bytes exceed any timestamp; extensions of userKey that also
// fall under the bound are removed by the predicate filter.
func inclusiveUserKeyEnd(key []byte) []byte {
	const maxVersionLen = 8
	out := make([]byte, len(key)+maxVersionLen)
	copy(out, key)
	for i := len(key); i < len(out); i++ {
		out[i] = 0xff
	}
	return out
}

// rowRangeBounds converts a primary-key predicate into a [start,end) scan over
// row keys. Inclusive upper bounds extend past all MVCC versions of the key;
// the executor re-checks the predicate, so TEXT keys that merely extend the
// bound are filtered out.
func rowRangeBounds(schema *Schema, cond *sqlparser.Condition) (start, end []byte, err error) {
	col, ok := resolveTableColumn(schema, cond.Column)
	if !ok {
		return nil, nil, fmt.Errorf("%w: unknown column %q", ErrBadColumn, cond.Column)
	}
	pk, err := encodePK(col, cond.Value)
	if err != nil {
		return nil, nil, err
	}
	start = tablePrefix(schema.ID)
	end = tablePrefixEnd(schema.ID)
	switch cond.Op {
	case sqlparser.OpEQ:
		start = rowKey(schema.ID, pk)
		end = append(append([]byte(nil), start...), 0)
	case sqlparser.OpGT:
		start = append(rowKey(schema.ID, pk), 0)
	case sqlparser.OpGE:
		start = rowKey(schema.ID, pk)
	case sqlparser.OpLT:
		end = rowKey(schema.ID, pk)
	case sqlparser.OpLE:
		end = inclusiveUserKeyEnd(rowKey(schema.ID, pk))
	case sqlparser.OpBetween:
		high, err := encodePK(col, cond.High)
		if err != nil {
			return nil, nil, err
		}
		if compareLiteral(col, cond.Value, cond.High) > 0 {
			// Empty range: make start == end.
			empty := tablePrefixEnd(schema.ID)
			return empty, empty, nil
		}
		start = rowKey(schema.ID, pk)
		end = inclusiveUserKeyEnd(rowKey(schema.ID, high))
	default:
		return nil, nil, fmt.Errorf("%w: predicate %s", ErrUnsupported, cond.Op)
	}
	return start, end, nil
}

// indexRangeBounds converts an indexed-column predicate into a [start,end)
// scan over index entries. INT values are fixed-width and ordered inside the
// index, so the bounds can be clipped; TEXT ranges fall back to a full index
// scan because the index encodes the value length first.
func indexRangeBounds(schema *Schema, indexName string, cond *sqlparser.Condition) (start, end []byte, err error) {
	var idx Index
	found := false
	for _, candidate := range schema.Indexes {
		if candidate.Name == indexName {
			idx, found = candidate, true
			break
		}
	}
	if !found {
		return nil, nil, ErrIndexNotFound
	}
	ci, ok := columnIndex(schema, idx.Column)
	if !ok {
		return nil, nil, fmt.Errorf("%w: unknown column %q", ErrBadColumn, idx.Column)
	}
	col := schema.Columns[ci]
	idxID := indexID(idx.Name)
	start = indexTableEnd(schema.ID, idxID)
	end = indexTableEnd(schema.ID, idxID)
	if cond.Op == sqlparser.OpNE {
		return indexTablePrefix(schema.ID, idxID), indexTableEnd(schema.ID, idxID), nil
	}
	if col.Type != "INT" && cond.Op != sqlparser.OpEQ {
		return indexTablePrefix(schema.ID, idxID), indexTableEnd(schema.ID, idxID), nil
	}
	encoded, err := encodeValue(col, cond.Value)
	if err != nil {
		return nil, nil, err
	}
	switch cond.Op {
	case sqlparser.OpEQ:
		return indexValueKey(schema.ID, idxID, encoded), indexValueEnd(schema.ID, idxID, encoded), nil
	case sqlparser.OpGT:
		start = indexNextValueKey(schema, idxID, col, cond.Value.Int)
		end = indexTableEnd(schema.ID, idxID)
	case sqlparser.OpGE:
		start = indexValueKey(schema.ID, idxID, encoded)
		end = indexTableEnd(schema.ID, idxID)
	case sqlparser.OpLT:
		start = indexTablePrefix(schema.ID, idxID)
		end = indexValueKey(schema.ID, idxID, encoded)
	case sqlparser.OpLE:
		start = indexTablePrefix(schema.ID, idxID)
		end = indexNextValueKey(schema, idxID, col, cond.Value.Int)
	case sqlparser.OpBetween:
		if compareLiteral(col, cond.Value, cond.High) > 0 {
			tblEnd := indexTableEnd(schema.ID, idxID)
			return tblEnd, tblEnd, nil
		}
		start = indexValueKey(schema.ID, idxID, encoded)
		end = indexNextValueKey(schema, idxID, col, cond.High.Int)
	default:
		return nil, nil, fmt.Errorf("%w: predicate %s", ErrUnsupported, cond.Op)
	}
	return start, end, nil
}

// indexNextValueKey returns the first index key of value+1, or the end of the
// index table when value is the maximum INT.
func indexNextValueKey(schema *Schema, idxID uint64, col Column, value int64) []byte {
	if value == math.MaxInt64 {
		return indexTableEnd(schema.ID, idxID)
	}
	encoded, _ := encodeValue(col, sqlparser.Value{Kind: sqlparser.ValueInt, Int: value + 1})
	return indexValueKey(schema.ID, idxID, encoded)
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

// readRows is the uncached scan used by aggregates and join inputs.
func (e *Engine) readRows(ctx context.Context, tx Txn, schema *Schema, where *sqlparser.Condition, limit int) ([]map[string]any, error) {
	a, err := chooseAccess(schema, where)
	if err != nil {
		return nil, err
	}
	plan := &Plan{}
	fillAccessPlan(plan, schema, a)
	return e.readRowsWithPlan(ctx, tx, schema, plan, where, limit)
}

// readRowsWithPlan executes the chosen access path and returns rows in scan
// order (primary-key order for row scans, index order for index scans).
func (e *Engine) readRowsWithPlan(ctx context.Context, tx Txn, schema *Schema, plan *Plan, where *sqlparser.Condition, limit int) ([]map[string]any, error) {
	if limit < 0 {
		return nil, fmt.Errorf("%w: negative LIMIT", ErrBadColumn)
	}
	applyLimit := 0
	if plan.LimitPushed {
		applyLimit = limit
	}
	switch plan.Access {
	case AccessTableScan:
		rows, err := scanRows(ctx, tx, tablePrefix(schema.ID), tablePrefixEnd(schema.ID), applyLimit)
		if err != nil {
			return nil, err
		}
		return decodeAndFilterRows(schema, rows, where, applyLimit)
	case AccessPKPoint:
		col, ok := resolveTableColumn(schema, where.Column)
		if !ok {
			return nil, fmt.Errorf("%w: unknown column %q", ErrBadColumn, where.Column)
		}
		pk, err := encodePK(col, where.Value)
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
		matches, matchErr := conditionMatches(schema, where, row)
		if matchErr != nil {
			return nil, matchErr
		}
		if !matches {
			return nil, nil
		}
		return []map[string]any{row}, nil
	case AccessPKRange:
		rangeStart, rangeEnd, err := rowRangeBounds(schema, where)
		if err != nil {
			return nil, err
		}
		rows, err := scanRows(ctx, tx, rangeStart, rangeEnd, applyLimit)
		if err != nil {
			return nil, err
		}
		return decodeAndFilterRows(schema, rows, where, applyLimit)
	case AccessIndexPoint, AccessIndexRange, AccessIndexFull:
		rangeStart, rangeEnd, err := indexRangeBounds(schema, plan.Index, where)
		if err != nil {
			return nil, err
		}
		indexRows, err := scanRows(ctx, tx, rangeStart, rangeEnd, applyLimit)
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(indexRows))
		for _, entry := range indexRows {
			pk, ok := indexEntryPK(entry.Key)
			if !ok {
				continue
			}
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
			match, err := conditionMatches(schema, where, row)
			if err != nil {
				return nil, err
			}
			if !match {
				continue
			}
			out = append(out, row)
			if applyLimit > 0 && len(out) >= applyLimit {
				return out, nil
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: access path %s", ErrUnsupported, plan.Access)
	}
}

func decodeAndFilterRows(schema *Schema, rows []KV, where *sqlparser.Condition, limit int) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(rows))
	for _, raw := range rows {
		var row map[string]any
		if err := json.Unmarshal(raw.Value, &row); err != nil {
			return nil, err
		}
		normalizeRow(schema, row)
		if where != nil {
			ok, err := conditionMatches(schema, where, row)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		out = append(out, row)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// conditionMatches evaluates a condition against a decoded row.
func conditionMatches(schema *Schema, cond *sqlparser.Condition, row map[string]any) (bool, error) {
	col, ok := resolveTableColumn(schema, cond.Column)
	if !ok {
		return false, fmt.Errorf("%w: unknown column %q", ErrBadColumn, cond.Column)
	}
	current, err := anyToLiteral(col, row[col.Name])
	if err != nil {
		return false, err
	}
	// BETWEEN is inclusive on both ends.
	if cond.Op == sqlparser.OpBetween {
		return compareLiteral(col, current, cond.Value) >= 0 && compareLiteral(col, current, cond.High) <= 0, nil
	}
	cmp := compareLiteral(col, current, cond.Value)
	switch cond.Op {
	case sqlparser.OpEQ:
		return cmp == 0, nil
	case sqlparser.OpNE:
		return cmp != 0, nil
	case sqlparser.OpLT:
		return cmp < 0, nil
	case sqlparser.OpLE:
		return cmp <= 0, nil
	case sqlparser.OpGT:
		return cmp > 0, nil
	case sqlparser.OpGE:
		return cmp >= 0, nil
	default:
		return false, fmt.Errorf("%w: predicate %s", ErrUnsupported, cond.Op)
	}
}

// indexEntryPK extracts the primary key suffix from an index entry key.
func indexEntryPK(key []byte) ([]byte, bool) {
	const headerLen = 17 + 4 // index table prefix + value length
	if len(key) < headerLen {
		return nil, false
	}
	n := int(binary.BigEndian.Uint32(key[17:21]))
	start := headerLen + n
	if start > len(key) {
		return nil, false
	}
	return append([]byte(nil), key[start:]...), true
}

// scanRows runs a scan through the optional cross-range extension, falling
// back to a single-range Scan for simple Txn implementations.
func scanRows(ctx context.Context, tx Txn, start, end []byte, limit int) ([]KV, error) {
	if rs, ok := tx.(RangeScanner); ok {
		return rs.ScanRanges(ctx, start, end, limit)
	}
	return tx.Scan(ctx, start, end, limit)
}

func bumpSchemaVersion(schema *Schema) {
	schema.Version++
	if schema.Version == 0 {
		schema.Version = 1
	}
}

// saveStats adjusts the approximate row count and rewrites the catalog entry
// in the same transaction as the data change.
func (e *Engine) saveStats(tx Txn, schema *Schema, delta int64) error {
	n := int64(schema.RowCount) + delta
	if n < 0 {
		n = 0
	}
	schema.RowCount = uint64(n)
	data, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	tx.Set(catalogKey(schema.Name), data)
	return nil
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
	return keyPrefixEnd(tablePrefix(id))
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
	return keyPrefixEnd(indexTablePrefix(tableID, idxID))
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
	return keyPrefixEnd(indexValueKey(tableID, idxID, value))
}

// keyPrefixEnd returns the smallest key greater than every key that has prefix
// as a prefix. It works for arbitrary suffix lengths (TEXT primary keys and
// long index entries), unlike padding with a fixed number of 0xff bytes. A
// prefix made only of 0xff bytes has no finite successor and falls back to the
// padded form.
func keyPrefixEnd(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] == 0xff {
			continue
		}
		end[i]++
		return end[:i+1]
	}
	padded := make([]byte, len(prefix)+16)
	copy(padded, prefix)
	for i := len(prefix); i < len(padded); i++ {
		padded[i] = 0xff
	}
	return padded
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
