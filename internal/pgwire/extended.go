package pgwire

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"distributed-db/internal/sqlengine"
	"distributed-db/internal/sqlparser"
)

const (
	// PostgreSQL allows at most 65535 parameters per statement. Keeping the
	// bound below the int16 limit also keeps the sentinel arithmetic below
	// 32-bit int limits for the parser.
	maxStatementParams = 32767
	// paramSentinelBase is substituted for $N while validating a query with
	// sqlparser. The resulting AST lets us infer parameter and result types
	// without exposing a parameter API in the engine.
	paramSentinelBase int64 = 2_000_000_000
	// maxExecuteAttempts bounds the (rare) retry loop used when an
	// unspecified parameter type can be rendered either as an integer or as a
	// quoted string.
	maxExecuteAttempts = 256
)

// wireError is a protocol-level error with an explicit SQLSTATE.
type wireError struct {
	code string
	msg  string
}

func (e *wireError) Error() string { return e.msg }

func wireErrorf(code, format string, args ...any) *wireError {
	return &wireError{code: code, msg: fmt.Sprintf(format, args...)}
}

func asWireError(err error) *wireError {
	var pe *wireError
	if errors.As(err, &pe) {
		return pe
	}
	return &wireError{code: sqlState(err), msg: err.Error()}
}

type paramKind uint8

const (
	kindUnknown paramKind = iota
	kindInt
	kindText
)

func oidKind(oid uint32) paramKind {
	switch oid {
	case 20, 21, 23, 26: // int8, int2, int4, oid
		return kindInt
	case 25, 1042, 1043, 19, 705: // text, bpchar, varchar, name, unknown
		return kindText
	default:
		return kindUnknown
	}
}

func oidForKind(kind paramKind) uint32 {
	if kind == kindInt {
		return oidInt8
	}
	return oidText
}

func kindForSchemaType(typ string) paramKind {
	if strings.EqualFold(typ, "INT") {
		return kindInt
	}
	if strings.EqualFold(typ, "TEXT") {
		return kindText
	}
	return kindUnknown
}

func oidForSchemaType(typ string) (uint32, int16) {
	if strings.EqualFold(typ, "INT") {
		return oidInt8, 8
	}
	return oidText, -1
}

// placeholder records one $N occurrence in a query string.
type placeholder struct {
	start int
	end   int
	index int // 1-based
}

// paramSpec describes how one statement parameter is reported to the client
// and decoded on Bind.
type paramSpec struct {
	oid       uint32 // OID reported in ParameterDescription
	provided  bool   // client supplied a non-zero OID in Parse
	kind      paramKind
	confident bool // type was supplied by the client or inferred from schema
}

// preparedStatement is a parsed statement. Named statements live for the
// connection; the unnamed statement is replaced by the next Parse.
type preparedStatement struct {
	name         string
	query        string
	placeholders []placeholder
	specs        []paramSpec
	paramOIDs    []uint32
	ast          sqlparser.Statement
	isSelect     bool
	isEmpty      bool
	resolved     []paramKind
	meta         []columnDesc
	metaReady    bool
}

// boundParam is one parameter value decoded from Bind. kindUnknown means the
// type was unspecified and not confidently inferable; the executor then tries
// the allowed renderings.
type boundParam struct {
	kind      paramKind
	intVal    int64
	textVal   string
	allowInt  bool
	allowText bool
	preferInt bool
}

func (p boundParam) kinds() []paramKind {
	switch p.kind {
	case kindInt:
		return []paramKind{kindInt}
	case kindText:
		return []paramKind{kindText}
	}
	out := make([]paramKind, 0, 2)
	if p.preferInt {
		if p.allowInt {
			out = append(out, kindInt)
		}
		if p.allowText {
			out = append(out, kindText)
		}
	} else {
		if p.allowText {
			out = append(out, kindText)
		}
		if p.allowInt {
			out = append(out, kindInt)
		}
	}
	return out
}

// portal is a bound statement. A portal keeps result rows between Execute
// messages so a row limit can suspend and later resume it.
type portal struct {
	name          string
	stmt          *preparedStatement
	params        []boundParam
	resultFormats []int16
	result        *sqlengine.Result
	meta          []columnDesc
	tag           string
	next          int
	done          bool
}

// connSession is the per-connection extended-protocol state.
type connSession struct {
	srv *Server
	bw  *bufio.Writer

	statements    map[string]*preparedStatement
	portals       map[string]*portal
	schemas       map[string]tableSchema
	extendedError bool
}

func newConnSession(srv *Server, bw *bufio.Writer) *connSession {
	return &connSession{
		srv:        srv,
		bw:         bw,
		statements: make(map[string]*preparedStatement),
		portals:    make(map[string]*portal),
		schemas:    make(map[string]tableSchema),
	}
}

func (sess *connSession) flush() error { return sess.bw.Flush() }

// fail writes ErrorResponse, enters extended-protocol error state (messages
// are ignored until Sync), and flushes so the client can observe the error.
func (sess *connSession) fail(code, message string) error {
	if err := writeError(sess.bw, code, message); err != nil {
		return err
	}
	sess.extendedError = true
	return sess.bw.Flush()
}

