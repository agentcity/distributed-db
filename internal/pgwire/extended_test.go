package pgwire

import (
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

// --- extended-protocol wire helpers ----------------------------------------

type extBindParam struct {
	null bool
	data []byte
}

func extTextParam(s string) extBindParam { return extBindParam{data: []byte(s)} }

func extInt8Param(n int64) extBindParam {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(n))
	return extBindParam{data: buf[:]}
}

func extInt4Param(n int32) extBindParam {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(n))
	return extBindParam{data: buf[:]}
}

func extNilParam() extBindParam { return extBindParam{null: true} }

func extParseMessage(name, query string, oids []uint32) []byte {
	body := appendCString(nil, name)
	body = appendCString(body, query)
	body = appendInt16(body, int16(len(oids)))
	for _, oid := range oids {
		body = appendUint32(body, oid)
	}
	return body
}

func extBindMessage(portal, stmt string, paramFormats []int16, params []extBindParam, resultFormats []int16) []byte {
	body := appendCString(nil, portal)
	body = appendCString(body, stmt)
	body = appendInt16(body, int16(len(paramFormats)))
	for _, f := range paramFormats {
		body = appendInt16(body, f)
	}
	body = appendInt16(body, int16(len(params)))
	for _, p := range params {
		if p.null {
			body = appendInt32(body, -1)
			continue
		}
		body = appendInt32(body, int32(len(p.data)))
		body = append(body, p.data...)
	}
	body = appendInt16(body, int16(len(resultFormats)))
	for _, f := range resultFormats {
		body = appendInt16(body, f)
	}
	return body
}

func extDescribeMessage(kind byte, name string) []byte {
	body := []byte{kind}
	return appendCString(body, name)
}

func extExecuteMessage(name string, maxRows int32) []byte {
	body := appendCString(nil, name)
	return appendInt32(body, maxRows)
}

func extCloseMessage(kind byte, name string) []byte {
	body := []byte{kind}
	return appendCString(body, name)
}

// --- test connection helpers ------------------------------------------------

func (c *testConn) extSend(typ byte, body []byte) {
	c.t.Helper()
	if err := writeMessage(c.conn, typ, body); err != nil {
		c.t.Fatalf("send message %q: %v", typ, err)
	}
}

func (c *testConn) extReadMsg() wireMsg {
	c.t.Helper()
	typ, body, err := readMessage(c.conn)
	if err != nil {
		c.t.Fatalf("read message: %v", err)
	}
	return wireMsg{typ: typ, body: body}
}

func (c *testConn) extExpectMsg(want byte) wireMsg {
	c.t.Helper()
	m := c.extReadMsg()
	if m.typ != want {
		fields := errorFields(m.body)
		c.t.Fatalf("message type = %q, want %q (error: %s %s)", m.typ, want, fields['C'], fields['M'])
	}
	return m
}

func (c *testConn) extExpectError(wantCode string) map[byte]string {
	c.t.Helper()
	m := c.extExpectMsg(msgErrorResponse)
	fields := errorFields(m.body)
	if fields['C'] != wantCode {
		c.t.Fatalf("SQLSTATE = %q, want %q (message: %s)", fields['C'], wantCode, fields['M'])
	}
	return fields
}

func (c *testConn) extSync() []wireMsg {
	c.t.Helper()
	c.extSend(msgSync, nil)
	msgs, err := readUntilReady(c.conn)
	if err != nil {
		c.t.Fatalf("read after Sync: %v", err)
	}
	return msgs
}

func (c *testConn) extFlushOutput() {
	c.t.Helper()
	c.extSend(msgFlush, nil)
}

func parseParameterDescription(t *testing.T, body []byte) []uint32 {
	t.Helper()
	if len(body) < 2 {
		t.Fatalf("ParameterDescription too short: %x", body)
	}
	n := int(int16(binary.BigEndian.Uint16(body[:2])))
	if len(body) != 2+4*n {
		t.Fatalf("ParameterDescription length = %d, want %d", len(body), 2+4*n)
	}
	oids := make([]uint32, n)
	for i := range oids {
		oids[i] = binary.BigEndian.Uint32(body[2+4*i:])
	}
	return oids
}

type extRawDataValue struct {
	null bool
	data []byte
}

