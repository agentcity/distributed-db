package sqlengine

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"distributed-db/internal/sqlparser"
)

// AccessKind is the access path chosen for a table.
type AccessKind string

const (
	// AccessTableScan is a full primary-key-ordered table scan.
	AccessTableScan AccessKind = "table_scan"
	// AccessPKPoint is a single-row lookup by primary key.
	AccessPKPoint AccessKind = "pk_point"
	// AccessPKRange is a bounded scan of the primary-key index.
	AccessPKRange AccessKind = "pk_range"
	// AccessIndexPoint is an equality lookup through a secondary index.
	AccessIndexPoint AccessKind = "index_point"
	// AccessIndexRange is a bounded secondary-index scan.
	AccessIndexRange AccessKind = "index_range"
	// AccessIndexFull scans an entire secondary index (used for predicates the
	// current index encoding cannot bound, such as TEXT ranges).
	AccessIndexFull AccessKind = "index_full"
	// AccessHashJoin is a two-table hash join.
	AccessHashJoin AccessKind = "hash_join"
	// AccessInsert is a primary-key insert.
	AccessInsert AccessKind = "insert"
)

// JoinPlan describes the chosen two-table join order.
type JoinPlan struct {
	LeftTable    string `json:"left_table"`
	RightTable   string `json:"right_table"`
	BuildSide    string `json:"build_side"` // "left" or "right"
	BuildTable   string `json:"build_table"`
	ProbeTable   string `json:"probe_table"`
	LeftRows     int64  `json:"left_rows"`
	RightRows    int64  `json:"right_rows"`
	ResultRows   int64  `json:"result_rows"`
	FilteredSide string `json:"filtered_side,omitempty"`
}

// Plan is the cached, inspectable execution plan for one statement. It is
// produced by a small cost-based planner: the estimated row count of the
// chosen access path decides between a table scan and an index, and join build
// side selection uses the estimated filtered input sizes.
type Plan struct {
	SQL           string     `json:"sql"`
	Table         string     `json:"table"`
	Access        AccessKind `json:"access"`
	Index         string     `json:"index,omitempty"`
	IndexColumn   string     `json:"index_column,omitempty"`
	EstimatedRows int64      `json:"estimated_rows"`
	EstimatedCost float64    `json:"estimated_cost"`
	Ordered       bool       `json:"ordered"`
	Sort          bool       `json:"sort"`
	OrderBy       string     `json:"order_by,omitempty"`
	OrderDesc     bool       `json:"order_desc,omitempty"`
	LimitPushed   bool       `json:"limit_pushed"`
	Aggregate     bool       `json:"aggregate,omitempty"`
	CacheHit      bool       `json:"cache_hit"`
	Join          *JoinPlan  `json:"join,omitempty"`
}

// PlanCacheStats reports plan-cache activity. Hits and misses count planFor
// lookups, and Invalidations counts DDL statements that cleared the cache.
type PlanCacheStats struct {
	Hits          int64 `json:"hits"`
	Misses        int64 `json:"misses"`
	Invalidations int64 `json:"invalidations"`
	Size          int   `json:"size"`
}

// Explain parses and plans sql without executing it.
func (e *Engine) Explain(ctx context.Context, sql string) (*Plan, error) {
	stmt, err := sqlparser.Parse(sql)
	if err != nil {
		return nil, err
	}
	tx, err := e.client.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	schemas, err := e.schemasFor(ctx, tx, stmt)
	if err != nil {
		return nil, err
	}
	return e.planFor(stmt, schemas...)
}

// PlanCacheStats returns a snapshot of plan-cache counters.
func (e *Engine) PlanCacheStats() PlanCacheStats {
	e.planMu.Lock()
	defer e.planMu.Unlock()
	return PlanCacheStats{
		Hits:          e.planHits,
		Misses:        e.planMisses,
		Invalidations: e.planInvalidations,
		Size:          len(e.plans),
	}
}

