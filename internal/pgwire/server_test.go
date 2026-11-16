package pgwire

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"distributed-db/internal/sqlengine"
)

// fakeClient is a minimal in-memory sqlengine.Client, copied from the engine
// tests so pgwire can be tested without a KV cluster. Begin holds the lock for
// the lifetime of the transaction, which serializes concurrent transactions.
type fakeClient struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newFakeClient() *fakeClient { return &fakeClient{data: make(map[string][]byte)} }

func (c *fakeClient) Begin(context.Context) (sqlengine.Txn, error) {
	c.mu.Lock()
	staged := make(map[string][]byte, len(c.data))
	for k, v := range c.data {
		staged[k] = append([]byte(nil), v...)
	}
	return &fakeTxn{base: c, data: staged}, nil
}

type fakeTxn struct {
	base *fakeClient
	data map[string][]byte
	done bool
}

func (t *fakeTxn) Get(_ context.Context, key []byte) ([]byte, error) {
	v, ok := t.data[string(key)]
	if !ok {
		return nil, sqlengine.ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

func (t *fakeTxn) Set(key, value []byte) { t.data[string(key)] = append([]byte(nil), value...) }
func (t *fakeTxn) Delete(key []byte)     { delete(t.data, string(key)) }

func (t *fakeTxn) Scan(_ context.Context, start, end []byte, limit int) ([]sqlengine.KV, error) {
	keys := make([]string, 0, len(t.data))
	for k := range t.data {
		kb := []byte(k)
		if string(kb) >= string(start) && (end == nil || string(kb) < string(end)) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	out := make([]sqlengine.KV, 0, len(keys))
	for _, k := range keys {
		out = append(out, sqlengine.KV{Key: []byte(k), Value: append([]byte(nil), t.data[k]...)})
	}
	return out, nil
}

func (t *fakeTxn) Commit(context.Context) error {
	if t.done {
		return nil
	}
	t.done = true
	t.base.data = t.data
	t.base.mu.Unlock()
	return nil
}

func (t *fakeTxn) Rollback(context.Context) error {
	if t.done {
		return nil
	}
	t.done = true
	t.base.mu.Unlock()
	return nil
}

var (
	_ sqlengine.Client = (*fakeClient)(nil)
	_ sqlengine.Txn    = (*fakeTxn)(nil)
)

func startTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	engine := sqlengine.New(newFakeClient())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(engine, logger)
	addr, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	return srv, addr
}

// --- wire helpers -----------------------------------------------------------

type wireMsg struct {
	typ  byte
	body []byte
}

func makeStartupPacket(code uint32, params map[string]string) []byte {
	var body []byte
	body = appendUint32(body, code)
	if code == protocolVersion3 {
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			body = appendCString(body, k)
			body = appendCString(body, params[k])
		}
		body = append(body, 0)
	}
	out := make([]byte, 4, 4+len(body))
	binary.BigEndian.PutUint32(out, uint32(4+len(body)))
	return append(out, body...)
}

// startupError preserves the SQLSTATE from an ErrorResponse so tests can
// assert both the code and the message.
type startupError struct {
	code    string
	message string
}

func (e *startupError) Error() string {
	return fmt.Sprintf("startup error: %s: %s", e.code, e.message)
}

func startupConn(conn net.Conn, params map[string]string) (map[string]string, error) {
	return startupConnWithPassword(conn, params, "", false)
}

// startupConnAuthed performs startup and answers a cleartext password
// AuthenticationRequest with password.
func startupConnAuthed(conn net.Conn, params map[string]string, password string) (map[string]string, error) {
	return startupConnWithPassword(conn, params, password, true)
}

func startupConnWithPassword(conn net.Conn, params map[string]string, password string, passwordAllowed bool) (map[string]string, error) {
	for _, req := range []uint32{gssEncRequestCode, sslRequestCode} {
		if _, err := conn.Write(makeStartupPacket(req, nil)); err != nil {
			return nil, err
		}
		var b [1]byte
		if _, err := io.ReadFull(conn, b[:]); err != nil {
			return nil, err
		}
		if b[0] != 'N' {
			return nil, fmt.Errorf("negotiation response = %q, want 'N'", b[0])
		}
	}
	if _, err := conn.Write(makeStartupPacket(protocolVersion3, params)); err != nil {
		return nil, err
	}

	statuses := make(map[string]string)
	for {
		typ, body, err := readMessage(conn)
		if err != nil {
			return nil, err
		}
		switch typ {
		case msgAuthenticationOk:
			if len(body) != 4 {
				return nil, fmt.Errorf("Authentication request body = %x, want 4 bytes", body)
			}
			switch code := binary.BigEndian.Uint32(body); code {
			case 0:
				// AuthenticationOk.
			case authCleartextPassword:
				if !passwordAllowed {
					return nil, errors.New("server requested a cleartext password but no password was supplied")
				}
				if err := writeMessage(conn, msgPasswordMessage, appendCString(nil, password)); err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("unsupported authentication request code %d", code)
			}
		case msgParameterStatus:
			key, rest, ok := readCString(body)
			if !ok {
				return nil, errors.New("ParameterStatus: missing key terminator")
			}
			value, _, ok := readCString(rest)
			if !ok {
				return nil, errors.New("ParameterStatus: missing value terminator")
			}
			statuses[key] = value
		case msgBackendKeyData:
			if len(body) != 8 {
				return nil, fmt.Errorf("BackendKeyData length = %d, want 8", len(body))
			}
		case msgReadyForQuery:
			if len(body) != 1 || body[0] != 'I' {
				return nil, fmt.Errorf("ReadyForQuery = %x, want I", body)
			}
			return statuses, nil
		case msgErrorResponse:
			fields := errorFields(body)
			return nil, &startupError{code: fields['C'], message: fields['M']}
		default:
			return nil, fmt.Errorf("unexpected startup message %q", typ)
		}
	}
}

func queryConn(conn net.Conn, q string) error {
	body := append([]byte(q), 0)
	return writeMessage(conn, msgQuery, body)
}

func readUntilReady(conn net.Conn) ([]wireMsg, error) {
	var msgs []wireMsg
	for i := 0; i < 10000; i++ {
		typ, body, err := readMessage(conn)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, wireMsg{typ: typ, body: body})
		if typ == msgReadyForQuery {
			if len(body) != 1 || body[0] != 'I' {
				return nil, fmt.Errorf("ReadyForQuery = %x, want I", body)
			}
			return msgs, nil
		}
	}
	return nil, errors.New("too many response messages")
}

