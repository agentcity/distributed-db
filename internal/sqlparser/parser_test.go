package sqlparser

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseValidStatements(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want Statement
	}{
		{
			name: "create table",
			sql:  `CREATE TABLE users (id INT PRIMARY KEY, name TEXT)`,
			want: &CreateTable{
				Name: "users",
				Columns: []ColumnDef{
					{Name: "id", Type: "INT", PrimaryKey: true},
					{Name: "name", Type: "TEXT"},
				},
			},
		},
		{
			name: "create table lowercase and semicolon",
			sql:  "  create\ttable kv (k text primary\tkey, v int) ;  ",
			want: &CreateTable{
				Name: "kv",
				Columns: []ColumnDef{
					{Name: "k", Type: "TEXT", PrimaryKey: true},
					{Name: "v", Type: "INT"},
				},
			},
		},
		{
			name: "create table mixed case keywords",
			sql:  `CrEaTe TaBlE T (Id InT PrImArY KeY)`,
			want: &CreateTable{
				Name:    "T",
				Columns: []ColumnDef{{Name: "Id", Type: "INT", PrimaryKey: true}},
			},
		},
		{
			name: "drop table",
			sql:  `DROP TABLE users;`,
			want: &DropTable{Name: "users"},
		},
		{
			name: "insert without columns",
			sql:  `INSERT INTO users VALUES (1, 'alice')`,
			want: &Insert{
				Table:  "users",
				Values: []Value{{Kind: ValueInt, Int: 1}, {Kind: ValueText, Str: "alice"}},
			},
		},
		{
			name: "insert with columns and negative int",
			sql:  `INSERT INTO users (name, id) VALUES ('O''Brien', -5);`,
			want: &Insert{
				Table:   "users",
				Columns: []string{"name", "id"},
				Values:  []Value{{Kind: ValueText, Str: "O'Brien"}, {Kind: ValueInt, Int: -5}},
			},
		},
		{
			name: "insert signed plus int",
			sql:  `INSERT INTO t VALUES (+7)`,
			want: &Insert{
				Table:  "t",
				Values: []Value{{Kind: ValueInt, Int: 7}},
			},
		},
		{
			name: "insert values count is unchecked without column list",
			sql:  `INSERT INTO t VALUES (1, 2, 3, 'x')`,
			want: &Insert{
				Table: "t",
				Values: []Value{
					{Kind: ValueInt, Int: 1},
					{Kind: ValueInt, Int: 2},
					{Kind: ValueInt, Int: 3},
					{Kind: ValueText, Str: "x"},
				},
			},
		},
		{
			name: "select star with where and limit",
			sql:  `SELECT * FROM users WHERE id = 42 LIMIT 10;`,
			want: &Select{
				Table:   "users",
				Columns: []string{"*"},
				Where:   &Condition{Column: "id", Value: Value{Kind: ValueInt, Int: 42}},
				Limit:   10,
			},
		},
		{
			name: "select columns lowercase",
			sql:  "select id , name from users",
			want: &Select{
				Table:   "users",
				Columns: []string{"id", "name"},
			},
		},
		{
			name: "select where text with escaped quote",
			sql:  `SELECT name FROM users WHERE name = 'it''s'`,
			want: &Select{
				Table:   "users",
				Columns: []string{"name"},
				Where:   &Condition{Column: "name", Value: Value{Kind: ValueText, Str: "it's"}},
			},
		},
		{
			name: "select limit zero",
			sql:  `SELECT * FROM t LIMIT 0`,
			want: &Select{Table: "t", Columns: []string{"*"}, Limit: 0},
		},
		{
			name: "select negative where literal",
			sql:  `SELECT * FROM t WHERE id = -9223372036854775808`,
			want: &Select{
				Table:   "t",
				Columns: []string{"*"},
				Where:   &Condition{Column: "id", Value: Value{Kind: ValueInt, Int: -9223372036854775808}},
			},
		},
		{
			name: "update with multiple assignments and where",
			sql:  `UPDATE users SET name = 'bob', id = -1 WHERE id = 7;`,
			want: &Update{
				Table: "users",
				Assignments: []Assignment{
					{Column: "name", Value: Value{Kind: ValueText, Str: "bob"}},
					{Column: "id", Value: Value{Kind: ValueInt, Int: -1}},
				},
				Where: &Condition{Column: "id", Value: Value{Kind: ValueInt, Int: 7}},
			},
		},
		{
			name: "update without where",
			sql:  `update t set a = 1`,
			want: &Update{
				Table:       "t",
				Assignments: []Assignment{{Column: "a", Value: Value{Kind: ValueInt, Int: 1}}},
			},
		},
		{
			name: "create table primary key last",
			sql:  `CREATE TABLE t (name TEXT, id INT PRIMARY KEY)`,
			want: &CreateTable{
				Name: "t",
				Columns: []ColumnDef{
					{Name: "name", Type: "TEXT"},
					{Name: "id", Type: "INT", PrimaryKey: true},
				},
			},
		},
		{
			name: "delete with where",
			sql:  `DELETE FROM users WHERE id = 3;`,
			want: &Delete{
				Table: "users",
				Where: &Condition{Column: "id", Value: Value{Kind: ValueInt, Int: 3}},
			},
		},
		{
			name: "delete without where",
			sql:  `delete from users`,
			want: &Delete{Table: "users"},
		},
		{
			name: "mixed case keywords",
			sql:  `sElEcT * FrOm T wHeRe ID = -7 LiMiT 2`,
			want: &Select{
				Table:   "T",
				Columns: []string{"*"},
				Where:   &Condition{Column: "ID", Value: Value{Kind: ValueInt, Int: -7}},
				Limit:   2,
			},
		},
		{
			name: "string escapes repeated",
			sql:  `SELECT * FROM t WHERE a = 'a''b''c'`,
			want: &Select{
				Table:   "t",
				Columns: []string{"*"},
				Where:   &Condition{Column: "a", Value: Value{Kind: ValueText, Str: "a'b'c"}},
			},
		},
		{
			name: "empty string literal",
			sql:  `INSERT INTO t VALUES ('')`,
			want: &Insert{Table: "t", Values: []Value{{Kind: ValueText, Str: ""}}},
		},
		{
			name: "unicode string literal",
			sql:  `INSERT INTO t VALUES ('Привет')`,
			want: &Insert{Table: "t", Values: []Value{{Kind: ValueText, Str: "Привет"}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.sql)
			if err != nil {
				t.Fatalf("Parse(%q) returned error: %v", tt.sql, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Parse(%q)\n got: %#v\nwant: %#v", tt.sql, got, tt.want)
			}
		})
	}
}

