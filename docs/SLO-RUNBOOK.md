# SLO Runbook: multi-host and 1 TB

Status: **procedure and harness, not an SLO claim.** No 1 TB run has been
executed as part of this repository change, and no p99/1 TB number is asserted
here. The harness measures one client host against a cluster you deploy
separately; it never starts a meta service or KV node.

## 1. What this runbook is for

Use `test/slo_multihost_test.go` together with `scripts/bench-multihost.sh`
when you need a reproducible, client-observed SLO measurement against an
already-running cluster:

- mixed single-key read/write transactions at a configurable ratio;
- unique key partitions per worker and a bounded number of in-flight
  operations;
- p50/p95/p99 from a bounded log-linear histogram, throughput, errors, and a
  verification sample;
- an optional streamed dataset preload (`SLO_PRELOAD_GB`) for large-dataset
  runs;
- explicit, fail-closed gates for error rate, p95/p99 and minimum throughput.

It is suitable for:

- comparing 3, 5 and 7 KV-node topologies on the same hardware and workload;
- regression tracking across builds when the cluster, client host and config
  are held constant;
- a gated acceptance run for a specific product SLO that you have already
  defined elsewhere.

It is **not** a TPC-C/YCSB benchmark, an availability test, a failover test, a
durability proof, or a replacement for monitoring in production.

## 2. Topology and hardware

### Cluster shapes

| Shape | KV nodes | Notes |
|---|---:|---|
| Minimum production-like | 3 | one replication group, RF=3; meta/TSO quorum separate |
| Mid-size | 5 | same client workload; more failure domains |
| Large | 7 | same client workload; higher coordination and network cost |

`SLO_NODES` is the KV gRPC address list used by the smart client for leader
discovery. `SLO_META` is one meta/TSO seed address; the meta service may itself
be a 3- or 5-node Raft group. This harness does not start or configure either
quorum.

### Reference hardware

Production evidence should come from:

- dedicated Linux hosts, not laptops or shared CI runners;
- local NVMe for the KV data directories, not network filesystems, loop
  devices, or containers sharing the host disk;
- a client host separate from the cluster nodes, on the same network class as
  production clients;
- at least 10 GbE between client and cluster (25 GbE or better for sustained
  1 TB throughput tests);
- synchronized clocks (chrony or equivalent) so logs from all hosts correlate.

For a 1 TB logical payload with RF=3, plan for at least 3x payload in raw
replicated bytes plus Raft/WAL, MVCC versions, SST compaction headroom and
snapshots. The harness preloads **value payload**, not on-disk bytes, so disk
usage will be higher. Verify free space before every stage.

### Client host

Run the harness on a host that will not be saturated by unrelated work. The
client is part of the measurement: CPU contention, GC pauses, a slow NIC or a
VM steal-time spike will show up as p99 latency. Record `nproc`, memory, NIC
speed and kernel before the run.

## 3. Environment reference

The test reads these variables. The script supplies defaults for the common
ones; values passed explicitly always win. The table shows the **test
defaults**; the script intentionally selects production-oriented defaults
(`SLO_CLIENTS=16`, `SLO_DURATION=10m`, `SLO_KEYSPACE=10000000`) and prints
the effective values at the top of every run.

### Required

| Variable | Meaning |
|---|---|
| `SLO_RUN=1` | Enables `TestSLOMultiHost`. The script always sets this. Any other value (or unset) makes the test skip. |
| `SLO_META` | Meta/TSO gRPC `host:port`. |
| `SLO_NODES` | Comma-separated KV gRPC `host:port` list. Duplicates and empty entries are rejected. |

### Workload

| Variable | Default | Meaning |
|---|---:|---|
| `SLO_CLIENTS` | `8` | Worker goroutines. Each worker owns a disjoint key partition. Max 1024. |
| `SLO_MAX_INFLIGHT` | same as clients | Global cap on concurrent operations. If clients exceed it, workers block before starting an operation. Max 8192. |