func errorFields(body []byte) map[byte]string {
	fields := make(map[byte]string)
	for len(body) > 0 {
		code := body[0]
		body = body[1:]
		if code == 0 {
			break
		}
		value, rest, ok := readCString(body)
		if !ok {
			break
		}
		fields[code] = value
		body = rest
	}
	return fields
}

func commandTagOf(t *testing.T, body []byte) string {
	t.Helper()
	tag, rest, ok := readCString(body)
	if !ok || len(rest) != 0 {
		t.Fatalf("malformed CommandComplete body %x", body)
	}
	return tag
}

type fieldDescription struct {
	name     string
	tableOID uint32
	attr     int16
	typeOID  uint32
	typeSize int16
	typeMod  int32
	format   int16
}

func parseRowDescription(t *testing.T, body []byte) []fieldDescription {
	t.Helper()
	if len(body) < 2 {
		t.Fatalf("RowDescription too short: %x", body)
	}
	n := int(int16(binary.BigEndian.Uint16(body[:2])))
	rest := body[2:]
	fields := make([]fieldDescription, 0, n)
	for i := 0; i < n; i++ {
		name, r, ok := readCString(rest)
		if !ok {
			t.Fatalf("RowDescription field %d: bad name", i)
		}
		rest = r
		if len(rest) < 18 {
			t.Fatalf("RowDescription field %d: %d bytes remain, want 18", i, len(rest))
		}
		fields = append(fields, fieldDescription{
			name:     name,
			tableOID: binary.BigEndian.Uint32(rest[0:4]),
			attr:     int16(binary.BigEndian.Uint16(rest[4:6])),
			typeOID:  binary.BigEndian.Uint32(rest[6:10]),
			typeSize: int16(binary.BigEndian.Uint16(rest[10:12])),
			typeMod:  int32(binary.BigEndian.Uint32(rest[12:16])),
			format:   int16(binary.BigEndian.Uint16(rest[16:18])),
		})
		rest = rest[18:]
	}
	return fields
}

