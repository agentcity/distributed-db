package kv

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"distributed-db/internal/model"
)

// The Raft package is not present yet; assert the method set locally so a
// signature drift breaks the build.
type stateMachine interface {
	Apply(model.Entry) error
}

var _ stateMachine = (*Store)(nil)

func newTestStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%q): %v", dir, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustApply(t *testing.T, s *Store, entry model.Entry) {
	t.Helper()
	if err := s.Apply(entry); err != nil {
		t.Fatalf("Apply(%+v): %v", entry, err)
	}
}

func mustGet(t *testing.T, s *Store, key string) []byte {
	t.Helper()
	value, ok := s.Get(key)
	if !ok {
		t.Fatalf("Get(%q): missing", key)
	}
	return value
}

func wantValue(t *testing.T, s *Store, key, want string) {
	t.Helper()
	if got := string(mustGet(t, s, key)); got != want {
		t.Fatalf("Get(%q) = %q, want %q", key, got, want)
	}
}

// walEntries reads and decodes every record in the store's WAL, verifying the
// newline framing along the way.
func walEntries(t *testing.T, dir string) []model.Entry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, walFileName))
	if err != nil {
		t.Fatalf("read WAL: %v", err)
	}
	if len(data) == 0 {
		return nil
	}
	if data[len(data)-1] != '\n' {
		t.Fatalf("WAL does not end with a newline: %q", data)
	}
	lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	entries := make([]model.Entry, 0, len(lines))
	for i, line := range lines {
		var entry model.Entry
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decode WAL record %d (%q): %v", i, line, err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestOpenCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "kv")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat store dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, walFileName)); err != nil {
		t.Fatalf("stat WAL: %v", err)
	}
	if s.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", s.Len())
	}

	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") returned nil error")
	}
}

func TestPutGetDelete(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	if _, ok := s.Get("missing"); ok {
		t.Fatal("Get(missing) reported a value")
	}

	mustApply(t, s, model.Entry{Type: model.CommandPut, Key: "alpha", Value: []byte("one")})
	wantValue(t, s, "alpha", "one")
	if s.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", s.Len())
	}

	// A put with a nil value is still a present key.
	mustApply(t, s, model.Entry{Type: model.CommandPut, Key: "nil"})
	if value, ok := s.Get("nil"); !ok || value != nil {
		t.Fatalf("Get(nil) = %v, %v; want nil, true", value, ok)
	}
	if s.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", s.Len())
	}

	mustApply(t, s, model.Entry{Type: model.CommandDelete, Key: "alpha"})
	if _, ok := s.Get("alpha"); ok {
		t.Fatal("deleted key still present")
	}
	if s.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", s.Len())
	}

	// Delete is idempotent, including for keys that were never written.
	for i := 0; i < 3; i++ {
		mustApply(t, s, model.Entry{Type: model.CommandDelete, Key: "alpha"})
	}
	mustApply(t, s, model.Entry{Type: model.CommandDelete, Key: "never-existed"})
	if s.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", s.Len())
	}
}

func TestOverwrite(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	for i := 0; i < 5; i++ {
		mustApply(t, s, model.Entry{
			Type:  model.CommandPut,
			Key:   "key",
			Value: []byte(strconv.Itoa(i)),
		})
		wantValue(t, s, "key", strconv.Itoa(i))
	}
	if s.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", s.Len())
	}
}

func TestPutAndGetCopySemantics(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	source := []byte("original")
	mustApply(t, s, model.Entry{Type: model.CommandPut, Key: "k", Value: source})

	// Mutating the caller's slice after Apply must not affect the store.
	source[0] = 'X'
	wantValue(t, s, "k", "original")

	// Mutating a returned slice must not affect the store.
	first, ok := s.Get("k")
	if !ok {
		t.Fatal("Get(k): missing")
	}
	first[0] = 'Y'
	wantValue(t, s, "k", "original")

	second, _ := s.Get("k")
	if &second[0] == &first[0] {
		t.Fatal("Get returned an aliased slice")
	}
}