func TestParseErrorPositions(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		pos  int
	}{
		{name: "empty", sql: "", pos: 0},
		{name: "only whitespace", sql: " \t\n ", pos: 4},
		{name: "only semicolon", sql: ";", pos: 0},
		{name: "unknown statement", sql: "ALTER TABLE t", pos: 0},
		{name: "trailing identifier", sql: "SELECT * FROM t extra", pos: strings.Index("SELECT * FROM t extra", "extra")},
		{name: "trailing after semicolon", sql: "SELECT * FROM t; x", pos: strings.Index("SELECT * FROM t; x", "x")},
		{name: "double semicolon", sql: "SELECT * FROM t;;", pos: strings.Index("SELECT * FROM t;;", ";;") + 1},
		{name: "malformed where trailing and", sql: "SELECT * FROM t WHERE id = 1 AND name = 'x'", pos: strings.Index("SELECT * FROM t WHERE id = 1 AND name = 'x'", "AND")},
		{name: "missing primary key", sql: "CREATE TABLE t (id INT)", pos: strings.Index("CREATE TABLE t (id INT)", ")")},
		{name: "duplicate column", sql: "CREATE TABLE t (id INT PRIMARY KEY, id TEXT)", pos: strings.Index("CREATE TABLE t (id INT PRIMARY KEY, id TEXT)", "id TEXT")},
		{name: "insert column value mismatch", sql: "INSERT INTO t (a, b) VALUES (1)", pos: strings.Index("INSERT INTO t (a, b) VALUES (1)", "VALUES (") + len("VALUES ")},
		{name: "unterminated string", sql: "INSERT INTO t VALUES ('abc)", pos: strings.Index("INSERT INTO t VALUES ('abc)", "'")},
		{name: "negative limit", sql: "SELECT * FROM t LIMIT -1", pos: strings.Index("SELECT * FROM t LIMIT -1", "-1")},
		{name: "byte offset after unicode literal", sql: "SELECT * FROM t WHERE name = 'Ж' extra", pos: strings.Index("SELECT * FROM t WHERE name = 'Ж' extra", "extra")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt, err := Parse(tt.sql)
			if err == nil {
				t.Fatalf("Parse(%q) = %#v, want error", tt.sql, stmt)
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("Parse(%q) error type = %T, want *ParseError", tt.sql, err)
			}
			if pe.Pos != tt.pos {
				t.Fatalf("Parse(%q) error pos = %d, want %d (msg %q)", tt.sql, pe.Pos, tt.pos, pe.Msg)
			}
			if pe.Msg == "" {
				t.Fatalf("Parse(%q) error message is empty", tt.sql)
			}
			if !strings.Contains(pe.Error(), "sql: syntax error at byte") {
				t.Fatalf("ParseError.Error() = %q, want sql-prefixed message", pe.Error())
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		// Empty and unknown input.
		{name: "empty", sql: ""},
		{name: "whitespace", sql: "   \n\t  "},
		{name: "semicolon only", sql: " ; "},
		{name: "unknown keyword", sql: "ALTER TABLE t"},
		{name: "garbage", sql: "not sql at all"},
		{name: "statement starts with integer", sql: "123"},

		// CREATE TABLE.
		{name: "create missing table keyword", sql: "CREATE users (id INT PRIMARY KEY)"},
		{name: "create missing table name", sql: "CREATE TABLE (id INT PRIMARY KEY)"},
		{name: "create missing parens", sql: "CREATE TABLE t id INT PRIMARY KEY"},
		{name: "create empty columns", sql: "CREATE TABLE t ()"},
		{name: "create missing close paren", sql: "CREATE TABLE t (id INT PRIMARY KEY"},
		{name: "create trailing comma", sql: "CREATE TABLE t (id INT PRIMARY KEY, )"},
		{name: "create missing primary key", sql: "CREATE TABLE t (id INT, name TEXT)"},
		{name: "create duplicate primary key across columns", sql: "CREATE TABLE t (id INT PRIMARY KEY, name TEXT PRIMARY KEY)"},
		{name: "create duplicate primary key same column", sql: "CREATE TABLE t (id INT PRIMARY KEY PRIMARY KEY)"},
		{name: "create primary without key", sql: "CREATE TABLE t (id INT PRIMARY KEY KEY)"},
		{name: "create primary missing key word", sql: "CREATE TABLE t (id INT PRIMARY)"},
		{name: "create duplicate column", sql: "CREATE TABLE t (id INT PRIMARY KEY, id TEXT)"},
		{name: "create bad type", sql: "CREATE TABLE t (id VARCHAR PRIMARY KEY)"},
		{name: "create bad constraint", sql: "CREATE TABLE t (id INT PRIMARY KEY, name TEXT NOT NULL)"},
		{name: "create missing column name", sql: "CREATE TABLE t (, id INT PRIMARY KEY)"},
		{name: "create missing column type", sql: "CREATE TABLE t (id PRIMARY KEY)"},
		{name: "create trailing tokens", sql: "CREATE TABLE t (id INT PRIMARY KEY) extra"},

		// DROP TABLE.
		{name: "drop missing table keyword", sql: "DROP users"},
		{name: "drop missing name", sql: "DROP TABLE"},
		{name: "drop trailing tokens", sql: "DROP TABLE t extra"},

		// INSERT.
		{name: "insert missing into", sql: "INSERT users VALUES (1)"},
		{name: "insert missing values", sql: "INSERT INTO t (a) (1)"},
		{name: "insert empty column list", sql: "INSERT INTO t () VALUES (1)"},
		{name: "insert trailing comma in columns", sql: "INSERT INTO t (a,) VALUES (1)"},
		{name: "insert missing comma in columns", sql: "INSERT INTO t (a b) VALUES (1, 2)"},
		{name: "insert missing comma in values", sql: "INSERT INTO t VALUES (1 2)"},
		{name: "insert column value mismatch too few", sql: "INSERT INTO t (a, b) VALUES (1)"},
		{name: "insert column value mismatch too many", sql: "INSERT INTO t (a, b) VALUES (1, 2, 3)"},
		{name: "insert empty values", sql: "INSERT INTO t VALUES ()"},
		{name: "insert trailing comma in values", sql: "INSERT INTO t VALUES (1,)"},
		{name: "insert missing values paren", sql: "INSERT INTO t VALUES 1"},
		{name: "insert missing close paren", sql: "INSERT INTO t VALUES (1"},
		{name: "insert multiple rows", sql: "INSERT INTO t VALUES (1), (2)"},
		{name: "insert trailing tokens", sql: "INSERT INTO t VALUES (1) garbage"},
		{name: "insert signed string", sql: "INSERT INTO t VALUES (-'x')"},
		{name: "insert dangling sign", sql: "INSERT INTO t VALUES (1, -)"},
		{name: "insert int overflow", sql: "INSERT INTO t VALUES (9223372036854775808)"},
		{name: "insert negative int overflow", sql: "INSERT INTO t VALUES (-9223372036854775809)"},

		// SELECT.
		{name: "select missing columns", sql: "SELECT FROM t"},
		{name: "select star plus column", sql: "SELECT *, id FROM t"},
		{name: "select trailing comma", sql: "SELECT id, FROM t"},
		{name: "select missing from", sql: "SELECT * t"},
		{name: "select missing table", sql: "SELECT * FROM"},
		{name: "select missing where column", sql: "SELECT * FROM t WHERE"},
		{name: "select missing equals in where", sql: "SELECT * FROM t WHERE id"},
		{name: "select where starts with equals", sql: "SELECT * FROM t WHERE = 1"},
		{name: "select missing where literal", sql: "SELECT * FROM t WHERE id ="},
		{name: "select where with and", sql: "SELECT * FROM t WHERE id = 1 AND id = 2"},
		{name: "select where with non literal", sql: "SELECT * FROM t WHERE id = name"},
		{name: "select negative limit", sql: "SELECT * FROM t LIMIT -1"},
		{name: "select non integer limit", sql: "SELECT * FROM t LIMIT x"},
		{name: "select limit out of range", sql: "SELECT * FROM t LIMIT 18446744073709551616"},
		{name: "select missing limit value", sql: "SELECT * FROM t LIMIT"},
		{name: "select where after limit", sql: "SELECT * FROM t LIMIT 5 WHERE id = 1"},
		{name: "select trailing tokens", sql: "SELECT * FROM t extra"},
		{name: "select trailing string", sql: "SELECT * FROM t 'x'"},

		// UPDATE.
		{name: "update missing set", sql: "UPDATE t a = 1"},
		{name: "update missing assignment", sql: "UPDATE t SET"},
		{name: "update missing equals", sql: "UPDATE t SET a 1"},
		{name: "update missing literal", sql: "UPDATE t SET a ="},
		{name: "update trailing comma", sql: "UPDATE t SET a = 1, WHERE id = 2"},
		{name: "update trailing comma at end", sql: "UPDATE t SET a = 1,"},
		{name: "update malformed where", sql: "UPDATE t SET a = 1 WHERE id"},
		{name: "update trailing tokens", sql: "UPDATE t SET a = 1 extra"},

		// DELETE.
		{name: "delete missing from", sql: "DELETE t WHERE id = 1"},
		{name: "delete missing table", sql: "DELETE FROM"},
		{name: "delete malformed where", sql: "DELETE FROM t WHERE id ="},
		{name: "delete trailing tokens", sql: "DELETE FROM t extra"},

		// Lexer.
		{name: "unterminated string", sql: "INSERT INTO t VALUES ('abc)"},
		{name: "unexpected character", sql: "SELECT * FROM t WHERE id = @"},
		{name: "unexpected character before statement", sql: "!SELECT * FROM t"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.sql)
			if err == nil {
				t.Fatalf("Parse(%q) = %#v, want error", tt.sql, got)
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("Parse(%q) error type = %T, want *ParseError", tt.sql, err)
			}
			if pe.Pos < 0 || pe.Pos > len(tt.sql) {
				t.Fatalf("Parse(%q) error pos = %d, want within [0,%d]", tt.sql, pe.Pos, len(tt.sql))
			}
			if pe.Msg == "" {
				t.Fatalf("Parse(%q) error message is empty", tt.sql)
			}
			if got != nil {
				t.Fatalf("Parse(%q) returned a statement %#v with error %v", tt.sql, got, err)
			}
		})
	}
}

