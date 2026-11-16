package main

import (
	"bytes"
	"math"
	"reflect"
	"testing"
)

func TestParseWorkload(t *testing.T) {
	for _, want := range []WorkloadKind{WorkloadUniform, WorkloadZipfian, WorkloadSequential} {
		got, err := ParseWorkload(string(want))
		if err != nil {
			t.Fatalf("ParseWorkload(%q) error: %v", want, err)
		}
		if got != want {
			t.Errorf("ParseWorkload(%q) = %q", want, got)
		}
	}
	for _, bad := range []string{"", "Uniform", "zipf", "random"} {
		if _, err := ParseWorkload(bad); err == nil {
			t.Errorf("ParseWorkload(%q) unexpectedly succeeded", bad)
		}
	}
}

func baseWorkload(kind WorkloadKind) WorkloadConfig {
	return WorkloadConfig{
		Kind:      kind,
		Keys:      1000,
		Ops:       10000,
		ReadRatio: 0.9,
		Seed:      42,
		ZipfS:     0.99,
	}
}

func TestGenerateWorkloadDeterministic(t *testing.T) {
	for _, kind := range []WorkloadKind{WorkloadUniform, WorkloadZipfian, WorkloadSequential} {
		cfg := baseWorkload(kind)
		a, err := GenerateWorkload(cfg)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		b, err := GenerateWorkload(cfg)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("%s: same seed produced different plans", kind)
		}

		cfg.Seed++
		c, err := GenerateWorkload(cfg)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if reflect.DeepEqual(a, c) {
			t.Errorf("%s: different seeds produced identical plans", kind)
		}
	}
}