func extParseDataRowRaw(t *testing.T, body []byte) []extRawDataValue {
	t.Helper()
	if len(body) < 2 {
		t.Fatalf("DataRow too short: %x", body)
	}
	n := int(int16(binary.BigEndian.Uint16(body[:2])))
	rest := body[2:]
	values := make([]extRawDataValue, n)
	for i := range values {
		if len(rest) < 4 {
			t.Fatalf("DataRow column %d: missing length", i)
		}
		l := int32(binary.BigEndian.Uint32(rest[:4]))
		rest = rest[4:]
		if l == -1 {
			values[i] = extRawDataValue{null: true}
			continue
		}
		if l < 0 || int(l) > len(rest) {
			t.Fatalf("DataRow column %d: invalid length %d (have %d)", i, l, len(rest))
		}
		values[i] = extRawDataValue{data: append([]byte(nil), rest[:l]...)}
		rest = rest[l:]
	}
	if len(rest) != 0 {
		t.Fatalf("DataRow has %d trailing bytes", len(rest))
	}
	return values
}

func extAssertRowDescriptionFormats(t *testing.T, body []byte, want []int16) []fieldDescription {
	t.Helper()
	fields := parseRowDescription(t, body)
	if len(fields) != len(want) {
		t.Fatalf("RowDescription columns = %d, want %d", len(fields), len(want))
	}
	for i := range fields {
		if fields[i].format != want[i] {
			t.Fatalf("field %d format = %d, want %d", i, fields[i].format, want[i])
		}
	}
	return fields
}

func (c *testConn) extSimpleSetup(stmts ...string) {
	c.t.Helper()
	for _, q := range stmts {
		msgs := c.query(q)
		c.assertNoError(msgs)
	}
}

// --- tests ------------------------------------------------------------------

func TestExtendedParseBindExecute(t *testing.T) {
	_, addr := startTestServer(t)
	c := dialTestConn(t, addr)
	c.startup(map[string]string{"user": "alice", "database": "testdb"})
	c.extSimpleSetup(
		"CREATE TABLE users (id INT PRIMARY KEY, name TEXT)",
		"INSERT INTO users VALUES (1, 'alice')",
		"INSERT INTO users VALUES (2, 'bob')",
	)

	c.extSend(msgParse, extParseMessage("s1", "SELECT id, name FROM users WHERE id = $1", []uint32{20}))
	c.extExpectMsg(msgParseComplete)

	c.extSend(msgDescribe, extDescribeMessage('S', "s1"))
	params := parseParameterDescription(t, c.extExpectMsg(msgParameterDescription).body)
	if len(params) != 1 || params[0] != 20 {
		t.Fatalf("ParameterDescription = %v, want [20]", params)
	}
	fields := extAssertRowDescriptionFormats(t, c.extExpectMsg(msgRowDescription).body, []int16{0, 0})
	if len(fields) != 2 || fields[0].name != "id" || fields[0].typeOID != 20 || fields[0].typeSize != 8 {
		t.Fatalf("id field = %+v", fields[0])
	}
	if fields[1].name != "name" || fields[1].typeOID != 25 || fields[1].typeSize != -1 {
		t.Fatalf("name field = %+v", fields[1])
	}

	c.extSend(msgBind, extBindMessage("p1", "s1", []int16{0}, []extBindParam{extTextParam("1")}, nil))
	c.extExpectMsg(msgBindComplete)

	c.extSend(msgExecute, extExecuteMessage("p1", 0))
	row := parseDataRow(t, c.extExpectMsg(msgDataRow).body)
	if fmt.Sprint(row) != "[1 alice]" {
		t.Fatalf("row = %v, want [1 alice]", row)
	}
	assertCommandTag(t, []wireMsg{c.extExpectMsg(msgCommandComplete)}, "SELECT 1")

	c.extSend(msgSync, nil)
	msgs, err := readUntilReady(c.conn)
	if err != nil {
		t.Fatalf("read after Sync: %v", err)
	}
	if len(msgs) != 1 || msgs[0].typ != msgReadyForQuery {
		t.Fatalf("after Sync: %v, want only ReadyForQuery", msgTypes(msgs))
	}
}

