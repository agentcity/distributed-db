# bench — KV workload generator

A small, dependency-free benchmark harness for the KV data path. It generates a
reproducible workload, drives an ordered key/value engine, and reports
throughput, latency percentiles, errors and byte volume as a table or JSON.

The harness is self-contained: it imports only the Go standard library and
`bench` itself, so it runs without a cluster, config file or data directory. The
in-memory engine is a stand-in for the real server; a future adapter only needs
to implement the `Engine` interface in [`engine.go`](engine.go) and can reuse
the workload generator, executor and reporters unchanged.

## Usage

```bash
export GOPATH=/tmp/dsh-gopath GOMODCACHE=/tmp/dsh-modcache \
       GOCACHE=/tmp/dsh-gocache GOSUMDB=off

# Smoke test
go run ./bench -keys 1000 -ops 1000

# Contract example: 100k keys, 1M operations, 90% reads
go run ./bench -workload uniform -keys 100000 -ops 1000000

# Skewed read-heavy workload
go run ./bench -workload zipfian -keys 100000 -ops 1000000 \
  -read-ratio 0.95 -seed 42 -concurrency 8

# Machine-readable output
go run ./bench -keys 1000 -ops 1000 -json

# All flags
go run ./bench -h
```

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-workload` | `uniform` | Key distribution: `uniform`, `zipfian` or `sequential`. |
| `-keys` | `100000` | Number of distinct keys in the key space. Keys are preloaded before measurement. |
| `-ops` | `1000000` | Number of measured operations. |
| `-read-ratio` | `0.9` | Fraction of operations that are reads, in `[0, 1]`. |
| `-seed` | `1` | Seed for the deterministic workload plan. |
| `-concurrency` | `NumCPU` | Number of concurrent workers issuing operations. |
| `-value-size` | `100` | Value size in bytes for writes. |
| `-zipf-s` | `0.99` | Zipf exponent (`0` = uniform, `0.99` = YCSB default, up to `2`). |
| `-json` | `false` | Emit a JSON report instead of the table. |

## Workload model

Keys are fixed-width, zero-padded decimal strings (`key-0000000000000001`), so
lexicographic key order matches numeric order and scans exercise the ordered
path. The engine's `Scan` is tested, but the measured workload uses point
`Get`/`Put` operations only.

The plan is generated **before** workers start, from a splitmix64 PRNG seeded
with `-seed`:

* each operation is a read with probability `-read-ratio`, otherwise a write;
* `uniform` draws every key independently with equal probability;
* `zipfian` inverts a continuous Pareto approximation of Zipf(`s`); low key
  indices are hot, high indices are cold, and the full rank range is reachable;
* `sequential` walks `0, 1, ..., keys-1, 0, ...` regardless of read/write mix.

Because the whole plan is fixed up front and workers claim indices from a shared
counter, a seed produces the same operation sequence at every concurrency level.
The PRNG is implemented in `workload.go` rather than using `math/rand`, so plans
do not change with Go releases.

All keys are loaded once before the measured phase; load time is reported
separately and excluded from throughput and latency. Reads therefore hit an
existing key, and a miss would be counted as an error.

## Metrics

* **Throughput** — measured operations divided by wall-clock time around the
  workers (including worker startup).
* **Latency** — time around each engine call (`Get` or `Put`), including only
  the call itself and `time.Now` overhead (key formatting happens outside the
  timed region). Percentiles use the nearest-rank definition: `p` maps to the
  `ceil(p/100 * n)`-th smallest sample. `p50`, `p95`, `p99`, `p999`, plus
  min/mean/max are reported.
* **Reads / writes** — successful operations counted by kind. Failed operations
  are counted under `errors` only.
* **Bytes read / written** — key+value payload bytes for successful reads and
  writes. The load phase is not included.
* **Errors** — failed `Get`/`Put` calls (for example `ErrNotFound` or an adapter
  transport error). The first error message is shown when non-zero. The process
  still exits 0; a non-zero error count is a signal, not a crash.

Percentiles are computed after merging one collector per worker, so the
measured path has no shared mutex. See [`stats.go`](stats.go).

## Example output

From `go run ./bench -keys 1000 -ops 1000` on a laptop (numbers vary per run):

```text
workload     uniform
keys         1,000
ops          1,000
read ratio   90.0%
seed         1
concurrency  10
value size   100 B
load         1,000 keys in 395.125µs (excluded from throughput)

