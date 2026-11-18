package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var fixedTime = time.Date(2026, 11, 18, 12, 34, 56, 123456789, time.UTC)

func fixedClock() time.Time { return fixedTime }

func TestOpenWritesJSONLAndAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "admin-audit.jsonl")
	l, err := Open(path, Options{Sync: true, Clock: fixedClock})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	err = l.Record(Event{
		Principal:  "operator",
		Role:       "operator",
		Method:     "/distributed_db.v2.KVService/SplitRange",
		Range:      "7",
		Result:     ResultSuccess,
		ErrorClass: ErrorClassNone,
		Auth:       "token",
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening must append, not truncate.
	l, err = Open(path, Options{Clock: fixedClock})
	if err != nil {
		t.Fatalf("Open second time: %v", err)
	}
	if err := l.Record(Event{
		Principal:  "client.test",
		Role:       "read-only",
		Method:     "/distributed_db.v2.MetaService/SetRanges",
		Range:      "cluster",
		Result:     ResultDenied,
		ErrorClass: ErrorClassPermissionDenied,
		Auth:       "mtls",
	}); err != nil {
		t.Fatalf("Record second event: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close second: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatalf("audit file does not end with a newline: %q", data)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("line count = %d, want 2: %q", len(lines), data)
	}

	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("first line is not JSON: %v\n%s", err, lines[0])
	}
	for _, field := range []string{"time", "principal", "role", "method", "range", "result", "error_class", "auth"} {
		if _, ok := first[field]; !ok {
			t.Fatalf("record is missing %q: %s", field, lines[0])
		}
	}
	if got, want := first["time"], "2026-11-18T12:34:56.123456789Z"; got != want {
		t.Fatalf("time = %v, want %v", got, want)
	}
	if first["result"] != ResultSuccess || first["error_class"] != ErrorClassNone {
		t.Fatalf("result/error_class = %v/%v", first["result"], first["error_class"])
	}

	var second map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("second line is not JSON: %v", err)
	}
	if second["role"] != "read-only" || second["result"] != ResultDenied ||
		second["error_class"] != ErrorClassPermissionDenied || second["auth"] != "mtls" {
		t.Fatalf("second record = %v", second)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open("", Options{}); err == nil {
		t.Fatal("Open(\"\") succeeded")
	}
	if _, err := Open("   ", Options{}); err == nil {
		t.Fatal("Open(whitespace) succeeded")
	}
}

func TestConcurrentRecordsRemainWholeLines(t *testing.T) {
	var buf bytes.Buffer
	l := NewWriter(&buf, Options{Clock: fixedClock})
	const goroutines = 16
	const perGoroutine = 25
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				if err := l.Record(Event{
					Principal:  "operator",
					Role:       "operator",
					Method:     "/method",
					Range:      "1",
					Result:     ResultSuccess,
					ErrorClass: ErrorClassNone,
				}); err != nil {
					t.Errorf("Record: %v", err)
				}
			}
		}(i)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != goroutines*perGoroutine {
		t.Fatalf("line count = %d, want %d", len(lines), goroutines*perGoroutine)
	}
	for i, line := range lines {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d is torn or invalid: %v\n%s", i, err, line)
		}
		if e.Result != ResultSuccess || e.ErrorClass != ErrorClassNone {
			t.Fatalf("line %d defaults = %+v", i, e)
		}
	}
}