func (sess *connSession) failPG(pe *wireError) error { return sess.fail(pe.code, pe.msg) }

func (sess *connSession) failErr(err error) error {
	pe := asWireError(err)
	return sess.fail(pe.code, pe.msg)
}

// --- message body decoding --------------------------------------------------

type bodyReader struct {
	buf []byte
	pos int
}

func (r *bodyReader) remaining() int { return len(r.buf) - r.pos }

func (r *bodyReader) cstring(what string) (string, *wireError) {
	i := bytes.IndexByte(r.buf[r.pos:], 0)
	if i < 0 {
		return "", wireErrorf("08P01", "pgwire: malformed %s: missing string terminator", what)
	}
	s := string(r.buf[r.pos : r.pos+i])
	r.pos += i + 1
	return s, nil
}

func (r *bodyReader) int16(what string) (int16, *wireError) {
	if r.remaining() < 2 {
		return 0, wireErrorf("08P01", "pgwire: malformed %s: missing int16", what)
	}
	v := int16(binary.BigEndian.Uint16(r.buf[r.pos:]))
	r.pos += 2
	return v, nil
}

func (r *bodyReader) int32(what string) (int32, *wireError) {
	if r.remaining() < 4 {
		return 0, wireErrorf("08P01", "pgwire: malformed %s: missing int32", what)
	}
	v := int32(binary.BigEndian.Uint32(r.buf[r.pos:]))
	r.pos += 4
	return v, nil
}

func (r *bodyReader) bytesN(n int, what string) ([]byte, *wireError) {
	if n < 0 || r.remaining() < n {
		return nil, wireErrorf("08P01", "pgwire: malformed %s: need %d bytes, have %d", what, n, r.remaining())
	}
	b := r.buf[r.pos : r.pos+n]
	r.pos += n
	return b, nil
}

func (r *bodyReader) finish(what string) *wireError {
	if r.remaining() != 0 {
		return wireErrorf("08P01", "pgwire: malformed %s: %d trailing bytes", what, r.remaining())
	}
	return nil
}

func decodeParse(body []byte) (name, query string, oids []uint32, perr *wireError) {
	r := &bodyReader{buf: body}
	if name, perr = r.cstring("Parse statement name"); perr != nil {
		return
	}
	if query, perr = r.cstring("Parse query"); perr != nil {
		return
	}
	n, perr := r.int16("Parse parameter count")
	if perr != nil {
		return "", "", nil, perr
	}
	if n < 0 {
		return "", "", nil, wireErrorf("08P01", "pgwire: Parse parameter count %d is negative", n)
	}
	if int(n) > r.remaining()/4 {
		return "", "", nil, wireErrorf("08P01", "pgwire: Parse claims %d parameter types but only %d bytes remain", n, r.remaining())
	}
	oids = make([]uint32, int(n))
	for i := range oids {
		v, e := r.int32("Parse parameter type")
		if e != nil {
			return "", "", nil, e
		}
		oids[i] = uint32(v)
	}
	if perr = r.finish("Parse"); perr != nil {
		return "", "", nil, perr
	}
	return name, query, oids, nil
}

type rawBindParam struct {
	null bool
	data []byte
}

func decodeBind(body []byte) (portalName, stmtName string, formats []int16, params []rawBindParam, resultFormats []int16, perr *wireError) {
	r := &bodyReader{buf: body}
	if portalName, perr = r.cstring("Bind portal name"); perr != nil {
		return
	}
	if stmtName, perr = r.cstring("Bind statement name"); perr != nil {
		return
	}
	nFormats, perr := r.int16("Bind parameter format count")
	if perr != nil {
		return "", "", nil, nil, nil, perr
	}
	if nFormats < 0 || int(nFormats) > r.remaining()/2 {
		return "", "", nil, nil, nil, wireErrorf("08P01", "pgwire: invalid Bind parameter format count %d", nFormats)
	}
	formats = make([]int16, int(nFormats))
	for i := range formats {
		v, e := r.int16("Bind parameter format")
		if e != nil {
			return "", "", nil, nil, nil, e
		}
		if v != 0 && v != 1 {
			return "", "", nil, nil, nil, wireErrorf("08P01", "pgwire: invalid parameter format code %d", v)
		}
		formats[i] = v
	}
	nParams, perr := r.int16("Bind parameter count")
	if perr != nil {
		return "", "", nil, nil, nil, perr
	}
	if nParams < 0 {
		return "", "", nil, nil, nil, wireErrorf("08P01", "pgwire: Bind parameter count %d is negative", nParams)
	}
	params = make([]rawBindParam, int(nParams))
	for i := range params {
		n, e := r.int32("Bind parameter length")
		if e != nil {
			return "", "", nil, nil, nil, e
		}
		switch {
		case n == -1:
			params[i] = rawBindParam{null: true}
		case n < 0:
			return "", "", nil, nil, nil, wireErrorf("08P01", "pgwire: invalid Bind parameter length %d", n)
		default:
			if int64(n) > int64(r.remaining()) {
				return "", "", nil, nil, nil, wireErrorf("08P01", "pgwire: Bind parameter length %d exceeds message", n)
			}
			b, e := r.bytesN(int(n), "Bind parameter value")
			if e != nil {
				return "", "", nil, nil, nil, e
			}
			params[i] = rawBindParam{data: b}
		}
	}
	nResultFormats, perr := r.int16("Bind result format count")
	if perr != nil {
		return "", "", nil, nil, nil, perr
	}
	if nResultFormats < 0 || int(nResultFormats) > r.remaining()/2 {
		return "", "", nil, nil, nil, wireErrorf("08P01", "pgwire: invalid Bind result format count %d", nResultFormats)
	}
	resultFormats = make([]int16, int(nResultFormats))
	for i := range resultFormats {
		v, e := r.int16("Bind result format")
		if e != nil {
			return "", "", nil, nil, nil, e
		}
		if v != 0 && v != 1 {
			return "", "", nil, nil, nil, wireErrorf("08P01", "pgwire: invalid result format code %d", v)
		}
		resultFormats[i] = v
	}
	if perr = r.finish("Bind"); perr != nil {
		return "", "", nil, nil, nil, perr
	}
	return portalName, stmtName, formats, params, resultFormats, nil
}

