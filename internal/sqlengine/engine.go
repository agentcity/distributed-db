package sqlengine

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
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
)

const (
	catalogPrefix byte = 0x01
	rowPrefix     byte = 0x02
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
	return &Result{RowsAffected: 1, Message: "row inserted"}, nil
}

func (e *Engine) selectRows(ctx context.Context, tx Txn, s sqlparser.Select) (*Result, error) {
	schema, err := e.loadSchema(ctx, tx, s.Table)
	if err != nil {
		return nil, err
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

func (e *Engine) update(ctx context.Context, tx Txn, s sqlparser.Update) (*Result, error) {
	schema, err := e.loadSchema(ctx, tx, s.Table)
	if err != nil {
		return nil, err
	}
	pkIdx, _ := schema.PrimaryKeyIndex()
	pkCol := schema.Columns[pkIdx]
	if s.Where == nil {
		return nil, fmt.Errorf("%w: UPDATE requires WHERE on the primary key", ErrUnsupported)
	}
	if !strings.EqualFold(s.Where.Column, pkCol.Name) {
		return nil, fmt.Errorf("%w: WHERE must use primary key %q", ErrUnsupported, pkCol.Name)
	}
	pk, err := encodePK(pkCol, s.Where.Value)
	if err != nil {
		return nil, err
	}
	key := rowKey(schema.ID, pk)
	raw, err := tx.Get(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrRowNotFound
	}
	if err != nil {
		return nil, err
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, err
	}
	if len(s.Assignments) == 0 {
		return nil, fmt.Errorf("%w: SET list is empty", ErrBadColumn)
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
	data, err := json.Marshal(row)
	if err != nil {
		return nil, err
	}
	tx.Set(key, data)
	return &Result{RowsAffected: 1, Message: "row updated"}, nil
}

func (e *Engine) delete(ctx context.Context, tx Txn, s sqlparser.Delete) (*Result, error) {
	schema, err := e.loadSchema(ctx, tx, s.Table)
	if err != nil {
		return nil, err
	}
	pkIdx, _ := schema.PrimaryKeyIndex()
	pkCol := schema.Columns[pkIdx]
	if s.Where == nil {
		return nil, fmt.Errorf("%w: DELETE requires WHERE on the primary key", ErrUnsupported)
	}
	if !strings.EqualFold(s.Where.Column, pkCol.Name) {
		return nil, fmt.Errorf("%w: WHERE must use primary key %q", ErrUnsupported, pkCol.Name)
	}
	pk, err := encodePK(pkCol, s.Where.Value)
	if err != nil {
		return nil, err
	}
	key := rowKey(schema.ID, pk)
	if _, err := tx.Get(ctx, key); errors.Is(err, ErrNotFound) {
		return &Result{RowsAffected: 0}, nil
	} else if err != nil {
		return nil, err
	}
	tx.Delete(key)
	return &Result{RowsAffected: 1, Message: "row deleted"}, nil
}

func (e *Engine) readRows(ctx context.Context, tx Txn, schema *Schema, where *sqlparser.Condition, limit int) ([]map[string]any, error) {
	if limit < 0 {
		return nil, fmt.Errorf("%w: negative LIMIT", ErrBadColumn)
	}
	pkIdx, _ := schema.PrimaryKeyIndex()
	pkCol := schema.Columns[pkIdx]
	if where != nil {
		if !strings.EqualFold(where.Column, pkCol.Name) {
			return nil, fmt.Errorf("%w: WHERE must use primary key %q", ErrUnsupported, pkCol.Name)
		}
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
	switch col.Type {
	case "INT":
		if v.Kind != sqlparser.ValueInt {
			return nil, fmt.Errorf("%w: primary key %q expects INT", ErrBadColumn, col.Name)
		}
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(v.Int)^(1<<63))
		return buf[:], nil
	case "TEXT":
		if v.Kind != sqlparser.ValueText {
			return nil, fmt.Errorf("%w: primary key %q expects TEXT", ErrBadColumn, col.Name)
		}
		return []byte(v.Str), nil
	default:
		return nil, fmt.Errorf("%w: type %q", ErrUnsupported, col.Type)
	}
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