func TestReopenPersistence(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)

	mustApply(t, s, model.Entry{Type: model.CommandPut, Key: "a", Value: []byte("1")})
	mustApply(t, s, model.Entry{Type: model.CommandPut, Key: "b", Value: []byte("2")})
	mustApply(t, s, model.Entry{Type: model.CommandDelete, Key: "a"})
	mustApply(t, s, model.Entry{Type: model.CommandPut, Key: "c", Value: []byte("3")})
	mustApply(t, s, model.Entry{Type: model.CommandPut, Key: "b", Value: []byte("22")})
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := newTestStore(t, dir)
	if reopened.Len() != 2 {
		t.Fatalf("Len() after reopen = %d, want 2", reopened.Len())
	}
	if _, ok := reopened.Get("a"); ok {
		t.Fatal("deleted key a came back after reopen")
	}
	wantValue(t, reopened, "b", "22")
	wantValue(t, reopened, "c", "3")

	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	again := newTestStore(t, dir)
	if again.Len() != 2 {
		t.Fatalf("Len() after second reopen = %d, want 2", again.Len())
	}
	wantValue(t, again, "b", "22")
}

func TestBinaryAndUnicodeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)

	key := "clé-鍵-🔑"
	value := []byte{0x00, 0xff, 0x0a, 0x0d, 0x7b, 0x22, 0x80}
	mustApply(t, s, model.Entry{Type: model.CommandPut, Key: key, Value: value})
	if got := mustGet(t, s, key); !bytes.Equal(got, value) {
		t.Fatalf("Get(%q) = %v, want %v", key, got, value)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := newTestStore(t, dir)
	if got := mustGet(t, reopened, key); !bytes.Equal(got, value) {
		t.Fatalf("Get(%q) after reopen = %v, want %v", key, got, value)
	}
}

func TestTornLastRecord(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)
	mustApply(t, s, model.Entry{Type: model.CommandPut, Key: "kept", Value: []byte("value")})
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	walPath := filepath.Join(dir, walFileName)
	before, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat WAL: %v", err)
	}
	torn := `{"term":2,"index":2,"type":"put","key":"torn","value":"eA==`
	f, err := os.OpenFile(walPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open WAL for append: %v", err)
	}
	if _, err := f.WriteString(torn); err != nil {
		t.Fatalf("write torn record: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync torn record: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close WAL: %v", err)
	}

	reopened := newTestStore(t, dir)
	wantValue(t, reopened, "kept", "value")
	if _, ok := reopened.Get("torn"); ok {
		t.Fatal("torn record was applied")
	}
	if reopened.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", reopened.Len())
	}

	// The torn bytes must have been truncated, and later appends must produce
	// a WAL that replays cleanly.
	data, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("read WAL: %v", err)
	}
	if bytes.Contains(data, []byte(`"torn"`)) {
		t.Fatalf("torn record still present in WAL: %q", data)
	}
	after, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat WAL: %v", err)
	}
	if after.Size() >= before.Size()+int64(len(torn)) {
		t.Fatalf("WAL was not truncated: size %d", after.Size())
	}

	mustApply(t, reopened, model.Entry{Type: model.CommandPut, Key: "after", Value: []byte("ok")})
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	final := newTestStore(t, dir)
	if final.Len() != 2 {
		t.Fatalf("Len() after recovery = %d, want 2", final.Len())
	}
	wantValue(t, final, "kept", "value")
	wantValue(t, final, "after", "ok")
	if got := walEntries(t, dir); len(got) != 2 {
		t.Fatalf("WAL has %d records, want 2", len(got))
	}
}

func TestCorruptWALRecordRejected(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, walFileName)
	content := "{\"type\":\"put\",\"key\":\"a\",\"value\":\"YQ==\"}\n" + "this is not json\n"
	if err := os.WriteFile(walPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write WAL: %v", err)
	}

	if _, err := Open(dir); err == nil {
		t.Fatal("Open accepted a corrupt WAL record")
	}

	// A syntactically valid record with an unknown command is also rejected.
	badCommand := "{\"type\":\"teleport\",\"key\":\"a\"}\n"
	if err := os.WriteFile(walPath, []byte(badCommand), 0o644); err != nil {
		t.Fatalf("write WAL: %v", err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("Open accepted an unknown command type from the WAL")
	}
}

func TestNoopAndUnknownCommand(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)

	mustApply(t, s, model.Entry{Type: model.CommandNoop})
	mustApply(t, s, model.Entry{Type: model.CommandNoop, Key: "ignored", Value: []byte("x")})
	if s.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", s.Len())
	}
	if records := walEntries(t, dir); len(records) != 0 {
		t.Fatalf("noop wrote %d WAL records, want 0", len(records))
	}

	err := s.Apply(model.Entry{Type: model.CommandType("teleport"), Key: "x", Value: []byte("y")})
	if err == nil {
		t.Fatal("Apply accepted an unknown command type")
	}
	if !strings.Contains(err.Error(), "unknown command type") {
		t.Fatalf("error = %v, want it to mention the unknown command type", err)
	}
	if _, ok := s.Get("x"); ok {
		t.Fatal("unknown command mutated the map")
	}
	if records := walEntries(t, dir); len(records) != 0 {
		t.Fatalf("unknown command wrote %d WAL records, want 0", len(records))
	}
}