Each worker issues one operation at a time, so the effective concurrency is
`min(SLO_CLIENTS, SLO_MAX_INFLIGHT)`. A cap above the client count does not add
pipelining; it is only useful to throttle concurrency below the worker count.
| `SLO_DURATION` | `60s` | Measured duration. Go duration (`90s`, `10m`) or bare seconds. Minimum 1s. |
| `SLO_VALUE_SIZE` | `1024` | Value payload bytes. Max 16 MiB. Larger values raise network and copy cost. |
| `SLO_KEYSPACE` | `1000000` | Total logical key budget. Each worker gets `floor(SLO_KEYSPACE / SLO_CLIENTS)` keys; the remainder is unused. Must be at least `SLO_CLIENTS`. Keys are never shared between workers. |
| `SLO_READ_RATIO` | `0.8` | Fraction of reads in the mix; writes are `1 - ratio`. Range 0..1. |
| `SLO_OP_TIMEOUT` | `10s` | Per-operation context deadline. A timeout is an error. |
| `SLO_SEED` | clock | Optional uint64 workload seed. Set it to reproduce the read/write sequence; the effective seed is in the report. |

A run starts with one unmeasured seed write per worker, so a read-only mix
(`SLO_READ_RATIO=1`) still has data to read. Those seed keys are outside the
measured latency/throughput window.

Client memory scales roughly with `SLO_CLIENTS * SLO_VALUE_SIZE`: each worker
keeps one reusable value buffer and each in-flight write also holds the
client's copy of the mutation. Keep that product comfortably below the client
host's RAM (this is one reason large values and high client counts are a bad
combination).

### Verification

| Variable | Default | Meaning |
|---|---:|---|
| `SLO_VERIFY_RATE` | `0.01` | Fraction of reads whose full deterministic payload is compared. Every read still checks the returned length. |
| `SLO_VERIFY_SAMPLE` | `32` | Post-run reads used to re-check up to this many workload keys and, when preloaded, up to this many spread-out dataset keys (`0` disables the post-run pass). Max 10000. |

Values are deterministic functions of the key and `SLO_VALUE_SIZE`, so a
verification read can recompute the expected bytes without retaining the
dataset. A mismatch is both a verification failure and a workload error.

### Optional preload

| Variable | Default | Meaning |
|---|---:|---|
| `SLO_PRELOAD_GB` | `0` | Value-payload target in GiB. `0` disables preload. Keys are `floor(target / value_size)` (at least one), so actual payload can be slightly below the target; batches are streamed and bounded in memory. |
| `SLO_PRELOAD_WORKERS` | `4` | Concurrent preload transactions. Each worker holds one bounded batch in memory. |

Preloaded keys live under `slo/preload/`; workload keys live under
`slo/workload/w<id>/`. Preload does **not** consume `SLO_KEYSPACE` and the
harness does **not** delete preloaded data. Use a dedicated cluster or a
disposable namespace/prefix, and budget disk accordingly.

### Gates and reporting

| Variable | Default | Meaning |
|---|---:|---|
| `SLO_MAX_ERROR_RATE` | `0` | Allowed workload error fraction, 0..1. The default fails on any error. Relax deliberately for noisy environments. |
| `SLO_MIN_THROUGHPUT` | `0` | Minimum aggregate ops/sec; `0` disables. |
| `SLO_P95_GATE_MS` | `0` | Optional p95 gate in milliseconds applied to all, read and write histograms; `0` disables. |
| `SLO_P99_GATE_MS` | `0` | Optional p99 gate in milliseconds applied to all, read and write histograms; `0` disables. |
| `SLO_REPORT_JSON` | unset | Path for the machine-readable report. The script sets a timestamped path by default. |
| `SLO_OUT_DIR` | `.tmp-slo` | Script-only output directory (resolved from the repository root). |
| `SLO_RUN_ID` | UTC timestamp + PID | Script-only run identifier used in artifact names. |
| `SLO_TEST_TIMEOUT` | `24h` | Script-only `go test -timeout`. Must exceed preload plus the measured run. |
| `SLO_ALLOW_SMALL_CLUSTER` | `0` | Script-only escape hatch for fewer than 3 KV nodes; not production evidence. |