func decodeDescribe(body []byte) (kind byte, name string, perr *wireError) {
	r := &bodyReader{buf: body}
	if r.remaining() < 1 {
		return 0, "", wireErrorf("08P01", "pgwire: malformed Describe: missing kind")
	}
	kind = r.buf[r.pos]
	r.pos++
	if kind != 'S' && kind != 'P' {
		return 0, "", wireErrorf("08P01", "pgwire: invalid Describe target %q", kind)
	}
	if name, perr = r.cstring("Describe name"); perr != nil {
		return
	}
	if perr = r.finish("Describe"); perr != nil {
		return
	}
	return kind, name, nil
}

func decodeExecute(body []byte) (name string, maxRows int32, perr *wireError) {
	r := &bodyReader{buf: body}
	if name, perr = r.cstring("Execute portal name"); perr != nil {
		return
	}
	if maxRows, perr = r.int32("Execute row limit"); perr != nil {
		return
	}
	if perr = r.finish("Execute"); perr != nil {
		return
	}
	return name, maxRows, nil
}

func decodeClose(body []byte) (kind byte, name string, perr *wireError) {
	r := &bodyReader{buf: body}
	if r.remaining() < 1 {
		return 0, "", wireErrorf("08P01", "pgwire: malformed Close: missing kind")
	}
	kind = r.buf[r.pos]
	r.pos++
	if kind != 'S' && kind != 'P' {
		return 0, "", wireErrorf("08P01", "pgwire: invalid Close target %q", kind)
	}
	if name, perr = r.cstring("Close name"); perr != nil {
		return
	}
	if perr = r.finish("Close"); perr != nil {
		return
	}
	return kind, name, nil
}

// --- Parse ------------------------------------------------------------------

func (sess *connSession) handleParse(body []byte) error {
	name, query, oids, perr := decodeParse(body)
	if perr != nil {
		return sess.failPG(perr)
	}
	if name != "" {
		if _, exists := sess.statements[name]; exists {
			return sess.fail("42P05", fmt.Sprintf("prepared statement %q already exists", name))
		}
	}
	stmt, perr := sess.newPreparedStatement(name, query, oids)
	if perr != nil {
		if name == "" {
			delete(sess.statements, "")
		}
		return sess.failPG(perr)
	}
	if name == "" {
		delete(sess.statements, "")
	}
	sess.statements[name] = stmt
	if err := writeParseComplete(sess.bw); err != nil {
		return err
	}
	return sess.flush()
}

func (sess *connSession) newPreparedStatement(name, query string, provided []uint32) (*preparedStatement, *wireError) {
	if strings.TrimSpace(query) == "" {
		if len(provided) != 0 {
			return nil, wireErrorf("42P02", "pgwire: there is no parameter $1 in an empty query")
		}
		return &preparedStatement{name: name, query: query, isEmpty: true}, nil
	}

	phs, count, perr := scanPlaceholders(query)
	if perr != nil {
		return nil, perr
	}
	if len(provided) > count {
		return nil, wireErrorf("42P02", "pgwire: there is no parameter $%d", len(provided))
	}

	stmt := &preparedStatement{
		name:         name,
		query:        query,
		placeholders: phs,
		resolved:     make([]paramKind, count),
	}

	// Validate the statement syntax after substituting sentinel integer
	// literals for the placeholders. The resulting AST is reused for type
	// inference.
	rendered := renderPlaceholders(query, phs, func(ph placeholder) string {
		return strconv.FormatInt(paramSentinelBase+int64(ph.index-1), 10)
	})
	ast, err := sqlparser.Parse(rendered)
	if err != nil {
		return nil, asWireError(err)
	}
	stmt.ast = ast
	_, stmt.isSelect = ast.(*sqlparser.Select)

	providedOIDs := make([]uint32, count)
	copy(providedOIDs, provided)
	hints := sess.inferParamHints(ast, count)

	stmt.paramOIDs = make([]uint32, count)
	stmt.specs = make([]paramSpec, count)
	for i := 0; i < count; i++ {
		spec := &stmt.specs[i]
		if oid := providedOIDs[i]; oid != 0 {
			spec.oid = oid
			spec.provided = true
			if kind := oidKind(oid); kind != kindUnknown {
				spec.kind = kind
				spec.confident = true
			}
		} else if hint := hints[i]; hint != kindUnknown {
			spec.kind = hint
			spec.confident = true
			spec.oid = oidForKind(hint)
		} else {
			// Unresolved parameters are reported as TEXT. Bind will still try
			// an integer rendering if the value looks numeric, so clients that
			// ignore ParameterDescription remain usable.
			spec.oid = oidText
		}
		stmt.paramOIDs[i] = spec.oid
	}

	// Cache RowDescription metadata when it can be inferred without executing
	// the statement. This also lets Bind validate result-format counts.
	if stmt.isSelect {
		if cols, ok := sess.inferSelectMeta(ast); ok {
			stmt.meta = cols
			stmt.metaReady = true
		}
	}
	return stmt, nil
}