func TestCloseIdempotent(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustApply(t, s, model.Entry{Type: model.CommandPut, Key: "k", Value: []byte("v")})

	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := s.Apply(model.Entry{Type: model.CommandPut, Key: "k2", Value: []byte("v2")}); !errors.Is(err, errClosed) {
		t.Fatalf("Apply after Close = %v, want %v", err, errClosed)
	}
	// Reads are still served from the in-memory map after Close.
	wantValue(t, s, "k", "v")
	if s.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", s.Len())
	}
}

func TestConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)

	const (
		writers = 4
		readers = 4
		ops     = 25
		keys    = 6
	)

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				key := fmt.Sprintf("key-%d", (w+i)%keys)
				if i%3 == 2 {
					if err := s.Apply(model.Entry{Type: model.CommandDelete, Key: key}); err != nil {
						t.Errorf("concurrent delete %q: %v", key, err)
						return
					}
					continue
				}
				value := []byte(fmt.Sprintf("w%d-i%d", w, i))
				if err := s.Apply(model.Entry{Type: model.CommandPut, Key: key, Value: value}); err != nil {
					t.Errorf("concurrent put %q: %v", key, err)
					return
				}
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops*4; i++ {
				key := fmt.Sprintf("key-%d", i%keys)
				if value, ok := s.Get(key); ok && len(value) == 0 {
					t.Errorf("Get(%q) returned an empty value for a present key", key)
					return
				}
				_ = s.Len()
			}
		}()
	}
	wg.Wait()

	want := make(map[string]string)
	for i := 0; i < keys; i++ {
		key := fmt.Sprintf("key-%d", i)
		if value, ok := s.Get(key); ok {
			want[key] = string(value)
		}
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := newTestStore(t, dir)
	if reopened.Len() != len(want) {
		t.Fatalf("Len() after reopen = %d, want %d", reopened.Len(), len(want))
	}
	for key, value := range want {
		wantValue(t, reopened, key, value)
	}
}

func TestCompaction(t *testing.T) {
	oldThreshold := compactThreshold
	compactThreshold = 4
	t.Cleanup(func() { compactThreshold = oldThreshold })

	dir := t.TempDir()
	s := newTestStore(t, dir)

	const applies = 21
	lastValues := make(map[string]string)
	for i := 0; i < applies; i++ {
		key := fmt.Sprintf("key-%d", i%3)
		value := strings.Repeat("v", 64) + strconv.Itoa(i)
		mustApply(t, s, model.Entry{Type: model.CommandPut, Key: key, Value: []byte(value)})
		lastValues[key] = value
	}

	records := walEntries(t, dir)
	if len(records) > compactThreshold {
		t.Fatalf("WAL has %d records after compaction, want <= %d", len(records), compactThreshold)
	}
	if len(records) != s.Len() {
		t.Fatalf("compacted WAL has %d records, live map has %d keys", len(records), s.Len())
	}

	// The snapshot must describe exactly the live state.
	seen := make(map[string]bool, len(records))
	for _, record := range records {
		if record.Type != model.CommandPut {
			t.Fatalf("snapshot contains a %q record", record.Type)
		}
		value, ok := s.Get(record.Key)
		if !ok {
			t.Fatalf("snapshot contains non-live key %q", record.Key)
		}
		if !bytes.Equal(value, record.Value) {
			t.Fatalf("snapshot value for %q = %q, live value = %q", record.Key, record.Value, value)
		}
		if seen[record.Key] {
			t.Fatalf("snapshot contains key %q more than once", record.Key)
		}
		seen[record.Key] = true
	}
	for key := range lastValues {
		if !seen[key] {
			t.Fatalf("snapshot is missing live key %q", key)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, walTmpFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("compaction temp file still exists (stat err: %v)", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := newTestStore(t, dir)
	if reopened.Len() != len(lastValues) {
		t.Fatalf("Len() after reopen = %d, want %d", reopened.Len(), len(lastValues))
	}
	for key, value := range lastValues {
		wantValue(t, reopened, key, value)
	}
}