`SLO_P95_GATE_MS`/`SLO_P99_GATE_MS` are single thresholds applied to the
aggregate, read and write histograms. If read and write need different gates,
run two mixes separately (for example a 95% read run and a 90% write run)
rather than expecting one threshold to express both.

## 4. Preflight

1. Deploy and stabilize the cluster with the intended topology, replication
   factor, data directories and network. The harness will not start servers.
2. Confirm every `SLO_NODES` address is reachable from the client host and
   that `SLO_META` is reachable. Use your deployment's health endpoints,
   `nc -vz`, or `kvctl` as appropriate; do not run the SLO harness as a
   substitute for a health check.
3. Verify the cluster is initialized and has a range map. The harness fails
   early if `ListRanges` returns no ranges.
4. Check free space. Preload is not automatically cleaned up.
5. Validate the environment without contacting the cluster:

```sh
SLO_META=10.20.0.10:9090 \
SLO_NODES=10.20.0.11:7001,10.20.0.12:7001,10.20.0.13:7001 \
./scripts/bench-multihost.sh --dry-run
```

6. Run a short external smoke (still writes a small amount of data):

```sh
export SLO_META=10.20.0.10:9090
export SLO_NODES=10.20.0.11:7001,10.20.0.12:7001,10.20.0.13:7001
export SLO_CLIENTS=4
export SLO_MAX_INFLIGHT=4
export SLO_DURATION=2m
export SLO_VALUE_SIZE=1024
export SLO_KEYSPACE=100000
export SLO_READ_RATIO=0.8
export SLO_P99_GATE_MS=250
export SLO_MAX_ERROR_RATE=0
./scripts/bench-multihost.sh
```

Treat this as connectivity/plumbing validation, not SLO evidence. A clean
short run should show a JSON report whose `gates.passed` is `true`; the script
also exits 0 only in that case.

## 5. Running 3, 5 or 7 nodes

The script does not start servers. Set the addresses and run.

### 3 KV nodes

```sh
export SLO_META=10.20.0.10:9090
export SLO_NODES=10.20.0.11:7001,10.20.0.12:7001,10.20.0.13:7001
export SLO_CLIENTS=16
export SLO_MAX_INFLIGHT=16
export SLO_DURATION=10m
export SLO_VALUE_SIZE=1024
export SLO_KEYSPACE=10000000
export SLO_READ_RATIO=0.8
export SLO_VERIFY_RATE=0.01
export SLO_VERIFY_SAMPLE=32
export SLO_P99_GATE_MS=50
export SLO_MAX_ERROR_RATE=0
./scripts/bench-multihost.sh
```

### 5 KV nodes

```sh
export SLO_META=10.20.0.10:9090
export SLO_NODES=10.20.0.11:7001,10.20.0.12:7001,10.20.0.13:7001,10.20.0.14:7001,10.20.0.15:7001
export SLO_CLIENTS=24
export SLO_MAX_INFLIGHT=24
export SLO_DURATION=15m
export SLO_KEYSPACE=20000000
export SLO_P99_GATE_MS=50
./scripts/bench-multihost.sh
```

### 7 KV nodes

```sh
export SLO_META=10.20.0.10:9090
export SLO_NODES=10.20.0.11:7001,10.20.0.12:7001,10.20.0.13:7001,10.20.0.14:7001,10.20.0.15:7001,10.20.0.16:7001,10.20.0.17:7001
export SLO_CLIENTS=32
export SLO_MAX_INFLIGHT=32
export SLO_DURATION=15m
export SLO_KEYSPACE=30000000
export SLO_P99_GATE_MS=50
./scripts/bench-multihost.sh
```

`scripts/bench-multihost.sh --example 3|5|7` prints the same shape without
touching the cluster.

To run the test directly:

```sh
SLO_RUN=1 SLO_META=... SLO_NODES=... \
  go test -run '^TestSLOMultiHost$' -count=1 -v -timeout 24h ./test
```

## 6. 1 TB procedure

This section describes how to run a 1 TB payload measurement. It is not a
record of one. In this document `GB` means GiB (1024^3 bytes), so the target
is 1024 GiB. At 1 KiB values that means about 1.07 billion keys, before
replication and storage overhead; this is a multi-hour operation and must be
staged.

### Stage plan

| Stage | `SLO_PRELOAD_GB` | Purpose |
|---|---:|---|
| Connectivity | `0` | Validate addresses, auth/TLS assumptions, range map, a 2-minute run. |
| Small preload | `1`–`10` | Validate batch sizing, disk growth, progress logging, timeouts. |
| Medium preload | `100` | Validate multi-hour stability and compaction behavior. |
| Target preload | `1024` | The 1 TB payload run. |

Do not jump to 1024 GiB on the first attempt. Each stage should finish with
zero preload errors, zero verification failures and stable disk free space.

### Example target command

```sh
export SLO_META=10.20.0.10:9090
export SLO_NODES=10.20.0.11:7001,10.20.0.12:7001,10.20.0.13:7001
export SLO_CLIENTS=32
export SLO_MAX_INFLIGHT=32
export SLO_VALUE_SIZE=1024
export SLO_KEYSPACE=100000000
export SLO_READ_RATIO=0.8
export SLO_VERIFY_RATE=0.001
export SLO_VERIFY_SAMPLE=64
export SLO_PRELOAD_GB=1024
export SLO_PRELOAD_WORKERS=8
export SLO_DURATION=30m
export SLO_P99_GATE_MS=100
export SLO_MAX_ERROR_RATE=0
export SLO_MIN_THROUGHPUT=0
export SLO_TEST_TIMEOUT=72h
./scripts/bench-multihost.sh
```

Notes:

- `SLO_PRELOAD_GB` is GiB of value payload, not total cluster disk.
- The harness writes under `slo/preload/` and `slo/workload/` and does **not**
  split ranges or move leaders. If the cluster starts with a single range,
  most traffic is served by one Raft leader regardless of how many KV nodes
  exist. Pre-split/balance a 1 TB keyspace and confirm range distribution
  before preload if the goal is a fan-out measurement; otherwise label the
  result as a single-group measurement.
- The preload is streamed in transactions bounded by about 4 MiB of value
  payload and 2048 keys, with `SLO_PRELOAD_WORKERS` transactions in flight.
  It does not hold the dataset in RAM.
- Preload can take hours. Progress is logged every 10 seconds; the test
  timeout must cover preload plus the measured run.
- Increase `SLO_VALUE_SIZE` if you need fewer keys and can afford larger
  transactions/values; the key count is `target / value_size`.
- Size `SLO_KEYSPACE` from the expected write volume:
  `SLO_KEYSPACE >= writes_per_second * duration_seconds`. If it is smaller,
  workers overwrite keys inside their partition; that is valid but changes the
  read working set and is not the same as a monotonically growing dataset.
- Preloaded keys are separate from workload keys. The optional post-run
  sample checks both.
- There is no cleanup command. Use a dedicated cluster, or plan to wipe and
  rebuild the data directories after the run.

### Repetitions

Run the measured phase at least three times after the preload has completed.
Keep the cluster, client, ratio, value size and keyspace fixed. Report the
median and the worst p99, not only the best run. If compaction or snapshotting
is still active after preload, either wait for a quiet period and record that
choice, or run additional repetitions to quantify the difference.

## 7. Reading the result

The script prints human-readable lines and writes a JSON report. The important
fields are:

```
workload.ops / reads / writes / errors / error_rate
workload.throughput_ops_per_sec
workload.latency.p50_ms / p95_ms / p99_ms
workload.read_latency.*
workload.write_latency.*
verification.inline_* / sample_* / total_failed
preload.bytes / seconds / mib_per_sec
gates.passed / failures
```

### Latency