// scanPlaceholders finds $N tokens outside string literals and comments.
func scanPlaceholders(query string) ([]placeholder, int, *wireError) {
	var phs []placeholder
	maxIndex := 0
	for i := 0; i < len(query); {
		switch query[i] {
		case '\'', '"':
			quote := query[i]
			i++
			for i < len(query) {
				if query[i] != quote {
					i++
					continue
				}
				if i+1 < len(query) && query[i+1] == quote {
					i += 2 // doubled quote (escape)
					continue
				}
				i++
				break
			}
		case '-':
			if i+1 < len(query) && query[i+1] == '-' {
				i += 2
				for i < len(query) && query[i] != '\n' {
					i++
				}
				continue
			}
			i++
		case '/':
			if i+1 < len(query) && query[i+1] == '*' {
				i += 2
				depth := 1
				for i < len(query) && depth > 0 {
					switch {
					case i+1 < len(query) && query[i] == '/' && query[i+1] == '*':
						depth++
						i += 2
					case i+1 < len(query) && query[i] == '*' && query[i+1] == '/':
						depth--
						i += 2
					default:
						i++
					}
				}
				continue
			}
			i++
		case '$':
			j := i + 1
			for j < len(query) && query[j] >= '0' && query[j] <= '9' {
				j++
			}
			if j == i+1 {
				i++
				continue
			}
			index, err := strconv.Atoi(query[i+1 : j])
			if err != nil || index <= 0 {
				return nil, 0, wireErrorf("42P02", "pgwire: there is no parameter %q", query[i:j])
			}
			if index > maxStatementParams {
				return nil, 0, wireErrorf("54000", "pgwire: too many parameters (maximum %d)", maxStatementParams)
			}
			phs = append(phs, placeholder{start: i, end: j, index: index})
			if index > maxIndex {
				maxIndex = index
			}
			i = j
		default:
			i++
		}
	}
	return phs, maxIndex, nil
}

func renderPlaceholders(query string, phs []placeholder, render func(placeholder) string) string {
	if len(phs) == 0 {
		return query
	}
	var sb strings.Builder
	sb.Grow(len(query) + 16*len(phs))
	last := 0
	for _, ph := range phs {
		sb.WriteString(query[last:ph.start])
		sb.WriteString(render(ph))
		last = ph.end
	}
	sb.WriteString(query[last:])
	return sb.String()
}

// --- Bind -------------------------------------------------------------------

func (sess *connSession) handleBind(body []byte) error {
	portalName, stmtName, formats, rawParams, resultFormats, perr := decodeBind(body)
	if perr != nil {
		return sess.failPG(perr)
	}
	stmt, ok := sess.statements[stmtName]
	if !ok {
		return sess.fail("26000", fmt.Sprintf("prepared statement %q does not exist", stmtName))
	}
	if portalName != "" {
		if _, exists := sess.portals[portalName]; exists {
			return sess.fail("42P03", fmt.Sprintf("portal %q already exists", portalName))
		}
	}
	if len(rawParams) != len(stmt.specs) {
		return sess.fail("08P01", fmt.Sprintf(
			"bind message supplies %d parameters, but prepared statement %q requires %d",
			len(rawParams), stmtName, len(stmt.specs)))
	}

	params, perr := bindParameters(stmt, formats, rawParams)
	if perr != nil {
		return sess.failPG(perr)
	}
	// If metadata is inferable, validate the result-format count now. It is
	// validated again before rows are encoded for cases that need execution.
	if stmt.isSelect && !stmt.metaReady {
		if cols, ok := sess.inferSelectMeta(stmt.ast); ok {
			stmt.meta = cols
			stmt.metaReady = true
		}
	}
	if stmt.metaReady {
		if _, perr := resolveFormatCodes(resultFormats, len(stmt.meta)); perr != nil {
			return sess.failPG(perr)
		}
	} else if !stmt.isSelect {
		if _, perr := resolveFormatCodes(resultFormats, 0); perr != nil {
			return sess.failPG(perr)
		}
	}

	if portalName == "" {
		delete(sess.portals, "")
	}
	sess.portals[portalName] = &portal{
		name:          portalName,
		stmt:          stmt,
		params:        params,
		resultFormats: resultFormats,
	}
	if err := writeBindComplete(sess.bw); err != nil {
		return err
	}
	return sess.flush()
}