func TestExtendedMultiplePortalsAndSuspension(t *testing.T) {
	_, addr := startTestServer(t)
	c := dialTestConn(t, addr)
	c.startup(map[string]string{"user": "alice"})
	c.extSimpleSetup(
		"CREATE TABLE nums (id INT PRIMARY KEY)",
		"INSERT INTO nums VALUES (10)",
		"INSERT INTO nums VALUES (20)",
		"INSERT INTO nums VALUES (30)",
	)

	c.extSend(msgParse, extParseMessage("scan", "SELECT id FROM nums", nil))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgDescribe, extDescribeMessage('S', "scan"))
	if got := parseParameterDescription(t, c.extExpectMsg(msgParameterDescription).body); len(got) != 0 {
		t.Fatalf("ParameterDescription = %v, want empty", got)
	}
	fields := extAssertRowDescriptionFormats(t, c.extExpectMsg(msgRowDescription).body, []int16{0})
	if len(fields) != 1 || fields[0].name != "id" || fields[0].typeOID != 20 {
		t.Fatalf("field = %+v, want id INT8", fields[0])
	}

	c.extSend(msgBind, extBindMessage("p1", "scan", nil, nil, nil))
	c.extExpectMsg(msgBindComplete)
	c.extSend(msgBind, extBindMessage("p2", "scan", nil, nil, nil))
	c.extExpectMsg(msgBindComplete)

	// Two rows, then suspension.
	c.extSend(msgExecute, extExecuteMessage("p1", 2))
	if row := parseDataRow(t, c.extExpectMsg(msgDataRow).body); fmt.Sprint(row) != "[10]" {
		t.Fatalf("first row = %v", row)
	}
	if row := parseDataRow(t, c.extExpectMsg(msgDataRow).body); fmt.Sprint(row) != "[20]" {
		t.Fatalf("second row = %v", row)
	}
	c.extExpectMsg(msgPortalSuspended)

	// Resume and finish p1.
	c.extSend(msgExecute, extExecuteMessage("p1", 1))
	if row := parseDataRow(t, c.extExpectMsg(msgDataRow).body); fmt.Sprint(row) != "[30]" {
		t.Fatalf("third row = %v", row)
	}
	assertCommandTag(t, []wireMsg{c.extExpectMsg(msgCommandComplete)}, "SELECT 3")

	// p2 was never consumed and runs from the beginning.
	c.extSend(msgExecute, extExecuteMessage("p2", 0))
	got := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		got = append(got, fmt.Sprint(parseDataRow(t, c.extExpectMsg(msgDataRow).body)))
	}
	if strings.Join(got, ",") != "[10],[20],[30]" {
		t.Fatalf("p2 rows = %v", got)
	}
	assertCommandTag(t, []wireMsg{c.extExpectMsg(msgCommandComplete)}, "SELECT 3")

	c.extSend(msgSync, nil)
	if msgs, err := readUntilReady(c.conn); err != nil || len(msgs) != 1 || msgs[0].typ != msgReadyForQuery {
		t.Fatalf("after Sync: msgs=%v err=%v", msgTypes(msgs), err)
	}

	// Named prepared statements survive Sync; a new portal can be bound.
	c.extSend(msgBind, extBindMessage("p3", "scan", nil, nil, nil))
	c.extExpectMsg(msgBindComplete)
	c.extSend(msgExecute, extExecuteMessage("p3", 1))
	parseDataRow(t, c.extExpectMsg(msgDataRow).body)
	c.extExpectMsg(msgPortalSuspended)
	c.extSend(msgSync, nil)
	if _, err := readUntilReady(c.conn); err != nil {
		t.Fatalf("read final Sync: %v", err)
	}
}

