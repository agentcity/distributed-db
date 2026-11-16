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