func bindParameters(stmt *preparedStatement, formats []int16, raw []rawBindParam) ([]boundParam, *wireError) {
	if len(formats) != 0 && len(formats) != 1 && len(formats) != len(raw) {
		return nil, wireErrorf("08P01",
			"bind message has %d parameter formats but %d parameters", len(formats), len(raw))
	}
	params := make([]boundParam, len(raw))
	for i := range raw {
		format := int16(0)
		if len(formats) == 1 {
			format = formats[0]
		} else if len(formats) > 1 {
			format = formats[i]
		}
		p, perr := decodeParam(stmt.specs[i], format, raw[i])
		if perr != nil {
			return nil, perr
		}
		params[i] = p
	}
	return params, nil
}

func decodeParam(spec paramSpec, format int16, raw rawBindParam) (boundParam, *wireError) {
	if raw.null {
		return boundParam{}, wireErrorf("0A000", "pgwire: NULL parameters are not supported by the P0 engine")
	}
	if spec.confident {
		switch spec.kind {
		case kindInt:
			n, perr := decodeIntParam(spec.oid, format, raw.data)
			if perr != nil {
				return boundParam{}, perr
			}
			return boundParam{kind: kindInt, intVal: n}, nil
		case kindText:
			return boundParam{kind: kindText, textVal: string(raw.data)}, nil
		}
	}
	return decodeFlexibleParam(format, raw.data), nil
}

func decodeIntParam(oid uint32, format int16, data []byte) (int64, *wireError) {
	if format == 0 {
		n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if err != nil {
			return 0, wireErrorf("22P02", "invalid input syntax for type %s: %q", intTypeName(oid), string(data))
		}
		if perr := checkIntRange(oid, n); perr != nil {
			return 0, perr
		}
		return n, nil
	}
	width := intTypeWidth(oid)
	if width == 0 {
		return 0, wireErrorf("0A000", "pgwire: binary parameter type OID %d is not supported", oid)
	}
	if len(data) != width {
		return 0, wireErrorf("22P03", "invalid binary representation for type %s: got %d bytes, want %d",
			intTypeName(oid), len(data), width)
	}
	switch width {
	case 2:
		return int64(int16(binary.BigEndian.Uint16(data))), nil
	case 4:
		return int64(int32(binary.BigEndian.Uint32(data))), nil
	default:
		return int64(binary.BigEndian.Uint64(data)), nil
	}
}

func checkIntRange(oid uint32, n int64) *wireError {
	switch oid {
	case 21:
		if n < -32768 || n > 32767 {
			return wireErrorf("22003", "value %d is out of range for type int2", n)
		}
	case 23:
		if n < -2147483648 || n > 2147483647 {
			return wireErrorf("22003", "value %d is out of range for type int4", n)
		}
	case 26:
		if n < 0 || n > 4294967295 {
			return wireErrorf("22003", "value %d is out of range for type oid", n)
		}
	}
	return nil
}

func intTypeWidth(oid uint32) int {
	switch oid {
	case 21:
		return 2
	case 23, 26:
		return 4
	case 20:
		return 8
	default:
		return 8
	}
}

func intTypeName(oid uint32) string {
	switch oid {
	case 21:
		return "int2"
	case 23:
		return "int4"
	case 26:
		return "oid"
	default:
		return "int8"
	}
}

func decodeFlexibleParam(format int16, data []byte) boundParam {
	p := boundParam{}
	if format == 0 {
		p.textVal = string(data)
		p.allowText = true
		p.preferInt = true
		if n, ok := parseIntText(p.textVal); ok {
			p.intVal = n
			p.allowInt = true
		}
		return p
	}

	// Binary with an unspecified/unsupported type. Printable text is treated
	// as binary TEXT; non-text bytes of a standard integer width are treated
	// as a big-endian integer.
	if utf8.Valid(data) && printableBytes(data) {
		p.textVal = string(data)
		p.allowText = true
		if n, ok := parseIntText(p.textVal); ok {
			p.intVal = n
			p.allowInt = true
			p.preferInt = true
		}
		return p
	}
	switch len(data) {
	case 2, 4, 8:
		p.intVal = decodeBigEndianInt(data)
		p.allowInt = true
		p.preferInt = true
	}
	return p
}

func parseIntText(s string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n, err == nil
}

func decodeBigEndianInt(data []byte) int64 {
	switch len(data) {
	case 2:
		return int64(int16(binary.BigEndian.Uint16(data)))
	case 4:
		return int64(int32(binary.BigEndian.Uint32(data)))
	default:
		return int64(binary.BigEndian.Uint64(data))
	}
}

func printableBytes(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	for _, c := range data {
		if c == 0 {
			return false
		}
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' {
			return false
		}
	}
	return true
}

// --- Describe ---------------------------------------------------------------

