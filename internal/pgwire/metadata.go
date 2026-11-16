package pgwire

import (
	"strings"

	"distributed-db/internal/sqlengine"
	"distributed-db/internal/sqlparser"
)

// columnInfo is one column of a table schema known to this connection.
type columnInfo struct {
	name string
	typ  string // INT or TEXT
}

type tableSchema struct {
	name    string
	columns []columnInfo
}

func (s tableSchema) lookup(ref string) (columnInfo, bool) {
	if i := strings.IndexByte(ref, '.'); i >= 0 {
		ref = ref[i+1:]
	}
	ref = strings.ToLower(ref)
	for _, col := range s.columns {
		if col.name == ref {
			return col, true
		}
	}
	return columnInfo{}, false
}

func validSQLIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			continue
		}
		if i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}

// observeSchema records the effect of CREATE/DROP TABLE so later Parse
// messages can infer parameter and result types, including for empty tables.
func (sess *connSession) observeSchema(ast sqlparser.Statement) {
	switch s := ast.(type) {
	case *sqlparser.CreateTable:
		cols := make([]columnInfo, len(s.Columns))
		for i, col := range s.Columns {
			cols[i] = columnInfo{name: strings.ToLower(col.Name), typ: strings.ToUpper(col.Type)}
		}
		sess.schemas[strings.ToLower(s.Name)] = tableSchema{name: strings.ToLower(s.Name), columns: cols}
	case *sqlparser.DropTable:
		delete(sess.schemas, strings.ToLower(s.Name))
	}
}

// tableSchema returns the schema of name, probing the engine with a one-row
// SELECT if this connection has not observed the CREATE TABLE. Empty tables
// created elsewhere cannot be introspected through the engine's public API.
func (sess *connSession) tableSchema(name string) (tableSchema, bool) {
	key := strings.ToLower(name)
	if schema, ok := sess.schemas[key]; ok {
		return schema, true
	}
	if !validSQLIdent(key) {
		return tableSchema{}, false
	}
	res, err := sess.srv.engine.Execute(sess.srv.ctx, "SELECT * FROM "+key+" LIMIT 1")
	if err != nil || res == nil || len(res.Columns) == 0 || len(res.Rows) == 0 {
		return tableSchema{}, false
	}
	cols := make([]columnInfo, len(res.Columns))
	for i, colName := range res.Columns {
		typ := ""
		for _, row := range res.Rows {
			if i >= len(row) || row[i] == nil {
				continue
			}
			switch row[i].(type) {
			case int64:
				typ = "INT"
			case string:
				typ = "TEXT"
			}
			if typ != "" {
				break
			}
		}
		if typ == "" {
			return tableSchema{}, false
		}
		cols[i] = columnInfo{name: strings.ToLower(colName), typ: typ}
	}
	schema := tableSchema{name: key, columns: cols}
	sess.schemas[key] = schema
	return schema, true
}

// inferParamHints maps every placeholder it recognizes to an INT/TEXT kind
// using the AST parsed from the sentinel-substituted query plus the table
// schema.
func (sess *connSession) inferParamHints(ast sqlparser.Statement, n int) []paramKind {
	hints := make([]paramKind, n)
	conflict := make([]bool, n)
	set := func(index int, kind paramKind) {
		if index < 0 || index >= n || kind == kindUnknown || conflict[index] {
			return
		}
		switch {
		case hints[index] == kindUnknown:
			hints[index] = kind
		case hints[index] != kind:
			hints[index] = kindUnknown
			conflict[index] = true
		}
	}
	markValue := func(v sqlparser.Value, typ string) {
		index, ok := sentinelParamIndex(v, n)
		if !ok {
			return
		}
		set(index, kindForSchemaType(typ))
	}
	markCondition := func(c *sqlparser.Condition, typ string) {
		if c == nil {
			return
		}
		markValue(c.Value, typ)
		markValue(c.High, typ)
	}

	switch s := ast.(type) {
	case *sqlparser.Select:
		if index, ok := sentinelLimitIndex(s.Limit, n); ok {
			set(index, kindInt)
		}
		if s.Where != nil {
			if typ, ok := sess.resolveSelectWhereType(s, s.Where.Column); ok {
				markCondition(s.Where, typ)
			}
		}
	case *sqlparser.Insert:
		schema, ok := sess.tableSchema(s.Table)
		if !ok {
			break
		}
		for i, v := range s.Values {
			index, ok := sentinelParamIndex(v, n)
			if !ok {
				continue
			}
			var colName string
			switch {
			case len(s.Columns) == 0 && i < len(schema.columns):
				colName = schema.columns[i].name
			case len(s.Columns) > 0 && i < len(s.Columns):
				colName = s.Columns[i]
			}
			if col, ok := schema.lookup(colName); ok {
				set(index, kindForSchemaType(col.typ))
			}
		}
	case *sqlparser.Update:
		schema, ok := sess.tableSchema(s.Table)
		if !ok {
			break
		}
		for _, as := range s.Assignments {
			if index, ok := sentinelParamIndex(as.Value, n); ok {
				if col, ok := schema.lookup(as.Column); ok {
					set(index, kindForSchemaType(col.typ))
				}
			}
		}
		if s.Where != nil {
			if col, ok := schema.lookup(s.Where.Column); ok {
				markCondition(s.Where, col.typ)
			}
		}
	case *sqlparser.Delete:
		schema, ok := sess.tableSchema(s.Table)
		if !ok || s.Where == nil {
			break
		}
		if col, ok := schema.lookup(s.Where.Column); ok {
			markCondition(s.Where, col.typ)
		}
	}
	return hints
}