func TestExtendedBinaryIntAndText(t *testing.T) {
	_, addr := startTestServer(t)
	c := dialTestConn(t, addr)
	c.startup(map[string]string{"user": "alice"})
	c.extSimpleSetup(
		"CREATE TABLE ex (id INT PRIMARY KEY, name TEXT)",
		"INSERT INTO ex VALUES (7, 'seven')",
	)

	c.extSend(msgParse, extParseMessage("sel", "SELECT id, name FROM ex WHERE id = $1", []uint32{20}))
	c.extExpectMsg(msgParseComplete)

	// Binary INT8 parameter, binary results for both columns.
	c.extSend(msgBind, extBindMessage("pb", "sel", []int16{1}, []extBindParam{extInt8Param(7)}, []int16{1}))
	c.extExpectMsg(msgBindComplete)
	c.extSend(msgDescribe, extDescribeMessage('P', "pb"))
	fields := extAssertRowDescriptionFormats(t, c.extExpectMsg(msgRowDescription).body, []int16{1, 1})
	if fields[0].typeOID != 20 || fields[1].typeOID != 25 {
		t.Fatalf("portal RowDescription = %+v", fields)
	}
	c.extSend(msgExecute, extExecuteMessage("pb", 0))
	raw := extParseDataRowRaw(t, c.extExpectMsg(msgDataRow).body)
	if len(raw) != 2 || raw[0].null || raw[1].null {
		t.Fatalf("binary DataRow = %+v", raw)
	}
	if len(raw[0].data) != 8 || int64(binary.BigEndian.Uint64(raw[0].data)) != 7 {
		t.Fatalf("binary INT8 = %x, want 7", raw[0].data)
	}
	if string(raw[1].data) != "seven" {
		t.Fatalf("binary TEXT = %q, want seven", raw[1].data)
	}
	assertCommandTag(t, []wireMsg{c.extExpectMsg(msgCommandComplete)}, "SELECT 1")
	c.extSend(msgSync, nil)
	if _, err := readUntilReady(c.conn); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Binary INT8 + binary TEXT parameters for INSERT.
	c.extSend(msgParse, extParseMessage("ins", "INSERT INTO ex VALUES ($1, $2)", []uint32{20, 25}))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgBind, extBindMessage("pi", "ins", []int16{1, 1},
		[]extBindParam{extInt8Param(8), extTextParam("eight")}, nil))
	c.extExpectMsg(msgBindComplete)
	c.extSend(msgExecute, extExecuteMessage("pi", 0))
	assertCommandTag(t, []wireMsg{c.extExpectMsg(msgCommandComplete)}, "INSERT 0 1")
	c.extSend(msgSync, nil)
	if _, err := readUntilReady(c.conn); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Mixed result formats: id binary, name text.
	c.extSend(msgBind, extBindMessage("pmix", "sel", []int16{0}, []extBindParam{extTextParam("7")}, []int16{1, 0}))
	c.extExpectMsg(msgBindComplete)
	c.extSend(msgExecute, extExecuteMessage("pmix", 0))
	raw = extParseDataRowRaw(t, c.extExpectMsg(msgDataRow).body)
	if len(raw[0].data) != 8 || string(raw[1].data) != "seven" {
		t.Fatalf("mixed DataRow = %+v", raw)
	}
	c.extExpectMsg(msgCommandComplete)
	c.extSend(msgSync, nil)
	if _, err := readUntilReady(c.conn); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Binary INT4 parameter (OID 23).
	c.extSend(msgParse, extParseMessage("int4", "SELECT id FROM ex WHERE id = $1", []uint32{23}))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgBind, extBindMessage("p4", "int4", []int16{1}, []extBindParam{extInt4Param(8)}, nil))
	c.extExpectMsg(msgBindComplete)
	c.extSend(msgExecute, extExecuteMessage("p4", 0))
	if row := parseDataRow(t, c.extExpectMsg(msgDataRow).body); fmt.Sprint(row) != "[8]" {
		t.Fatalf("int4 row = %v, want [8]", row)
	}
	c.extExpectMsg(msgCommandComplete)
	c.extSend(msgSync, nil)
	if _, err := readUntilReady(c.conn); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}

func TestExtendedTextParametersAndQuoting(t *testing.T) {
	_, addr := startTestServer(t)
	c := dialTestConn(t, addr)
	c.startup(map[string]string{"user": "alice"})
	c.extSimpleSetup(
		"CREATE TABLE people (id INT PRIMARY KEY, name TEXT)",
		"CREATE INDEX people_name ON people (name)",
	)

	c.extSend(msgParse, extParseMessage("ins", "INSERT INTO people VALUES ($1, $2)", []uint32{20, 25}))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgBind, extBindMessage("pi", "ins", []int16{0, 0},
		[]extBindParam{extTextParam("5"), extTextParam("O'Reilly")}, nil))
	c.extExpectMsg(msgBindComplete)
	c.extSend(msgExecute, extExecuteMessage("pi", 0))
	assertCommandTag(t, []wireMsg{c.extExpectMsg(msgCommandComplete)}, "INSERT 0 1")
	c.extSend(msgSync, nil)
	if _, err := readUntilReady(c.conn); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	msgs := c.query("SELECT name FROM people WHERE id = 5")
	c.assertNoError(msgs)
	row := parseDataRow(t, msgOfType(msgs, msgDataRow))
	if fmt.Sprint(row) != "[O'Reilly]" {
		t.Fatalf("stored name = %v, want [O'Reilly]", row)
	}

	// Unspecified OIDs: schema inference reports TEXT for the name column and
	// the numeric-looking value is still quoted as TEXT.
	c.extSend(msgParse, extParseMessage("byname", "SELECT id FROM people WHERE name = $1", nil))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgDescribe, extDescribeMessage('S', "byname"))
	if got := parseParameterDescription(t, c.extExpectMsg(msgParameterDescription).body); len(got) != 1 || got[0] != 25 {
		t.Fatalf("inferred ParameterDescription = %v, want [25]", got)
	}
	c.extExpectMsg(msgRowDescription)
	c.extSend(msgBind, extBindMessage("pn", "byname", nil, []extBindParam{extTextParam("O'Reilly")}, nil))
	c.extExpectMsg(msgBindComplete)
	c.extSend(msgExecute, extExecuteMessage("pn", 0))
	if row := parseDataRow(t, c.extExpectMsg(msgDataRow).body); fmt.Sprint(row) != "[5]" {
		t.Fatalf("row = %v, want [5]", row)
	}
	c.extExpectMsg(msgCommandComplete)
	c.extSend(msgSync, nil)
	if _, err := readUntilReady(c.conn); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}