func (sess *connSession) handleDescribe(body []byte) error {
	kind, name, perr := decodeDescribe(body)
	if perr != nil {
		return sess.failPG(perr)
	}
	switch kind {
	case 'S':
		stmt, ok := sess.statements[name]
		if !ok {
			return sess.fail("26000", fmt.Sprintf("prepared statement %q does not exist", name))
		}
		// Resolve row metadata before emitting any response so a failure does
		// not leave a partial ParameterDescription behind.
		var cols []columnDesc
		if stmt.isSelect && !stmt.isEmpty {
			var perr *wireError
			cols, perr = sess.ensureMeta(stmt)
			if perr != nil {
				return sess.failPG(perr)
			}
		}
		if err := writeParameterDescription(sess.bw, stmt.paramOIDs); err != nil {
			return err
		}
		if stmt.isEmpty || !stmt.isSelect {
			if err := writeNoData(sess.bw); err != nil {
				return err
			}
			return sess.flush()
		}
		if len(cols) == 0 {
			if err := writeNoData(sess.bw); err != nil {
				return err
			}
		} else {
			formats, _ := resolveFormatCodes(nil, len(cols))
			if err := writeRowDescriptionCols(sess.bw, cols, formats); err != nil {
				return err
			}
		}
	case 'P':
		p, ok := sess.portals[name]
		if !ok {
			return sess.fail("34000", fmt.Sprintf("portal %q does not exist", name))
		}
		if p.stmt.isEmpty || !p.stmt.isSelect {
			if err := writeNoData(sess.bw); err != nil {
				return err
			}
			return sess.flush()
		}
		cols, perr := sess.ensurePortalMeta(p)
		if perr != nil {
			return sess.failPG(perr)
		}
		if len(cols) == 0 {
			if err := writeNoData(sess.bw); err != nil {
				return err
			}
			return sess.flush()
		}
		formats, perr := resolveFormatCodes(p.resultFormats, len(cols))
		if perr != nil {
			return sess.failPG(perr)
		}
		if err := writeRowDescriptionCols(sess.bw, cols, formats); err != nil {
			return err
		}
	}
	return sess.flush()
}

func (sess *connSession) ensureMeta(stmt *preparedStatement) ([]columnDesc, *wireError) {
	if stmt.metaReady {
		return stmt.meta, nil
	}
	if !stmt.isSelect {
		stmt.metaReady = true
		return nil, nil
	}
	if cols, ok := sess.inferSelectMeta(stmt.ast); ok {
		stmt.meta = cols
		stmt.metaReady = true
		return cols, nil
	}
	res, err := sess.executeStatement(stmt, dummyParams(stmt), false)
	if err != nil {
		return nil, asWireError(err)
	}
	cols := sess.resultMeta(stmt, res)
	stmt.meta = cols
	stmt.metaReady = true
	return cols, nil
}

func dummyParams(stmt *preparedStatement) []boundParam {
	params := make([]boundParam, len(stmt.specs))
	for i, spec := range stmt.specs {
		if spec.confident {
			if spec.kind == kindInt {
				// Use 1 rather than 0 so a dummy LIMIT does not turn into an
				// unbounded scan while only metadata is needed.
				params[i] = boundParam{kind: kindInt, intVal: 1}
			} else {
				params[i] = boundParam{kind: kindText}
			}
			continue
		}
		params[i] = boundParam{allowInt: true, allowText: true, preferInt: true, intVal: 1}
	}
	return params
}

// --- Execute ----------------------------------------------------------------

func (sess *connSession) handleExecute(body []byte) error {
	name, maxRows, perr := decodeExecute(body)
	if perr != nil {
		return sess.failPG(perr)
	}
	if maxRows < 0 {
		return sess.fail("08P01", fmt.Sprintf("pgwire: Execute row limit %d is negative", maxRows))
	}
	p, ok := sess.portals[name]
	if !ok {
		return sess.fail("34000", fmt.Sprintf("portal %q does not exist", name))
	}
	if p.done {
		return sess.fail("55000", fmt.Sprintf("portal %q cannot be run: already completed", name))
	}
	if p.stmt.isEmpty {
		p.done = true
		if err := writeEmptyQueryResponse(sess.bw); err != nil {
			return err
		}
		return sess.flush()
	}
	if !p.stmt.isSelect {
		if p.result == nil {
			res, err := sess.executeStatement(p.stmt, p.params, true)
			if err != nil {
				return sess.failErr(err)
			}
			p.result = res
			p.meta = sess.resultMeta(p.stmt, res)
			p.tag = commandTag(p.stmt.query, res)
		}
		p.done = true
		if err := writeCommandComplete(sess.bw, p.tag); err != nil {
			return err
		}
		return sess.flush()
	}

	cols, perr := sess.ensurePortalResult(p)
	if perr != nil {
		return sess.failPG(perr)
	}
	formats, perr := resolveFormatCodes(p.resultFormats, len(cols))
	if perr != nil {
		return sess.failPG(perr)
	}
	for i, f := range formats {
		if f == 1 && cols[i].oid != oidInt8 && cols[i].oid != oidText {
			return sess.fail("0A000", fmt.Sprintf("pgwire: binary result format for type OID %d is not supported", cols[i].oid))
		}
	}

	rows := p.result.Rows
	end := len(rows)
	if maxRows > 0 && p.next+int(maxRows) < end {
		end = p.next + int(maxRows)
	}
	for i := p.next; i < end; i++ {
		if perr := validateDataRowEncoding(rows[i], cols, formats); perr != nil {
			return sess.failPG(perr)
		}
	}
	for p.next < end {
		if err := writeDataRowCols(sess.bw, rows[p.next], cols, formats); err != nil {
			return err
		}
		p.next++
	}
	if p.next < len(rows) {
		if err := writePortalSuspended(sess.bw); err != nil {
			return err
		}
		return sess.flush()
	}
	p.done = true
	p.tag = fmt.Sprintf("SELECT %d", len(rows))
	if err := writeCommandComplete(sess.bw, p.tag); err != nil {
		return err
	}
	return sess.flush()
}

