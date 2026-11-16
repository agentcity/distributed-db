package pgwire

import (
	"bytes"
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

// PostgreSQL protocol message types used by this server.
const (
	msgQuery              byte = 'Q'
	msgTerminate          byte = 'X'
	msgAuthenticationOk   byte = 'R'
	msgParameterStatus    byte = 'S'
	msgBackendKeyData     byte = 'K'
	msgReadyForQuery      byte = 'Z'
	msgRowDescription     byte = 'T'
	msgDataRow            byte = 'D'
	msgCommandComplete    byte = 'C'
	msgErrorResponse      byte = 'E'
	msgEmptyQueryResponse byte = 'I'
)

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
	body := make([]byte, 0, 2+20*len(res.Columns))
	body = appendInt16(body, int16(len(res.Columns)))
	for i, name := range res.Columns {
		oid, size := columnType(res, i)
		body = appendCString(body, name)
		body = appendUint32(body, 0) // table OID: unknown
		body = appendInt16(body, 0)  // column attribute number
		body = appendUint32(body, oid)
		body = appendInt16(body, size)
		body = appendInt32(body, -1) // type modifier
		body = appendInt16(body, 0)  // text format
	}
	return writeMessage(w, msgRowDescription, body)
}

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
			return 20, 8 // INT8
		case string:
			return 25, -1 // TEXT
		default:
			return 25, -1
		}
	}
	return 25, -1 // all NULL or no rows: report TEXT
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
	if errors.Is(err, sqlengine.ErrUnsupported) {
		return "42601" // unsupported statement: report as invalid SQL
	}
	return "XX000" // internal_error
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