func TestExtendedInferredTypesAndLimit(t *testing.T) {
	_, addr := startTestServer(t)
	c := dialTestConn(t, addr)
	c.startup(map[string]string{"user": "alice"})
	c.extSimpleSetup(
		"CREATE TABLE inv (id INT PRIMARY KEY, code TEXT)",
		"CREATE INDEX inv_code ON inv (code)",
		"INSERT INTO inv VALUES (1, '123')",
		"INSERT INTO inv VALUES (2, '456')",
	)

	// LIMIT parameters are inferred as INT8.
	c.extSend(msgParse, extParseMessage("lim", "SELECT id FROM inv LIMIT $1", nil))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgDescribe, extDescribeMessage('S', "lim"))
	if got := parseParameterDescription(t, c.extExpectMsg(msgParameterDescription).body); len(got) != 1 || got[0] != 20 {
		t.Fatalf("LIMIT ParameterDescription = %v, want [20]", got)
	}
	c.extExpectMsg(msgRowDescription)
	c.extSend(msgBind, extBindMessage("pl", "lim", nil, []extBindParam{extTextParam("1")}, nil))
	c.extExpectMsg(msgBindComplete)
	c.extSend(msgExecute, extExecuteMessage("pl", 0))
	parseDataRow(t, c.extExpectMsg(msgDataRow).body)
	assertCommandTag(t, []wireMsg{c.extExpectMsg(msgCommandComplete)}, "SELECT 1")
	c.extSend(msgSync, nil)
	if _, err := extReadyNoMessages(t, c); err != nil {
		t.Fatal(err)
	}

	// Numeric text compared with a TEXT column stays text.
	c.extSend(msgParse, extParseMessage("codeq", "SELECT id FROM inv WHERE code = $1", nil))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgBind, extBindMessage("pc", "codeq", nil, []extBindParam{extTextParam("123")}, nil))
	c.extExpectMsg(msgBindComplete)
	c.extSend(msgExecute, extExecuteMessage("pc", 0))
	if row := parseDataRow(t, c.extExpectMsg(msgDataRow).body); fmt.Sprint(row) != "[1]" {
		t.Fatalf("row = %v, want [1]", row)
	}
	c.extExpectMsg(msgCommandComplete)
	c.extSend(msgSync, nil)
	if _, err := readUntilReady(c.conn); err != nil {
		t.Fatal(err)
	}
}

func TestExtendedUnknownParameterTypesFallback(t *testing.T) {
	srv, addr := startTestServer(t)
	_ = srv

	// Create an empty table on a different connection. The second connection
	// cannot infer its schema from data, so it must fall back to flexible
	// parameter rendering.
	creator := dialTestConn(t, addr)
	creator.startup(map[string]string{"user": "alice"})
	creator.extSimpleSetup("CREATE TABLE external (id INT PRIMARY KEY, name TEXT)")

	c := dialTestConn(t, addr)
	c.startup(map[string]string{"user": "alice"})
	c.extSend(msgParse, extParseMessage("ins", "INSERT INTO external VALUES ($1, $2)", nil))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgBind, extBindMessage("pi", "ins", nil,
		[]extBindParam{extTextParam("1"), extTextParam("alice")}, nil))
	c.extExpectMsg(msgBindComplete)
	c.extSend(msgExecute, extExecuteMessage("pi", 0))
	assertCommandTag(t, []wireMsg{c.extExpectMsg(msgCommandComplete)}, "INSERT 0 1")
	c.extSend(msgSync, nil)
	if _, err := readUntilReady(c.conn); err != nil {
		t.Fatal(err)
	}

	msgs := c.query("SELECT id, name FROM external")
	c.assertNoError(msgs)
	row := parseDataRow(t, msgOfType(msgs, msgDataRow))
	if fmt.Sprint(row) != "[1 alice]" {
		t.Fatalf("row = %v, want [1 alice]", row)
	}
}