func TestParseValueAsAny(t *testing.T) {
	stmt, err := Parse(`INSERT INTO t VALUES (-3, 'x')`)
	if err != nil {
		t.Fatal(err)
	}
	ins := stmt.(*Insert)
	if got, want := ins.Values[0].AsAny(), any(int64(-3)); got != want {
		t.Fatalf("int AsAny() = %#v, want %#v", got, want)
	}
	if got, want := ins.Values[1].AsAny(), any("x"); got != want {
		t.Fatalf("text AsAny() = %#v, want %#v", got, want)
	}
}

func TestParseIndexStatements(t *testing.T) {
	stmt, err := Parse("CREATE INDEX idx_name ON users (name)")
	if err != nil {
		t.Fatalf("create index: %v", err)
	}
	ci, ok := stmt.(*CreateIndex)
	if !ok || ci.Name != "idx_name" || ci.Table != "users" || ci.Column != "name" {
		t.Fatalf("create index = %#v", stmt)
	}
	stmt, err = Parse("DROP INDEX idx_name ON users")
	if err != nil {
		t.Fatalf("drop index: %v", err)
	}
	di, ok := stmt.(*DropIndex)
	if !ok || di.Name != "idx_name" || di.Table != "users" {
		t.Fatalf("drop index = %#v", stmt)
	}
	if _, err := Parse("CREATE INDEX idx ON users"); err == nil {
		t.Fatal("CREATE INDEX without column accepted")
	}
}

