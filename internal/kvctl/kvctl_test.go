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

	setRangesErr   error
	setRangesCalls [][]RangeInfo

	splitNewID uint64
	splitErr   error
	splitCalls []splitCall

	mergeErr   error
	mergeCalls []mergeCall

	backupResult BackupResult
	backupErr    error
	backupCalls  []BackupOptions

	decommissionResult DecommissionResult
	decommissionErr    error
	decommissionCalls  []DecommissionOptions
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

func (f *fakeBackend) SetRanges(_ context.Context, ranges []RangeInfo) error {
	if f.setRangesErr != nil {
		return f.setRangesErr
	}
	copied := make([]RangeInfo, len(ranges))
	for i, r := range ranges {
		copied[i] = r
		copied[i].Start = append([]byte(nil), r.Start...)
		copied[i].End = append([]byte(nil), r.End...)
		copied[i].Replicas = append([]string(nil), r.Replicas...)
	}
	f.setRangesCalls = append(f.setRangesCalls, copied)
	f.ranges = copied
	return nil
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

func (f *fakeBackend) Decommission(_ context.Context, opts DecommissionOptions) (DecommissionResult, error) {
	f.decommissionCalls = append(f.decommissionCalls, opts)
	if f.decommissionErr != nil {
		return DecommissionResult{}, f.decommissionErr
	}
	return f.decommissionResult, nil
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

func TestTLSConfigForwarded(t *testing.T) {
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
	code := r.Run(context.Background(), []string{
		"-nodes", "n1", "-tls-ca", "ca.pem", "-tls-cert", "c.pem",
		"-tls-key", "k.pem", "-tls-server-name", "kv.test", "get", "k",
	})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if got.TLSCA != "ca.pem" || got.TLSCert != "c.pem" || got.TLSKey != "k.pem" || got.TLSServerName != "kv.test" {
		t.Fatalf("tls config = %#v", got)
	}
}

func TestAdminTokenFlagAndEnv(t *testing.T) {
	t.Run("flag", func(t *testing.T) {
		t.Setenv("KVCTL_ADMIN_TOKEN", "")
		f := newFake()
		run := runCLIArgs(t, f, "", "-meta", "m1", "-admin-token", "from-flag", "ranges")
		if run.code != 0 {
			t.Fatalf("ranges = code %d err %q", run.code, run.errOut)
		}
		if run.cfg.AdminToken != "from-flag" {
			t.Fatalf("AdminToken = %q, want from-flag", run.cfg.AdminToken)
		}
	})

	t.Run("env", func(t *testing.T) {
		t.Setenv("KVCTL_ADMIN_TOKEN", "from-env")
		f := newFake()
		run := runCLIArgs(t, f, "", "-meta", "m1", "split", "1", "m")
		if run.code != 0 {
			t.Fatalf("split = code %d err %q", run.code, run.errOut)
		}
		if run.cfg.AdminToken != "from-env" {
			t.Fatalf("AdminToken = %q, want from-env", run.cfg.AdminToken)
		}
	})

	t.Run("flag overrides env", func(t *testing.T) {
		t.Setenv("KVCTL_ADMIN_TOKEN", "from-env")
		f := newFake()
		run := runCLIArgs(t, f, "", "-meta", "m1", "-admin-token", "from-flag", "merge", "1", "2")
		if run.code != 0 {
			t.Fatalf("merge = code %d err %q", run.code, run.errOut)
		}
		if run.cfg.AdminToken != "from-flag" {
			t.Fatalf("AdminToken = %q, want from-flag", run.cfg.AdminToken)
		}
	})
}

func TestAdminTokenNotPrinted(t *testing.T) {
	const token = "super-secret-admin-token"
	f := newFake()
	f.splitErr = errors.New("meta: unauthenticated")
	run := runCLIArgs(t, f, "", "-meta", "m1", "-admin-token", token, "split", "1", "m")
	if run.code == 0 {
		t.Fatal("split unexpectedly succeeded")
	}
	if strings.Contains(run.out, token) || strings.Contains(run.errOut, token) {
		t.Fatalf("token leaked to output: out=%q err=%q", run.out, run.errOut)
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
	want := "ID\tSTART\tEND\tLEADER\tLEADER_KNOWN\tREPLICAS\tLIVE_VOTERS\tVERSION\tSTALE\n" +
		"1\t\tm\tn1\tyes\tn1,n2\t-\t2\tno\n" +
		"2\tm\ts\tn1\tyes\tn1,n2,n3\t-\t3\tno\n" +
		"3\ts\t\tn2\tyes\tn2\t-\t4\tno\n"
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
		{
			ID: 1, Start: "", End: "m", Leader: "n1", LeaderKnown: true,
			Replicas: []string{"n1", "n2"}, ReplicaCount: 2, LiveVoters: []string{},
			Version: 2, Epoch: 2,
		},
		{
			ID: 2, Start: "m", End: "s", Leader: "n1", LeaderKnown: true,
			Replicas: []string{"n1", "n2", "n3"}, ReplicaCount: 3, LiveVoters: []string{},
			Version: 3, Epoch: 3,
		},
		{
			ID: 3, Start: "s", End: "", Leader: "n2", LeaderKnown: true,
			Replicas: []string{"n2"}, ReplicaCount: 1, LiveVoters: []string{},
			Version: 4, Epoch: 4,
		},
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
	if !strings.Contains(out, `"live_voters":[]`) {
		t.Fatalf("live_voters should encode as [], got %q", out)
	}
}

func TestRangesHealthJSONAndTable(t *testing.T) {
	f := newFake()
	f.ranges = []RangeInfo{
		{
			ID: 1, End: []byte("m"), Leader: "n1", LeaderKnown: true,
			Replicas: []string{"n1", "n2", "n3"}, ReplicaCount: 3,
			LiveVoters: []string{"n1", "n2"}, Version: 5, Epoch: 5,
		},
		{
			ID: 2, Start: []byte("m"), Leader: "n2", LeaderKnown: true,
			Replicas: []string{"n2", "n3"}, ReplicaCount: 2,
			LiveVoters: []string{"n3"}, Version: 6, Epoch: 6, Stale: true,
		},
	}

	code, out, errOut := runCLI(t, f, "", "-meta", "m1", "ranges")
	if code != 0 {
		t.Fatalf("ranges = code %d err %q", code, errOut)
	}
	wantTable := "ID\tSTART\tEND\tLEADER\tLEADER_KNOWN\tREPLICAS\tLIVE_VOTERS\tVERSION\tSTALE\n" +
		"1\t\tm\tn1\tyes\tn1,n2,n3\tn1,n2\t5\tno\n" +
		"2\tm\t\tn2\tyes\tn2,n3\tn3\t6\tyes\n"
	if out != wantTable {
		t.Fatalf("ranges health table =\n%q\nwant\n%q", out, wantTable)
	}

	code, out, _ = runCLI(t, f, "", "-format", "json", "ranges")
	if code != 0 {
		t.Fatalf("ranges json code = %d", code)
	}
	var got []rangeJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json: %v (%q)", err, out)
	}
	if len(got) != 2 {
		t.Fatalf("ranges = %+v", got)
	}
	if got[0].ReplicaCount != 3 || !got[0].LeaderKnown || got[0].Stale ||
		got[0].Epoch != 5 || len(got[0].LiveVoters) != 2 {
		t.Fatalf("range 1 health = %+v", got[0])
	}
	if got[1].ReplicaCount != 2 || !got[1].LeaderKnown || !got[1].Stale ||
		got[1].Epoch != 6 || len(got[1].LiveVoters) != 1 {
		t.Fatalf("range 2 health = %+v", got[1])
	}
}

func TestRangesUnknownLeaderNotFaked(t *testing.T) {
	f := newFake()
	f.ranges = []RangeInfo{{ID: 1, Replicas: []string{"n1"}, Version: 3}}

	code, out, _ := runCLI(t, f, "", "-format", "json", "ranges")
	if code != 0 {
		t.Fatalf("ranges json code = %d", code)
	}
	var got []rangeJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json: %v (%q)", err, out)
	}
	if len(got) != 1 {
		t.Fatalf("ranges = %+v", got)
	}
	if got[0].Leader != "" || got[0].LeaderKnown || got[0].Stale {
		t.Fatalf("unknown leader fabricated: %+v", got[0])
	}
	if got[0].ReplicaCount != 1 || got[0].Epoch != 3 {
		t.Fatalf("health = %+v", got[0])
	}

	code, out, _ = runCLI(t, f, "", "ranges")
	if code != 0 {
		t.Fatalf("ranges table code = %d", code)
	}
	if !strings.Contains(out, "1\t\t\t-\tno\tn1\t-\t3\tno") {
		t.Fatalf("unknown leader table row = %q", out)
	}
}

// TestRangesJSONBackwardCompatible proves the pre-health JSON shape is still
// emittable and decodable: every original field keeps its name and type, and
// the new fields are additive.
func TestRangesJSONBackwardCompatible(t *testing.T) {
	f := newFake()
	f.ranges = []RangeInfo{{
		ID: 1, End: []byte("m"), Leader: "n1",
		Replicas: []string{"n1", "n2"}, Version: 2,
	}}
	_, out, _ := runCLI(t, f, "", "-format", "json", "ranges")

	var legacy []struct {
		ID       uint64   `json:"id"`
		Start    string   `json:"start"`
		End      string   `json:"end"`
		Leader   string   `json:"leader"`
		Replicas []string `json:"replicas"`
		Version  uint64   `json:"version"`
	}
	if err := json.Unmarshal([]byte(out), &legacy); err != nil {
		t.Fatalf("legacy decode: %v (%q)", err, out)
	}
	want := []struct {
		ID       uint64   `json:"id"`
		Start    string   `json:"start"`
		End      string   `json:"end"`
		Leader   string   `json:"leader"`
		Replicas []string `json:"replicas"`
		Version  uint64   `json:"version"`
	}{
		{ID: 1, Start: "", End: "m", Leader: "n1", Replicas: []string{"n1", "n2"}, Version: 2},
	}
	if !reflect.DeepEqual(legacy, want) {
		t.Fatalf("legacy fields = %#v, want %#v", legacy, want)
	}
	for _, key := range []string{
		`"leader_known":`, `"replica_count":`, `"live_voters":`, `"epoch":`, `"stale":`,
	} {
		if !strings.Contains(out, key) {
			t.Fatalf("health key %s missing from %q", key, out)
		}
	}
}

func TestRangesEmpty(t *testing.T) {
	f := newFake()
	code, out, _ := runCLI(t, f, "", "ranges")
	if code != 0 || out != "ID\tSTART\tEND\tLEADER\tLEADER_KNOWN\tREPLICAS\tLIVE_VOTERS\tVERSION\tSTALE\n" {
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
	f.ranges = []RangeInfo{{ID: 1, Version: 1}}
	cases := [][]string{
		{"ranges"},
		{"status"},
		{"split", "1", "m"},
		{"merge", "1", "2"},
		{"init-ranges", "-replicas", "n1,n2,n3"},
	}
	for _, args := range cases {
		r := runCLIArgs(t, f, "", args...)
		if r.code != 0 || r.opens != 1 || len(r.cfg.Nodes) != 0 {
			t.Errorf("args %v = code %d opens %d cfg %#v", args, r.code, r.opens, r.cfg)
		}
	}
}

func TestInitRangesBootstrapsFreshMeta(t *testing.T) {
	f := newFake()
	f.ranges = []RangeInfo{{ID: 1, Version: 1}}
	code, out, errOut := runCLI(t, f, "",
		"-meta", "m1", "-format", "json", "init-ranges", "-replicas", "n1,n2,n3")
	if code != 0 || errOut != "" {
		t.Fatalf("init-ranges = code %d err %q", code, errOut)
	}
	var got initRangesJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json: %v (%q)", err, out)
	}
	if got.Status != "ok" || got.RangeID != 1 || got.AlreadyInitialized ||
		!reflect.DeepEqual(got.Replicas, []string{"n1", "n2", "n3"}) {
		t.Fatalf("init-ranges json = %#v", got)
	}
	if len(f.setRangesCalls) != 1 {
		t.Fatalf("SetRanges calls = %d, want 1", len(f.setRangesCalls))
	}
	sent := f.setRangesCalls[0]
	if len(sent) != 1 || sent[0].ID != 1 || sent[0].Version != 1 ||
		sent[0].Start != nil || sent[0].End != nil ||
		!reflect.DeepEqual(sent[0].Replicas, []string{"n1", "n2", "n3"}) {
		t.Fatalf("SetRanges sent = %#v", sent)
	}
}

func TestInitRangesIsIdempotent(t *testing.T) {
	f := newFake()
	f.ranges = []RangeInfo{{
		ID:       1,
		Replicas: []string{"n3", "n1", "n2"},
		Leader:   "n1",
		Version:  4,
	}}
	code, out, errOut := runCLI(t, f, "",
		"-meta", "m1", "init-ranges", "-replicas", "n1,n2,n3")
	if code != 0 || errOut != "" {
		t.Fatalf("init-ranges = code %d err %q", code, errOut)
	}
	if !strings.Contains(out, "already initialized") {
		t.Fatalf("output = %q, want already initialized", out)
	}
	if len(f.setRangesCalls) != 0 {
		t.Fatalf("SetRanges calls = %d, want 0 for an initialized map", len(f.setRangesCalls))
	}
}

func TestInitRangesRefusesExistingDifferentReplicas(t *testing.T) {
	f := newFake()
	f.ranges = []RangeInfo{{ID: 1, Replicas: []string{"n9"}, Version: 1}}
	code, _, errOut := runCLI(t, f, "",
		"-meta", "m1", "init-ranges", "-replicas", "n1,n2,n3")
	if code != 1 || !strings.Contains(errOut, "refusing to replace") {
		t.Fatalf("init-ranges = code %d err %q", code, errOut)
	}
	if len(f.setRangesCalls) != 0 {
		t.Fatalf("SetRanges calls = %d, want 0", len(f.setRangesCalls))
	}
}

func TestInitRangesRefusesSplitMap(t *testing.T) {
	f := newFake()
	f.ranges = []RangeInfo{
		{ID: 1, End: []byte("m"), Replicas: []string{"n1"}},
		{ID: 2, Start: []byte("m"), Replicas: []string{"n1"}},
	}
	code, _, errOut := runCLI(t, f, "",
		"-meta", "m1", "init-ranges", "-replicas", "n1,n2,n3")
	if code != 1 || !strings.Contains(errOut, "2 ranges") {
		t.Fatalf("init-ranges = code %d err %q", code, errOut)
	}
	if len(f.setRangesCalls) != 0 {
		t.Fatalf("SetRanges calls = %d, want 0", len(f.setRangesCalls))
	}
}

func TestInitRangesValidation(t *testing.T) {
	f := newFake()
	f.ranges = []RangeInfo{{ID: 1, Version: 1}}
	cases := [][]string{
		{"init-ranges"},
		{"init-ranges", "extra", "-replicas", "n1"},
		{"-format", "raw", "init-ranges", "-replicas", "n1"},
		{"init-ranges", "-replicas", "n1,n1"},
		{"init-ranges", "-replicas", "n1,n2,n3,n4,n5,n6"},
		{"init-ranges", "-replicas", "n1", "-version", "not-a-number"},
	}
	for _, args := range cases {
		code, _, _ := runCLI(t, f, "", append([]string{"-meta", "m1"}, args...)...)
		if code != 2 {
			t.Errorf("args %v = code %d, want 2", args, code)
		}
	}
	if len(f.setRangesCalls) != 0 {
		t.Fatalf("validation wrote ranges: %#v", f.setRangesCalls)
	}
}

func TestInitRangesUsesNodesFallback(t *testing.T) {
	f := newFake()
	f.ranges = []RangeInfo{{ID: 1, Version: 1}}
	code, _, errOut := runCLI(t, f, "",
		"-meta", "m1", "-nodes", "n1,n2,n3", "init-ranges")
	if code != 0 || errOut != "" {
		t.Fatalf("init-ranges = code %d err %q", code, errOut)
	}
	if len(f.setRangesCalls) != 1 ||
		!reflect.DeepEqual(f.setRangesCalls[0][0].Replicas, []string{"n1", "n2", "n3"}) {
		t.Fatalf("SetRanges calls = %#v", f.setRangesCalls)
	}
}

func TestInitRangesRefusesEmptyMeta(t *testing.T) {
	f := newFake()
	code, _, errOut := runCLI(t, f, "",
		"-meta", "m1", "init-ranges", "-replicas", "n1,n2,n3")
	if code != 1 || !strings.Contains(errOut, "no ranges") {
		t.Fatalf("init-ranges = code %d err %q", code, errOut)
	}
	if len(f.setRangesCalls) != 0 {
		t.Fatalf("SetRanges calls = %d, want 0", len(f.setRangesCalls))
	}
}

func TestInitRangesSetRangesError(t *testing.T) {
	f := newFake()
	f.ranges = []RangeInfo{{ID: 1, Version: 1}}
	f.setRangesErr = errors.New("meta: unauthenticated")
	code, _, errOut := runCLI(t, f, "",
		"-meta", "m1", "init-ranges", "-replicas", "n1,n2,n3")
	if code != 1 || !strings.Contains(errOut, "set ranges") {
		t.Fatalf("init-ranges = code %d err %q", code, errOut)
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
