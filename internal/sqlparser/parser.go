package sqlparser

import (
	"fmt"
	"strconv"
	"strings"
)

// parser is a recursive-descent parser over the token slice produced by lex.
type parser struct {
	toks []token
	pos  int
}

// parse parses exactly one statement. The optional trailing semicolon is
// consumed, but anything else after the statement is rejected.
func parse(sql string) (Statement, error) {
	toks, lexErr := lex(sql)
	if lexErr != nil {
		return nil, lexErr
	}
	p := &parser{toks: toks}

	if t := p.peek(); t.kind == tokenEOF || t.kind == tokenSemicolon {
		return nil, p.errorf(t.pos, "empty statement")
	}

	stmt, err := p.parseStatement()
	if err != nil {
		return nil, err
	}
	if p.peek().kind == tokenSemicolon {
		p.next()
	}
	if t := p.peek(); t.kind != tokenEOF {
		return nil, p.errorf(t.pos, "unexpected %s after statement", describeToken(t))
	}
	return stmt, nil
}

func (p *parser) peek() token { return p.toks[p.pos] }

func (p *parser) next() token {
	t := p.toks[p.pos]
	if t.kind != tokenEOF {
		p.pos++
	}
	return t
}

func (p *parser) errorf(pos int, format string, args ...any) *ParseError {
	return &ParseError{Pos: pos, Msg: fmt.Sprintf(format, args...)}
}

// isKeyword reports whether t is the given keyword, case-insensitively.
func isKeyword(t token, kw string) bool {
	return t.kind == tokenIdent && strings.EqualFold(t.text, kw)
}

// matchKeyword consumes and reports whether the next token is kw.
func (p *parser) matchKeyword(kw string) bool {
	if isKeyword(p.peek(), kw) {
		p.next()
		return true
	}
	return false
}

func (p *parser) expectKeyword(kw, context string) *ParseError {
	t := p.peek()
	if !isKeyword(t, kw) {
		return p.errorf(t.pos, "expected keyword %s%s, found %s", kw, context, describeToken(t))
	}
	p.next()
	return nil
}

func (p *parser) expectKind(kind tokenKind, what string) (token, *ParseError) {
	t := p.peek()
	if t.kind != kind {
		return token{}, p.errorf(t.pos, "expected %s, found %s", what, describeToken(t))
	}
	p.next()
	return t, nil
}

func (p *parser) expectIdent(what string) (token, *ParseError) {
	t := p.peek()
	if t.kind != tokenIdent {
		return token{}, p.errorf(t.pos, "expected %s, found %s", what, describeToken(t))
	}
	p.next()
	return t, nil
}

func (p *parser) parseStatement() (Statement, error) {
	t := p.peek()
	if t.kind != tokenIdent {
		return nil, p.errorf(t.pos, "expected a SQL statement, found %s", describeToken(t))
	}
	switch strings.ToUpper(t.text) {
	case "CREATE":
		p.next()
		if p.matchKeyword("INDEX") {
			return p.parseCreateIndex()
		}
		return p.parseCreateTable()
	case "DROP":
		p.next()
		if p.matchKeyword("INDEX") {
			return p.parseDropIndex()
		}
		return p.parseDropTable()
	case "INSERT":
		p.next()
		return p.parseInsert()
	case "SELECT":
		p.next()
		return p.parseSelect()
	case "UPDATE":
		p.next()
		return p.parseUpdate()
	case "DELETE":
		p.next()
		return p.parseDelete()
	default:
		return nil, p.errorf(t.pos, "unknown statement %q", t.text)
	}
}