func TestParseAggregates(t *testing.T) {
	cases := []struct {
		sql  string
		fn   string
		col  string
		star bool
	}{
		{"SELECT COUNT(*) FROM users", "COUNT", "", true},
		{"SELECT count(id) FROM users", "COUNT", "id", false},
		{"SELECT SUM(amount) FROM orders WHERE id = 1", "SUM", "amount", false},
		{"SELECT AVG(amount) FROM orders", "AVG", "amount", false},
		{"SELECT MIN(name) FROM users", "MIN", "name", false},
		{"SELECT MAX(name) FROM users", "MAX", "name", false},
	}
	for _, tc := range cases {
		stmt, err := Parse(tc.sql)
		if err != nil {
			t.Fatalf("%q: %v", tc.sql, err)
		}
		sel, ok := stmt.(*Select)
		if !ok || sel.Aggregate == nil {
			t.Fatalf("%q: aggregate not parsed: %#v", tc.sql, stmt)
		}
		a := sel.Aggregate
		if a.Func != tc.fn || a.Column != tc.col || a.Star != tc.star {
			t.Fatalf("%q: aggregate = %+v", tc.sql, a)
		}
	}
	if _, err := Parse("SELECT SUM(*) FROM users"); err == nil {
		t.Fatal("SUM(*) accepted")
	}
}

