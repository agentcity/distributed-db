package kvctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

type splitCall struct {
	rangeID  uint64
	splitKey []byte
}

type mergeCall struct {
	leftID  uint64
	rightID uint64
}

type fakeBackend struct {
	data    map[string][]byte
	closed  bool
	txnCall int
	failTxn error

	ranges   []RangeInfo
	listErr  error
	metaLead string
	metaTerm uint64
	metaErr  error

	splitNewID uint64
	splitErr   error
	splitCalls []splitCall

	mergeErr   error
	mergeCalls []mergeCall

	backupResult BackupResult
	backupErr    error
	backupCalls  []BackupOptions
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

func (f *fakeBackend) ListRanges(context.Context) ([]RangeInfo, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]RangeInfo(nil), f.ranges...), nil
}

func (f *fakeBackend) SplitRange(_ context.Context, rangeID uint64, splitKey []byte) (uint64, error) {
	f.splitCalls = append(f.splitCalls, splitCall{
		rangeID:  rangeID,
		splitKey: append([]byte(nil), splitKey...),
	})
	if f.splitErr != nil {
		return 0, f.splitErr
	}
	return f.splitNewID, nil
}

func (f *fakeBackend) MergeRanges(_ context.Context, leftID, rightID uint64) error {
	f.mergeCalls = append(f.mergeCalls, mergeCall{leftID: leftID, rightID: rightID})
	return f.mergeErr
}

func (f *fakeBackend) MetaLeader(context.Context) (string, uint64, error) {
	if f.metaErr != nil {
		return "", 0, f.metaErr
	}
	return f.metaLead, f.metaTerm, nil
}

func (f *fakeBackend) Backup(_ context.Context, opts BackupOptions) (BackupResult, error) {
	f.backupCalls = append(f.backupCalls, opts)
	if f.backupErr != nil {
		return BackupResult{}, f.backupErr
	}
	return f.backupResult, nil
}

type cliRun struct {
	code   int
	out    string
	errOut string
	cfg    Config
	opens  int
}

func runCLIArgs(t *testing.T, fake *fakeBackend, stdin string, args ...string) cliRun {
	t.Helper()
	var out, errOut bytes.Buffer
	var got Config
	opens := 0
	r := &Runner{
		In:  strings.NewReader(stdin),
		Out: &out,
		Err: &errOut,
		Open: func(_ context.Context, cfg Config) (Backend, error) {
			opens++
			got = cfg
			return fake, nil
		},
	}
	code := r.Run(context.Background(), args)
	return cliRun{code: code, out: out.String(), errOut: errOut.String(), cfg: got, opens: opens}
}

func runCLI(t *testing.T, fake *fakeBackend, stdin string, args ...string) (int, string, string) {
	t.Helper()
	r := runCLIArgs(t, fake, stdin, args...)
	return r.code, r.out, r.errOut
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
		{"ranges", "extra"},
		{"status", "extra"},
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

func sampleRanges() []RangeInfo {
	return []RangeInfo{
		{ID: 3, Start: []byte("s"), Leader: "n2", Replicas: []string{"n2"}, Version: 4},
		{ID: 1, End: []byte("m"), Leader: "n1", Replicas: []string{"n1", "n2"}, Version: 2},
		{ID: 2, Start: []byte("m"), End: []byte("s"), Leader: "n1", Replicas: []string{"n1", "n2", "n3"}, Version: 3},
	}
}

func TestRangesTableStableOrder(t *testing.T) {
	f := newFake()
	f.ranges = sampleRanges()
	code, out, errOut := runCLI(t, f, "", "-meta", "m1", "ranges")
	if code != 0 || errOut != "" {
		t.Fatalf("ranges = code %d err %q", code, errOut)
	}
	want := "ID\tSTART\tEND\tLEADER\tREPLICAS\tVERSION\n" +
		"1\t\tm\tn1\tn1,n2\t2\n" +
		"2\tm\ts\tn1\tn1,n2,n3\t3\n" +
		"3\ts\t\tn2\tn2\t4\n"
	if out != want {
		t.Fatalf("ranges table =\n%q\nwant\n%q", out, want)
	}
}

func TestRangesJSON(t *testing.T) {
	f := newFake()
	f.ranges = sampleRanges()
	code, out, _ := runCLI(t, f, "", "-format", "json", "ranges")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	var got []rangeJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json: %v (%q)", err, out)
	}
	want := []rangeJSON{
		{ID: 1, Start: "", End: "m", Leader: "n1", Replicas: []string{"n1", "n2"}, Version: 2},
		{ID: 2, Start: "m", End: "s", Leader: "n1", Replicas: []string{"n1", "n2", "n3"}, Version: 3},
		{ID: 3, Start: "s", End: "", Leader: "n2", Replicas: []string{"n2"}, Version: 4},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ranges json = %#v, want %#v", got, want)
	}
}

