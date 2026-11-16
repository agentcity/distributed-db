package pgwire

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"distributed-db/internal/sqlengine"
	"distributed-db/internal/sqlparser"
)

// PostgreSQL protocol message types used by this server. Frontend and backend
// messages share the same one-byte type space but travel in opposite
// directions, so 'S' (Sync vs. ParameterStatus) and 'D' (Describe vs. DataRow)
// intentionally appear twice. Authentication requests and AuthenticationOk
// both use backend type 'R'; the four-byte body code distinguishes them.
const (
	// Frontend messages.
	msgQuery           byte = 'Q'
	msgParse           byte = 'P'
	msgBind            byte = 'B'
	msgDescribe        byte = 'D'
	msgExecute         byte = 'E'
	msgSync            byte = 'S'
	msgClose           byte = 'C'
	msgFlush           byte = 'H'
	msgTerminate       byte = 'X'
	msgPasswordMessage byte = 'p'

	// Backend messages.
	msgAuthenticationRequest byte = 'R'
	msgAuthenticationOk      byte = 'R'
	msgParameterStatus       byte = 'S'
	msgBackendKeyData        byte = 'K'
	msgReadyForQuery         byte = 'Z'
	msgRowDescription        byte = 'T'
	msgDataRow               byte = 'D'
	msgCommandComplete       byte = 'C'
	msgErrorResponse         byte = 'E'
	msgEmptyQueryResponse    byte = 'I'
	msgParseComplete         byte = '1'
	msgBindComplete          byte = '2'
	msgCloseComplete         byte = '3'
	msgParameterDescription  byte = 't'
	msgNoData                byte = 'n'
	msgPortalSuspended       byte = 's'
)

// columnDesc is the subset of a RowDescription field this server produces.
type columnDesc struct {
	name string
	oid  uint32
	size int16
}