// parseDataRow returns string values, with nil for SQL NULL.
func parseDataRow(t *testing.T, body []byte) []any {
	t.Helper()
	if len(body) < 2 {
		t.Fatalf("DataRow too short: %x", body)
	}
	n := int(int16(binary.BigEndian.Uint16(body[:2])))
	rest := body[2:]
	values := make([]any, n)
	for i := range values {
		if len(rest) < 4 {
			t.Fatalf("DataRow column %d: missing length", i)
		}
		l := int32(binary.BigEndian.Uint32(rest[:4]))
		rest = rest[4:]
		if l == -1 {
			values[i] = nil
			continue
		}
		if l < 0 || int(l) > len(rest) {
			t.Fatalf("DataRow column %d: invalid length %d (have %d)", i, l, len(rest))
		}
		values[i] = string(rest[:l])
		rest = rest[l:]
	}
	if len(rest) != 0 {
		t.Fatalf("DataRow has %d trailing bytes", len(rest))
	}
	return values
}

func msgOfType(msgs []wireMsg, typ byte) []byte {
	for _, m := range msgs {
		if m.typ == typ {
			return m.body
		}
	}
	return nil
}

func assertCommandTag(t *testing.T, msgs []wireMsg, want string) {
	t.Helper()
	body := msgOfType(msgs, msgCommandComplete)
	if body == nil {
		t.Fatalf("no CommandComplete in %v", msgTypes(msgs))
	}
	if got := commandTagOf(t, body); got != want {
		t.Fatalf("CommandComplete = %q, want %q", got, want)
	}
}

func msgTypes(msgs []wireMsg) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = string(m.typ)
	}
	return out
}

type testConn struct {
	t    *testing.T
	conn net.Conn
}

func dialTestConn(t *testing.T, addr string) *testConn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &testConn{t: t, conn: conn}
}

func (c *testConn) startup(params map[string]string) map[string]string {
	c.t.Helper()
	statuses, err := startupConn(c.conn, params)
	if err != nil {
		c.t.Fatalf("startup: %v", err)
	}
	return statuses
}

func (c *testConn) query(q string) []wireMsg {
	c.t.Helper()
	if err := queryConn(c.conn, q); err != nil {
		c.t.Fatalf("send query %q: %v", q, err)
	}
	msgs, err := readUntilReady(c.conn)
	if err != nil {
		c.t.Fatalf("read response to %q: %v", q, err)
	}
	return msgs
}

func (c *testConn) assertNoError(msgs []wireMsg) {
	c.t.Helper()
	if body := msgOfType(msgs, msgErrorResponse); body != nil {
		fields := errorFields(body)
		c.t.Fatalf("unexpected ErrorResponse: code=%s message=%s", fields['C'], fields['M'])
	}
}

// --- tests ------------------------------------------------------------------