// schemasFor loads the schemas a statement plans against.
func (e *Engine) schemasFor(ctx context.Context, tx Txn, stmt sqlparser.Statement) ([]*Schema, error) {
	switch s := stmt.(type) {
	case *sqlparser.Select:
		left, err := e.loadSchema(ctx, tx, s.Table)
		if err != nil {
			return nil, err
		}
		if s.Join == nil {
			return []*Schema{left}, nil
		}
		right, err := e.loadSchema(ctx, tx, s.Join.RightTable)
		if err != nil {
			return nil, err
		}
		return []*Schema{left, right}, nil
	case *sqlparser.Update:
		schema, err := e.loadSchema(ctx, tx, s.Table)
		if err != nil {
			return nil, err
		}
		return []*Schema{schema}, nil
	case *sqlparser.Delete:
		schema, err := e.loadSchema(ctx, tx, s.Table)
		if err != nil {
			return nil, err
		}
		return []*Schema{schema}, nil
	case *sqlparser.Insert:
		schema, err := e.loadSchema(ctx, tx, s.Table)
		if err != nil {
			return nil, err
		}
		return []*Schema{schema}, nil
	default:
		return nil, nil
	}
}

// planFor returns a cached plan or builds and stores a new one. The cache key
// is the literal-free normalized SQL plus the version of every schema the
// statement touches, so DDL that bumps a schema version cannot reuse a stale
// plan.
func (e *Engine) planFor(stmt sqlparser.Statement, schemas ...*Schema) (*Plan, error) {
	key := planKey(stmt, schemas)
	e.planMu.Lock()
	if e.plans == nil {
		e.plans = make(map[string]*Plan)
	}
	if p, ok := e.plans[key]; ok {
		e.planHits++
		cp := *p
		cp.CacheHit = true
		e.planMu.Unlock()
		return &cp, nil
	}
	e.planMisses++
	e.planMu.Unlock()

	plan, err := e.buildPlan(stmt, schemas...)
	if err != nil {
		return nil, err
	}
	e.planMu.Lock()
	if existing, ok := e.plans[key]; ok {
		// Another goroutine won the race; return its copy.
		cp := *existing
		cp.CacheHit = true
		e.planMu.Unlock()
		return &cp, nil
	}
	e.plans[key] = plan
	cp := *plan
	cp.CacheHit = false
	e.planMu.Unlock()
	return &cp, nil
}

// invalidatePlans drops every cached plan. It is called after successful DDL.
func (e *Engine) invalidatePlans() {
	e.planMu.Lock()
	if len(e.plans) > 0 {
		e.planInvalidations++
	}
	e.plans = make(map[string]*Plan)
	e.planMu.Unlock()
}

func planKey(stmt sqlparser.Statement, schemas []*Schema) string {
	var b strings.Builder
	b.WriteString(sqlparser.Normalize(stmt))
	for _, s := range schemas {
		b.WriteByte('|')
		b.WriteString(s.Name)
		b.WriteByte(':')
		b.WriteString(strconv.FormatUint(s.Version, 10))
	}
	return b.String()
}

// buildPlan chooses an access path and estimates its cost from table stats.
func (e *Engine) buildPlan(stmt sqlparser.Statement, schemas ...*Schema) (*Plan, error) {
	switch s := stmt.(type) {
	case *sqlparser.Select:
		if s.Join != nil {
			if len(schemas) != 2 {
				return nil, fmt.Errorf("%w: JOIN needs two schemas", ErrUnsupported)
			}
			return buildJoinPlan(s, schemas[0], schemas[1])
		}
		if len(schemas) != 1 {
			return nil, fmt.Errorf("%w: SELECT needs one schema", ErrUnsupported)
		}
		return buildSelectPlan(s, schemas[0])
	case *sqlparser.Update:
		if len(schemas) != 1 {
			return nil, fmt.Errorf("%w: UPDATE needs one schema", ErrUnsupported)
		}
		return buildDMLPlan("update", s.Table, s.Where, schemas[0])
	case *sqlparser.Delete:
		if len(schemas) != 1 {
			return nil, fmt.Errorf("%w: DELETE needs one schema", ErrUnsupported)
		}
		return buildDMLPlan("delete", s.Table, s.Where, schemas[0])
	case *sqlparser.Insert:
		if len(schemas) != 1 {
			return nil, fmt.Errorf("%w: INSERT needs one schema", ErrUnsupported)
		}
		return &Plan{
			SQL:           sqlparser.Normalize(stmt),
			Table:         schemas[0].Name,
			Access:        AccessInsert,
			EstimatedRows: 1,
			EstimatedCost: 1,
		}, nil
	default:
		return &Plan{SQL: sqlparser.Normalize(stmt)}, nil
	}
}