// parseCreateTable parses CREATE TABLE name (col TYPE [PRIMARY KEY], ...).
func (p *parser) parseCreateTable() (Statement, error) {
	if err := p.expectKeyword("TABLE", " after CREATE"); err != nil {
		return nil, err
	}
	name, err := p.expectIdent("table name")
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKind(tokenLParen, "'(' after table name"); err != nil {
		return nil, err
	}

	var cols []ColumnDef
	seen := make(map[string]bool)
	pkSeen := false
	for {
		colTok, err := p.expectIdent("column name")
		if err != nil {
			return nil, err
		}
		typeTok, err := p.expectIdent("column type")
		if err != nil {
			return nil, err
		}
		typ := strings.ToUpper(typeTok.text)
		if typ != "INT" && typ != "TEXT" {
			return nil, p.errorf(typeTok.pos, "unsupported column type %q (expected INT or TEXT)", typeTok.text)
		}

		col := ColumnDef{Name: colTok.text, Type: typ}
		for isKeyword(p.peek(), "PRIMARY") {
			primTok := p.next()
			if err := p.expectKeyword("KEY", " after PRIMARY"); err != nil {
				return nil, err
			}
			if pkSeen {
				return nil, p.errorf(primTok.pos, "duplicate primary key")
			}
			pkSeen = true
			col.PrimaryKey = true
		}
		if seen[colTok.text] {
			return nil, p.errorf(colTok.pos, "duplicate column %q", colTok.text)
		}
		seen[colTok.text] = true
		cols = append(cols, col)

		switch p.peek().kind {
		case tokenComma:
			p.next()
		case tokenRParen:
			rparen := p.next()
			if !pkSeen {
				return nil, p.errorf(rparen.pos, "table must have exactly one primary key")
			}
			return &CreateTable{Name: name.text, Columns: cols}, nil
		default:
			return nil, p.errorf(p.peek().pos, "expected ',' or ')' in column definitions, found %s", describeToken(p.peek()))
		}
	}
}

// parseDropTable parses DROP TABLE name.
func (p *parser) parseCreateIndex() (Statement, error) {
	name, err := p.expectIdent("index name")
	if err != nil {
		return nil, err
	}
	if err := p.expectKeyword("ON", " in CREATE INDEX"); err != nil {
		return nil, err
	}
	table, err := p.expectIdent("table name")
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKind(tokenLParen, "'('"); err != nil {
		return nil, err
	}
	col, err := p.expectIdent("column name")
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKind(tokenRParen, "')'"); err != nil {
		return nil, err
	}
	return &CreateIndex{Name: name.text, Table: table.text, Column: col.text}, nil
}

func (p *parser) parseDropIndex() (Statement, error) {
	name, err := p.expectIdent("index name")
	if err != nil {
		return nil, err
	}
	if err := p.expectKeyword("ON", " in DROP INDEX"); err != nil {
		return nil, err
	}
	table, err := p.expectIdent("table name")
	if err != nil {
		return nil, err
	}
	return &DropIndex{Name: name.text, Table: table.text}, nil
}

func (p *parser) parseDropTable() (Statement, error) {
	if err := p.expectKeyword("TABLE", " after DROP"); err != nil {
		return nil, err
	}
	name, err := p.expectIdent("table name")
	if err != nil {
		return nil, err
	}
	return &DropTable{Name: name.text}, nil
}

// parseInsert parses INSERT INTO name [(col, ...)] VALUES (literal, ...).
func (p *parser) parseInsert() (Statement, error) {
	if err := p.expectKeyword("INTO", " after INSERT"); err != nil {
		return nil, err
	}
	table, err := p.expectIdent("table name")
	if err != nil {
		return nil, err
	}

	var columns []string
	if p.peek().kind == tokenLParen {
		p.next()
		for {
			col, err := p.expectIdent("column name")
			if err != nil {
				return nil, err
			}
			columns = append(columns, col.text)
			if p.peek().kind == tokenComma {
				p.next()
				continue
			}
			if p.peek().kind == tokenRParen {
				p.next()
				break
			}
			return nil, p.errorf(p.peek().pos, "expected ',' or ')' in column list, found %s", describeToken(p.peek()))
		}
	}

	if err := p.expectKeyword("VALUES", " after INSERT target"); err != nil {
		return nil, err
	}
	valuesOpen, err := p.expectKind(tokenLParen, "'(' after VALUES")
	if err != nil {
		return nil, err
	}

	var values []Value
	for {
		v, litErr := p.parseLiteral()
		if litErr != nil {
			return nil, litErr
		}
		values = append(values, v)
		if p.peek().kind == tokenComma {
			p.next()
			continue
		}
		if p.peek().kind == tokenRParen {
			p.next()
			break
		}
		return nil, p.errorf(p.peek().pos, "expected ',' or ')' in VALUES list, found %s", describeToken(p.peek()))
	}

	if len(columns) > 0 && len(columns) != len(values) {
		return nil, p.errorf(valuesOpen.pos, "INSERT lists %d columns but supplies %d values", len(columns), len(values))
	}
	return &Insert{Table: table.text, Columns: columns, Values: values}, nil
}