metric         value
------         -----
duration       876.208µs
throughput     1,141,282 ops/sec
latency min    42ns
latency mean   8.08µs
latency p50    375ns
latency p95    24.667µs
latency p99    166.792µs
latency p999   559.875µs
latency max    559.875µs
reads          914 (91.4%)
writes         86 (8.6%)
errors         0
bytes read     107.1 KiB
bytes written  10.1 KiB
```

A 1M-operation zipfian run:

```text
workload       zipfian
keys           100,000
ops            1,000,000
read ratio     95.0%
seed           42
concurrency    8
value size     100 B
zipf exponent  0.99
load           100,000 keys in 132.732625ms (excluded from throughput)

metric         value
------         -----
duration       1.079851417s
throughput     926,053 ops/sec
latency min    41ns
latency mean   8.489µs
latency p50    1µs
latency p95    23.542µs
latency p99    83.417µs
latency p999   918.417µs
latency max    3.014333ms
reads          949,973 (95.0%)
writes         50,027 (5.0%)
errors         0
bytes read     108.7 MiB
bytes written  5.7 MiB
```

`-json` emits the same data as a single object; latencies are in microseconds so
the values stay exact to the nanosecond. (`duration_seconds` and `load_seconds`
are floating-point seconds.)

```json
{
  "workload": "uniform",
  "keys": 1000,
  "ops": 1000,
  "read_ratio": 0.9,
  "seed": 1,
  "concurrency": 10,
  "value_size": 100,
  "duration_seconds": 0.000779708,
  "load_seconds": 0.00042475,
  "ops_per_sec": 1282531.41,
  "latency_us": {
    "min": 0.041,
    "mean": 7.359,
    "p50": 0.375,
    "p95": 18.5,
    "p99": 149.584,
    "p999": 520.958,
    "max": 520.958
  },
  "latency_samples": 1000,
  "reads": 914,
  "writes": 86,
  "errors": 0,
  "bytes_read": 109680,
  "bytes_written": 10320
}
```

`zipf_s` is present only for `-workload zipfian`; `error_sample` is present only
when `errors > 0`.

## In-memory engine

`MemEngine` in [`engine.go`](engine.go) is an ordered treap:

* O(log n) expected `Get`/`Put`/`Delete`, in-order `Scan([start,end), limit)`;
* priorities are FNV-1a hashes of the keys, so the tree shape is deterministic
  and independent of insertion order;
* one `RWMutex` protects the tree; readers run concurrently;
* `Get`/`Scan` return copies and `Put` copies its inputs, matching the
  ownership rules a network adapter will need.

It is a harness fixture, not a production store: no durability, no background
compaction, no replication.

## Adapter contract

A server-backed implementation should satisfy the same `Engine` interface:

```go
type Engine interface {
    Get(key []byte) ([]byte, error)
    Put(key, value []byte) error
    Delete(key []byte) error
    Scan(start, end []byte, limit int) ([]KV, error)
}
```

Implementations must be safe for concurrent use, must not retain caller slices,
must return values that stay valid after the call, and `Delete` must be
idempotent. `ErrNotFound` is the only sentinel the harness relies on. The
executor handles all other errors by counting them; it does not retry.

## Development

```bash
export GOPATH=/tmp/dsh-gopath GOMODCACHE=/tmp/dsh-modcache \
       GOCACHE=/tmp/dsh-gocache GOSUMDB=off

gofmt -w bench
go vet ./bench/...
go test ./bench/...
go test -race ./bench/...
```

Tests cover percentile edge cases (empty, single sample, unsorted input,
nearest-rank boundaries, merge/reset), deterministic workload and key
generation, read/write mix, zipf bounds/skew, the ordered engine (CRUD, scan
bounds/limits, copies, concurrency), flag parsing/validation, executor
accounting, and both reporters.

## Files

| File | Purpose |
| --- | --- |
| [`main.go`](main.go) | CLI flags, validation, preload/execute, worker loop. |
| [`workload.go`](workload.go) | Workload kinds, deterministic plan + PRNG, key format, zipf rank. |
| [`stats.go`](stats.go) | Latency collector and nearest-rank percentile math. |
| [`engine.go`](engine.go) | `Engine` interface and in-memory ordered treap. |
| [`report.go`](report.go) | Table and JSON reporters, formatting helpers. |
| `*_test.go` | Unit tests for all of the above. |

## Limitations / notes

* The measured workload is point `Get`/`Put` only; `Scan` and `Delete` are
  implemented and tested but not driven by the CLI.
* The load phase is single-threaded and excluded from metrics.
* There is no warmup phase; short runs are dominated by scheduler noise, so use
  a few hundred thousand operations for meaningful percentiles.
* Zipf uses a continuous approximation; exact discrete-Zipf cumulative tables
  were not used. `-zipf-s 0` is exactly uniform.
* The in-memory engine's RWMutex makes write-heavy runs serialize; this is a
  property of the fixture, not of the harness.
