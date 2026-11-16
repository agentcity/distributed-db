package kvctl

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

type fakeBackend struct {
	data    map[string][]byte
	closed  bool
	txnCall int
	failTxn error
}

func newFake() *fakeBackend { return &fakeBackend{data: map[string][]byte{}} }

func (f *fakeBackend) Put(_ context.Context, key, value []byte) error {
	f.data[string(key)] = append([]byte(nil), value...)
	return nil
}

func (f *fakeBackend) Get(_ context.Context, key []byte) ([]byte, error) {
	v, ok := f.data[string(key)]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

func (f *fakeBackend) Delete(_ context.Context, key []byte) error {
	delete(f.data, string(key))
	return nil
}

func (f *fakeBackend) Scan(_ context.Context, start, end []byte, limit int) ([]KV, error) {
	var keys []string
	for k := range f.data {
		if string(start) != "" && k < string(start) {
			continue
		}
		if string(end) != "" && k >= string(end) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	out := make([]KV, 0, len(keys))
	for _, k := range keys {
		out = append(out, KV{Key: []byte(k), Value: append([]byte(nil), f.data[k]...)})
	}
	return out, nil
}

func (f *fakeBackend) Txn(_ context.Context, ops []Op) error {
	f.txnCall++
	if f.failTxn != nil {
		return f.failTxn
	}
	next := make(map[string][]byte, len(f.data))
	for k, v := range f.data {
		next[k] = v
	}
	for _, op := range ops {
		if op.Delete {
			delete(next, string(op.Key))
		} else {
			next[string(op.Key)] = append([]byte(nil), op.Value...)
		}
	}
	f.data = next
	return nil
}

func (f *fakeBackend) Ping(context.Context) error { return nil }
func (f *fakeBackend) Close() error               { f.closed = true; return nil }

func runCLI(t *testing.T, fake *fakeBackend, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	var got Config
	r := &Runner{
		In:  strings.NewReader(stdin),
		Out: &out,
		Err: &errOut,
		Open: func(_ context.Context, cfg Config) (Backend, error) {
			got = cfg
			return fake, nil
		},
	}
	code := r.Run(context.Background(), args)
	_ = got
	return code, out.String(), errOut.String()
}

func TestPutGetDelete(t *testing.T) {
	f := newFake()
	code, out, errOut := runCLI(t, f, "", "-nodes", "n1", "put", "a", "hello")
	if code != 0 || strings.TrimSpace(out) != "OK" || errOut != "" {
		t.Fatalf("put = code %d out %q err %q", code, out, errOut)
	}
	code, out, _ = runCLI(t, f, "", "-nodes", "n1", "get", "a")
	if code != 0 || out != "a\thello\n" {
		t.Fatalf("get = code %d out %q", code, out)
	}
	code, _, _ = runCLI(t, f, "", "-nodes", "n1", "delete", "a")
	if code != 0 {
		t.Fatalf("delete code %d", code)
	}
	code, _, errOut = runCLI(t, f, "", "-nodes", "n1", "get", "a")
	if code != 1 || !strings.Contains(errOut, "not found") {
		t.Fatalf("missing get = code %d err %q", code, errOut)
	}
}

func TestJSONAndRaw(t *testing.T) {
	f := newFake()
	f.data["a"] = []byte("hello")
	code, out, _ := runCLI(t, f, "", "-nodes", "n1", "-format", "json", "get", "a")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json: %v (%q)", err, out)
	}
	if got["key"] != "a" || got["value"] != "hello" {
		t.Fatalf("json = %#v", got)
	}
	code, out, _ = runCLI(t, f, "", "-nodes", "n1", "-format", "raw", "get", "a")
	if code != 0 || out != "hello" {
		t.Fatalf("raw = code %d out %q", code, out)
	}
}

func TestScanPrefixAndLimit(t *testing.T) {
	f := newFake()
	for _, k := range []string{"user:1", "user:2", "other:1"} {
		f.data[k] = []byte("v" + k)
	}
	code, out, _ := runCLI(t, f, "", "-nodes", "n1", "-format", "json", "scan", "-prefix", "user:",
		"-limit", "1")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	var rows []map[string]string
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("json: %v (%q)", err, out)
	}
	if len(rows) != 1 || rows[0]["key"] != "user:1" {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestTxnAtomic(t *testing.T) {
	f := newFake()
	f.data["old"] = []byte("x")
	code, out, errOut := runCLI(t, f, "put a 1\nput b two words\ndelete old\n",
		"-nodes", "n1", "txn")
	if code != 0 || !strings.Contains(out, "committed 3 operations") || errOut != "" {
		t.Fatalf("txn = code %d out %q err %q", code, out, errOut)
	}
	if _, ok := f.data["old"]; ok {
		t.Fatal("old key not deleted")
	}
	if string(f.data["b"]) != "two words" {
		t.Fatalf("b = %q", f.data["b"])
	}
}

func TestTxnParseErrorDoesNotCommit(t *testing.T) {
	f := newFake()
	code, _, errOut := runCLI(t, f, "put a 1\nbogus b\n", "-nodes", "n1", "txn")
	if code != 1 || !strings.Contains(errOut, "unknown operation") {
		t.Fatalf("code %d err %q", code, errOut)
	}
	if f.txnCall != 0 {
		t.Fatalf("Txn called %d times", f.txnCall)
	}
}

func TestHexRoundTrip(t *testing.T) {
	f := newFake()
	code, _, _ := runCLI(t, f, "", "-nodes", "n1", "-hex", "put", "6b", "76")
	if code != 0 || string(f.data["k"]) != "v" {
		t.Fatalf("hex put code %d data %#v", code, f.data)
	}
	code, out, _ := runCLI(t, f, "", "-nodes", "n1", "-hex", "-format", "raw", "get", "6b")
	if code != 0 || out != "76" {
		t.Fatalf("hex get code %d out %q", code, out)
	}
}

func TestValueFromStdinAndFile(t *testing.T) {
	f := newFake()
	code, _, _ := runCLI(t, f, "from-stdin", "-nodes", "n1", "put", "k", "-")
	if code != 0 || string(f.data["k"]) != "from-stdin" {
		t.Fatalf("stdin code %d data %#v", code, f.data)
	}
}

func TestUsageErrors(t *testing.T) {
	f := newFake()
	cases := [][]string{
		{},
		{"-nodes", "n1", "get"},
		{"-nodes", "n1", "-format", "xml", "get", "a"},
		{"get", "a"},
		{"-nodes", "n1", "put", "a"},
		{"-nodes", "n1", "unknown"},
	}
	for _, args := range cases {
		code, _, _ := runCLI(t, f, "", args...)
		if code != 2 {
			t.Errorf("args %v = code %d, want 2", args, code)
		}
	}
}

func TestEnvConfig(t *testing.T) {
	t.Setenv("KVCTL_META", "meta:1")
	t.Setenv("KVCTL_NODES", "a:1,b:2")
	f := newFake()
	f.data["k"] = []byte("v")
	var got Config
	r := &Runner{
		In:  strings.NewReader(""),
		Out: ioDiscard{},
		Err: ioDiscard{},
		Open: func(_ context.Context, cfg Config) (Backend, error) {
			got = cfg
			return f, nil
		},
	}
	if code := r.Run(context.Background(), []string{"get", "k"}); code != 0 {
		t.Fatalf("code %d", code)
	}
	if got.Meta != "meta:1" || !reflect.DeepEqual(got.Nodes, []string{"a:1", "b:2"}) {
		t.Fatalf("config = %#v", got)
	}
}

func TestShell(t *testing.T) {
	f := newFake()
	code, out, _ := runCLI(t, f, "put a 1\nget a\nquit\n", "-nodes", "n1", "shell")
	if code != 0 || !strings.Contains(out, "a\t1") {
		t.Fatalf("shell code %d out %q", code, out)
	}
}

func TestPrefixEnd(t *testing.T) {
	cases := []struct {
		in, want []byte
	}{
		{[]byte("a"), []byte("b")},
		{[]byte("az"), []byte("a{")},
		{[]byte{0xff}, nil},
		{[]byte("a\xff"), []byte("b")},
	}
	for _, c := range cases {
		if got := prefixEnd(c.in); !bytes.Equal(got, c.want) {
			t.Errorf("prefixEnd(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
