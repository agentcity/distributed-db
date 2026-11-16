package mvcc

import (
	"errors"
	"testing"

	"distributed-db/internal/storage"
)

// openTestEngineAt opens a real Pebble-backed engine in dir. Tests use this
// instead of the fake wherever failure injection is not required.
func openTestEngineAt(t *testing.T, dir string) storage.Engine {
	t.Helper()
	engine, err := storage.Open(dir, storage.Options{
		CacheSize:    8 << 20,
		MemTableSize: 1 << 20,
		Compression:  "none",
	})
	if err != nil {
		t.Fatalf("storage.Open(%q): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Errorf("engine.Close: %v", err)
		}
	})
	return engine
}

func newTestStoreWithEngine(t *testing.T) (*Store, storage.Engine) {
	t.Helper()
	engine := openTestEngineAt(t, t.TempDir())
	return New(engine), engine
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, _ := newTestStoreWithEngine(t)
	return store
}

func putValue(t *testing.T, store *Store, key, value string, startTS, commitTS uint64) {
	t.Helper()
	mutation := Mutation{Key: []byte(key), Value: []byte(value)}
	if err := store.Prewrite([]Mutation{mutation}, []byte(key), startTS); err != nil {
		t.Fatalf("Prewrite(%q, startTS=%d): %v", key, startTS, err)
	}
	if err := store.Commit([][]byte{[]byte(key)}, startTS, commitTS); err != nil {
		t.Fatalf("Commit(%q, startTS=%d, commitTS=%d): %v", key, startTS, commitTS, err)
	}
}

func deleteValue(t *testing.T, store *Store, key string, startTS, commitTS uint64) {
	t.Helper()
	mutation := Mutation{Key: []byte(key), Delete: true}
	if err := store.Prewrite([]Mutation{mutation}, []byte(key), startTS); err != nil {
		t.Fatalf("Prewrite(delete %q, startTS=%d): %v", key, startTS, err)
	}
	if err := store.Commit([][]byte{[]byte(key)}, startTS, commitTS); err != nil {
		t.Fatalf("Commit(delete %q, startTS=%d, commitTS=%d): %v", key, startTS, commitTS, err)
	}
}

func requireValue(t *testing.T, store *Store, key string, ts uint64, want string) {
	t.Helper()
	got, err := store.Get([]byte(key), ts)
	if err != nil {
		t.Fatalf("Get(%q, ts=%d): unexpected error %v", key, ts, err)
	}
	if string(got) != want {
		t.Fatalf("Get(%q, ts=%d) = %q, want %q", key, ts, got, want)
	}
}

func requireKeyNotFound(t *testing.T, store *Store, key string, ts uint64) {
	t.Helper()
	got, err := store.Get([]byte(key), ts)
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Get(%q, ts=%d) = %q, %v; want ErrKeyNotFound", key, ts, got, err)
	}
}

func requireErrIs(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error = %v, want errors.Is(_, %v)", err, target)
	}
}

func engineHas(t *testing.T, engine storage.Engine, key []byte) bool {
	t.Helper()
	_, err := engine.Get(key)
	if err == nil {
		return true
	}
	if errors.Is(err, storage.ErrNotFound) {
		return false
	}
	t.Fatalf("engine.Get(%q): %v", key, err)
	return false
}

func scanKeys(t *testing.T, store *Store, start, end []byte, ts uint64, limit int) []string {
	t.Helper()
	kvs, err := store.Scan(start, end, ts, limit)
	if err != nil {
		t.Fatalf("Scan(%q, %q, ts=%d, limit=%d): %v", start, end, ts, limit, err)
	}
	keys := make([]string, len(kvs))
	for i, kv := range kvs {
		keys[i] = string(kv.Key)
	}
	return keys
}

func requireKeys(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("keys = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keys = %q, want %q", got, want)
		}
	}
}
