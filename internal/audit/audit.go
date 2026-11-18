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
// A Logger opened with Open appends to one file with mode 0600. Options.Sync
// enables an fsync after every record for durability at the cost of
// throughput; the default batches through the operating system page cache.
//
// # Rotation
//
// Open honors Options.MaxBytes and Options.MaxBackups for size-based rotation.
// A rotation happens before a record that would push the active file past
// MaxBytes: the active file is closed, shifted to "<path>.1" (with older
// backups shifted to .2, .3, ... and the oldest beyond MaxBackups deleted),
// and a fresh active file is opened with O_CREATE|O_APPEND. Rotation is
// serialized by the Logger mutex, so records are never interleaved across the
// rename and no writer keeps appending to the rotated inode. MaxBackups == 0
// keeps no backup copies and discards the rotated data.
//
// MaxBytes == 0 disables rotation; that is the default and the historical
// behaviour, where operators rotate externally (for example with logrotate
// copy/truncate). Rotation is only available through Open: NewWriter has no
// file path and therefore ignores the rotation options.
//
// Write, fsync and rotation-reopen errors are reported through Options.OnError
// (usually the server logger) and returned to the caller. The interceptors
// treat audit failures as best-effort by default: they do not turn an
// otherwise valid admin RPC into a failure. Servers configured with
// AuditRequired set fail closed instead and reject the protected RPC.
package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// Default rotation values used by the CLI flag defaults. The audit package
// itself keeps rotation disabled unless Options.MaxBytes is set.
const (
	// DefaultMaxBytes is the default size threshold for one audit file
	// (100 MiB).
	DefaultMaxBytes int64 = 100 << 20
	// DefaultMaxBackups is the default number of rotated audit files kept.
	DefaultMaxBackups = 5
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
	// MaxBytes enables size-based rotation at that threshold. Zero disables
	// rotation. It is honored by Open only: NewWriter has no file path and
	// ignores it.
	MaxBytes int64
	// MaxBackups is the number of rotated files to keep. With rotation
	// enabled, zero discards the rotated data instead of keeping a backup.
	// Negative values make Open fail.
	MaxBackups int
	// Clock supplies record timestamps. It defaults to time.Now and exists for
	// deterministic tests.
	Clock func() time.Time
	// OnError receives write/fsync/rotation errors. It is called outside the
	// Logger mutex, so it may call back into the Logger.
	OnError func(error)
}

// Logger is a concurrency-safe JSONL audit sink. A nil *Logger is valid and
// drops records, which lets a server keep a nil logger when audit is disabled.
type Logger struct {
	mu         sync.Mutex
	w          io.Writer
	file       *os.File
	path       string
	size       int64
	maxBytes   int64
	maxBackups int
	clock      func() time.Time
	sync       bool
	onError    func(error)
	closed     bool
	broken     error
}

// failWriter keeps a broken file logger from panicking or silently succeeding.
// It is installed when rotation cannot reopen the active file.
type failWriter struct{ err error }

func (w failWriter) Write([]byte) (int, error) { return 0, w.err }

// Open appends audit records to path, creating the parent directory and the
// file with mode 0600 when needed. The file is opened with O_APPEND, so every
// record is appended even if another writer touched the file. When
// Options.MaxBytes is positive, the file is rotated at that size as described
// in the package documentation.
func Open(path string, opts Options) (*Logger, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("audit: empty log path")
	}
	if opts.MaxBytes < 0 {
		return nil, errors.New("audit: MaxBytes must not be negative")
	}
	if opts.MaxBackups < 0 {
		return nil, errors.New("audit: MaxBackups must not be negative")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("audit: create log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit: open log: %w", err)
	}
	var size int64
	if info, statErr := f.Stat(); statErr == nil {
		size = info.Size()
	}
	l := NewWriter(f, opts)
	l.file = f
	l.path = path
	l.size = size
	l.maxBytes = opts.MaxBytes
	l.maxBackups = opts.MaxBackups
	return l, nil
}

// NewWriter returns a Logger writing to w. It does not take ownership of w:
// Close marks the Logger closed but does not close w. A nil w discards records.
// Rotation options are ignored because NewWriter has no file path; use Open.
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
// When rotation is enabled, the active file is rotated first if this record
// would exceed MaxBytes.
func (l *Logger) Record(e Event) error {
	if l == nil {
		return nil
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
	if err != nil {
		return err
	}
	line = append(line, '\n')

	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return ErrClosed
	}
	if l.broken != nil {
		err = l.broken
		l.mu.Unlock()
		if l.onError != nil {
			l.onError(err)
		}
		return err
	}
	var warn error
	// Rotate before writing the record that would cross the threshold, so a
	// file stays at or below MaxBytes except for a single oversized record.
	if l.maxBytes > 0 && l.size > 0 && l.size+int64(len(line)) > l.maxBytes {
		err, warn = l.rotateLocked()
	}
	if err == nil {
		var n int
		n, err = l.w.Write(line)
		l.size += int64(n)
		if err == nil && l.sync && l.file != nil {
			err = l.file.Sync()
		}
	}
	onError := l.onError
	l.mu.Unlock()
	if warn != nil && onError != nil {
		onError(warn)
	}
	if err != nil && onError != nil {
		onError(err)
	}
	return err
}

// rotateLocked closes the active file, shifts backups and opens a fresh active
// file. It must be called with l.mu held. It returns a fatal error when the
// active file cannot be reopened (the Logger then stays broken and rejects all
// later records) and a non-fatal warning for backup-shift problems.
func (l *Logger) rotateLocked() (fatal, warning error) {
	if l.file != nil {
		if l.sync {
			if err := l.file.Sync(); err != nil {
				warning = errors.Join(warning, fmt.Errorf("audit: sync log before rotation: %w", err))
			}
		}
		if err := l.file.Close(); err != nil {
			warning = errors.Join(warning, fmt.Errorf("audit: close log before rotation: %w", err))
		}
		l.file = nil
	}
	if l.maxBackups > 0 {
		if err := os.Remove(l.backupPath(l.maxBackups)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			warning = errors.Join(warning, fmt.Errorf("audit: remove oldest backup: %w", err))
		}
		for i := l.maxBackups - 1; i >= 1; i-- {
			if err := os.Rename(l.backupPath(i), l.backupPath(i+1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				warning = errors.Join(warning, fmt.Errorf("audit: shift backup %d: %w", i, err))
			}
		}
		if err := os.Rename(l.path, l.backupPath(1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			warning = errors.Join(warning, fmt.Errorf("audit: move log to backup: %w", err))
		}
	} else {
		// Zero backups means retain nothing. The records in the rotated file
		// are discarded by operator choice.
		if err := os.Remove(l.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			warning = errors.Join(warning, fmt.Errorf("audit: remove rotated log: %w", err))
		}
	}
	nf, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fatal = fmt.Errorf("audit: reopen log after rotation: %w", err)
		l.broken = fatal
		l.w = failWriter{err: fatal}
		return fatal, warning
	}
	l.file = nf
	l.w = nf
	l.size = 0
	return nil, warning
}

// backupPath returns the rotated file name for backup i, where 1 is the most
// recent rotation.
func (l *Logger) backupPath(i int) string {
	return l.path + "." + strconv.Itoa(i)
}

// Healthy reports whether the Logger can still accept records: nil while it is
// open and working, ErrClosed after Close, or the rotation/reopen error that
// made it unusable. Servers with fail-closed audit call it before executing a
// protected RPC.
func (l *Logger) Healthy() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	return l.broken
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