func TestGenerateWorkloadSequential(t *testing.T) {
	cfg := baseWorkload(WorkloadSequential)
	cfg.Keys = 5
	cfg.Ops = 12
	plan, err := GenerateWorkload(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 12 {
		t.Fatalf("len(plan) = %d, want 12", len(plan))
	}
	for i, op := range plan {
		if want := int64(i % 5); op.Key != want {
			t.Errorf("op %d key = %d, want %d", i, op.Key, want)
		}
	}
}

func TestWorkloadKeysInRange(t *testing.T) {
	seeds := []int64{0, 1, -1, math.MaxInt64, math.MinInt64}
	sValues := []float64{0, 0.5, 0.99, 1, 1.5, 2}
	for _, kind := range []WorkloadKind{WorkloadUniform, WorkloadZipfian, WorkloadSequential} {
		for _, seed := range seeds {
			for _, s := range sValues {
				cfg := WorkloadConfig{
					Kind:      kind,
					Keys:      97,
					Ops:       500,
					ReadRatio: 0.5,
					Seed:      seed,
					ZipfS:     s,
				}
				plan, err := GenerateWorkload(cfg)
				if err != nil {
					t.Fatalf("%s seed=%d s=%v: %v", kind, seed, s, err)
				}
				for i, op := range plan {
					if op.Key < 0 || op.Key >= int64(cfg.Keys) {
						t.Fatalf("%s seed=%d s=%v: op %d key %d out of range [0,%d)",
							kind, seed, s, i, op.Key, cfg.Keys)
					}
					if op.Kind != OpGet && op.Kind != OpPut {
						t.Fatalf("%s: op %d has invalid kind %d", kind, i, op.Kind)
					}
				}
			}
		}
	}
}

func TestWorkloadReadRatio(t *testing.T) {
	tests := []struct {
		ratio    float64
		wantFrac float64
		exact    bool
	}{
		{0, 0, true},
		{1, 1, true},
		{0.3, 0.3, false},
		{0.9, 0.9, false},
	}
	for _, tt := range tests {
		cfg := baseWorkload(WorkloadUniform)
		cfg.ReadRatio = tt.ratio
		cfg.Ops = 100000
		plan, err := GenerateWorkload(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var reads int
		for _, op := range plan {
			if op.Kind == OpGet {
				reads++
			}
		}
		frac := float64(reads) / float64(len(plan))
		if tt.exact {
			if frac != tt.wantFrac {
				t.Errorf("read ratio %v: got fraction %v, want %v", tt.ratio, frac, tt.wantFrac)
			}
			continue
		}
		if math.Abs(frac-tt.wantFrac) > 0.01 {
			t.Errorf("read ratio %v: got fraction %v, want within 0.01", tt.ratio, frac)
		}
	}
}

func TestZipfRankBoundsAndMonotonic(t *testing.T) {
	for _, s := range []float64{0, 0.5, 0.99, 1, 1.5, 2} {
		const n = 1000
		prev := int64(-1)
		for u := 0.0; u < 1.0; u += 0.001 {
			got := zipfRank(u, n, s)
			if got < 0 || got >= n {
				t.Fatalf("s=%v u=%v: rank %d out of [0,%d)", s, u, got, n)
			}
			if got < prev {
				t.Fatalf("s=%v u=%v: rank decreased from %d to %d", s, u, prev, got)
			}
			prev = got
		}
		if got := zipfRank(0, n, s); got != 0 {
			t.Errorf("s=%v: zipfRank(0) = %d, want 0", s, got)
		}
		// Every rank, including the coldest key n-1, must be reachable for
		// u < 1.
		if got := zipfRank(math.Nextafter(1, 0), n, s); got != n-1 {
			t.Errorf("s=%v: zipfRank(~1) = %d, want %d", s, got, n-1)
		}
	}
}

func TestZipfRankS0IsUniform(t *testing.T) {
	const n = 1000
	for i := 0; i < n; i++ {
		u := (float64(i) + 0.5) / n
		if got, want := zipfRank(u, n, 0), int64(i); got != want {
			t.Errorf("zipfRank(%v, %d, 0) = %d, want %d", u, n, got, want)
		}
	}
}

func TestZipfRankSmallN(t *testing.T) {
	for _, n := range []int64{-5, 0, 1} {
		if got := zipfRank(0.5, n, 0.99); got != 0 {
			t.Errorf("zipfRank with n=%d = %d, want 0", n, got)
		}
	}
	if got := zipfRank(0.5, 2, 0.99); got < 0 || got > 1 {
		t.Errorf("zipfRank n=2 = %d, want 0 or 1", got)
	}
}

func TestZipfianSkew(t *testing.T) {
	cfg := WorkloadConfig{
		Kind:      WorkloadZipfian,
		Keys:      1000,
		Ops:       50000,
		ReadRatio: 0,
		Seed:      7,
		ZipfS:     0.99,
	}
	plan, err := GenerateWorkload(cfg)
	if err != nil {
		t.Fatal(err)
	}
	counts := make([]int, cfg.Keys)
	for _, op := range plan {
		counts[op.Key]++
	}
	if counts[0] <= counts[999] {
		t.Errorf("hottest key count %d is not above coldest %d", counts[0], counts[999])
	}
	if counts[0] < 10*counts[999] {
		t.Errorf("distribution is not skewed enough: hot=%d cold=%d", counts[0], counts[999])
	}
	if counts[0] <= counts[100] {
		t.Errorf("rank 0 count %d should exceed rank 100 count %d", counts[0], counts[100])
	}
}

func TestKeyForIndex(t *testing.T) {
	tests := []struct {
		idx  int64
		want string
	}{
		{0, "key-0000000000000000"},
		{1, "key-0000000000000001"},
		{42, "key-0000000000000042"},
		{1 << 40, "key-0001099511627776"},
	}
	for _, tt := range tests {
		if got := string(KeyForIndex(tt.idx)); got != tt.want {
			t.Errorf("KeyForIndex(%d) = %q, want %q", tt.idx, got, tt.want)
		}
	}

	// Lexicographic order must match numeric order, otherwise the "ordered"
	// part of the in-memory engine would return scans in the wrong order.
	var prev []byte
	for i := int64(0); i < 2000; i++ {
		cur := KeyForIndex(i)
		if i > 0 && bytes.Compare(prev, cur) >= 0 {
			t.Fatalf("key order broken at %d: %q >= %q", i, prev, cur)
		}
		prev = cur
	}
}

func TestAppendKeyReusesBuffer(t *testing.T) {
	buf := make([]byte, 0, 32)
	buf = append(buf, "garbage"...)
	got := appendKey(buf[:0], 7)
	if want := KeyForIndex(7); !bytes.Equal(got, want) {
		t.Errorf("appendKey = %q, want %q", got, want)
	}
	got2 := appendKey(got[:0], 8)
	if want := KeyForIndex(8); !bytes.Equal(got2, want) {
		t.Errorf("second appendKey = %q, want %q", got2, want)
	}
}

func TestRNGDeterministic(t *testing.T) {
	a := newRNG(12345)
	b := newRNG(12345)
	c := newRNG(12346)
	for i := 0; i < 100; i++ {
		av, bv, cv := a.next(), b.next(), c.next()
		if av != bv {
			t.Fatalf("same seed diverged at draw %d: %d != %d", i, av, bv)
		}
		if i == 0 && av == cv {
			t.Fatalf("different seeds produced the same first draw")
		}
	}
}

func TestRNGInt64nRange(t *testing.T) {
	r := newRNG(99)
	for _, n := range []int64{1, 2, 3, 7, 100, 1000003} {
		for i := 0; i < 1000; i++ {
			got := r.int64n(n)
			if got < 0 || got >= n {
				t.Fatalf("int64n(%d) = %d, out of range", n, got)
			}
		}
	}
}

func TestRNGFloat64Range(t *testing.T) {
	r := newRNG(3)
	for i := 0; i < 10000; i++ {
		got := r.float64()
		if got < 0 || got >= 1 {
			t.Fatalf("float64() = %v, out of [0,1)", got)
		}
	}
}

func TestGenerateWorkloadValidation(t *testing.T) {
	base := baseWorkload(WorkloadUniform)
	tests := []struct {
		name string
		cfg  WorkloadConfig
	}{
		{"bad kind", WorkloadConfig{Kind: "nope", Keys: 10, Ops: 10}},
		{"zero keys", WorkloadConfig{Kind: WorkloadUniform, Keys: 0, Ops: 10}},
		{"negative ops", WorkloadConfig{Kind: WorkloadUniform, Keys: 10, Ops: -1}},
		{"read ratio high", WorkloadConfig{Kind: WorkloadUniform, Keys: 10, Ops: 10, ReadRatio: 1.5}},
		{"read ratio NaN", WorkloadConfig{Kind: WorkloadUniform, Keys: 10, Ops: 10, ReadRatio: math.NaN()}},
		{"zipf exponent high", WorkloadConfig{Kind: WorkloadZipfian, Keys: 10, Ops: 10, ReadRatio: 0.5, ZipfS: 3}},
		{"zipf exponent negative", WorkloadConfig{Kind: WorkloadZipfian, Keys: 10, Ops: 10, ReadRatio: 0.5, ZipfS: -0.1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := GenerateWorkload(tt.cfg); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}

	base.Ops = 0
	plan, err := GenerateWorkload(base)
	if err != nil {
		t.Fatalf("zero ops should be allowed for the generator: %v", err)
	}
	if len(plan) != 0 {
		t.Fatalf("zero ops produced %d operations", len(plan))
	}
}