func TestParseGroupBy(t *testing.T) {
	valid := []struct {
		sql   string
		group string
	}{
		{"SELECT COUNT(*) FROM users GROUP BY city", "city"},
		{"SELECT sum(amount) FROM orders WHERE id = 1 GROUP BY region", "region"},
		{"SELECT AVG(amount) FROM orders WHERE id = 1 GROUP BY region LIMIT 3", "region"},
	}
	for _, tc := range valid {
		stmt, err := Parse(tc.sql)
		if err != nil {
			t.Fatalf("%q: %v", tc.sql, err)
		}
		sel, ok := stmt.(*Select)
		if !ok || sel.Aggregate == nil {
			t.Fatalf("%q: aggregate not parsed: %#v", tc.sql, stmt)
		}
		if sel.GroupBy != tc.group {
			t.Fatalf("%q: GroupBy = %q, want %q", tc.sql, sel.GroupBy, tc.group)
		}
	}

	invalid := []string{
		"SELECT id FROM users GROUP BY id",
		"SELECT * FROM users GROUP BY city",
		"SELECT COUNT(*) FROM users GROUP BY city, region",
		"SELECT COUNT(*) FROM users GROUP BY",
		"SELECT COUNT(*) FROM users GROUP BY 1",
		"SELECT COUNT(*) FROM users GROUP city",
		"SELECT COUNT(*) FROM users GROUP BY city WHERE id = 1",
	}
	for _, sql := range invalid {
		stmt, err := Parse(sql)
		if err == nil {
			t.Fatalf("Parse(%q) = %#v, want error", sql, stmt)
		}
		var pe *ParseError
		if !errors.As(err, &pe) {
			t.Fatalf("Parse(%q) error type = %T, want *ParseError", sql, err)
		}
	}
}