func TestExtendedDescribeNoDataAndEmptyQuery(t *testing.T) {
	_, addr := startTestServer(t)
	c := dialTestConn(t, addr)
	c.startup(map[string]string{"user": "alice"})

	c.extSend(msgParse, extParseMessage("", "", nil))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgDescribe, extDescribeMessage('S', ""))
	if got := parseParameterDescription(t, c.extExpectMsg(msgParameterDescription).body); len(got) != 0 {
		t.Fatalf("empty query ParameterDescription = %v, want empty", got)
	}
	c.extExpectMsg(msgNoData)
	c.extSend(msgBind, extBindMessage("", "", nil, nil, nil))
	c.extExpectMsg(msgBindComplete)
	c.extSend(msgExecute, extExecuteMessage("", 0))
	c.extExpectMsg(msgEmptyQueryResponse)
	c.extSend(msgSync, nil)
	if _, err := readUntilReady(c.conn); err != nil {
		t.Fatal(err)
	}

	c.extSend(msgParse, extParseMessage("ddl", "CREATE TABLE t2 (id INT PRIMARY KEY)", nil))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgDescribe, extDescribeMessage('S', "ddl"))
	if got := parseParameterDescription(t, c.extExpectMsg(msgParameterDescription).body); len(got) != 0 {
		t.Fatalf("DDL ParameterDescription = %v, want empty", got)
	}
	c.extExpectMsg(msgNoData)
	c.extSend(msgBind, extBindMessage("pddl", "ddl", nil, nil, nil))
	c.extExpectMsg(msgBindComplete)
	c.extSend(msgExecute, extExecuteMessage("pddl", 0))
	assertCommandTag(t, []wireMsg{c.extExpectMsg(msgCommandComplete)}, "OK")
	c.extSend(msgSync, nil)
	if _, err := readUntilReady(c.conn); err != nil {
		t.Fatal(err)
	}

	// DML with a parameter also has NoData, but a non-empty ParameterDescription.
	c.extSend(msgParse, extParseMessage("ins", "INSERT INTO t2 VALUES ($1)", nil))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgDescribe, extDescribeMessage('S', "ins"))
	if got := parseParameterDescription(t, c.extExpectMsg(msgParameterDescription).body); len(got) != 1 || got[0] != 20 {
		t.Fatalf("DML ParameterDescription = %v, want [20]", got)
	}
	c.extExpectMsg(msgNoData)
	c.extSend(msgSync, nil)
	if _, err := readUntilReady(c.conn); err != nil {
		t.Fatal(err)
	}
}

func extReadyNoMessages(t *testing.T, c *testConn) ([]wireMsg, error) {
	t.Helper()
	msgs, err := readUntilReady(c.conn)
	if err != nil {
		return nil, err
	}
	if len(msgs) != 1 || msgs[0].typ != msgReadyForQuery {
		t.Fatalf("expected only ReadyForQuery, got %v", msgTypes(msgs))
	}
	return msgs, nil
}

func TestExtendedErrorRecovery(t *testing.T) {
	_, addr := startTestServer(t)
	c := dialTestConn(t, addr)
	c.startup(map[string]string{"user": "alice"})

	// Syntax error: ErrorResponse only, no ReadyForQuery.
	c.extSend(msgParse, extParseMessage("bad", "SELEC 1", nil))
	c.extExpectError("42601")

	// Until Sync, all messages are ignored.
	c.extSend(msgParse, extParseMessage("ignored", "CREATE TABLE ignored (id INT PRIMARY KEY)", nil))
	c.extSend(msgBind, extBindMessage("p", "ignored", nil, nil, nil))
	c.extSend(msgQuery, append([]byte("CREATE TABLE ignored (id INT PRIMARY KEY)"), 0))
	c.extSend(msgSync, nil)
	msgs, err := readUntilReady(c.conn)
	if err != nil {
		t.Fatalf("read after Sync: %v", err)
	}
	if len(msgs) != 1 || msgs[0].typ != msgReadyForQuery {
		t.Fatalf("messages after recovery Sync = %v, want only ReadyForQuery", msgTypes(msgs))
	}

	// The ignored CREATE TABLE did not run.
	msgs = c.query("CREATE TABLE ignored (id INT PRIMARY KEY)")
	c.assertNoError(msgs)
	assertCommandTag(t, msgs, "OK")

	c.extSimpleSetup("CREATE TABLE t (id INT PRIMARY KEY)")

	// Unknown prepared statement.
	c.extSend(msgBind, extBindMessage("p", "missing", nil, nil, nil))
	c.extExpectError("26000")
	c.extSync()

	// Unknown portal.
	c.extSend(msgExecute, extExecuteMessage("missing", 0))
	c.extExpectError("34000")
	c.extSync()

	// Parameter count mismatch.
	c.extSend(msgParse, extParseMessage("byid", "SELECT id FROM t WHERE id = $1", []uint32{20}))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgBind, extBindMessage("p", "byid", nil, nil, nil))
	c.extExpectError("08P01")
	c.extSync()

	// Execution error: missing table.
	c.extSend(msgParse, extParseMessage("gone", "SELECT * FROM gone WHERE id = $1", []uint32{20}))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgBind, extBindMessage("pg", "gone", nil, []extBindParam{extTextParam("1")}, nil))
	c.extExpectMsg(msgBindComplete)
	c.extSend(msgExecute, extExecuteMessage("pg", 0))
	c.extExpectError("42P01")
	c.extSync()

	// NULL parameters are not supported but are reported cleanly.
	c.extSend(msgBind, extBindMessage("pn", "byid", nil, []extBindParam{extNilParam()}, nil))
	c.extExpectError("0A000")
	c.extSync()

	// The connection remains usable and simple Query still works.
	msgs = c.query("SELECT id FROM t")
	c.assertNoError(msgs)
	assertCommandTag(t, msgs, "SELECT 0")
}

