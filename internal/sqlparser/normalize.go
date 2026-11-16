package sqlparser

import (
	"strconv"
	"strings"
)

// Normalize renders a statement into a canonical, literal-free form suitable
// as a plan-cache key. Identifiers are lower-cased, whitespace is canonical
// and every literal is replaced by '?', so two statements that differ only in
// literal values or keyword case share a key.
func Normalize(stmt Statement) string {
	var b strings.Builder
	switch s := stmt.(type) {
	case *CreateTable:
		b.WriteString("CREATE TABLE ")
		b.WriteString(lowerName(s.Name))
		b.WriteString(" (")
		for i, c := range s.Columns {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(lowerName(c.Name))
			b.WriteByte(' ')
			b.WriteString(strings.ToUpper(c.Type))
			if c.PrimaryKey {
				b.WriteString(" PRIMARY KEY")
			}
		}
		b.WriteByte(')')
	case *DropTable:
		b.WriteString("DROP TABLE ")
		b.WriteString(lowerName(s.Name))
	case *CreateIndex:
		b.WriteString("CREATE INDEX ")
		b.WriteString(lowerName(s.Name))
		b.WriteString(" ON ")
		b.WriteString(lowerName(s.Table))
		b.WriteString(" (")
		b.WriteString(lowerName(s.Column))
		b.WriteByte(')')
	case *DropIndex:
		b.WriteString("DROP INDEX ")
		b.WriteString(lowerName(s.Name))
		b.WriteString(" ON ")
		b.WriteString(lowerName(s.Table))
	case *Insert:
		b.WriteString("INSERT INTO ")
		b.WriteString(lowerName(s.Table))
		if len(s.Columns) > 0 {
			b.WriteString(" (")
			for i, c := range s.Columns {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(lowerName(c))
			}
			b.WriteByte(')')
		}
		b.WriteString(" VALUES (")
		for i := range s.Values {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteByte('?')
		}
		b.WriteByte(')')
	case *Select:
		b.WriteString("SELECT ")
		switch {
		case s.Aggregate != nil:
			b.WriteString(strings.ToUpper(s.Aggregate.Func))
			b.WriteByte('(')
			if s.Aggregate.Star {
				b.WriteByte('*')
			} else {
				b.WriteString(lowerName(s.Aggregate.Column))
			}
			b.WriteByte(')')
		case len(s.Columns) == 0:
			b.WriteByte('*')
		default:
			for i, c := range s.Columns {
				if i > 0 {
					b.WriteString(", ")
				}
				if c == "*" {
					b.WriteByte('*')
				} else {
					b.WriteString(normalizeColumnRef(c))
				}
			}
		}
		b.WriteString(" FROM ")
		b.WriteString(lowerName(s.Table))
		if s.Join != nil {
			b.WriteString(" JOIN ")
			b.WriteString(lowerName(s.Join.RightTable))
			b.WriteString(" ON ")
			b.WriteString(normalizeColumnRef(s.Join.LeftColumn))
			b.WriteString(" = ")
			b.WriteString(normalizeColumnRef(s.Join.RightColumn))
		}
		if s.Where != nil {
			b.WriteString(" WHERE ")
			b.WriteString(normalizeCondition(s.Where))
		}
		if s.GroupBy != "" {
			b.WriteString(" GROUP BY ")
			b.WriteString(lowerName(s.GroupBy))
		}
		if s.OrderBy != "" {
			b.WriteString(" ORDER BY ")
			b.WriteString(normalizeColumnRef(s.OrderBy))
			if s.OrderDesc {
				b.WriteString(" DESC")
			}
		}
		if s.Limit > 0 {
			b.WriteString(" LIMIT ")
			b.WriteString(strconv.Itoa(s.Limit))
		}
	case *Update:
		b.WriteString("UPDATE ")
		b.WriteString(lowerName(s.Table))
		b.WriteString(" SET ")
		for i, a := range s.Assignments {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(lowerName(a.Column))
			b.WriteString(" = ?")
		}
		if s.Where != nil {
			b.WriteString(" WHERE ")
			b.WriteString(normalizeCondition(s.Where))
		}
	case *Delete:
		b.WriteString("DELETE FROM ")
		b.WriteString(lowerName(s.Table))
		if s.Where != nil {
			b.WriteString(" WHERE ")
			b.WriteString(normalizeCondition(s.Where))
		}
	}
	return b.String()
}

func normalizeCondition(c *Condition) string {
	var b strings.Builder
	b.WriteString(normalizeColumnRef(c.Column))
	if c.Op == OpBetween {
		b.WriteString(" BETWEEN ? AND ?")
		return b.String()
	}
	b.WriteByte(' ')
	b.WriteString(c.Op.String())
	b.WriteString(" ?")
	return b.String()
}

// normalizeColumnRef lower-cases the qualifier and column but keeps the dot.
func normalizeColumnRef(ref string) string {
	q, col, ok := splitQualified(ref)
	if !ok {
		return lowerName(ref)
	}
	return lowerName(q) + "." + lowerName(col)
}

func splitQualified(ref string) (qualifier, column string, ok bool) {
	pos := strings.IndexByte(ref, '.')
	if pos < 0 {
		return "", ref, false
	}
	return ref[:pos], ref[pos+1:], true
}

func lowerName(s string) string { return strings.ToLower(s) }