- Latencies include client-side scheduling, gRPC, TSO/leader discovery, the
  server and network. They are **client-observed**.
- Quantiles are conservative upper bounds of a bounded histogram with eight
  sub-buckets per power of two, clamped to the observed maximum. Do not present
  them as exact microsecond measurements.
- For a meaningful p99, collect far more than 100 operations. A 10-minute run
  at even a few hundred ops/sec yields enough samples; a 2-second smoke run
  does not.
- Compare like with like. A read-only run and an 80% read run are different
  distributions; read and write latency are reported separately for a reason.
- A p99 gate set from a local loopback benchmark is not a production SLO. Set
  gates from a product requirement and a representative topology.

### Throughput

- `throughput_ops_per_sec` counts attempted operations (successful and failed);
  `errors` and `error_rate` describe the failures.
- `mib_per_sec` counts payload returned and written by successful operations.
  It excludes protocol, key and replication overhead.
- The measured mix can deviate from `SLO_READ_RATIO` because of the per-worker
  seed write and random selection. Check `reads`/`writes` in the report rather
  than assuming the configured ratio.

### Errors

The default `SLO_MAX_ERROR_RATE=0` fails on the first workload error. This is
deliberate for acceptance runs. For an environment with known transient
network noise, set an explicit, documented allowance (for example `0.001`)
and still inspect `workload.first_error`. A non-zero allowance is a decision,
not a default.

### Verification

A zero failure count means the sampled keys matched their deterministic
payload. It does **not** prove crash durability, replication correctness under
failure, or that unsampled keys are intact. Increase
`SLO_VERIFY_RATE`/`SLO_VERIFY_SAMPLE` for stronger (and slower) sampling.

### Preload

`preload.mib_per_sec` is value-payload throughput, not disk throughput. Disk
consumption includes replication, WAL/SST and metadata; measure it on the
cluster hosts rather than inferring it from the harness.

## 8. Linux/NVMe checklist

Record the exact commands and outputs with the run artifacts.

- Kernel, distro, CPU model/count, memory, `go version`, build/commit SHA.
- NVMe model, firmware, health (`nvme smart-log`), and free space per node.
- Filesystem type and mount options; local disk only, no NFS/CIFS/overlay
  that hides the real device.
- A short `fio` baseline for random read/write on the data device; retain it
  with the SLO report.
- NIC speed, errors/drops, MTU, and whether the client and cluster are on the
  same L2/L3 path as production.
- `ulimit -n`, `somaxconn`, and any container CPU/memory limits.
- CPU governor, IRQ distribution, swap and transparent-hugepage settings as
  required by your platform team. Change these only with the platform owner;
  do not silently tune them for a single run.
- During the run, sample `iostat -x 1`, `mpstat -P ALL 1`, `sar -n DEV 1`, and
  cluster metrics. Client-side saturation invalidates the measurement.
- Do not run the client on a cluster node. The noise contaminates both sides.

## 9. Evidence and reproducibility

For every claimed result, retain:

- the report JSON and the full log path printed by the script;
- the exact environment block printed at the top of the log;
- commit SHA and container/binary provenance;
- topology diagram or address list, replication factor and meta quorum size;
- hardware and Linux/NVMe checklist outputs;
- whether data was preloaded, how long it took, and whether compaction was
  quiet before the measured phase;
- the three repetitions and which one is being cited;
- the gate values and who approved them.

A minimal evidence table:

| Field | Value |
|---|---|
| Date / operator | |
| Commit / image digest | |
| Topology | 3 / 5 / 7 KV nodes, RF= |
| Client host | |
| NVMe / filesystem | |
| Payload | `SLO_PRELOAD_GB`, value size |
| Workload | clients, in-flight, ratio, duration, keyspace |
| Gates | p95/p99/error/throughput |
| Result | ops/s, p50/p95/p99 read/write, error rate |
| Verification | inline/sample checked, failed |
| Verdict | pass/fail + link to report |

## 10. Exit codes and artifacts

