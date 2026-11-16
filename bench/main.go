// Command bench is a self-contained KV workload generator and latency
// benchmark. It drives an in-memory ordered store today; a server-backed
// implementation can be plugged in by implementing the Engine interface in
// engine.go. It intentionally imports nothing from the rest of the repository,
// so it runs without a cluster, a data directory or a network.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Config is the fully parsed CLI configuration.
type Config struct {
	Workload    WorkloadKind
	Keys        int
	Ops         int
	ReadRatio   float64
	Seed        int64
	Concurrency int
	ValueSize   int
	ZipfS       float64
	JSON        bool
}

// DefaultConfig returns the flag defaults. The defaults match the examples in
// docs/CONTRACTS-V2.md.
func DefaultConfig() Config {
	return Config{
		Workload:    WorkloadUniform,
		Keys:        100000,
		Ops:         1000000,
		ReadRatio:   0.9,
		Seed:        1,
		Concurrency: runtime.NumCPU(),
		ValueSize:   100,
		ZipfS:       0.99,
	}
}

// Validate checks the CLI configuration.
func (c Config) Validate() error {
	if _, err := ParseWorkload(string(c.Workload)); err != nil {
		return err
	}
	if c.Keys < 1 {
		return fmt.Errorf("-keys must be >= 1, got %d", c.Keys)
	}
	if c.Ops < 1 {
		return fmt.Errorf("-ops must be >= 1, got %d", c.Ops)
	}
	if math.IsNaN(c.ReadRatio) || c.ReadRatio < 0 || c.ReadRatio > 1 {
		return fmt.Errorf("-read-ratio must be in [0, 1], got %v", c.ReadRatio)
	}
	if c.Concurrency < 1 {
		return fmt.Errorf("-concurrency must be >= 1, got %d", c.Concurrency)
	}
	if c.ValueSize < 0 {
		return fmt.Errorf("-value-size must be >= 0, got %d", c.ValueSize)
	}
	if c.Workload == WorkloadZipfian && (math.IsNaN(c.ZipfS) || c.ZipfS < 0 || c.ZipfS > 2) {
		return fmt.Errorf("-zipf-s must be in [0, 2], got %v", c.ZipfS)
	}
	return nil
}

func (c Config) workloadConfig() WorkloadConfig {
	return WorkloadConfig{
		Kind:      c.Workload,
		Keys:      c.Keys,
		Ops:       c.Ops,
		ReadRatio: c.ReadRatio,
		Seed:      c.Seed,
		ZipfS:     c.ZipfS,
	}
}

// Result holds everything the reporters need.
type Result struct {
	Config       Config
	LoadDuration time.Duration
	Duration     time.Duration
	OpsPerSec    float64
	Latency      LatencySummary
	Reads        int64
	Writes       int64
	Errors       int64
	ErrorSample  string
	BytesRead    int64
	BytesWritten int64
}

func main() {
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(os.Stdout)
			return
		}
		fmt.Fprintf(os.Stderr, "bench: %v\nrun 'go run ./bench -h' for usage\n", err)
		os.Exit(2)
	}

	res, err := run(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench: %v\n", err)
		os.Exit(1)
	}

	if cfg.JSON {
		err = writeJSON(os.Stdout, res)
	} else {
		err = writeTable(os.Stdout, res)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench: write report: %v\n", err)
		os.Exit(1)
	}
}

// flagValues holds the flag defaults plus the raw string form of -workload,
// which cannot be bound directly to a typed WorkloadKind.
type flagValues struct {
	Config
	workload string
}

func newFlagSet(out io.Writer) (*flag.FlagSet, *flagValues) {
	v := &flagValues{Config: DefaultConfig()}
	v.workload = string(v.Workload)

	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprintln(out, "usage: go run ./bench [flags]")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "flags:")
		fs.PrintDefaults()
	}

	fs.StringVar(&v.workload, "workload", v.workload, "key distribution: uniform, zipfian or sequential")
	fs.IntVar(&v.Keys, "keys", v.Keys, "number of distinct keys in the key space")
	fs.IntVar(&v.Ops, "ops", v.Ops, "number of measured operations")
	fs.Float64Var(&v.ReadRatio, "read-ratio", v.ReadRatio, "fraction of operations that are reads, in [0, 1]")
	fs.Int64Var(&v.Seed, "seed", v.Seed, "deterministic workload seed")
	fs.IntVar(&v.Concurrency, "concurrency", v.Concurrency, "number of concurrent workers")
	fs.IntVar(&v.ValueSize, "value-size", v.ValueSize, "value size in bytes used by writes")
	fs.Float64Var(&v.ZipfS, "zipf-s", v.ZipfS, "zipfian exponent (0 = uniform, 0.99 = YCSB default)")
	fs.BoolVar(&v.JSON, "json", v.JSON, "emit a JSON report instead of the table")
	return fs, v
}