func sentinelParamIndex(v sqlparser.Value, n int) (int, bool) {
	if v.Kind != sqlparser.ValueInt {
		return 0, false
	}
	off := v.Int - paramSentinelBase
	if off < 0 || off >= int64(n) {
		return 0, false
	}
	return int(off), true
}

func sentinelLimitIndex(limit, n int) (int, bool) {
	off := int64(limit) - paramSentinelBase
	if off < 0 || off >= int64(n) {
		return 0, false
	}
	return int(off), true
}

// resolveSelectWhereType resolves the column on the left side of a SELECT
// WHERE condition. JOIN support is best effort: unqualified columns are
// accepted only when both sides agree or only one side defines the column.
func (sess *connSession) resolveSelectWhereType(sel *sqlparser.Select, ref string) (string, bool) {
	if sel.Join == nil {
		schema, ok := sess.tableSchema(sel.Table)
		if !ok {
			return "", false
		}
		col, ok := schema.lookup(ref)
		return col.typ, ok
	}

	left, leftOK := sess.tableSchema(sel.Table)
	right, rightOK := sess.tableSchema(sel.Join.RightTable)
	qualifier, colName := splitColumnRef(ref)
	if qualifier != "" {
		leftNames := []string{sel.Table, sel.Join.LeftAlias}
		for _, name := range leftNames {
			if name != "" && strings.EqualFold(qualifier, name) {
				if !leftOK {
					return "", false
				}
				col, ok := left.lookup(colName)
				return col.typ, ok
			}
		}
		rightNames := []string{sel.Join.RightTable, sel.Join.RightAlias}
		for _, name := range rightNames {
			if name != "" && strings.EqualFold(qualifier, name) {
				if !rightOK {
					return "", false
				}
				col, ok := right.lookup(colName)
				return col.typ, ok
			}
		}
		return "", false
	}

	var leftCol, rightCol columnInfo
	var haveLeft, haveRight bool
	if leftOK {
		leftCol, haveLeft = left.lookup(colName)
	}
	if rightOK {
		rightCol, haveRight = right.lookup(colName)
	}
	switch {
	case haveLeft && haveRight:
		if leftCol.typ == rightCol.typ {
			return leftCol.typ, true
		}
		return "", false
	case haveLeft:
		return leftCol.typ, true
	case haveRight:
		return rightCol.typ, true
	default:
		return "", false
	}
}

func splitColumnRef(ref string) (qualifier, column string) {
	if i := strings.IndexByte(ref, '.'); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return "", ref
}

// inferSelectMeta derives a RowDescription from the parsed SELECT and the
// table schema. It covers single-table projections and aggregates; JOINs and
// other constructs fall back to executing the query for metadata.
func (sess *connSession) inferSelectMeta(ast sqlparser.Statement) ([]columnDesc, bool) {
	sel, ok := ast.(*sqlparser.Select)
	if !ok || sel.Join != nil {
		return nil, false
	}
	schema, ok := sess.tableSchema(sel.Table)
	if !ok {
		return nil, false
	}
	if sel.Aggregate != nil {
		return inferAggregateMeta(sel, schema)
	}
	if len(sel.Columns) == 0 || (len(sel.Columns) == 1 && sel.Columns[0] == "*") {
		out := make([]columnDesc, len(schema.columns))
		for i, col := range schema.columns {
			out[i] = descForSchemaColumn(col)
		}
		return out, true
	}
	out := make([]columnDesc, 0, len(sel.Columns))
	for _, ref := range sel.Columns {
		col, ok := schema.lookup(ref)
		if !ok {
			return nil, false
		}
		out = append(out, descForSchemaColumn(col))
	}
	return out, true
}

func inferAggregateMeta(sel *sqlparser.Select, schema tableSchema) ([]columnDesc, bool) {
	fn := strings.ToUpper(sel.Aggregate.Func)
	name := strings.ToLower(fn)
	agg := columnDesc{name: name}
	if sel.Aggregate.Star {
		if fn != "COUNT" {
			return nil, false
		}
		agg.oid, agg.size = oidInt8, 8
	} else {
		col, ok := schema.lookup(sel.Aggregate.Column)
		if !ok {
			return nil, false
		}
		switch fn {
		case "COUNT":
			agg.oid, agg.size = oidInt8, 8
		case "SUM", "AVG":
			if kindForSchemaType(col.typ) != kindInt {
				return nil, false
			}
			agg.oid, agg.size = oidInt8, 8
		case "MIN", "MAX":
			agg.oid, agg.size = oidForSchemaType(col.typ)
		default:
			return nil, false
		}
	}
	if sel.GroupBy != "" {
		group, ok := schema.lookup(sel.GroupBy)
		if !ok {
			return nil, false
		}
		return []columnDesc{descForSchemaColumn(group), agg}, true
	}
	return []columnDesc{agg}, true
}

func descForSchemaColumn(col columnInfo) columnDesc {
	oid, size := oidForSchemaType(col.typ)
	return columnDesc{name: col.name, oid: oid, size: size}
}

// resultMeta builds the column descriptions for an executed result. Schema
// inference wins when available so an empty result still reports INT/TEXT
// correctly; otherwise the values are inspected.
func (sess *connSession) resultMeta(stmt *preparedStatement, res *sqlengine.Result) []columnDesc {
	if res == nil {
		return nil
	}
	inferred, haveInferred := sess.inferSelectMeta(stmt.ast)
	out := make([]columnDesc, len(res.Columns))
	for i, name := range res.Columns {
		if haveInferred && i < len(inferred) {
			desc := inferred[i]
			desc.name = name
			out[i] = desc
			continue
		}
		oid, size := columnType(res, i)
		out[i] = columnDesc{name: name, oid: oid, size: size}
	}
	return out
}