// tryParseAggregate recognizes COUNT(*)/COUNT(col)/SUM/AVG/MIN/MAX(col).
func (p *parser) tryParseAggregate() (*Aggregate, bool, error) {
	t := p.peek()
	if t.kind != tokenIdent {
		return nil, false, nil
	}
	fn := strings.ToUpper(t.text)
	switch fn {
	case "COUNT", "SUM", "AVG", "MIN", "MAX":
	default:
		return nil, false, nil
	}
	// Only treat it as an aggregate when followed by '('; otherwise it is a
	// normal column named count/sum/...
	if p.pos+1 >= len(p.toks) || p.toks[p.pos+1].kind != tokenLParen {
		return nil, false, nil
	}
	p.next() // function
	if _, err := p.expectKind(tokenLParen, "'('"); err != nil {
		return nil, false, err
	}
	agg := &Aggregate{Func: fn}
	if p.peek().kind == tokenStar {
		if fn != "COUNT" {
			return nil, false, p.errorf(p.peek().pos, "%s(*) is not supported", fn)
		}
		p.next()
		agg.Star = true
	} else {
		col, err := p.expectIdent("column name")
		if err != nil {
			return nil, false, err
		}
		agg.Column = col.text
	}
	if _, err := p.expectKind(tokenRParen, "')'"); err != nil {
		return nil, false, err
	}
	return agg, true, nil
}

// parseSelect parses SELECT *|col[, ...] FROM name [WHERE ...]
// [GROUP BY col] [LIMIT n]. GROUP BY is only valid with an aggregate.
func (p *parser) parseSelect() (Statement, error) {
	sel := &Select{}
	if p.peek().kind == tokenStar {
		p.next()
		sel.Columns = []string{"*"}
	} else if agg, ok, err := p.tryParseAggregate(); err != nil {
		return nil, err
	} else if ok {
		sel.Aggregate = agg
	} else {
		for {
			col, err := p.expectIdent("column name")
			if err != nil {
				return nil, err
			}
			sel.Columns = append(sel.Columns, col.text)
			if p.peek().kind == tokenComma {
				p.next()
				continue
			}
			break
		}
	}

	if err := p.expectKeyword("FROM", " after SELECT list"); err != nil {
		return nil, err
	}
	table, err := p.expectIdent("table name")
	if err != nil {
		return nil, err
	}
	sel.Table = table.text

	if p.matchKeyword("WHERE") {
		cond, err := p.parseCondition()
		if err != nil {
			return nil, err
		}
		sel.Where = cond
	}
	if groupTok := p.peek(); isKeyword(groupTok, "GROUP") {
		if sel.Aggregate == nil {
			return nil, p.errorf(groupTok.pos, "GROUP BY requires an aggregate")
		}
		p.next()
		if err := p.expectKeyword("BY", " after GROUP"); err != nil {
			return nil, err
		}
		col, err := p.expectIdent("column name in GROUP BY")
		if err != nil {
			return nil, err
		}
		sel.GroupBy = col.text
		if p.peek().kind == tokenComma {
			return nil, p.errorf(p.peek().pos, "GROUP BY supports exactly one column")
		}
	}
	if p.matchKeyword("LIMIT") {
		limit, err := p.parseLimit()
		if err != nil {
			return nil, err
		}
		sel.Limit = limit
	}
	return sel, nil
}