func TestExtendedCloseFlushAndDuplicates(t *testing.T) {
	_, addr := startTestServer(t)
	c := dialTestConn(t, addr)
	c.startup(map[string]string{"user": "alice"})
	c.extSimpleSetup("CREATE TABLE things (id INT PRIMARY KEY)")

	c.extSend(msgParse, extParseMessage("s", "SELECT id FROM things", nil))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgBind, extBindMessage("p", "s", nil, nil, nil))
	c.extExpectMsg(msgBindComplete)
	c.extSend(msgDescribe, extDescribeMessage('P', "p"))
	c.extExpectMsg(msgRowDescription)
	c.extSend(msgClose, extCloseMessage('P', "p"))
	c.extExpectMsg(msgCloseComplete)
	c.extSend(msgExecute, extExecuteMessage("p", 0))
	c.extExpectError("34000")
	c.extSync()

	// Re-bind and use Flush to receive output without Sync.
	c.extSend(msgBind, extBindMessage("p", "s", nil, nil, nil))
	c.extFlushOutput()
	c.extExpectMsg(msgBindComplete)
	c.extSend(msgExecute, extExecuteMessage("p", 0))
	c.extFlushOutput()
	c.extExpectMsg(msgCommandComplete)
	c.extSend(msgSync, nil)
	if _, err := readUntilReady(c.conn); err != nil {
		t.Fatal(err)
	}

	// Close the named statement; binding it afterwards fails.
	c.extSend(msgClose, extCloseMessage('S', "s"))
	c.extExpectMsg(msgCloseComplete)
	c.extSend(msgBind, extBindMessage("p2", "s", nil, nil, nil))
	c.extExpectError("26000")
	c.extSync()

	// Closing an unknown statement/portal is not an error.
	c.extSend(msgClose, extCloseMessage('S', "does-not-exist"))
	c.extExpectMsg(msgCloseComplete)
	c.extSend(msgClose, extCloseMessage('P', "does-not-exist"))
	c.extExpectMsg(msgCloseComplete)

	// Duplicate statement name.
	c.extSend(msgParse, extParseMessage("dup", "SELECT id FROM things", nil))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgParse, extParseMessage("dup", "SELECT id FROM things", nil))
	c.extExpectError("42P05")
	c.extSync()

	// Duplicate portal name.
	c.extSend(msgBind, extBindMessage("dup-portal", "dup", nil, nil, nil))
	c.extExpectMsg(msgBindComplete)
	c.extSend(msgBind, extBindMessage("dup-portal", "dup", nil, nil, nil))
	c.extExpectError("42P03")
	c.extSync()

	// Invalid result-format count is caught at Bind once columns are known.
	c.extSend(msgBind, extBindMessage("badfmt", "dup", nil, nil, []int16{0, 0}))
	c.extExpectError("08P01")
	c.extSync()

	// Invalid format code.
	c.extSend(msgBind, extBindMessage("badcode", "dup", []int16{2}, []extBindParam{extTextParam("x")}, nil))
	c.extExpectError("08P01")
	c.extSync()

	// Parse with more type OIDs than parameters.
	c.extSend(msgParse, extParseMessage("extra", "SELECT id FROM things", []uint32{20}))
	c.extExpectError("42P02")
	c.extSync()

	// Malformed Parse body.
	c.extSend(msgParse, []byte{0, 0})
	c.extExpectError("08P01")
	c.extSync()
}