func TestRecordDefaultsAndNilLogger(t *testing.T) {
	var buf bytes.Buffer
	l := NewWriter(&buf, Options{Clock: fixedClock})
	if err := l.Record(Event{Method: "/m"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	var e Event
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &e); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if e.Role != "unknown" || e.Result != ResultError || e.ErrorClass != ErrorClassUnknown {
		t.Fatalf("defaults = %+v, want unknown/error/unknown", e)
	}

	var nilLogger *Logger
	if err := nilLogger.Record(Event{}); err != nil {
		t.Fatalf("nil Record: %v", err)
	}
	if err := nilLogger.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}

func TestRecordAfterClose(t *testing.T) {
	l := NewWriter(&bytes.Buffer{}, Options{})
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := l.Record(Event{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Record after Close = %v, want ErrClosed", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestRecordReportsWriteError(t *testing.T) {
	var got error
	l := NewWriter(failingWriter{}, Options{
		OnError: func(err error) { got = err },
	})
	err := l.Record(Event{Method: "/m", Result: ResultSuccess})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("Record error = %v, want disk full", err)
	}
	if got == nil || !strings.Contains(got.Error(), "disk full") {
		t.Fatalf("OnError = %v, want disk full", got)
	}
}

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", want: ErrorClassNone},
		{name: "ok", err: status.Error(codes.OK, ""), want: ErrorClassNone},
		{name: "unauthenticated", err: status.Error(codes.Unauthenticated, ""), want: ErrorClassUnauthenticated},
		{name: "permission denied", err: status.Error(codes.PermissionDenied, ""), want: ErrorClassPermissionDenied},
		{name: "invalid argument", err: status.Error(codes.InvalidArgument, ""), want: "invalid_argument"},
		{name: "unavailable", err: status.Error(codes.Unavailable, ""), want: "unavailable"},
		{name: "plain error", err: errors.New("boom"), want: ErrorClassUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyError(tc.err); got != tc.want {
				t.Fatalf("ClassifyError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

type rangeRequest struct{ id uint64 }

func (r *rangeRequest) GetRangeId() uint64 { return r.id }

type mergeRequest struct{ left, right uint64 }

func (m *mergeRequest) GetLeftRangeId() uint64  { return m.left }
func (m *mergeRequest) GetRightRangeId() uint64 { return m.right }

type plainRequest struct{}

type failingResponse struct{}

func (*failingResponse) GetError() string { return "not hosted" }

type unsuccessfulResponse struct{}

func (*unsuccessfulResponse) GetSuccess() bool { return false }

type successfulResponse struct{}

func (*successfulResponse) GetSuccess() bool { return true }

func TestRequestRange(t *testing.T) {
	cases := []struct {
		name string
		req  any
		want string
	}{
		{name: "range id", req: &rangeRequest{id: 7}, want: "7"},
		{name: "zero range id", req: &rangeRequest{}, want: ""},
		{name: "merge", req: &mergeRequest{left: 3, right: 4}, want: "3-4"},
		{name: "merge left only", req: &mergeRequest{left: 3}, want: "3-0"},
		{name: "plain", req: &plainRequest{}, want: ""},
		{name: "nil", req: nil, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RequestRange(tc.req); got != tc.want {
				t.Fatalf("RequestRange(%T) = %q, want %q", tc.req, got, tc.want)
			}
		})
	}
}

func TestOutcome(t *testing.T) {
	cases := []struct {
		name       string
		resp       any
		err        error
		wantResult string
		wantClass  string
	}{
		{name: "success", resp: &successfulResponse{}, wantResult: ResultSuccess, wantClass: ErrorClassNone},
		{name: "no response", wantResult: ResultSuccess, wantClass: ErrorClassNone},
		{name: "application error", resp: &failingResponse{}, wantResult: ResultError, wantClass: ErrorClassApplication},
		{name: "application unsuccessful", resp: &unsuccessfulResponse{}, wantResult: ResultError, wantClass: ErrorClassApplication},
		{name: "transport error wins", resp: &failingResponse{}, err: status.Error(codes.Unavailable, ""), wantResult: ResultError, wantClass: "unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, class := Outcome(tc.resp, tc.err)
			if result != tc.wantResult || class != tc.wantClass {
				t.Fatalf("Outcome = (%q, %q), want (%q, %q)", result, class, tc.wantResult, tc.wantClass)
			}
		})
	}
}

func TestRecordNeverContainsCallerToken(t *testing.T) {
	const token = "top-secret-bearer-token"
	var buf bytes.Buffer
	l := NewWriter(&buf, Options{Clock: fixedClock})
	if err := l.Record(Event{
		Principal: "operator",
		Role:      "operator",
		Method:    "/method",
		Result:    ResultSuccess,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if strings.Contains(buf.String(), token) {
		t.Fatalf("audit record leaked a token: %s", buf.String())
	}
}

// rotationEvent returns an event whose JSON encoding is a stable size, so
// rotation tests can size MaxBytes deterministically.
func rotationEvent(principal string) Event {
	return Event{
		Time:       fixedTime,
		Principal:  principal,
		Role:       "operator",
		Method:     "/m",
		Result:     ResultSuccess,
		ErrorClass: ErrorClassNone,
	}
}

func rotationLineLen(t *testing.T) int {
	t.Helper()
	line, err := json.Marshal(rotationEvent("p0"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return len(line) + 1 // newline
}

func readAuditLines(t *testing.T, paths ...string) []Event {
	t.Helper()
	var out []Event
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
			if line == "" {
				continue
			}
			var e Event
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Fatalf("line in %s is not JSON: %v\n%s", path, err, line)
			}
			out = append(out, e)
		}
	}
	return out
}

func TestRotationKeepsRecordsAcrossBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	lineLen := rotationLineLen(t)
	l, err := Open(path, Options{
		Sync:       true,
		Clock:      fixedClock,
		MaxBytes:   int64(lineLen) + 1, // exactly one record per file
		MaxBackups: 10,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	const records = 6
	for i := 0; i < records; i++ {
		if err := l.Record(rotationEvent("p" + strconv.Itoa(i))); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	paths := []string{path}
	for i := 1; i <= records; i++ {
		paths = append(paths, path+"."+strconv.Itoa(i))
	}
	events := readAuditLines(t, paths...)
	if len(events) != records {
		t.Fatalf("retained records = %d, want %d", len(events), records)
	}
	seen := make(map[string]bool, records)
	for _, e := range events {
		seen[e.Principal] = true
	}
	for i := 0; i < records; i++ {
		if p := "p" + strconv.Itoa(i); !seen[p] {
			t.Fatalf("rotated files lost record %q; events=%+v", p, events)
		}
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("first backup missing after rotation: %v", err)
	}
}

func TestRotationCapsBackupsAndKeepsNewest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	lineLen := rotationLineLen(t)
	l, err := Open(path, Options{
		Clock:      fixedClock,
		MaxBytes:   int64(lineLen) + 1,
		MaxBackups: 2,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 6; i++ {
		if err := l.Record(rotationEvent("p" + strconv.Itoa(i))); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := readAuditLines(t, path, path+".1", path+".2")
	if len(events) != 3 {
		t.Fatalf("retained records = %d, want 3 (active + 2 backups): %+v", len(events), events)
	}
	want := map[string]bool{"p3": true, "p4": true, "p5": true}
	for _, e := range events {
		if !want[e.Principal] {
			t.Fatalf("retained an old record %q, want only p3..p5: %+v", e.Principal, events)
		}
	}
	if _, err := os.Stat(path + ".3"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup beyond MaxBackups exists: err=%v", err)
	}
}

func TestRotationZeroBackupsKeepsOnlyActiveFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	lineLen := rotationLineLen(t)
	l, err := Open(path, Options{Clock: fixedClock, MaxBytes: int64(lineLen) + 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := l.Record(rotationEvent("p" + strconv.Itoa(i))); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := readAuditLines(t, path)
	if len(events) != 1 || events[0].Principal != "p2" {
		t.Fatalf("active records = %+v, want only p2", events)
	}
	if _, err := os.Stat(path + ".1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("MaxBackups=0 kept a backup file: err=%v", err)
	}
}

func TestOpenRejectsNegativeRotationOptions(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(filepath.Join(dir, "a.jsonl"), Options{MaxBytes: -1}); err == nil {
		t.Fatal("Open accepted negative MaxBytes")
	}
	if _, err := Open(filepath.Join(dir, "b.jsonl"), Options{MaxBytes: 1, MaxBackups: -1}); err == nil {
		t.Fatal("Open accepted negative MaxBackups")
	}
}

func TestNewWriterIgnoresRotationOptions(t *testing.T) {
	var buf bytes.Buffer
	l := NewWriter(&buf, Options{MaxBytes: 1, MaxBackups: 1, Clock: fixedClock})
	for i := 0; i < 5; i++ {
		if err := l.Record(rotationEvent("p0")); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if got := strings.Count(buf.String(), "\n"); got != 5 {
		t.Fatalf("NewWriter rotated a non-file sink: %d lines, want 5", got)
	}
}

func TestHealthyTracksClosedLogger(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "audit.jsonl"), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Healthy(); err != nil {
		t.Fatalf("Healthy open logger = %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := l.Healthy(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Healthy closed logger = %v, want ErrClosed", err)
	}
	var nilLogger *Logger
	if err := nilLogger.Healthy(); err != nil {
		t.Fatalf("nil Healthy = %v", err)
	}
}

func TestRotationReopenFailureMarksLoggerBroken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	var reported error
	l, err := Open(path, Options{
		Clock:      fixedClock,
		MaxBytes:   1,
		MaxBackups: 1,
		OnError:    func(err error) { reported = err },
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Record(rotationEvent("p0")); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	// Point the logger at an unopenable path so the next rotation close
	// succeeds but the reopen fails.
	l.mu.Lock()
	l.path = filepath.Join(t.TempDir(), "missing", "audit.jsonl")
	l.mu.Unlock()

	if err := l.Record(rotationEvent("p1")); err == nil {
		t.Fatal("Record succeeded after the rotated file could not be reopened")
	}
	if reported == nil {
		t.Fatal("OnError was not called for the rotation failure")
	}
	if err := l.Healthy(); err == nil {
		t.Fatal("Healthy reports a broken logger as healthy")
	}
	// Every later record is rejected with the latched error; audit must not
	// silently resume on a different file.
	if err := l.Record(rotationEvent("p2")); err == nil {
		t.Fatal("Record succeeded after the logger was marked broken")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