func buildSelectPlan(s *sqlparser.Select, schema *Schema) (*Plan, error) {
	plan := &Plan{
		SQL:       sqlparser.Normalize(s),
		Table:     schema.Name,
		Aggregate: s.Aggregate != nil,
	}
	access, err := chooseAccess(schema, s.Where)
	if err != nil {
		return nil, err
	}
	fillAccessPlan(plan, schema, access)

	if s.Aggregate != nil {
		// Aggregates consume every matching row; ORDER BY / LIMIT do not
		// reduce the scan and are handled by the aggregate executor.
		plan.Ordered = false
		plan.LimitPushed = false
		return plan, nil
	}
	if s.GroupBy != "" {
		plan.Ordered = false
		plan.LimitPushed = false
		return plan, nil
	}
	if err := applyOrdering(plan, schema, s); err != nil {
		return nil, err
	}
	return plan, nil
}

func buildDMLPlan(kind, table string, where *sqlparser.Condition, schema *Schema) (*Plan, error) {
	access, err := chooseAccess(schema, where)
	if err != nil {
		return nil, err
	}
	plan := &Plan{SQL: strings.ToUpper(kind) + " " + strings.ToLower(table), Table: schema.Name}
	fillAccessPlan(plan, schema, access)
	return plan, nil
}

// access describes the chosen path before it is copied into a Plan.
type access struct {
	kind    AccessKind
	index   string
	column  string
	cond    *sqlparser.Condition
	ordered bool // rows arrive in ascending key/order-column order
	exact   bool // every scanned key matches the predicate
}

// chooseAccess picks the cheapest available access path for a WHERE clause.
func chooseAccess(schema *Schema, where *sqlparser.Condition) (access, error) {
	if where == nil {
		return access{kind: AccessTableScan, ordered: true, exact: true}, nil
	}
	column, ok := resolveTableColumn(schema, where.Column)
	if !ok {
		return access{}, fmt.Errorf("%w: unknown column %q", ErrBadColumn, where.Column)
	}
	if err := checkConditionTypes(column, where); err != nil {
		return access{}, err
	}
	colName := column.Name
	if column.PrimaryKey {
		switch where.Op {
		case sqlparser.OpEQ:
			return access{kind: AccessPKPoint, column: colName, cond: where, ordered: true, exact: true}, nil
		case sqlparser.OpNE:
			return access{kind: AccessTableScan, column: colName, cond: where, ordered: true, exact: false}, nil
		default:
			return access{kind: AccessPKRange, column: colName, cond: where, ordered: true, exact: true}, nil
		}
	}
	idx, indexed := indexForColumn(schema, colName)
	if where.Op == sqlparser.OpNE {
		if indexed {
			return access{kind: AccessIndexFull, index: idx.Name, column: colName, cond: where, exact: false}, nil
		}
		return access{kind: AccessTableScan, column: colName, cond: where, ordered: true, exact: false}, nil
	}
	if !indexed {
		return access{}, fmt.Errorf("%w: WHERE must use the primary key or an indexed column", ErrUnsupported)
	}
	if where.Op == sqlparser.OpEQ {
		return access{kind: AccessIndexPoint, index: idx.Name, column: colName, cond: where, ordered: true, exact: true}, nil
	}
	if column.Type == "INT" {
		return access{kind: AccessIndexRange, index: idx.Name, column: colName, cond: where, ordered: true, exact: true}, nil
	}
	// The index key format sorts by (value length, value bytes) for TEXT, so
	// a text range cannot be clipped to a contiguous key interval. Scan the
	// index and filter instead.
	return access{kind: AccessIndexFull, index: idx.Name, column: colName, cond: where, exact: false}, nil
}

func fillAccessPlan(plan *Plan, schema *Schema, a access) {
	plan.Access = a.kind
	plan.Index = a.index
	if a.index != "" {
		plan.IndexColumn = a.column
	}
	plan.Ordered = a.ordered
	plan.EstimatedRows = estimateAccessRows(schema, a)
	plan.EstimatedCost = float64(plan.EstimatedRows)
	exact := a.exact
	// Primary-key range bounds deliberately over-select in two edges
	// (GT includes the boundary key's versions; inclusive upper bounds may
	// include TEXT extensions), so the executor must see every scanned row
	// before applying LIMIT.
	if a.kind == AccessPKRange {
		exact = false
	}
	plan.LimitPushed = exact && plan.EstimatedRows > 0
}