func TestRangesJSONUsesEmptyReplicaArray(t *testing.T) {
	f := newFake()
	f.ranges = []RangeInfo{{ID: 1, Version: 1}}
	_, out, _ := runCLI(t, f, "", "-format", "json", "ranges")
	if !strings.Contains(out, `"replicas":[]`) {
		t.Fatalf("replicas should encode as [], got %q", out)
	}
}

func TestRangesEmpty(t *testing.T) {
	f := newFake()
	code, out, _ := runCLI(t, f, "", "ranges")
	if code != 0 || out != "ID\tSTART\tEND\tLEADER\tREPLICAS\tVERSION\n" {
		t.Fatalf("empty table = code %d out %q", code, out)
	}
	code, out, _ = runCLI(t, f, "", "-format", "json", "ranges")
	if code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty json = code %d out %q", code, out)
	}
}

func TestRangesMetaOnlyAndError(t *testing.T) {
	f := newFake()
	f.ranges = sampleRanges()
	r := runCLIArgs(t, f, "", "-meta", "m1", "ranges")
	if r.code != 0 || r.opens != 1 {
		t.Fatalf("meta-only ranges = code %d opens %d", r.code, r.opens)
	}
	if len(r.cfg.Nodes) != 0 || r.cfg.Meta != "m1" {
		t.Fatalf("config = %#v", r.cfg)
	}

	f.listErr = errors.New("meta down")
	code, _, errOut := runCLI(t, f, "", "-meta", "m1", "ranges")
	if code != 1 || !strings.Contains(errOut, "ranges: meta down") {
		t.Fatalf("ranges error = code %d err %q", code, errOut)
	}
}

func TestAdminRawFormatRejected(t *testing.T) {
	f := newFake()
	for _, args := range [][]string{
		{"-format", "raw", "ranges"},
		{"-format", "raw", "status"},
		{"-format", "raw", "split", "1", "m"},
		{"-format", "raw", "merge", "1", "2"},
		{"-format", "raw", "-nodes", "n1", "backup", "-out", "dir"},
	} {
		if code, _, errOut := runCLI(t, f, "", args...); code != 2 || !strings.Contains(errOut, "not supported") {
			t.Errorf("args %v = code %d err %q", args, code, errOut)
		}
	}
	if len(f.splitCalls) != 0 || len(f.mergeCalls) != 0 || len(f.backupCalls) != 0 {
		t.Fatal("rejected format still invoked an admin backend method")
	}
}

func TestStatusJSON(t *testing.T) {
	f := newFake()
	f.ranges = []RangeInfo{
		{ID: 1, Leader: "n1"},
		{ID: 2, Leader: "n1"},
		{ID: 3, Leader: "n2"},
		{ID: 4},
	}
	f.metaLead = "m1"
	f.metaTerm = 5
	code, out, errOut := runCLI(t, f, "", "-meta", "m1", "-nodes", "n1,n2", "-format", "json", "status")
	if code != 0 || errOut != "" {
		t.Fatalf("status = code %d err %q", code, errOut)
	}
	var got statusJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json: %v (%q)", err, out)
	}
	want := statusJSON{
		MetaLeader: "m1", MetaTerm: 5, Ranges: 4,
		Leaders: map[string]int{"n1": 2, "n2": 1},
		Meta:    "m1", Nodes: []string{"n1", "n2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("status json = %#v, want %#v", got, want)
	}
}