// resultMetaForStatement uses metadata already exposed to the client when it
// exists, so DataRow encoding stays consistent with the RowDescription that
// was sent earlier. Only statements that were never described derive types
// from the actual result values.
func (sess *connSession) resultMetaForStatement(stmt *preparedStatement, res *sqlengine.Result) []columnDesc {
	if stmt.metaReady && len(stmt.meta) == len(res.Columns) {
		out := make([]columnDesc, len(res.Columns))
		for i, name := range res.Columns {
			out[i] = stmt.meta[i]
			out[i].name = name
		}
		return out
	}
	return sess.resultMeta(stmt, res)
}

func validateDataRowEncoding(row []any, cols []columnDesc, formats []int16) *wireError {
	for i, f := range formats {
		if f != 1 || i >= len(cols) {
			continue
		}
		var v any
		if i < len(row) {
			v = row[i]
		}
		if v == nil {
			continue
		}
		switch cols[i].oid {
		case oidInt8:
			if _, ok := int64Value(v); !ok {
				return wireErrorf("0A000", "pgwire: column %q value %T cannot be encoded as INT8", cols[i].name, v)
			}
		case oidText:
		default:
			return wireErrorf("0A000", "pgwire: binary result format for type OID %d is not supported", cols[i].oid)
		}
	}
	return nil
}

// ensurePortalMeta resolves a portal's RowDescription without executing its
// query when statement metadata is already known. The engine has no separate
// describe API, so a statement whose metadata cannot be inferred is run with
// dummy parameter values (SELECT only).
func (sess *connSession) ensurePortalMeta(p *portal) ([]columnDesc, *wireError) {
	if p.result != nil {
		return p.meta, nil
	}
	if p.stmt.metaReady {
		p.meta = p.stmt.meta
		return p.meta, nil
	}
	cols, perr := sess.ensureMeta(p.stmt)
	if perr != nil {
		return nil, perr
	}
	p.meta = cols
	return cols, nil
}

func (sess *connSession) ensurePortalResult(p *portal) ([]columnDesc, *wireError) {
	if p.result != nil {
		return p.meta, nil
	}
	res, err := sess.executeStatement(p.stmt, p.params, true)
	if err != nil {
		return nil, asWireError(err)
	}
	p.result = res
	p.meta = sess.resultMetaForStatement(p.stmt, res)
	p.tag = commandTag(p.stmt.query, res)
	if !p.stmt.metaReady {
		p.stmt.meta = p.meta
		p.stmt.metaReady = true
	}
	return p.meta, nil
}

// executeStatement renders the placeholders and runs the statement through the
// engine. When a parameter type is unknown, several renderings may be tried;
// failed attempts roll back inside the engine, so retrying is safe. observe
// records one query SLO observation for the whole attempt sequence; metadata
// probes pass false so Describe does not inflate query counts.
func (sess *connSession) executeStatement(stmt *preparedStatement, params []boundParam, observe bool) (res *sqlengine.Result, err error) {
	if observe {
		done := sess.srv.trackQuery()
		defer func() {
			observed := err
			if observed == nil && res == nil {
				observed = errNilEngineResult
			}
			done(observed)
		}()
	}
	if len(params) != len(stmt.specs) {
		return nil, wireErrorf("08P01", "pgwire: parameter count mismatch")
	}
	choices := make([][]paramKind, len(params))
	var total = 1
	for i, p := range params {
		ks := p.kinds()
		if len(ks) == 0 {
			return nil, wireErrorf("22P03", "pgwire: parameter $%d has no usable representation", i+1)
		}
		if p.kind == kindUnknown && stmt.resolved[i] != kindUnknown {
			ks = preferKind(ks, stmt.resolved[i])
		}
		choices[i] = ks
		total *= len(ks)
		if total > maxExecuteAttempts {
			total = maxExecuteAttempts
		}
	}

	combos := make([][]paramKind, 0, minInt(total, maxExecuteAttempts))
	var build func(int, []paramKind)
	build = func(i int, cur []paramKind) {
		if len(combos) >= maxExecuteAttempts {
			return
		}
		if i == len(choices) {
			combo := make([]paramKind, len(cur))
			copy(combo, cur)
			combos = append(combos, combo)
			return
		}
		for _, kind := range choices[i] {
			build(i+1, append(cur, kind))
		}
	}
	build(0, nil)

	var firstErr error
	for _, combo := range combos {
		query := renderPlaceholders(stmt.query, stmt.placeholders, func(ph placeholder) string {
			return renderParam(params[ph.index-1], combo[ph.index-1])
		})
		res, err := sess.srv.engine.Execute(sess.srv.ctx, query)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if !retryableExecuteError(err) {
				return nil, err
			}
			continue
		}
		if res == nil {
			return nil, errors.New("pgwire: engine returned a nil result")
		}
		for i := range stmt.resolved {
			if stmt.resolved[i] == kindUnknown && params[i].kind == kindUnknown {
				stmt.resolved[i] = combo[i]
			}
		}
		sess.observeSchema(stmt.ast)
		return res, nil
	}
	if firstErr == nil {
		firstErr = wireErrorf("22P03", "pgwire: no parameter representation could be executed")
	}
	return nil, firstErr
}

