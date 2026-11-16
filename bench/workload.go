package main

import (
	"fmt"
	"math"
	"strconv"
)

// WorkloadKind selects how the key for each operation is chosen.
type WorkloadKind string

const (
	// WorkloadUniform draws every key independently with equal probability.
	WorkloadUniform WorkloadKind = "uniform"
	// WorkloadZipfian draws keys from a power-law distribution; low indices
	// are hot and high indices are cold.
	WorkloadZipfian WorkloadKind = "zipfian"
	// WorkloadSequential walks the key space in order (0, 1, ..., keys-1, 0,
	// ...), which is friendly to range partitioning and prefix caches.
	WorkloadSequential WorkloadKind = "sequential"
)

// ParseWorkload validates a -workload flag value.
func ParseWorkload(s string) (WorkloadKind, error) {
	switch WorkloadKind(s) {
	case WorkloadUniform, WorkloadZipfian, WorkloadSequential:
		return WorkloadKind(s), nil
	default:
		return "", fmt.Errorf("unknown workload %q (want uniform, zipfian or sequential)", s)
	}
}

// OpKind is a benchmark operation kind.
type OpKind uint8

const (
	// OpGet is a point read.
	OpGet OpKind = iota
	// OpPut is a point write.
	OpPut
)

func (k OpKind) String() string {
	if k == OpGet {
		return "get"
	}
	return "put"
}

// Op is one pre-generated operation. Plans are generated before workers start,
// so a seed produces the same operation sequence at every concurrency level.
type Op struct {
	Kind OpKind
	Key  int64 // index into the key space, 0 <= Key < Keys
}

// WorkloadConfig configures plan generation.
type WorkloadConfig struct {
	Kind      WorkloadKind
	Keys      int
	Ops       int
	ReadRatio float64
	Seed      int64
	ZipfS     float64 // exponent for the zipfian distribution
}

// Validate checks the generator inputs.
func (c WorkloadConfig) Validate() error {
	if _, err := ParseWorkload(string(c.Kind)); err != nil {
		return err
	}
	if c.Keys < 1 {
		return fmt.Errorf("keys must be >= 1, got %d", c.Keys)
	}
	if c.Ops < 0 {
		return fmt.Errorf("ops must be >= 0, got %d", c.Ops)
	}
	if c.ReadRatio < 0 || c.ReadRatio > 1 || math.IsNaN(c.ReadRatio) {
		return fmt.Errorf("read ratio must be in [0, 1], got %v", c.ReadRatio)
	}
	if c.Kind == WorkloadZipfian {
		if c.ZipfS < 0 || c.ZipfS > 2 || math.IsNaN(c.ZipfS) {
			return fmt.Errorf("zipf exponent must be in [0, 2], got %v", c.ZipfS)
		}
	}
	return nil
}

// GenerateWorkload builds a deterministic operation plan. The same config
// (including seed) always yields the same plan, independent of machine,
// Go version and worker count.
func GenerateWorkload(cfg WorkloadConfig) ([]Op, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	plan := make([]Op, cfg.Ops)
	rng := newRNG(cfg.Seed)
	keys := int64(cfg.Keys)

	var sequential int64
	for i := range plan {
		kind := OpPut
		if rng.float64() < cfg.ReadRatio {
			kind = OpGet
		}

		var key int64
		switch cfg.Kind {
		case WorkloadSequential:
			key = sequential % keys
			sequential++
		case WorkloadUniform:
			key = rng.int64n(keys)
		case WorkloadZipfian:
			key = zipfRank(rng.float64(), keys, cfg.ZipfS)
		}
		plan[i] = Op{Kind: kind, Key: key}
	}
	return plan, nil
}

// keyPrefix and keyDigits define the fixed-width, lexicographically ordered
// key format. Zero padding keeps byte order equal to numeric order.
const (
	keyPrefix = "key-"
	keyDigits = 16
)

// KeyForIndex renders key i in the canonical format. Indices must be >= 0.
func KeyForIndex(i int64) []byte {
	return appendKey(nil, i)
}

// appendKey appends the canonical key for i to dst. Workers reuse a small
// buffer per goroutine to keep key formatting out of the allocation path.
func appendKey(dst []byte, i int64) []byte {
	dst = append(dst, keyPrefix...)
	var scratch [20]byte
	digits := strconv.AppendInt(scratch[:0], i, 10)
	for n := len(digits); n < keyDigits; n++ {
		dst = append(dst, '0')
	}
	return append(dst, digits...)
}

// zipfRank maps u in [0, 1) to a rank in [0, n) using the continuous
// (Pareto) approximation of the Zipf distribution, inverted analytically.
// s is the exponent: s == 0 is uniform, larger values concentrate accesses on
// low ranks. s == 0.99 is the YCSB default; s == 1 uses the logarithmic limit
// to avoid a division by zero.
//
// The inverse CDF yields a value in [1, n), so floor(x)-1 alone would leave
// the coldest rank (n-1) with zero probability. Scaling by n/(n-1) before the
// floor keeps the full [0, n-1] range reachable and makes s == 0 an exact
// uniform draw; the distortion of the distribution is O(1/n).
func zipfRank(u float64, n int64, s float64) int64 {
	if n <= 1 {
		return 0
	}
	if u < 0 {
		u = 0
	}
	if u >= 1 {
		u = math.Nextafter(1, 0)
	}

	var x float64
	if math.Abs(s-1) < 1e-9 {
		x = math.Pow(float64(n), u)
	} else {
		// Inverse CDF of p(x) ~ x^-s on [1, n].
		x = math.Pow(1+u*(math.Pow(float64(n), 1-s)-1), 1/(1-s))
	}

	// Stretch [1, n) to [0, n) and floor; clamp defensively against rounding
	// at the top end.
	rank := int64((x - 1) * float64(n) / float64(n-1))
	if rank < 0 {
		return 0
	}
	if rank >= n {
		return n - 1
	}
	return rank
}

// rng is a splitmix64 generator. It is small, fast, dependency-free and its
// output is fully specified by the seed, so workload plans are stable across
// Go releases (unlike math/rand's internals, which we do not want to depend
// on for reproducible benchmark input).
type rng struct {
	state uint64
}

func newRNG(seed int64) *rng {
	return &rng{state: uint64(seed)}
}

func (r *rng) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// float64 returns a value in [0, 1) with 53 bits of precision.
func (r *rng) float64() float64 {
	return float64(r.next()>>11) * (1.0 / (1 << 53))
}

// int64n returns a value in [0, n) using rejection sampling to avoid modulo
// bias.
func (r *rng) int64n(n int64) int64 {
	if n <= 1 {
		return 0
	}
	bound := ^uint64(0) - ^uint64(0)%uint64(n) // largest multiple of n
	for {
		if x := r.next(); x < bound {
			return int64(x % uint64(n))
		}
	}
}