func TestStatusTable(t *testing.T) {
	f := newFake()
	f.ranges = []RangeInfo{{ID: 1, Leader: "n1"}, {ID: 2, Leader: "n2"}, {ID: 3, Leader: "n1"}}
	f.metaLead = "m1"
	f.metaTerm = 5
	code, out, _ := runCLI(t, f, "", "-meta", "m1", "-nodes", "n1,n2", "status")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	want := "META_LEADER\tTERM\tRANGES\tMETA\tNODES\n" +
		"m1\t5\t3\tm1\tn1,n2\n" +
		"LEADER\tRANGES\n" +
		"n1\t2\n" +
		"n2\t1\n"
	if out != want {
		t.Fatalf("status table =\n%q\nwant\n%q", out, want)
	}
}

func TestStatusMetaOnlyAndErrors(t *testing.T) {
	f := newFake()
	f.metaLead = "m1"
	f.metaTerm = 2
	r := runCLIArgs(t, f, "", "-meta", "m1", "status")
	if r.code != 0 || r.opens != 1 || len(r.cfg.Nodes) != 0 {
		t.Fatalf("meta-only status = code %d opens %d cfg %#v", r.code, r.opens, r.cfg)
	}

	f.metaErr = errors.New("meta down")
	code, _, errOut := runCLI(t, f, "", "-meta", "m1", "status")
	if code != 1 || !strings.Contains(errOut, "status: meta down") {
		t.Fatalf("status error = code %d err %q", code, errOut)
	}

	f.metaErr = nil
	f.listErr = errors.New("list failed")
	code, _, errOut = runCLI(t, f, "", "-meta", "m1", "status")
	if code != 1 || !strings.Contains(errOut, "status: list failed") {
		t.Fatalf("status list error = code %d err %q", code, errOut)
	}
}

func TestSplit(t *testing.T) {
	f := newFake()
	f.splitNewID = 9
	code, out, errOut := runCLI(t, f, "", "-meta", "m1", "split", "4", "m")
	if code != 0 || out != "new range id: 9\n" || errOut != "" {
		t.Fatalf("split = code %d out %q err %q", code, out, errOut)
	}
	want := []splitCall{{rangeID: 4, splitKey: []byte("m")}}
	if !reflect.DeepEqual(f.splitCalls, want) {
		t.Fatalf("split calls = %#v, want %#v", f.splitCalls, want)
	}
}

func TestSplitJSONAndHexKey(t *testing.T) {
	f := newFake()
	f.splitNewID = 9
	code, out, _ := runCLI(t, f, "", "-hex", "-format", "json", "split", "4", "6d")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	var got splitJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json: %v (%q)", err, out)
	}
	want := splitJSON{Status: "ok", RangeID: 4, SplitKey: "6d", NewRangeID: 9}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("split json = %#v, want %#v", got, want)
	}
	if len(f.splitCalls) != 1 || string(f.splitCalls[0].splitKey) != "m" {
		t.Fatalf("split calls = %#v", f.splitCalls)
	}
}

func TestSplitServerError(t *testing.T) {
	f := newFake()
	f.splitErr = errors.New("split range 4: range not found")
	code, out, errOut := runCLI(t, f, "", "-meta", "m1", "split", "4", "m")
	if code != 1 || out != "" || !strings.Contains(errOut, "split: split range 4: range not found") {
		t.Fatalf("split error = code %d out %q err %q", code, out, errOut)
	}
	if len(f.splitCalls) != 1 {
		t.Fatalf("split calls = %d, want 1", len(f.splitCalls))
	}
}

func TestSplitBadArgs(t *testing.T) {
	f := newFake()
	for _, args := range [][]string{
		{"split"},
		{"split", "4"},
		{"split", "4", "m", "extra"},
		{"split", "abc", "m"},
		{"split", "0", "m"},
		{"split", "-1", "m"},
		{"split", "4", ""},
	} {
		code, _, _ := runCLI(t, f, "", args...)
		if code != 2 {
			t.Errorf("args %v = code %d, want 2", args, code)
		}
	}
	if len(f.splitCalls) != 0 {
		t.Fatalf("bad args still called SplitRange %#v", f.splitCalls)
	}
}