// parseUpdate parses UPDATE name SET col = literal[, ...] [WHERE ...].
func (p *parser) parseUpdate() (Statement, error) {
	table, err := p.expectIdent("table name")
	if err != nil {
		return nil, err
	}
	if err := p.expectKeyword("SET", " after table name"); err != nil {
		return nil, err
	}

	upd := &Update{Table: table.text}
	for {
		col, err := p.expectIdent("column name in SET")
		if err != nil {
			return nil, err
		}
		if _, err := p.expectKind(tokenEq, "'=' after column name in SET"); err != nil {
			return nil, err
		}
		v, litErr := p.parseLiteral()
		if litErr != nil {
			return nil, litErr
		}
		upd.Assignments = append(upd.Assignments, Assignment{Column: col.text, Value: v})
		if p.peek().kind != tokenComma {
			break
		}
		p.next()
	}

	if p.matchKeyword("WHERE") {
		cond, err := p.parseCondition()
		if err != nil {
			return nil, err
		}
		upd.Where = cond
	}
	return upd, nil
}

// parseDelete parses DELETE FROM name [WHERE ...].
func (p *parser) parseDelete() (Statement, error) {
	if err := p.expectKeyword("FROM", " after DELETE"); err != nil {
		return nil, err
	}
	table, err := p.expectIdent("table name")
	if err != nil {
		return nil, err
	}
	del := &Delete{Table: table.text}
	if p.matchKeyword("WHERE") {
		cond, err := p.parseCondition()
		if err != nil {
			return nil, err
		}
		del.Where = cond
	}
	return del, nil
}

// parseCondition parses the only supported WHERE form: col = literal.
func (p *parser) parseCondition() (*Condition, error) {
	col, err := p.expectIdent("column name in WHERE")
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKind(tokenEq, "'=' in WHERE condition"); err != nil {
		return nil, err
	}
	v, litErr := p.parseLiteral()
	if litErr != nil {
		return nil, litErr
	}
	return &Condition{Column: col.text, Value: v}, nil
}

// parseLiteral parses a signed decimal integer or a single-quoted string.
func (p *parser) parseLiteral() (Value, error) {
	t := p.peek()
	switch t.kind {
	case tokenInt:
		p.next()
		n, err := strconv.ParseInt(t.text, 10, 64)
		if err != nil {
			return Value{}, p.errorf(t.pos, "integer literal out of range")
		}
		return Value{Kind: ValueInt, Int: n}, nil
	case tokenString:
		p.next()
		return Value{Kind: ValueText, Str: t.text}, nil
	case tokenPlus, tokenMinus:
		p.next()
		num, err := p.expectKind(tokenInt, "integer literal after sign")
		if err != nil {
			return Value{}, err
		}
		n, parseErr := strconv.ParseInt(t.text+num.text, 10, 64)
		if parseErr != nil {
			return Value{}, p.errorf(t.pos, "integer literal out of range")
		}
		return Value{Kind: ValueInt, Int: n}, nil
	default:
		return Value{}, p.errorf(t.pos, "expected literal value, found %s", describeToken(t))
	}
}

// parseLimit parses the non-negative integer following LIMIT.
func (p *parser) parseLimit() (int, error) {
	t := p.peek()
	if t.kind != tokenInt {
		return 0, p.errorf(t.pos, "expected non-negative integer after LIMIT, found %s", describeToken(t))
	}
	p.next()
	u, err := strconv.ParseUint(t.text, 10, 64)
	maxInt := int(^uint(0) >> 1)
	if err != nil || u > uint64(maxInt) {
		return 0, p.errorf(t.pos, "LIMIT value out of range")
	}
	return int(u), nil
}

func describeToken(t token) string {
	switch t.kind {
	case tokenEOF:
		return "end of input"
	case tokenIdent:
		return fmt.Sprintf("%q", t.text)
	case tokenInt:
		return fmt.Sprintf("integer %q", t.text)
	case tokenString:
		return "string literal"
	case tokenLParen:
		return "'('"
	case tokenRParen:
		return "')'"
	case tokenComma:
		return "','"
	case tokenSemicolon:
		return "';'"
	case tokenStar:
		return "'*'"
	case tokenEq:
		return "'='"
	case tokenPlus:
		return "'+'"
	case tokenMinus:
		return "'-'"
	default:
		return "token"
	}
}
