package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"math"
	"strings"
	"testing"
	"time"
)

func TestParseConfigDefaults(t *testing.T) {
	got, err := parseConfig(nil)
	if err != nil {
		t.Fatalf("parseConfig(nil): %v", err)
	}
	if want := DefaultConfig(); got != want {
		t.Fatalf("defaults = %+v, want %+v", got, want)
	}
}

func TestParseConfigAllFlags(t *testing.T) {
	got, err := parseConfig([]string{
		"-workload", "sequential",
		"-keys", "10",
		"-ops", "20",
		"-read-ratio", "0.25",
		"-seed", "7",
		"-concurrency", "3",
		"-value-size", "8",
		"-zipf-s", "1.2",
		"-json",
	})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	want := Config{
		Workload:    WorkloadSequential,
		Keys:        10,
		Ops:         20,
		ReadRatio:   0.25,
		Seed:        7,
		Concurrency: 3,
		ValueSize:   8,
		ZipfS:       1.2,
		JSON:        true,
	}
	if got != want {
		t.Fatalf("config = %+v, want %+v", got, want)
	}
}

func TestParseConfigInvalid(t *testing.T) {
	tests := []struct {
		args    []string
		wantSub string
	}{
		{[]string{"-workload", "bogus"}, "unknown workload"},
		{[]string{"-keys", "0"}, "-keys"},
		{[]string{"-ops", "0"}, "-ops"},
		{[]string{"-read-ratio", "1.5"}, "-read-ratio"},
		{[]string{"-concurrency", "0"}, "-concurrency"},
		{[]string{"-value-size", "-1"}, "-value-size"},
		{[]string{"-workload", "zipfian", "-zipf-s", "3"}, "-zipf-s"},
		{[]string{"extra"}, "unexpected argument"},
	}
	for _, tt := range tests {
		_, err := parseConfig(tt.args)
		if err == nil {
			t.Errorf("parseConfig(%v) succeeded, want error containing %q", tt.args, tt.wantSub)
			continue
		}
		if !strings.Contains(err.Error(), tt.wantSub) {
			t.Errorf("parseConfig(%v) error = %q, want substring %q", tt.args, err, tt.wantSub)
		}
	}
}