func TestParseJoin(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want *Select
	}{
		{
			name: "join with aliases where and limit",
			sql:  `SELECT u.name, o.total FROM users AS u JOIN orders AS o ON u.id = o.user_id WHERE o.total = 100 LIMIT 5`,
			want: &Select{
				Table:   "users",
				Columns: []string{"u.name", "o.total"},
				Join: &JoinClause{
					LeftTable:   "users",
					RightTable:  "orders",
					LeftAlias:   "u",
					RightAlias:  "o",
					LeftColumn:  "u.id",
					RightColumn: "o.user_id",
				},
				Where: &Condition{Column: "o.total", Value: Value{Kind: ValueInt, Int: 100}},
				Limit: 5,
			},
		},
		{
			name: "select star join without aliases",
			sql:  `select * from users join orders on users.id = orders.user_id limit 2`,
			want: &Select{
				Table:   "users",
				Columns: []string{"*"},
				Join: &JoinClause{
					LeftTable:   "users",
					RightTable:  "orders",
					LeftColumn:  "users.id",
					RightColumn: "orders.user_id",
				},
				Limit: 2,
			},
		},
		{
			name: "inner join keyword",
			sql:  `SELECT u.id FROM users AS u INNER JOIN orders AS o ON u.id = o.user_id`,
			want: &Select{
				Table:   "users",
				Columns: []string{"u.id"},
				Join: &JoinClause{
					LeftTable:   "users",
					RightTable:  "orders",
					LeftAlias:   "u",
					RightAlias:  "o",
					LeftColumn:  "u.id",
					RightColumn: "o.user_id",
				},
			},
		},
		{
			name: "unqualified on columns",
			sql:  `SELECT * FROM a JOIN b ON id = ref`,
			want: &Select{
				Table:   "a",
				Columns: []string{"*"},
				Join: &JoinClause{
					LeftTable:   "a",
					RightTable:  "b",
					LeftColumn:  "id",
					RightColumn: "ref",
				},
			},
		},
		{
			name: "mixed case join keywords",
			sql:  `SeLeCt U.Name FrOm Users AS U jOiN Orders As O oN U.Id = O.User_Id WhErE U.Id = 1;`,
			want: &Select{
				Table:   "Users",
				Columns: []string{"U.Name"},
				Join: &JoinClause{
					LeftTable:   "Users",
					RightTable:  "Orders",
					LeftAlias:   "U",
					RightAlias:  "O",
					LeftColumn:  "U.Id",
					RightColumn: "O.User_Id",
				},
				Where: &Condition{Column: "U.Id", Value: Value{Kind: ValueInt, Int: 1}},
			},
		},
		{
			name: "left alias only",
			sql:  `SELECT * FROM users AS u JOIN orders ON u.id = orders.user_id`,
			want: &Select{
				Table:   "users",
				Columns: []string{"*"},
				Join: &JoinClause{
					LeftTable:   "users",
					RightTable:  "orders",
					LeftAlias:   "u",
					LeftColumn:  "u.id",
					RightColumn: "orders.user_id",
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt, err := Parse(tt.sql)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.sql, err)
			}
			if !reflect.DeepEqual(stmt, tt.want) {
				t.Fatalf("Parse(%q)\n got: %#v\nwant: %#v", tt.sql, stmt, tt.want)
			}
		})
	}
}