func TestStartupAuthAndReady(t *testing.T) {
	srv, addr := startTestServer(t)
	if got := srv.Addr(); got != addr {
		t.Fatalf("Addr() = %q, want %q", got, addr)
	}

	c := dialTestConn(t, addr)
	statuses := c.startup(map[string]string{
		"user":             "alice",
		"database":         "testdb",
		"application_name": "pgwire-test",
	})
	want := map[string]string{
		"server_version":  "14.0",
		"client_encoding": "UTF8",
		"DateStyle":       "ISO",
	}
	if len(statuses) != len(want) {
		t.Fatalf("ParameterStatus count = %d (%v), want %d", len(statuses), statuses, len(want))
	}
	for k, v := range want {
		if statuses[k] != v {
			t.Errorf("ParameterStatus[%q] = %q, want %q", k, statuses[k], v)
		}
	}

	// Terminate must close the connection without a response.
	if err := writeMessage(c.conn, msgTerminate, nil); err != nil {
		t.Fatalf("send Terminate: %v", err)
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var b [1]byte
	if _, err := c.conn.Read(b[:]); err == nil {
		t.Fatal("connection is still open after Terminate")
	}
}

func TestSimpleQueryLifecycle(t *testing.T) {
	_, addr := startTestServer(t)
	c := dialTestConn(t, addr)
	c.startup(map[string]string{"user": "alice", "database": "testdb"})

	msgs := c.query("CREATE TABLE users (id INT PRIMARY KEY, name TEXT)")
	c.assertNoError(msgs)
	assertCommandTag(t, msgs, "OK")

	msgs = c.query("INSERT INTO users VALUES (1, 'alice')")
	c.assertNoError(msgs)
	assertCommandTag(t, msgs, "INSERT 0 1")

	msgs = c.query("INSERT INTO users VALUES (2, 'bob')")
	c.assertNoError(msgs)
	assertCommandTag(t, msgs, "INSERT 0 1")

	msgs = c.query("SELECT id, name FROM users")
	c.assertNoError(msgs)
	if got := msgTypes(msgs); strings.Join(got, "") != "TDDCZ" {
		t.Fatalf("SELECT messages = %v, want T D D C Z", got)
	}
	fields := parseRowDescription(t, msgOfType(msgs, msgRowDescription))
	if len(fields) != 2 {
		t.Fatalf("RowDescription fields = %d, want 2", len(fields))
	}
	if fields[0].name != "id" || fields[0].typeOID != 20 || fields[0].typeSize != 8 {
		t.Fatalf("id field = %+v, want name=id oid=20 size=8", fields[0])
	}
	if fields[1].name != "name" || fields[1].typeOID != 25 || fields[1].typeSize != -1 {
		t.Fatalf("name field = %+v, want name=name oid=25 size=-1", fields[1])
	}
	for i, f := range fields {
		if f.tableOID != 0 || f.attr != 0 || f.format != 0 || f.typeMod != -1 {
			t.Fatalf("field %d metadata = %+v, want tableOID=0 attr=0 format=0 typmod=-1", i, f)
		}
	}
	var rows [][]any
	for _, m := range msgs {
		if m.typ == msgDataRow {
			rows = append(rows, parseDataRow(t, m.body))
		}
	}
	if len(rows) != 2 {
		t.Fatalf("DataRow count = %d, want 2", len(rows))
	}
	if fmt.Sprint(rows[0]) != "[1 alice]" || fmt.Sprint(rows[1]) != "[2 bob]" {
		t.Fatalf("rows = %v, want [[1 alice] [2 bob]]", rows)
	}
	assertCommandTag(t, msgs, "SELECT 2")

	msgs = c.query("UPDATE users SET name = 'carol' WHERE id = 1")
	c.assertNoError(msgs)
	assertCommandTag(t, msgs, "UPDATE 1")

	msgs = c.query("SELECT name FROM users WHERE id = 1")
	c.assertNoError(msgs)
	row := parseDataRow(t, msgOfType(msgs, msgDataRow))
	if len(row) != 1 || row[0] != "carol" {
		t.Fatalf("updated row = %v, want [carol]", row)
	}
	assertCommandTag(t, msgs, "SELECT 1")

	msgs = c.query("DELETE FROM users WHERE id = 2")
	c.assertNoError(msgs)
	assertCommandTag(t, msgs, "DELETE 1")

	msgs = c.query("SELECT * FROM users")
	c.assertNoError(msgs)
	assertCommandTag(t, msgs, "SELECT 1")
}

func TestEmptyAndNullRows(t *testing.T) {
	_, addr := startTestServer(t)
	c := dialTestConn(t, addr)
	c.startup(map[string]string{"user": "alice"})

	msgs := c.query("CREATE TABLE t (id INT PRIMARY KEY)")
	c.assertNoError(msgs)

	// Empty result set: RowDescription, no DataRow, SELECT 0.
	msgs = c.query("SELECT * FROM t")
	c.assertNoError(msgs)
	if got := msgTypes(msgs); strings.Join(got, "") != "TCZ" {
		t.Fatalf("empty SELECT messages = %v, want T C Z", got)
	}
	assertCommandTag(t, msgs, "SELECT 0")

	// SUM over no rows yields a single SQL NULL (DataRow length -1).
	msgs = c.query("SELECT SUM(id) FROM t")
	c.assertNoError(msgs)
	if got := msgTypes(msgs); strings.Join(got, "") != "TDCZ" {
		t.Fatalf("NULL SELECT messages = %v, want T D C Z", got)
	}
	row := parseDataRow(t, msgOfType(msgs, msgDataRow))
	if len(row) != 1 || row[0] != nil {
		t.Fatalf("SUM row = %#v, want one nil value", row)
	}
	assertCommandTag(t, msgs, "SELECT 1")

	// An empty Query message is acknowledged with EmptyQueryResponse.
	msgs = c.query("")
	c.assertNoError(msgs)
	if got := msgTypes(msgs); strings.Join(got, "") != "IZ" {
		t.Fatalf("empty query messages = %v, want I Z", got)
	}
}

func TestErrorsKeepConnectionUsable(t *testing.T) {
	_, addr := startTestServer(t)
	c := dialTestConn(t, addr)
	c.startup(map[string]string{"user": "alice"})

	msgs := c.query("SELEC 1")
	if len(msgs) != 2 || msgs[0].typ != msgErrorResponse || msgs[1].typ != msgReadyForQuery {
		t.Fatalf("syntax error messages = %v, want E Z", msgTypes(msgs))
	}
	fields := errorFields(msgs[0].body)
	if fields['C'] != "42601" {
		t.Fatalf("syntax SQLSTATE = %q, want 42601", fields['C'])
	}
	if fields['M'] == "" {
		t.Fatal("syntax error message is empty")
	}

	msgs = c.query("SELECT * FROM missing")
	if len(msgs) != 2 || msgs[0].typ != msgErrorResponse || msgs[1].typ != msgReadyForQuery {
		t.Fatalf("missing table messages = %v, want E Z", msgTypes(msgs))
	}
	fields = errorFields(msgs[0].body)
	if fields['C'] != "42P01" {
		t.Fatalf("missing table SQLSTATE = %q, want 42P01", fields['C'])
	}
	if !strings.Contains(fields['M'], "table not found") {
		t.Fatalf("missing table message = %q, want it to mention table not found", fields['M'])
	}

	// The connection must still execute statements after errors.
	msgs = c.query("CREATE TABLE after_error (id INT PRIMARY KEY)")
	c.assertNoError(msgs)
	assertCommandTag(t, msgs, "OK")

	// Multi-statement strings are rejected by the single-statement engine.
	msgs = c.query("SELECT * FROM after_error; SELECT * FROM after_error")
	if body := msgOfType(msgs, msgErrorResponse); body == nil {
		t.Fatal("multi-statement query did not produce an error")
	} else if fields := errorFields(body); fields['C'] != "42601" {
		t.Fatalf("multi-statement SQLSTATE = %q, want 42601", fields['C'])
	}
}

func TestUnknownMessageGetsErrorAndReady(t *testing.T) {
	_, addr := startTestServer(t)
	c := dialTestConn(t, addr)
	c.startup(map[string]string{"user": "alice"})

	// 'Y' is not a PostgreSQL frontend message type.
	if err := writeMessage(c.conn, 'Y', []byte{0, 0, 0, 0}); err != nil {
		t.Fatalf("send unknown message: %v", err)
	}
	msgs, err := readUntilReady(c.conn)
	if err != nil {
		t.Fatalf("read unknown-message response: %v", err)
	}
	if len(msgs) != 2 || msgs[0].typ != msgErrorResponse {
		t.Fatalf("unknown message response = %v, want E Z", msgTypes(msgs))
	}
	fields := errorFields(msgs[0].body)
	if fields['C'] != "08P01" {
		t.Fatalf("unknown message SQLSTATE = %q, want 08P01", fields['C'])
	}

	msgs = c.query("CREATE TABLE still_alive (id INT PRIMARY KEY)")
	c.assertNoError(msgs)
	assertCommandTag(t, msgs, "OK")
}

func TestMalformedMessageLengthCloses(t *testing.T) {
	_, addr := startTestServer(t)
	c := dialTestConn(t, addr)
	c.startup(map[string]string{"user": "alice"})

	// A message length below 4 is a protocol violation.
	if _, err := c.conn.Write([]byte{'Q', 0, 0, 0, 3}); err != nil {
		t.Fatalf("write malformed message: %v", err)
	}
	typ, body, err := readMessage(c.conn)
	if err != nil {
		t.Fatalf("read malformed-message response: %v", err)
	}
	if typ != msgErrorResponse {
		t.Fatalf("malformed-message response type = %q, want E", typ)
	}
	if fields := errorFields(body); fields['C'] != "08P01" {
		t.Fatalf("malformed-message SQLSTATE = %q, want 08P01", fields['C'])
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var b [1]byte
	if _, err := c.conn.Read(b[:]); err == nil {
		t.Fatal("connection is still open after a malformed frame")
	}
}

func TestStopAndLifecycle(t *testing.T) {
	srv, addr := startTestServer(t)
	if addr == "" {
		t.Fatal("empty listen address")
	}

	if _, err := srv.Start("127.0.0.1:0"); err == nil {
		t.Fatal("second Start succeeded, want error")
	}
	srv.Stop()
	conn, err := net.DialTimeout("tcp", srv.Addr(), 500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("dial succeeded after Stop")
	}
	srv.Stop() // idempotent
	if _, err := srv.Start("127.0.0.1:0"); err == nil {
		t.Fatal("Start after Stop succeeded, want error")
	}
}

func TestConcurrentClients(t *testing.T) {
	_, addr := startTestServer(t)
	const clients = 8

	errs := make(chan error, clients)
	for i := 0; i < clients; i++ {
		go func(i int) {
			errs <- runClientSession(addr, fmt.Sprintf("t%d", i))
		}(i)
	}
	for i := 0; i < clients; i++ {
		if err := <-errs; err != nil {
			t.Errorf("client session: %v", err)
		}
	}
}

// runClientSession is a t-free client used by the concurrency test.
func runClientSession(addr, table string) error {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))

	if _, err := startupConn(conn, map[string]string{"user": "u", "database": "d"}); err != nil {
		return fmt.Errorf("startup: %w", err)
	}
	steps := []struct {
		query  string
		tag    string
		values []any
	}{
		{fmt.Sprintf("CREATE TABLE %s (id INT PRIMARY KEY)", table), "OK", nil},
		{fmt.Sprintf("INSERT INTO %s VALUES (1)", table), "INSERT 0 1", nil},
		{fmt.Sprintf("SELECT id FROM %s", table), "SELECT 1", []any{"1"}},
	}
	for _, step := range steps {
		if err := queryConn(conn, step.query); err != nil {
			return err
		}
		msgs, err := readUntilReady(conn)
		if err != nil {
			return err
		}
		if body := msgOfType(msgs, msgErrorResponse); body != nil {
			fields := errorFields(body)
			return fmt.Errorf("%s: %s: %s", step.query, fields['C'], fields['M'])
		}
		if body := msgOfType(msgs, msgCommandComplete); body == nil {
			return fmt.Errorf("%s: no CommandComplete", step.query)
		} else if tag, _, _ := readCString(body); tag != step.tag {
			return fmt.Errorf("%s: tag %q, want %q", step.query, tag, step.tag)
		}
		if step.values != nil {
			body := msgOfType(msgs, msgDataRow)
			if body == nil {
				return fmt.Errorf("%s: no DataRow", step.query)
			}
			row, err := parseDataRowErr(body)
			if err != nil {
				return err
			}
			if fmt.Sprint(row) != fmt.Sprint(step.values) {
				return fmt.Errorf("%s: row %v, want %v", step.query, row, step.values)
			}
		}
	}
	return nil
}

func parseDataRowErr(body []byte) ([]any, error) {
	if len(body) < 2 {
		return nil, errors.New("DataRow too short")
	}
	n := int(int16(binary.BigEndian.Uint16(body[:2])))
	rest := body[2:]
	values := make([]any, n)
	for i := range values {
		if len(rest) < 4 {
			return nil, errors.New("DataRow missing length")
		}
		l := int32(binary.BigEndian.Uint32(rest[:4]))
		rest = rest[4:]
		if l == -1 {
			values[i] = nil
			continue
		}
		if l < 0 || int(l) > len(rest) {
			return nil, fmt.Errorf("DataRow invalid length %d", l)
		}
		values[i] = string(rest[:l])
		rest = rest[l:]
	}
	return values, nil
}
