// Package audit records control-plane (admin) operations as structured,
// append-only JSON Lines.
//
// This is an MVP audit trail for the KV and meta admin gRPC interceptors, not
// a full database audit log: data-plane reads and writes are not recorded.
// Each record is one JSON object followed by a newline and contains:
//
//	time         RFC3339Nano UTC timestamp
//	principal    mTLS Common Name, or the role name for a token identity
//	role         admin | operator | read-only | unknown
//	method       fully-qualified gRPC method
//	range        "1", "1-2" for a merge, "cluster" for a cluster-wide
//	             operation, or omitted when the request has no range
//	result       success | denied | error
//	error_class  none | unauthenticated | permission_denied | application |
//	             a gRPC status class (invalid_argument, unavailable, ...)
//	auth         token | mtls (omitted when unauthenticated)
//
// The record deliberately has no free-form error message and never contains a
// token, so it cannot leak a credential. Callers must not put secrets in
// Principal.
//
// # Format and durability
//
// A Logger opened with Open appends to one file with mode 0600 and no rotation.
// There is no built-in size- or time-based rotation: operators rotate the file
// externally (for example with logrotate copy/truncate) and are responsible for
// retention and access control. Options.Sync enables an fsync after every
// record for durability at the cost of throughput; the default batches through
// the operating system page cache.
//
// Write errors are reported through Options.OnError (usually the server
// logger) and returned to the caller. The interceptors treat audit failures as
// best-effort: they do not turn an otherwise valid admin RPC into a failure.
// This is a documented MVP limitation; fail-closed audit is a follow-up.
package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Event result values.
const (
	ResultSuccess = "success"
	ResultDenied  = "denied"
	ResultError   = "error"
)

// Event error classes. The remaining classes are produced by ClassifyError from
// the gRPC status code.
const (
	ErrorClassNone             = "none"
	ErrorClassUnauthenticated  = "unauthenticated"
	ErrorClassPermissionDenied = "permission_denied"
	ErrorClassApplication      = "application"
	ErrorClassUnknown          = "unknown"
)

// ErrClosed is returned when recording after Close.
var ErrClosed = errors.New("audit: logger is closed")

// Event is one audit record. It contains no free-form message or token.
type Event struct {
	Time       time.Time `json:"time"`
	Principal  string    `json:"principal,omitempty"`
	Role       string    `json:"role"`
	Method     string    `json:"method"`
	Range      string    `json:"range,omitempty"`
	Result     string    `json:"result"`
	ErrorClass string    `json:"error_class"`
	Auth       string    `json:"auth,omitempty"`
}

// Options configures a Logger.
type Options struct {
	// Sync fsyncs the audit file after every record. It is ignored by
	// NewWriter because the writer may not be an *os.File.
	Sync bool
	// Clock supplies record timestamps. It defaults to time.Now and exists for
	// deterministic tests.
	Clock func() time.Time
	// OnError receives write/fsync errors. It is called outside the Logger
	// mutex, so it may call back into the Logger.
	OnError func(error)
}

// Logger is a concurrency-safe JSONL audit sink. A nil *Logger is valid and
// drops records, which lets a server keep a nil logger when audit is disabled.
type Logger struct {
	mu      sync.Mutex
	w       io.Writer
	file    *os.File
	clock   func() time.Time
	sync    bool
	onError func(error)
	closed  bool
}

// Open appends audit records to path, creating the parent directory and the
// file with mode 0600 when needed. The file is opened with O_APPEND, so every
// record is appended even if another writer touched the file. There is no
// rotation; see the package documentation.
func Open(path string, opts Options) (*Logger, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("audit: empty log path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("audit: create log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit: open log: %w", err)
	}
	l := NewWriter(f, opts)
	l.file = f
	return l, nil
}

// NewWriter returns a Logger writing to w. It does not take ownership of w:
// Close marks the Logger closed but does not close w. A nil w discards records.
func NewWriter(w io.Writer, opts Options) *Logger {
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	if w == nil {
		w = io.Discard
	}
	return &Logger{w: w, clock: clock, sync: opts.Sync, onError: opts.OnError}
}