func readStartupPacket(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := int32(binary.BigEndian.Uint32(hdr[:]))
	if n < 8 || n > maxStartupPacket {
		return nil, fmt.Errorf("%w: invalid startup packet length %d", errBadMessage, n)
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

func readMessage(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int32(binary.BigEndian.Uint32(hdr[1:]))
	if n < 4 || n > maxMessageSize {
		return 0, nil, fmt.Errorf("%w: invalid message length %d", errBadMessage, n)
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return hdr[0], body, nil
}

func writeMessage(w io.Writer, typ byte, body []byte) error {
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(body))+4)
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	_, err := w.Write(body)
	return err
}

func writeReadyForQuery(w io.Writer, status byte) error {
	return writeMessage(w, msgReadyForQuery, []byte{status})
}

func writeParseComplete(w io.Writer) error {
	return writeMessage(w, msgParseComplete, nil)
}

func writeBindComplete(w io.Writer) error {
	return writeMessage(w, msgBindComplete, nil)
}

func writeCloseComplete(w io.Writer) error {
	return writeMessage(w, msgCloseComplete, nil)
}

func writeNoData(w io.Writer) error {
	return writeMessage(w, msgNoData, nil)
}

func writeEmptyQueryResponse(w io.Writer) error {
	return writeMessage(w, msgEmptyQueryResponse, nil)
}

func writePortalSuspended(w io.Writer) error {
	return writeMessage(w, msgPortalSuspended, nil)
}

func writeParameterDescription(w io.Writer, oids []uint32) error {
	body := make([]byte, 0, 2+4*len(oids))
	body = appendInt16(body, int16(len(oids)))
	for _, oid := range oids {
		body = appendUint32(body, oid)
	}
	return writeMessage(w, msgParameterDescription, body)
}

func writeError(w io.Writer, code, msg string) error {
	body := make([]byte, 0, len(code)+len(msg)+16)
	body = append(body, 'S')
	body = appendCString(body, "ERROR")
	body = append(body, 'V')
	body = appendCString(body, "ERROR")
	body = append(body, 'C')
	body = appendCString(body, code)
	body = append(body, 'M')
	body = appendCString(body, msg)
	body = append(body, 0)
	return writeMessage(w, msgErrorResponse, body)
}

// writeResult emits RowDescription and DataRow messages for a result set,
// followed by CommandComplete. Non-query statements get only CommandComplete.
func writeResult(w io.Writer, query string, res *sqlengine.Result) error {
	if len(res.Columns) > 0 {
		if err := writeRowDescription(w, res); err != nil {
			return err
		}
		for _, row := range res.Rows {
			if err := writeDataRow(w, row); err != nil {
				return err
			}
		}
	}
	return writeCommandComplete(w, commandTag(query, res))
}

func writeRowDescription(w io.Writer, res *sqlengine.Result) error {
	cols := make([]columnDesc, len(res.Columns))
	for i, name := range res.Columns {
		oid, size := columnType(res, i)
		cols[i] = columnDesc{name: name, oid: oid, size: size}
	}
	return writeRowDescriptionCols(w, cols, nil)
}

// writeRowDescriptionCols writes a RowDescription for cols. formats, if
// non-nil, must contain one format code per column.
func writeRowDescriptionCols(w io.Writer, cols []columnDesc, formats []int16) error {
	body := make([]byte, 0, 2+20*len(cols))
	body = appendInt16(body, int16(len(cols)))
	for i, c := range cols {
		format := int16(0)
		if i < len(formats) {
			format = formats[i]
		}
		body = appendCString(body, c.name)
		body = appendUint32(body, 0) // table OID: unknown
		body = appendInt16(body, 0)  // column attribute number
		body = appendUint32(body, c.oid)
		body = appendInt16(body, c.size)
		body = appendInt32(body, -1) // type modifier
		body = appendInt16(body, format)
	}
	return writeMessage(w, msgRowDescription, body)
}

const (
	oidInt8 uint32 = 20
	oidText uint32 = 25
)

// columnType reports the PostgreSQL type OID and fixed size (or -1 for
// variable length) for a result column. The JSON-backed engine only produces
// int64 and string values; anything else is transmitted as text.
func columnType(res *sqlengine.Result, i int) (oid uint32, size int16) {
	for _, row := range res.Rows {
		if i >= len(row) || row[i] == nil {
			continue
		}
		switch row[i].(type) {
		case int64:
			return oidInt8, 8
		case string:
			return oidText, -1
		default:
			return oidText, -1
		}
	}
	return oidText, -1 // all NULL or no rows: report TEXT
}

func writeDataRow(w io.Writer, row []any) error {
	body := make([]byte, 0, 2+8*len(row))
	body = appendInt16(body, int16(len(row)))
	for _, v := range row {
		switch x := v.(type) {
		case nil:
			body = appendInt32(body, -1)
		case int64:
			body = appendText(body, strconv.FormatInt(x, 10))
		case string:
			body = appendText(body, x)
		default:
			body = appendText(body, fmt.Sprint(x))
		}
	}
	return writeMessage(w, msgDataRow, body)
}

func appendText(body []byte, s string) []byte {
	body = appendInt32(body, int32(len(s)))
	return append(body, s...)
}

// writeDataRowCols writes a DataRow using the per-column format codes in
// formats (one entry per column). A nil value is always encoded as length -1.
func writeDataRowCols(w io.Writer, row []any, cols []columnDesc, formats []int16) error {
	body := make([]byte, 0, 2+8*len(cols))
	body = appendInt16(body, int16(len(cols)))
	for i := range cols {
		var v any
		if i < len(row) {
			v = row[i]
		}
		if v == nil {
			body = appendInt32(body, -1)
			continue
		}
		if i >= len(formats) || formats[i] == 0 {
			body = appendText(body, formatValueText(v))
			continue
		}
		switch cols[i].oid {
		case oidInt8:
			n, ok := int64Value(v)
			if !ok {
				return fmt.Errorf("pgwire: column %q value %T cannot be encoded as INT8", cols[i].name, v)
			}
			var buf [8]byte
			binary.BigEndian.PutUint64(buf[:], uint64(n))
			body = appendInt32(body, 8)
			body = append(body, buf[:]...)
		case oidText:
			// The binary representation of TEXT is its raw bytes.
			body = appendText(body, formatValueText(v))
		default:
			return fmt.Errorf("pgwire: binary format for type OID %d is not supported", cols[i].oid)
		}
	}
	return writeMessage(w, msgDataRow, body)
}

func formatValueText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case int64:
		return strconv.FormatInt(x, 10)
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

func int64Value(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func writeCommandComplete(w io.Writer, tag string) error {
	return writeMessage(w, msgCommandComplete, appendCString(nil, tag))
}

func commandTag(query string, res *sqlengine.Result) string {
	switch leadingKeyword(query) {
	case "SELECT":
		return "SELECT " + strconv.Itoa(len(res.Rows))
	case "INSERT":
		return fmt.Sprintf("INSERT 0 %d", res.RowsAffected)
	case "UPDATE":
		return fmt.Sprintf("UPDATE %d", res.RowsAffected)
	case "DELETE":
		return fmt.Sprintf("DELETE %d", res.RowsAffected)
	default:
		return "OK"
	}
}

func leadingKeyword(query string) string {
	query = strings.TrimSpace(query)
	end := 0
	for end < len(query) {
		c := query[end]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			end++
			continue
		}
		break
	}
	return strings.ToUpper(query[:end])
}

func sqlState(err error) string {
	var parseErr *sqlparser.ParseError
	if errors.As(err, &parseErr) {
		return "42601" // syntax_error
	}
	switch {
	case errors.Is(err, sqlengine.ErrTableNotFound):
		return "42P01" // undefined_table
	case errors.Is(err, sqlengine.ErrTableExists),
		errors.Is(err, sqlengine.ErrIndexExists):
		return "42P07" // duplicate_table
	case errors.Is(err, sqlengine.ErrDuplicateKey):
		return "23505" // unique_violation
	case errors.Is(err, sqlengine.ErrIndexNotFound):
		return "42704" // undefined_object
	case errors.Is(err, sqlengine.ErrBadColumn):
		if strings.Contains(err.Error(), "unknown column") {
			return "42703" // undefined_column
		}
		return "42804" // datatype_mismatch
	case errors.Is(err, sqlengine.ErrRowNotFound):
		return "P0002" // no_data_found
	case errors.Is(err, sqlengine.ErrUnsupported):
		return "0A000" // feature_not_supported
	case errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return "57014" // query_canceled
	default:
		return "XX000" // internal_error
	}
}

func parseStartupParams(body []byte) (map[string]string, error) {
	params := make(map[string]string, 4)
	for {
		key, rest, ok := readCString(body)
		if !ok {
			return nil, fmt.Errorf("%w: unterminated startup parameter", errBadMessage)
		}
		body = rest
		if key == "" {
			return params, nil
		}
		value, rest, ok := readCString(body)
		if !ok {
			return nil, fmt.Errorf("%w: missing value for startup parameter %q", errBadMessage, key)
		}
		body = rest
		params[key] = value
	}
}

func readCString(b []byte) (string, []byte, bool) {
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return "", nil, false
	}
	return string(b[:i]), b[i+1:], true
}

func appendCString(b []byte, s string) []byte {
	b = append(b, s...)
	return append(b, 0)
}

func appendInt16(b []byte, v int16) []byte {
	return append(b, byte(v>>8), byte(v))
}

func appendInt32(b []byte, v int32) []byte {
	return binary.BigEndian.AppendUint32(b, uint32(v))
}

func appendUint32(b []byte, v uint32) []byte {
	return binary.BigEndian.AppendUint32(b, v)
}

func (s *Server) backendKey() (pid, secret int32) {
	pid = s.nextPID.Add(1)
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return pid, pid ^ int32(uint32(s.nextPID.Add(1)))
	}
	return pid, int32(binary.BigEndian.Uint32(b[:]) & 0x7fffffff)
}