func TestMerge(t *testing.T) {
	f := newFake()
	code, out, errOut := runCLI(t, f, "", "-meta", "m1", "merge", "2", "3")
	if code != 0 || out != "OK\n" || errOut != "" {
		t.Fatalf("merge = code %d out %q err %q", code, out, errOut)
	}
	want := []mergeCall{{leftID: 2, rightID: 3}}
	if !reflect.DeepEqual(f.mergeCalls, want) {
		t.Fatalf("merge calls = %#v, want %#v", f.mergeCalls, want)
	}
}

func TestMergeJSON(t *testing.T) {
	f := newFake()
	code, out, _ := runCLI(t, f, "", "-format", "json", "merge", "2", "3")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	var got mergeJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json: %v (%q)", err, out)
	}
	want := mergeJSON{Status: "ok", LeftRangeID: 2, RightRangeID: 3}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge json = %#v, want %#v", got, want)
	}
}

func TestMergeServerError(t *testing.T) {
	f := newFake()
	f.mergeErr = errors.New("merge: ranges 2 and 3 are not adjacent")
	code, out, errOut := runCLI(t, f, "", "-meta", "m1", "merge", "2", "3")
	if code != 1 || out != "" || !strings.Contains(errOut, "not adjacent") {
		t.Fatalf("merge error = code %d out %q err %q", code, out, errOut)
	}
	if len(f.mergeCalls) != 1 {
		t.Fatalf("merge calls = %d, want 1", len(f.mergeCalls))
	}
}

func TestMergeBadArgs(t *testing.T) {
	f := newFake()
	for _, args := range [][]string{
		{"merge"},
		{"merge", "2"},
		{"merge", "2", "3", "4"},
		{"merge", "x", "3"},
		{"merge", "2", "y"},
		{"merge", "0", "3"},
	} {
		code, _, _ := runCLI(t, f, "", args...)
		if code != 2 {
			t.Errorf("args %v = code %d, want 2", args, code)
		}
	}
	if len(f.mergeCalls) != 0 {
		t.Fatalf("bad args still called MergeRanges %#v", f.mergeCalls)
	}
}

func TestBackup(t *testing.T) {
	f := newFake()
	f.backupResult = BackupResult{OutDir: "outdir", SnapshotTS: 42, Keys: 5, Bytes: 100, Ranges: 2}
	code, out, errOut := runCLI(t, f, "",
		"-nodes", "n1",
		"backup", "-out", "outdir", "-chunk-keys", "7", "-chunk-bytes", "1024")
	if code != 0 || errOut != "" {
		t.Fatalf("backup = code %d err %q", code, errOut)
	}
	wantOut := "backup complete: outdir\nsnapshot_ts=42 keys=5 bytes=100 ranges=2\n"
	if out != wantOut {
		t.Fatalf("backup out = %q, want %q", out, wantOut)
	}
	wantOpts := []BackupOptions{{OutDir: "outdir", ChunkKeys: 7, ChunkBytes: 1024}}
	if !reflect.DeepEqual(f.backupCalls, wantOpts) {
		t.Fatalf("backup calls = %#v, want %#v", f.backupCalls, wantOpts)
	}
}

func TestBackupJSONAndError(t *testing.T) {
	f := newFake()
	f.backupResult = BackupResult{OutDir: "outdir", SnapshotTS: 42, Keys: 5, Bytes: 100, Ranges: 2}
	code, out, _ := runCLI(t, f, "", "-format", "json", "-nodes", "n1", "backup", "-out", "outdir")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	var got backupJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json: %v (%q)", err, out)
	}
	want := backupJSON{Dir: "outdir", SnapshotTS: 42, Keys: 5, Bytes: 100, Ranges: 2}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("backup json = %#v, want %#v", got, want)
	}

	f.backupErr = errors.New("backup: output directory outdir already exists")
	code, out, errOut := runCLI(t, f, "", "-nodes", "n1", "backup", "-out", "outdir")
	if code != 1 || out != "" || !strings.Contains(errOut, "backup: backup: output directory") {
		t.Fatalf("backup error = code %d out %q err %q", code, out, errOut)
	}
}