func TestParseJoinErrors(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{name: "missing on", sql: `SELECT * FROM a JOIN b`},
		{name: "missing equals", sql: `SELECT * FROM a JOIN b ON a.id`},
		{name: "missing right column", sql: `SELECT * FROM a JOIN b ON a.id =`},
		{name: "missing left column", sql: `SELECT * FROM a JOIN b ON = b.id`},
		{name: "missing right table", sql: `SELECT * FROM a JOIN ON a.id = b.id`},
		{name: "second join", sql: `SELECT * FROM a JOIN b ON a.id = b.id JOIN c ON b.id = c.id`},
		{name: "second inner join", sql: `SELECT * FROM a JOIN b ON a.id = b.id INNER JOIN c ON b.id = c.id`},
		{name: "left join unsupported", sql: `SELECT * FROM a LEFT JOIN b ON a.id = b.id`},
		{name: "right join unsupported", sql: `SELECT * FROM a RIGHT JOIN b ON a.id = b.id`},
		{name: "cross join unsupported", sql: `SELECT * FROM a CROSS JOIN b ON a.id = b.id`},
		{name: "self join without aliases", sql: `SELECT * FROM t JOIN t ON t.id = t.id`},
		{name: "duplicate alias", sql: `SELECT * FROM a AS x JOIN b AS x ON x.id = x.id`},
		{name: "alias without join", sql: `SELECT * FROM a AS x`},
		{name: "join target qualified", sql: `SELECT * FROM a JOIN db.b ON a.id = b.id`},
		{name: "non equality operator", sql: `SELECT * FROM a JOIN b ON a.id != b.id`},
		{name: "non equality greater", sql: `SELECT * FROM a JOIN b ON a.id > b.id`},
		{name: "double equals", sql: `SELECT * FROM a JOIN b ON a.id = b.id = c.id`},
		{name: "and after on", sql: `SELECT * FROM a JOIN b ON a.id = b.id AND a.x = 1`},
		{name: "star plus column", sql: `SELECT *, a.id FROM a JOIN b ON a.id = b.id`},
		{name: "qualified star", sql: `SELECT a.* FROM a JOIN b ON a.id = b.id`},
		{name: "where and", sql: `SELECT * FROM a JOIN b ON a.id = b.id WHERE a.x = 1 AND b.y = 2`},
		{name: "group by with join", sql: `SELECT COUNT(*) FROM a JOIN b ON a.id = b.id GROUP BY a.x`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt, err := Parse(tt.sql)
			if err == nil {
				t.Fatalf("Parse(%q) = %#v, want error", tt.sql, stmt)
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("Parse(%q) error type = %T, want *ParseError", tt.sql, err)
			}
		})
	}
}