// resolveTableColumn resolves an optionally table-qualified column reference.
func resolveTableColumn(schema *Schema, ref string) (Column, bool) {
	qualifier, name, qualified := splitColumnRef(ref)
	if qualified && !strings.EqualFold(qualifier, schema.Name) {
		return Column{}, false
	}
	idx, ok := columnIndex(schema, strings.ToLower(name))
	if !ok {
		return Column{}, false
	}
	return schema.Columns[idx], true
}

func checkConditionTypes(col Column, cond *sqlparser.Condition) error {
	if err := checkLiteralType(col, cond.Value); err != nil {
		return err
	}
	if cond.Op == sqlparser.OpBetween {
		if err := checkLiteralType(col, cond.High); err != nil {
			return err
		}
	}
	return nil
}

func estimateAccessRows(schema *Schema, a access) int64 {
	rows := int64(schema.RowCount)
	if rows < 1 {
		rows = 1
	}
	switch a.kind {
	case AccessPKPoint:
		return 1
	case AccessPKRange:
		return clampRows(estimateRangeSelectivity(rows, a.cond), 1, rows)
	case AccessIndexPoint:
		return clampRows(rows/10, 1, rows)
	case AccessIndexRange:
		return clampRows(estimateRangeSelectivity(rows, a.cond), 1, rows)
	case AccessIndexFull:
		return rows
	case AccessTableScan:
		if a.cond != nil {
			return clampRows(rows/2, 1, rows)
		}
		return rows
	default:
		return rows
	}
}

func estimateRangeSelectivity(rows int64, cond *sqlparser.Condition) int64 {
	if cond == nil {
		return rows
	}
	switch cond.Op {
	case sqlparser.OpBetween:
		return rows / 2
	case sqlparser.OpLT, sqlparser.OpLE, sqlparser.OpGT, sqlparser.OpGE:
		return rows / 3
	default:
		return rows / 10
	}
}