func TestBackupFlagValidation(t *testing.T) {
	f := newFake()
	for _, args := range [][]string{
		{"-nodes", "n1", "backup"},
		{"-nodes", "n1", "backup", "-out", "dir", "extra"},
		{"-nodes", "n1", "backup", "-out", "dir", "-chunk-keys", "-1"},
		{"-nodes", "n1", "backup", "-out", "dir", "-chunk-bytes", "-1"},
		{"-nodes", "n1", "backup", "-out", "dir", "-bogus"},
	} {
		code, _, _ := runCLI(t, f, "", args...)
		if code != 2 {
			t.Errorf("args %v = code %d, want 2", args, code)
		}
	}
	if len(f.backupCalls) != 0 {
		t.Fatalf("invalid flags still called Backup %#v", f.backupCalls)
	}
}

func TestBackupDefaultTimeoutIsLonger(t *testing.T) {
	f := newFake()
	f.backupResult = BackupResult{OutDir: "dir"}
	r := runCLIArgs(t, f, "", "-nodes", "n1", "backup", "-out", "dir")
	if r.code != 0 || r.cfg.Timeout != 10*time.Minute {
		t.Fatalf("backup default timeout = %s (code %d)", r.cfg.Timeout, r.code)
	}
	r = runCLIArgs(t, f, "", "-timeout", "3s", "-nodes", "n1", "backup", "-out", "dir")
	if r.code != 0 || r.cfg.Timeout != 3*time.Second {
		t.Fatalf("backup explicit timeout = %s (code %d)", r.cfg.Timeout, r.code)
	}
}

func TestBackupRequiresNodes(t *testing.T) {
	f := newFake()
	r := runCLIArgs(t, f, "", "-meta", "m1", "backup", "-out", "dir")
	if r.code != 2 || r.opens != 0 || !strings.Contains(r.errOut, "-nodes") {
		t.Fatalf("backup without nodes = code %d opens %d err %q", r.code, r.opens, r.errOut)
	}
	if len(f.backupCalls) != 0 {
		t.Fatalf("backup ran without nodes: %#v", f.backupCalls)
	}
}

func TestDataCommandsRequireNodes(t *testing.T) {
	f := newFake()
	cases := [][]string{
		{"get", "a"},
		{"put", "a", "b"},
		{"delete", "a"},
		{"scan"},
		{"txn"},
		{"ping"},
		{"shell"},
	}
	for _, args := range cases {
		r := runCLIArgs(t, f, "quit\n", args...)
		if r.code != 2 || r.opens != 0 || !strings.Contains(r.errOut, "-nodes") {
			t.Errorf("args %v = code %d opens %d err %q", args, r.code, r.opens, r.errOut)
		}
	}
}

func TestMetaOnlyCommandsAllowNoNodes(t *testing.T) {
	f := newFake()
	f.splitNewID = 9
	cases := [][]string{
		{"ranges"},
		{"status"},
		{"split", "1", "m"},
		{"merge", "1", "2"},
	}
	for _, args := range cases {
		r := runCLIArgs(t, f, "", args...)
		if r.code != 0 || r.opens != 1 || len(r.cfg.Nodes) != 0 {
			t.Errorf("args %v = code %d opens %d cfg %#v", args, r.code, r.opens, r.cfg)
		}
	}
}

func TestShellRejectsAdminCommands(t *testing.T) {
	f := newFake()
	code, out, errOut := runCLI(t, f, "ranges\nbackup -out dir\nquit\n", "-nodes", "n1", "shell")
	if code != 0 {
		t.Fatalf("shell code %d", code)
	}
	if !strings.Contains(errOut, "ranges is not available in the shell") ||
		!strings.Contains(errOut, "backup is not available in the shell") {
		t.Fatalf("shell errors = %q", errOut)
	}
	if strings.Contains(out, "ID\tSTART") {
		t.Fatalf("shell ran ranges: %q", out)
	}
	if len(f.backupCalls) != 0 {
		t.Fatalf("shell ran backup: %#v", f.backupCalls)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