func TestParseRangePredicates(t *testing.T) {
	tests := []struct {
		sql string
		op  ComparisonOp
		low Value
		hi  Value
	}{
		{"SELECT * FROM t WHERE id > 5", OpGT, Value{Kind: ValueInt, Int: 5}, Value{}},
		{"SELECT * FROM t WHERE id >= 5", OpGE, Value{Kind: ValueInt, Int: 5}, Value{}},
		{"SELECT * FROM t WHERE id < -5", OpLT, Value{Kind: ValueInt, Int: -5}, Value{}},
		{"SELECT * FROM t WHERE id <= 5", OpLE, Value{Kind: ValueInt, Int: 5}, Value{}},
		{"SELECT * FROM t WHERE name <> 'x'", OpNE, Value{Kind: ValueText, Str: "x"}, Value{}},
		{"SELECT * FROM t WHERE name != 'x'", OpNE, Value{Kind: ValueText, Str: "x"}, Value{}},
		{"SELECT * FROM t WHERE id BETWEEN -2 AND 7", OpBetween, Value{Kind: ValueInt, Int: -2}, Value{Kind: ValueInt, Int: 7}},
		{"SELECT * FROM t WHERE name BETWEEN 'a' AND 'm'", OpBetween, Value{Kind: ValueText, Str: "a"}, Value{Kind: ValueText, Str: "m"}},
	}
	for _, tc := range tests {
		stmt, err := Parse(tc.sql)
		if err != nil {
			t.Fatalf("%q: %v", tc.sql, err)
		}
		sel := stmt.(*Select)
		if sel.Where == nil || sel.Where.Op != tc.op {
			t.Fatalf("%q: condition = %+v", tc.sql, sel.Where)
		}
		if !reflect.DeepEqual(sel.Where.Value, tc.low) || !reflect.DeepEqual(sel.Where.High, tc.hi) {
			t.Fatalf("%q: bounds = %+v / %+v", tc.sql, sel.Where.Value, sel.Where.High)
		}
	}

	// Equality keeps the zero value so older AST literals stay valid.
	stmt, err := Parse("SELECT * FROM t WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if got := stmt.(*Select).Where.Op; got != OpEQ {
		t.Fatalf("equality op = %v, want OpEQ", got)
	}

	for _, sql := range []string{
		"SELECT * FROM t WHERE id BETWEEN 1",
		"SELECT * FROM t WHERE id BETWEEN 1 AND",
		"SELECT * FROM t WHERE id >",
		"SELECT * FROM t WHERE id >< 1",
	} {
		if _, err := Parse(sql); err == nil {
			t.Errorf("%q: expected error", sql)
		}
	}
}

func TestParseOrderBy(t *testing.T) {
	tests := []struct {
		sql  string
		col  string
		desc bool
	}{
		{"SELECT * FROM t ORDER BY id", "id", false},
		{"SELECT * FROM t ORDER BY id ASC", "id", false},
		{"SELECT * FROM t ORDER BY id DESC", "id", true},
		{"SELECT * FROM t WHERE id > 1 ORDER BY n.age DESC LIMIT 5", "n.age", true},
	}
	for _, tc := range tests {
		stmt, err := Parse(tc.sql)
		if err != nil {
			t.Fatalf("%q: %v", tc.sql, err)
		}
		sel := stmt.(*Select)
		if sel.OrderBy != tc.col || sel.OrderDesc != tc.desc {
			t.Fatalf("%q: OrderBy = %q/%v, want %q/%v", tc.sql, sel.OrderBy, sel.OrderDesc, tc.col, tc.desc)
		}
	}
	for _, sql := range []string{
		"SELECT * FROM t ORDER BY",
		"SELECT * FROM t ORDER id",
		"SELECT * FROM t ORDER BY id, name",
		"SELECT * FROM t ORDER BY id DESC ASC",
		"SELECT * FROM t ORDER BY id LIMIT 1 WHERE id = 2",
	} {
		if _, err := Parse(sql); err == nil {
			t.Errorf("%q: expected error", sql)
		}
	}
}

func TestNormalizeStatementShape(t *testing.T) {
	cases := []struct{ a, b string }{
		{"SELECT * FROM users WHERE id = 1", "select * from USERS where ID = 999"},
		{"SELECT name FROM users ORDER BY id DESC LIMIT 5", "select NAME from users order by ID desc limit 5"},
		{"UPDATE t SET a = 1, b = 'x' WHERE id = 3", "update T set A = 9, B = 'y' where ID = 4"},
		{"DELETE FROM t WHERE id BETWEEN 1 AND 2", "delete from T where ID between 8 and 9"},
	}
	for _, tc := range cases {
		sa, err := Parse(tc.a)
		if err != nil {
			t.Fatal(err)
		}
		sb, err := Parse(tc.b)
		if err != nil {
			t.Fatal(err)
		}
		if na, nb := Normalize(sa), Normalize(sb); na != nb {
			t.Errorf("normalize differs:\n%q\n%q", na, nb)
		}
	}
	if got := Normalize(mustParse(t, "SELECT * FROM t WHERE name > 'x'")); got != "SELECT * FROM t WHERE name > ?" {
		t.Fatalf("normalized = %q", got)
	}
}

func mustParse(t *testing.T, sql string) Statement {
	t.Helper()
	stmt, err := Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	return stmt
}