`scripts/bench-multihost.sh`:

| Exit | Meaning |
|---:|---|
| `0` | Test passed and the report exists. |
| `1` | Test failed (gate, verification, workload error, timeout) or the report is missing. |
| `2` | Usage or environment validation error; the cluster was not contacted. |

Artifacts (unless overridden):

```
.tmp-slo/slo-multihost-<run-id>.log
.tmp-slo/slo-multihost-<run-id>.json
```

`SLO_REPORT_JSON` overrides the report path. Relative paths are resolved from
the repository root.

## 11. What is not covered

- **No 1 TB result is claimed.** This runbook and harness only define the
  procedure.
- No in-process cluster, container orchestration, server startup or deployment.
- No failover, leader transfer, partition, crash, rolling upgrade or chaos
  testing. Use the dedicated chaos/Jepsen harnesses for that.
- No proof of durability or recovery. Verification is a sampled read-back of
  committed values, not a crash-consistency test.
- No TLS, mTLS or authentication support in this harness. `v2client.New` uses
  plaintext; run it only on an isolated/trusted network.
- A single `SLO_META` seed address; meta failover is exercised by the client,
  but the run's meta seed list is not configurable here.
- A single client host. It does not model many client machines, WAN latency or
  per-tenant isolation.
- No SQL/pgwire layer; the workload is KV transactions.
- No automatic range splitting, rebalancing or leader placement. The harness
  writes to `slo/...` and does not force splits. A cluster with one range will
  send most traffic to one leader even with 3/5/7 nodes; that is a
  single-group measurement, not a fan-out measurement.
- No server-side CPU, disk, compaction, Raft or per-range metrics in the
  report. Correlate with the cluster's metrics separately.
- Preload counts value payload only; disk usage, replication and compaction
  overhead are not estimated by the harness.
- No automatic cleanup of warmup, workload or preloaded keys.
- No long-term soak guarantee; a 30-minute run is not a 24-hour soak.
- Gates are operator-provided. The harness does not define a product SLO or
  certify that one was met.

## 12. Troubleshooting

| Symptom | Likely cause / action |
|---|---|
| Test skipped | `SLO_RUN` is not `1`, or `-short` was used. Use the script or set `SLO_RUN=1`. |
| `SLO config: ...` failure | Invalid/missing `SLO_*` value; run `--dry-run` for the effective config and fix the named variable. |
| `cannot reach meta` | Address, firewall, TLS/auth mismatch, or meta not ready. This harness is plaintext-only. |
| `meta returned no ranges` | Cluster is not initialized; bootstrap/deploy it first. |
| `SLO warmup: ...` failure | KV route unreachable or the cluster rejects the transaction; check node health and range map. |
| Preload fails partway | Disk full, per-op/commit timeout, or node failure. Preload is not resumable; fix and restart from zero or wipe the prefix. |
| Report missing after exit 0 | `SLO_REPORT_JSON` directory is not writable; the script turns this into a failure. |
| `go test` timeout | Raise `SLO_TEST_TIMEOUT`; for 1024 GiB preload plus a measured run, 72h is a reasonable start. |
| High p99, low throughput | Check client CPU/NIC, GC, disk `iostat`, cluster compaction, TSO/leader hot spots, and whether `SLO_MAX_INFLIGHT` is capping concurrency. |
| Errors mention context deadline | Higher client/network latency or an undersized `SLO_OP_TIMEOUT`; investigate before relaxing. |
| Verification failures | Stop and investigate data-path correctness; do not treat the run as valid. |

## 13. Related files

- `test/slo_multihost_test.go` — external-cluster SLO test and cluster-free
  self-check (`TestSLOSelfCheck`).
- `scripts/bench-multihost.sh` — environment validation, execution, output
  capture and exit-code handling.
- `docs/PERFORMANCE.md` — current loopback/Linux-container measurements; they
  are explicitly not a multi-host SLO.
- `scripts/bench-linux.sh` — in-container speed benchmarks, not a multi-host
  or NVMe SLO run.