func parseConfig(args []string) (Config, error) {
	fs, v := newFlagSet(io.Discard)
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	kind, err := ParseWorkload(v.workload)
	if err != nil {
		return Config{}, err
	}
	v.Workload = kind

	if err := v.Config.Validate(); err != nil {
		return Config{}, err
	}
	return v.Config, nil
}

func printUsage(w io.Writer) {
	fs, _ := newFlagSet(w)
	fs.Usage()
}

// run preloads the key space (excluded from throughput), executes the measured
// operations and returns the report data.
func run(cfg Config) (Result, error) {
	if err := cfg.Validate(); err != nil {
		return Result{}, err
	}

	plan, err := GenerateWorkload(cfg.workloadConfig())
	if err != nil {
		return Result{}, fmt.Errorf("generate workload: %w", err)
	}

	engine := NewMemEngine()

	// Writes share one immutable payload; Engine.Put copies it, and only its
	// length is part of the reported traffic.
	value := make([]byte, cfg.ValueSize)
	for i := range value {
		value[i] = byte('a' + i%26)
	}

	loadStart := time.Now()
	keyBuf := make([]byte, 0, len(keyPrefix)+keyDigits)
	for i := 0; i < cfg.Keys; i++ {
		keyBuf = appendKey(keyBuf[:0], int64(i))
		if err := engine.Put(keyBuf, value); err != nil {
			return Result{}, fmt.Errorf("preload key %d: %w", i, err)
		}
	}
	loadDuration := time.Since(loadStart)

	res := execute(engine, cfg, plan, value)
	res.LoadDuration = loadDuration
	return res, nil
}

type workerStats struct {
	latency      LatencyCollector
	reads        int64
	writes       int64
	errors       int64
	bytesRead    int64
	bytesWritten int64
	errorSample  string
}

// execute runs the plan across cfg.Concurrency workers. The operation plan is
// pre-generated and workers claim indices from a shared counter, so wall-clock
// scheduling does not change which operations are issued by a given seed.
func execute(engine Engine, cfg Config, plan []Op, value []byte) Result {
	workers := cfg.Concurrency
	if workers > len(plan) && len(plan) > 0 {
		workers = len(plan)
	}
	if workers < 1 {
		workers = 1
	}

	stats := make([]workerStats, workers)
	var next atomic.Int64
	var wg sync.WaitGroup

	start := time.Now()
	for i := range stats {
		wg.Add(1)
		go func(ws *workerStats) {
			defer wg.Done()
			runWorker(engine, plan, &next, ws, value)
		}(&stats[i])
	}
	wg.Wait()
	elapsed := time.Since(start)

	res := Result{Config: cfg, Duration: elapsed}
	latencies := NewLatencyCollector(len(plan))
	for i := range stats {
		s := &stats[i]
		latencies.Merge(&s.latency)
		res.Reads += s.reads
		res.Writes += s.writes
		res.Errors += s.errors
		res.BytesRead += s.bytesRead
		res.BytesWritten += s.bytesWritten
		if res.ErrorSample == "" {
			res.ErrorSample = s.errorSample
		}
	}
	res.Latency = latencies.Summary()
	if elapsed > 0 && len(plan) > 0 {
		res.OpsPerSec = float64(len(plan)) / elapsed.Seconds()
	}
	return res
}

func runWorker(engine Engine, plan []Op, next *atomic.Int64, ws *workerStats, value []byte) {
	keyBuf := make([]byte, 0, len(keyPrefix)+keyDigits)

	for {
		idx := int(next.Add(1)) - 1
		if idx >= len(plan) {
			return
		}
		op := plan[idx]
		key := appendKey(keyBuf[:0], op.Key)

		switch op.Kind {
		case OpGet:
			start := time.Now()
			got, err := engine.Get(key)
			ws.latency.Add(time.Since(start))
			if err != nil {
				ws.errors++
				if ws.errorSample == "" {
					ws.errorSample = err.Error()
				}
				continue
			}
			ws.reads++
			ws.bytesRead += int64(len(key) + len(got))
		case OpPut:
			start := time.Now()
			err := engine.Put(key, value)
			ws.latency.Add(time.Since(start))
			if err != nil {
				ws.errors++
				if ws.errorSample == "" {
					ws.errorSample = err.Error()
				}
				continue
			}
			ws.writes++
			ws.bytesWritten += int64(len(key) + len(value))
		}
	}
}