func TestExtendedNoRowDescriptionOnExecute(t *testing.T) {
	_, addr := startTestServer(t)
	c := dialTestConn(t, addr)
	c.startup(map[string]string{"user": "alice"})
	c.extSimpleSetup(
		"CREATE TABLE plain (id INT PRIMARY KEY)",
		"INSERT INTO plain VALUES (1)",
	)
	c.extSend(msgParse, extParseMessage("s", "SELECT id FROM plain", nil))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgBind, extBindMessage("p", "s", nil, nil, nil))
	c.extExpectMsg(msgBindComplete)
	c.extSend(msgExecute, extExecuteMessage("p", 0))
	if m := c.extReadMsg(); m.typ != msgDataRow {
		t.Fatalf("first Execute response = %q, want D (RowDescription must not be repeated by Execute)", m.typ)
	}
	c.extSend(msgSync, nil)
	if _, err := readUntilReady(c.conn); err != nil {
		t.Fatal(err)
	}
}

func TestExtendedUpdateDeleteParameters(t *testing.T) {
	_, addr := startTestServer(t)
	c := dialTestConn(t, addr)
	c.startup(map[string]string{"user": "alice"})
	c.extSimpleSetup(
		"CREATE TABLE upd (id INT PRIMARY KEY, name TEXT)",
		"INSERT INTO upd VALUES (1, 'a')",
		"INSERT INTO upd VALUES (2, 'b')",
	)

	c.extSend(msgParse, extParseMessage("upd", "UPDATE upd SET name = $1 WHERE id = $2", nil))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgDescribe, extDescribeMessage('S', "upd"))
	if got := parseParameterDescription(t, c.extExpectMsg(msgParameterDescription).body); len(got) != 2 || got[0] != 25 || got[1] != 20 {
		t.Fatalf("UPDATE ParameterDescription = %v, want [25 20]", got)
	}
	c.extExpectMsg(msgNoData)
	c.extSend(msgBind, extBindMessage("pu", "upd", nil, []extBindParam{extTextParam("z"), extTextParam("1")}, nil))
	c.extExpectMsg(msgBindComplete)
	c.extSend(msgExecute, extExecuteMessage("pu", 0))
	assertCommandTag(t, []wireMsg{c.extExpectMsg(msgCommandComplete)}, "UPDATE 1")
	c.extSend(msgSync, nil)
	if _, err := readUntilReady(c.conn); err != nil {
		t.Fatal(err)
	}

	c.extSend(msgParse, extParseMessage("del", "DELETE FROM upd WHERE id = $1", nil))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgBind, extBindMessage("pd", "del", nil, []extBindParam{extTextParam("2")}, nil))
	c.extExpectMsg(msgBindComplete)
	c.extSend(msgExecute, extExecuteMessage("pd", 0))
	assertCommandTag(t, []wireMsg{c.extExpectMsg(msgCommandComplete)}, "DELETE 1")
	c.extSend(msgSync, nil)
	if _, err := readUntilReady(c.conn); err != nil {
		t.Fatal(err)
	}

	msgs := c.query("SELECT id, name FROM upd")
	c.assertNoError(msgs)
	rows := make([]string, 0, 1)
	for _, m := range msgs {
		if m.typ == msgDataRow {
			rows = append(rows, fmt.Sprint(parseDataRow(t, m.body)))
		}
	}
	if strings.Join(rows, ",") != "[1 z]" {
		t.Fatalf("rows = %v, want [[1 z]]", rows)
	}
}

func TestExtendedParameterConversionErrors(t *testing.T) {
	_, addr := startTestServer(t)
	c := dialTestConn(t, addr)
	c.startup(map[string]string{"user": "alice"})
	c.extSimpleSetup("CREATE TABLE perr (id INT PRIMARY KEY)")

	c.extSend(msgParse, extParseMessage("", "SELECT id FROM perr WHERE id = $1", []uint32{23}))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgBind, extBindMessage("p", "", nil, []extBindParam{extTextParam("99999999999")}, nil))
	c.extExpectError("22003")
	c.extSync()

	c.extSend(msgParse, extParseMessage("", "SELECT id FROM perr WHERE id = $1", []uint32{20}))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgBind, extBindMessage("p", "", []int16{1}, []extBindParam{extInt4Param(1)}, nil))
	c.extExpectError("22P03")
	c.extSync()

	c.extSend(msgParse, extParseMessage("", "SELECT id FROM perr WHERE id = $1", []uint32{20}))
	c.extExpectMsg(msgParseComplete)
	c.extSend(msgBind, extBindMessage("p", "", nil, []extBindParam{extTextParam("abc")}, nil))
	c.extExpectError("22P02")
	c.extSync()

	c.extSend(msgBind, extBindMessage("p", "", nil, []extBindParam{extNilParam()}, nil))
	c.extExpectError("0A000")
	c.extSync()

	// The connection is still usable.
	msgs := c.query("SELECT id FROM perr")
	c.assertNoError(msgs)
	assertCommandTag(t, msgs, "SELECT 0")
}