func TestParseConfigHelp(t *testing.T) {
	if _, err := parseConfig([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parseConfig(-h) error = %v, want flag.ErrHelp", err)
	}
}

func preloadAndPlan(t *testing.T, keys, ops int) (Engine, []Op, []byte) {
	t.Helper()
	value := []byte("0123456789")
	e := NewMemEngine()
	for i := 0; i < keys; i++ {
		if err := e.Put(KeyForIndex(int64(i)), value); err != nil {
			t.Fatal(err)
		}
	}
	plan := make([]Op, ops)
	for i := range plan {
		kind := OpPut
		if i%2 == 0 {
			kind = OpGet
		}
		plan[i] = Op{Kind: kind, Key: int64(i % keys)}
	}
	return e, plan, value
}

func TestExecuteMetrics(t *testing.T) {
	engine, plan, value := preloadAndPlan(t, 100, 100)
	cfg := DefaultConfig()
	cfg.Keys = 100
	cfg.Ops = len(plan)
	cfg.Concurrency = 4

	res := execute(engine, cfg, plan, value)
	if res.Reads != 50 || res.Writes != 50 {
		t.Fatalf("reads/writes = %d/%d, want 50/50", res.Reads, res.Writes)
	}
	if res.Errors != 0 {
		t.Fatalf("errors = %d, want 0", res.Errors)
	}
	if res.Latency.Count != len(plan) {
		t.Fatalf("latency samples = %d, want %d", res.Latency.Count, len(plan))
	}
	wantBytes := int64(50 * (len(KeyForIndex(0)) + len(value)))
	if res.BytesRead != wantBytes {
		t.Errorf("bytes read = %d, want %d", res.BytesRead, wantBytes)
	}
	if res.BytesWritten != wantBytes {
		t.Errorf("bytes written = %d, want %d", res.BytesWritten, wantBytes)
	}
	if res.Duration <= 0 {
		t.Errorf("duration = %v, want > 0", res.Duration)
	}
	if res.OpsPerSec <= 0 {
		t.Errorf("ops/sec = %v, want > 0", res.OpsPerSec)
	}
}

func TestExecuteDeterministicCounts(t *testing.T) {
	engine1, plan, value := preloadAndPlan(t, 50, 200)
	cfg := DefaultConfig()
	cfg.Concurrency = 8
	cfg.Ops = len(plan)

	a := execute(engine1, cfg, plan, value)

	engine2 := NewMemEngine()
	for i := 0; i < 50; i++ {
		if err := engine2.Put(KeyForIndex(int64(i)), value); err != nil {
			t.Fatal(err)
		}
	}
	b := execute(engine2, cfg, plan, value)

	if a.Reads != b.Reads || a.Writes != b.Writes || a.Errors != b.Errors ||
		a.BytesRead != b.BytesRead || a.BytesWritten != b.BytesWritten ||
		a.Latency.Count != b.Latency.Count {
		t.Fatalf("execution with the same plan differed:\n a=%+v\n b=%+v", a, b)
	}
}

type failingEngine struct{ err error }

func (f failingEngine) Get([]byte) ([]byte, error)             { return nil, f.err }
func (f failingEngine) Put([]byte, []byte) error               { return f.err }
func (f failingEngine) Delete([]byte) error                    { return f.err }
func (f failingEngine) Scan([]byte, []byte, int) ([]KV, error) { return nil, f.err }

func TestExecuteCountsErrors(t *testing.T) {
	plan := []Op{
		{Kind: OpGet, Key: 0},
		{Kind: OpPut, Key: 1},
		{Kind: OpGet, Key: 2},
	}
	cfg := DefaultConfig()
	cfg.Ops = len(plan)
	cfg.Concurrency = 2

	res := execute(failingEngine{err: errors.New("boom")}, cfg, plan, []byte("v"))
	if res.Errors != int64(len(plan)) {
		t.Fatalf("errors = %d, want %d", res.Errors, len(plan))
	}
	if res.Reads != 0 || res.Writes != 0 || res.BytesRead != 0 || res.BytesWritten != 0 {
		t.Fatalf("successful counters should be zero: %+v", res)
	}
	if res.ErrorSample != "boom" {
		t.Fatalf("error sample = %q, want boom", res.ErrorSample)
	}
	if res.Latency.Count != len(plan) {
		t.Fatalf("latency samples = %d, want %d", res.Latency.Count, len(plan))
	}
}

func TestExecuteEmptyPlan(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Ops = 1 // Config validation is not involved here.
	res := execute(NewMemEngine(), cfg, nil, nil)
	if res.OpsPerSec != 0 || res.Latency.Count != 0 || res.Reads != 0 || res.Writes != 0 {
		t.Fatalf("empty plan produced non-zero result: %+v", res)
	}
}

func TestRunSmall(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Keys = 50
	cfg.Ops = 100
	cfg.ReadRatio = 0.5
	cfg.Concurrency = 2
	cfg.ValueSize = 16
	cfg.Seed = 11

	plan, err := GenerateWorkload(cfg.workloadConfig())
	if err != nil {
		t.Fatal(err)
	}
	var wantReads, wantWrites int64
	for _, op := range plan {
		if op.Kind == OpGet {
			wantReads++
		} else {
			wantWrites++
		}
	}

	res, err := run(cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Reads != wantReads || res.Writes != wantWrites {
		t.Fatalf("reads/writes = %d/%d, want %d/%d", res.Reads, res.Writes, wantReads, wantWrites)
	}
	if res.Errors != 0 {
		t.Fatalf("errors = %d, want 0", res.Errors)
	}
	if res.LoadDuration <= 0 || res.Duration <= 0 {
		t.Fatalf("load/duration = %v/%v, want > 0", res.LoadDuration, res.Duration)
	}
	if res.Latency.Count != cfg.Ops {
		t.Fatalf("latency samples = %d, want %d", res.Latency.Count, cfg.Ops)
	}
	payload := int64(len(KeyForIndex(0)) + cfg.ValueSize)
	if want := wantReads * payload; res.BytesRead != want {
		t.Errorf("bytes read = %d, want %d", res.BytesRead, want)
	}
	if want := wantWrites * payload; res.BytesWritten != want {
		t.Errorf("bytes written = %d, want %d", res.BytesWritten, want)
	}
}

func sampleResult() Result {
	return Result{
		Config: Config{
			Workload:    WorkloadZipfian,
			Keys:        1000,
			Ops:         1000,
			ReadRatio:   0.9,
			Seed:        3,
			Concurrency: 2,
			ValueSize:   100,
			ZipfS:       0.99,
		},
		LoadDuration: 250 * time.Millisecond,
		Duration:     1500 * time.Millisecond,
		OpsPerSec:    1234.567,
		Latency: LatencySummary{
			Count: 1000,
			Min:   100 * time.Nanosecond,
			Mean:  250 * time.Nanosecond,
			Max:   time.Millisecond,
			P50:   200 * time.Nanosecond,
			P95:   500 * time.Nanosecond,
			P99:   900 * time.Nanosecond,
			P999:  time.Millisecond,
		},
		Reads:        900,
		Writes:       100,
		BytesRead:    4800,
		BytesWritten: 120,
	}
}

func TestWriteJSON(t *testing.T) {
	res := sampleResult()
	var buf bytes.Buffer
	if err := writeJSON(&buf, res); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}

	var rep jsonReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if rep.Workload != "zipfian" || rep.Keys != 1000 || rep.Ops != 1000 {
		t.Errorf("workload/keys/ops = %q/%d/%d", rep.Workload, rep.Keys, rep.Ops)
	}
	if rep.ReadRatio != 0.9 || rep.Seed != 3 || rep.Concurrency != 2 || rep.ValueSize != 100 {
		t.Errorf("scalar config mismatch: %+v", rep)
	}
	if rep.ZipfS == nil || *rep.ZipfS != 0.99 {
		t.Errorf("zipf_s = %v, want 0.99", rep.ZipfS)
	}
	if rep.OpsPerSec != 1234.57 {
		t.Errorf("ops_per_sec = %v, want 1234.57", rep.OpsPerSec)
	}
	if rep.DurationSeconds != 1.5 || rep.LoadSeconds != 0.25 {
		t.Errorf("duration/load seconds = %v/%v", rep.DurationSeconds, rep.LoadSeconds)
	}
	if rep.LatencyUS.Min != 0.1 || rep.LatencyUS.P50 != 0.2 || rep.LatencyUS.P999 != 1000 {
		t.Errorf("latency_us = %+v", rep.LatencyUS)
	}
	if rep.LatencySamples != 1000 || rep.Reads != 900 || rep.Writes != 100 || rep.Errors != 0 {
		t.Errorf("counters = %+v", rep)
	}
	if rep.BytesRead != 4800 || rep.BytesWritten != 120 {
		t.Errorf("bytes = %d/%d", rep.BytesRead, rep.BytesWritten)
	}
}

func TestWriteJSONUniformOmitsZipf(t *testing.T) {
	res := sampleResult()
	res.Config.Workload = WorkloadUniform
	res.Errors = 2
	res.ErrorSample = "boom"

	var buf bytes.Buffer
	if err := writeJSON(&buf, res); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "zipf_s") {
		t.Errorf("uniform report should omit zipf_s:\n%s", buf.String())
	}

	var rep map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep["error_sample"] != "boom" || rep["errors"].(float64) != 2 {
		t.Errorf("error fields = %v/%v", rep["error_sample"], rep["errors"])
	}
}