func preferKind(kinds []paramKind, want paramKind) []paramKind {
	for i, k := range kinds {
		if k == want {
			if i == 0 {
				return kinds
			}
			out := make([]paramKind, 0, len(kinds))
			out = append(out, want)
			for _, other := range kinds {
				if other != want {
					out = append(out, other)
				}
			}
			return out
		}
	}
	return kinds
}

func renderParam(p boundParam, kind paramKind) string {
	if kind == kindInt {
		return strconv.FormatInt(p.intVal, 10)
	}
	return quoteSQLText(p.textVal)
}

func quoteSQLText(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func retryableExecuteError(err error) bool {
	var parseErr *sqlparser.ParseError
	if errors.As(err, &parseErr) {
		return true
	}
	return errors.Is(err, sqlengine.ErrBadColumn)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// --- Sync / Close / Flush ---------------------------------------------------

func (sess *connSession) handleSync() error {
	sess.extendedError = false
	// Portals belong to the implicit transaction that Sync ends.
	sess.portals = make(map[string]*portal)
	if err := writeReadyForQuery(sess.bw, 'I'); err != nil {
		return err
	}
	return sess.flush()
}

func (sess *connSession) handleClose(body []byte) error {
	kind, name, perr := decodeClose(body)
	if perr != nil {
		return sess.failPG(perr)
	}
	switch kind {
	case 'S':
		delete(sess.statements, name)
	case 'P':
		delete(sess.portals, name)
	}
	if err := writeCloseComplete(sess.bw); err != nil {
		return err
	}
	return sess.flush()
}

func (sess *connSession) handleFlush() error { return sess.flush() }

// --- simple Query -----------------------------------------------------------

// handleSimpleQuery executes one SQL statement and writes its full response,
// including ReadyForQuery, then flushes. The simple-query protocol destroys all
// portals and the unnamed prepared statement, as PostgreSQL does.
func (sess *connSession) handleSimpleQuery(body []byte) error {
	query := string(body)
	if i := strings.IndexByte(query, 0); i >= 0 {
		query = query[:i]
	}
	query = strings.TrimSpace(query)
	sess.portals = make(map[string]*portal)
	delete(sess.statements, "")

	if query == "" {
		if err := writeEmptyQueryResponse(sess.bw); err != nil {
			return err
		}
		if err := writeReadyForQuery(sess.bw, 'I'); err != nil {
			return err
		}
		return sess.flush()
	}

	ast, parseErr := sqlparser.Parse(query)
	done := sess.srv.trackQuery()
	res, err := sess.srv.engine.Execute(sess.srv.ctx, query)
	observed := err
	if observed == nil && res == nil {
		observed = errNilEngineResult
	}
	done(observed)
	switch {
	case err != nil:
		if werr := writeError(sess.bw, sqlState(err), err.Error()); werr != nil {
			return werr
		}
	case res == nil:
		if werr := writeError(sess.bw, "XX000", "pgwire: engine returned a nil result"); werr != nil {
			return werr
		}
	default:
		if parseErr == nil {
			sess.observeSchema(ast)
		}
		if werr := writeResult(sess.bw, query, res); werr != nil {
			return werr
		}
	}
	if err := writeReadyForQuery(sess.bw, 'I'); err != nil {
		return err
	}
	return sess.flush()
}

// resolveFormatCodes expands a Bind result-format list to one code per column.
func resolveFormatCodes(codes []int16, n int) ([]int16, *wireError) {
	for _, c := range codes {
		if c != 0 && c != 1 {
			return nil, wireErrorf("08P01", "pgwire: invalid result format code %d", c)
		}
	}
	switch len(codes) {
	case 0:
		return make([]int16, n), nil
	case 1:
		out := make([]int16, n)
		for i := range out {
			out[i] = codes[0]
		}
		return out, nil
	default:
		if len(codes) != n {
			return nil, wireErrorf("08P01",
				"bind message has %d result formats but query has %d columns", len(codes), n)
		}
		return codes, nil
	}
}