// Record appends one event. Missing time, role, result and error class are
// filled with safe defaults (UTC now, "unknown", ResultError, ErrorClassUnknown).
func (l *Logger) Record(e Event) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return ErrClosed
	}
	if e.Time.IsZero() {
		e.Time = l.clock()
	}
	e.Time = e.Time.UTC()
	if e.Role == "" {
		e.Role = "unknown"
	}
	if e.Result == "" {
		e.Result = ResultError
	}
	if e.ErrorClass == "" {
		e.ErrorClass = ErrorClassUnknown
	}
	line, err := json.Marshal(e)
	if err == nil {
		line = append(line, '\n')
		_, err = l.w.Write(line)
		if err == nil && l.sync && l.file != nil {
			err = l.file.Sync()
		}
	}
	onError := l.onError
	l.mu.Unlock()
	if err != nil && onError != nil {
		onError(err)
	}
	return err
}

// Close closes the underlying file, if any. It is idempotent; a nil Logger is
// a no-op. When Options.Sync is set, the file is fsynced before closing.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	f := l.file
	l.file = nil
	sync := l.sync
	l.mu.Unlock()
	if f == nil {
		return nil
	}
	var errs []error
	if sync {
		if err := f.Sync(); err != nil {
			errs = append(errs, fmt.Errorf("audit: sync log: %w", err))
		}
	}
	if err := f.Close(); err != nil {
		errs = append(errs, fmt.Errorf("audit: close log: %w", err))
	}
	return errors.Join(errs...)
}

// ClassifyError maps an error to a stable audit error class. It returns
// ErrorClassNone for nil and ErrorClassUnknown for a non-status error.
func ClassifyError(err error) string {
	if err == nil {
		return ErrorClassNone
	}
	code := status.Code(err)
	if code == codes.OK {
		return ErrorClassNone
	}
	if class, ok := errorClasses[code]; ok {
		return class
	}
	return ErrorClassUnknown
}

var errorClasses = map[codes.Code]string{
	codes.Canceled:           "canceled",
	codes.Unknown:            ErrorClassUnknown,
	codes.InvalidArgument:    "invalid_argument",
	codes.DeadlineExceeded:   "deadline_exceeded",
	codes.NotFound:           "not_found",
	codes.AlreadyExists:      "already_exists",
	codes.PermissionDenied:   ErrorClassPermissionDenied,
	codes.ResourceExhausted:  "resource_exhausted",
	codes.FailedPrecondition: "failed_precondition",
	codes.Aborted:            "aborted",
	codes.OutOfRange:         "out_of_range",
	codes.Unimplemented:      "unimplemented",
	codes.Internal:           "internal",
	codes.Unavailable:        "unavailable",
	codes.DataLoss:           "data_loss",
	codes.Unauthenticated:    ErrorClassUnauthenticated,
}

// Outcome derives the (result, error_class) pair for a completed admin call.
// A transport error wins; otherwise an application-level failure carried in a
// response with a non-empty Error or Success == false is classified as
// ErrorClassApplication.
func Outcome(resp any, err error) (string, string) {
	switch {
	case err != nil:
		return ResultError, ClassifyError(err)
	case ResponseFailed(resp):
		return ResultError, ErrorClassApplication
	default:
		return ResultSuccess, ErrorClassNone
	}
}

// ResponseFailed reports whether an admin response carries an application-level
// failure. It understands the generated GetError/GetSuccess methods without
// importing the protobuf package.
func ResponseFailed(resp any) bool {
	if resp == nil {
		return false
	}
	if r, ok := resp.(interface{ GetError() string }); ok {
		if strings.TrimSpace(r.GetError()) != "" {
			return true
		}
	}
	if r, ok := resp.(interface{ GetSuccess() bool }); ok {
		return !r.GetSuccess()
	}
	return false
}

// RequestRange extracts a human-readable range label from an admin request:
// "1" for a request with range_id, "1-2" for a request with left/right range
// ids, or "" when the request has no range. It uses only generated getters, so
// it does not depend on the protobuf package.
func RequestRange(req any) string {
	if req == nil {
		return ""
	}
	if r, ok := req.(interface{ GetRangeId() uint64 }); ok {
		if id := r.GetRangeId(); id != 0 {
			return strconv.FormatUint(id, 10)
		}
	}
	if r, ok := req.(interface {
		GetLeftRangeId() uint64
		GetRightRangeId() uint64
	}); ok {
		left, right := r.GetLeftRangeId(), r.GetRightRangeId()
		if left != 0 || right != 0 {
			return fmt.Sprintf("%d-%d", left, right)
		}
	}
	return ""
}