func TestWriteTable(t *testing.T) {
	res := sampleResult()
	var buf bytes.Buffer
	if err := writeTable(&buf, res); err != nil {
		t.Fatalf("writeTable: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"workload", "zipfian",
		"1,000",
		"read ratio", "90.0%",
		"concurrency", "2",
		"zipf exponent", "0.99",
		"metric", "value",
		"throughput", "1,235 ops/sec",
		"latency p50", "200ns",
		"latency p99", "900ns",
		"latency p999", "1ms",
		"reads", "900 (90.0%)",
		"writes", "100 (10.0%)",
		"errors", "4.7 KiB", "120 B",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q:\n%s", want, out)
		}
	}
}

func TestFormatInt(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1,000"},
		{1234567, "1,234,567"},
		{-1234, "-1,234"},
		{math.MaxInt64, "9,223,372,036,854,775,807"},
		{math.MinInt64, "-9,223,372,036,854,775,808"},
	}
	for _, tt := range tests {
		if got := formatInt(tt.in); got != tt.want {
			t.Errorf("formatInt(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1048576, "1.0 MiB"},
		{5 * 1024 * 1024, "5.0 MiB"},
	}
	for _, tt := range tests {
		if got := formatBytes(tt.in); got != tt.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestPercent(t *testing.T) {
	if got := percent(1, 4); got != 25 {
		t.Errorf("percent(1,4) = %v, want 25", got)
	}
	if got := percent(1, 0); got != 0 {
		t.Errorf("percent(1,0) = %v, want 0", got)
	}
}

func TestBuildJSONReportPyramid(t *testing.T) {
	res := sampleResult()
	rep := buildJSONReport(res)
	if rep.LatencyUS.Min > rep.LatencyUS.P50 || rep.LatencyUS.P50 > rep.LatencyUS.P999 {
		t.Errorf("latency pyramid inverted: %+v", rep.LatencyUS)
	}
	if rep.Seed != 3 || rep.ErrorSample != "" {
		t.Errorf("unexpected report fields: %+v", rep)
	}
}