func clampRows(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// applyOrdering decides whether the access path already satisfies ORDER BY and
// whether LIMIT can be pushed into the scan.
func applyOrdering(plan *Plan, schema *Schema, s *sqlparser.Select) error {
	if s.OrderBy == "" {
		plan.LimitPushed = plan.LimitPushed && s.Limit > 0
		plan.Sort = false
		return nil
	}
	col, ok := resolveTableColumn(schema, s.OrderBy)
	if !ok {
		return fmt.Errorf("%w: unknown column %q in ORDER BY", ErrBadColumn, s.OrderBy)
	}
	plan.OrderBy = col.Name
	plan.OrderDesc = s.OrderDesc

	orderedByAccess := false
	switch plan.Access {
	case AccessTableScan, AccessPKPoint, AccessPKRange:
		// These paths always return primary-key ascending order.
		orderedByAccess = col.PrimaryKey && plan.Ordered
	case AccessIndexPoint, AccessIndexRange, AccessIndexFull:
		// Index scans are ordered by the indexed column's encoded value, not
		// by the primary key.
		orderedByAccess = plan.IndexColumn != "" && plan.OrderBy == plan.IndexColumn && plan.Ordered
	}
	// A secondary-index scan yields ascending values; a DESC request (or an
	// unordered scan) needs an explicit sort. ORDER BY on the indexed column
	// with an equality predicate is trivially ordered because all values are
	// equal.
	if orderedByAccess && !s.OrderDesc {
		plan.Sort = false
	} else if !orderedByAccess {
		plan.Sort = true
	} else {
		plan.Sort = true
	}
	plan.LimitPushed = !plan.Sort && s.Limit > 0 && plan.LimitPushed
	return nil
}

func buildJoinPlan(s *sqlparser.Select, left, right *Schema) (*Plan, error) {
	join := s.Join
	leftAlias := joinQualifier(join.LeftAlias, left.Name)
	rightAlias := joinQualifier(join.RightAlias, right.Name)

	leftJoinCol, err := resolveJoinSideColumn(join.LeftColumn, left, leftAlias, "left")
	if err != nil {
		return nil, err
	}
	rightJoinCol, err := resolveJoinSideColumn(join.RightColumn, right, rightAlias, "right")
	if err != nil {
		return nil, err
	}
	if leftJoinCol.Type != rightJoinCol.Type {
		return nil, fmt.Errorf("%w: JOIN columns %q and %q have different types", ErrBadColumn, join.LeftColumn, join.RightColumn)
	}

	leftRows := estimateSchemaRows(left)
	rightRows := estimateSchemaRows(right)
	filteredSide := ""
	if s.Where != nil {
		side, col, err := resolveJoinColumn(s.Where.Column, left, right, leftAlias, rightAlias)
		if err != nil {
			return nil, err
		}
		if err := checkConditionTypes(col, s.Where); err != nil {
			return nil, err
		}
		switch side {
		case joinSideLeft:
			filteredSide = "left"
			leftRows = estimateFilteredRows(left, s.Where)
		case joinSideRight:
			filteredSide = "right"
			rightRows = estimateFilteredRows(right, s.Where)
		}
	}

	buildSide := "right"
	buildTable, probeTable := right.Name, left.Name
	if leftRows < rightRows {
		buildSide = "left"
		buildTable, probeTable = left.Name, right.Name
	}
	resultRows := int64(1)
	if leftRows > 0 && rightRows > 0 {
		// Ten distinct join values is a deliberately simple assumption; there
		// are no per-column distinct stats yet.
		resultRows = leftRows * rightRows / 10
		if resultRows < 1 {
			resultRows = 1
		}
		if resultRows > maxJoinResultRows {
			resultRows = maxJoinResultRows
		}
	}

	return &Plan{
		SQL:           sqlparser.Normalize(s),
		Table:         left.Name,
		Access:        AccessHashJoin,
		EstimatedRows: resultRows,
		EstimatedCost: float64(leftRows + rightRows),
		Ordered:       false,
		LimitPushed:   false,
		Join: &JoinPlan{
			LeftTable:    left.Name,
			RightTable:   right.Name,
			BuildSide:    buildSide,
			BuildTable:   buildTable,
			ProbeTable:   probeTable,
			LeftRows:     leftRows,
			RightRows:    rightRows,
			ResultRows:   resultRows,
			FilteredSide: filteredSide,
		},
	}, nil
}

func estimateSchemaRows(schema *Schema) int64 {
	if schema.RowCount < 1 {
		return 1
	}
	return int64(schema.RowCount)
}

func estimateFilteredRows(schema *Schema, where *sqlparser.Condition) int64 {
	rows := estimateSchemaRows(schema)
	if where == nil {
		return rows
	}
	col, ok := resolveTableColumn(schema, where.Column)
	if !ok {
		return rows
	}
	switch {
	case col.PrimaryKey && where.Op == sqlparser.OpEQ:
		return 1
	case where.Op.IsRange():
		return clampRows(rows/3, 1, rows)
	case where.Op == sqlparser.OpEQ:
		return clampRows(rows/10, 1, rows)
	default:
		return rows
	}
}

// sortRows orders rows by col (and by primary key for stable output).
func sortRows(schema *Schema, rows []map[string]any, col Column, desc bool) error {
	pkIdx, _ := schema.PrimaryKeyIndex()
	pkCol := schema.Columns[pkIdx]
	keys := make([]sqlparser.Value, len(rows))
	for i, row := range rows {
		v, err := anyToLiteral(col, row[col.Name])
		if err != nil {
			return err
		}
		keys[i] = v
	}
	order := make([]int, len(rows))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		c := compareLiteral(col, keys[order[i]], keys[order[j]])
		if c == 0 {
			a, errA := anyToLiteral(pkCol, rows[order[i]][pkCol.Name])
			b, errB := anyToLiteral(pkCol, rows[order[j]][pkCol.Name])
			if errA == nil && errB == nil {
				c = compareLiteral(pkCol, a, b)
			}
		}
		if desc {
			return c > 0
		}
		return c < 0
	})
	sorted := make([]map[string]any, len(rows))
	for i, idx := range order {
		sorted[i] = rows[idx]
	}
	copy(rows, sorted)
	return nil
}

func compareLiteral(col Column, a, b sqlparser.Value) int {
	if col.Type == "INT" {
		switch {
		case a.Int < b.Int:
			return -1
		case a.Int > b.Int:
			return 1
		default:
			return 0
		}
	}
	return strings.Compare(a.Str, b.Str)
}
