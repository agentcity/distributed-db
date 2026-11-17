# SOAK: 72 h and 1 TiB multi-host harness

Status: **harness and procedure only**. No 72 h or 1 TiB run has been
performed or recorded in this repository. The commands below are the procedure
for producing that evidence on a dedicated cluster; they are not evidence by
themselves.

The harness is `TestSoakMultiHost` in `test/soak_test.go`. The wrapper is
`scripts/run-soak.sh`. Neither starts a meta service, a KV node, a container or
a database: both require an already-deployed cluster reached through
`SOAK_META` and `SOAK_NODES`.

## What the harness measures

One client host runs a closed-loop mixed workload against an external cluster:

- single-key `Begin`/`Get` transactions and `Begin`/`Set`/`Commit`
  transactions;
- one disjoint key partition per client goroutine, so the hot path has no
  cross-client coordination;
- a global in-flight semaphore (`SOAK_MAX_INFLIGHT`);
- p50/p95/p99 latency for all operations, reads and writes;
- aggregate/read/write throughput, byte counts and error rate;
- the same latency/throughput/error/byte metrics in periodic buckets, so
  latency drift is visible over the run;
- `runtime.ReadMemStats` heap fields, GC pause totals and goroutine counts per
  bucket;
- process RSS when the runner exposes `/proc/self/statm` or
  `/proc/self/status` (Linux; omitted on platforms without `/proc`);
- node reachability/term/commit-index via the KV `Status` RPC;
- best-effort disk growth by summing `RangeStats.size_bytes` for the ranges
  returned by the meta service;
- optional WAL and compaction metrics scraped from Prometheus text endpoints.

The measured phase excludes warmup and preload. The optional preload streams a
deterministic dataset before the measured phase; its keys are never deleted by
the harness.

## Prerequisites

- 3 or 5 KV nodes, already running, with a working meta/TSO service.
- At least 3 nodes for a multi-host claim. The wrapper enforces this unless
  `SOAK_ALLOW_SMALL_CLUSTER=1` is set for a local experiment.
- A dedicated client host. Do not run the soak client on a KV node: client CPU,
  heap and network would compete with the server under test.
- Enough capacity for the preload. `SOAK_PRELOAD_GB` counts **value payload
  only**. At replication factor 3, 1 TiB of values needs roughly 3.3 TiB of raw
  cluster capacity before WAL/SST/compaction headroom. Stage capacity and free
  space first; a full disk turns the run into a disk-gate test, not a soak.
- A supported metrics path for WAL evidence. The current `Status` and
  `RangeStats` RPCs do not expose WAL bytes or compactions, so a WAL gate
  requires `SOAK_METRICS_URLS` (or `SOAK_WAL_METRIC` against an exporter).
- Clock synchronization on the client and servers (NTP or equivalent). The
  report uses client UTC timestamps and cluster counters; unsynchronized clocks
  make cross-source correlation unreliable.
- Go 1.23+ on the client host.
- Filesystem space for the report and log. A 72 h run at the default 60 s
  sampling interval produces about 4,300 buckets plus the log. A 10 s interval
  produces about 26,000 buckets and a much larger JSON file.

## Quick checks (no cluster required)

```bash
# Package compiles without running any test.
go test ./test -run '^$'

# Always-on harness smoke test: config, histogram, summaries and gates.
go test -short -run 'TestSoakSelf' -v ./test

# Wrapper syntax and examples.
bash -n scripts/run-soak.sh
./scripts/run-soak.sh --example 3
./scripts/run-soak.sh --example 5
```

`TestSoakSelf` never contacts a cluster. `TestSoakMultiHost` skips unless
`SOAK_RUN=1` and also skips under `-short`.

## 72 h procedure

### 1. Prepare the cluster and the client

Replace the example addresses with the real ones. Keep the same client host,
Go build, seed, value size and topology for runs that will be compared.

```bash
export SOAK_META=10.20.0.10:9090
export SOAK_NODES=10.20.0.11:7001,10.20.0.12:7001,10.20.0.13:7001
export SOAK_CLIENTS=16
export SOAK_MAX_INFLIGHT=16
export SOAK_VALUE_SIZE=1024
export SOAK_KEYSPACE=10000000
export SOAK_READ_RATIO=0.8
export SOAK_PRELOAD_GB=1024
export SOAK_SAMPLE_INTERVAL=1m
export SOAK_REPORT_JSON=/var/tmp/soak-evidence/soak-72h.json
export SOAK_OUT_DIR=/var/tmp/soak-evidence
```

### 2. Validate without contacting the cluster

```bash
./scripts/run-soak.sh --duration 72h --preload-gb 1024 --dry-run
```

Check the printed node list, duration, timeout, gates, report path and output
directory. With a 1 TiB preload the derived `go test -timeout` is about 103 h
(72 h measured + an estimated preload at 10 MiB/s + 2 h slack). Override with
`--timeout` if the preload is faster or slower, or if the client is paused
during the run.

### 3. Stage the dataset

The preload runs inside the same test before the measured window. For 1 TiB:

```bash
./scripts/run-soak.sh --duration 72h --preload-gb 1024 \
  --p99-ms 250 \
  --memory-growth-mib-per-hour 64 \
  --wal-growth-mib-per-hour 128 \
  --metrics-urls http://10.20.0.11:9100/metrics,http://10.20.0.12:9100/metrics,http://10.20.0.13:9100/metrics
```

The wrapper `tee`s the full output and prints the log and report paths at the
end. With 2,048 keys per batch (at 1 KiB values, capped by 4 MiB), the preload
writes about 1.07 billion keys as roughly 524,000 transactions. At 10 MiB/s the
value payload alone takes about 29 h; at 50 MiB/s about 6 h. Preload throughput
is reported separately and is never included in the measured throughput or p99.

Preloaded keys live under `soak/preload/<ordinal>`. They are not removed by the
harness; cleanup is an operator decision.

Preload throughput depends heavily on value size, transaction size, commit
latency, replication and compaction. Measure a 10–50 GiB pilot on the same
cluster, then set the timeout from the measured rate:

```text
timeout = measured_duration + preload_GiB*1024/measured_MiBps*3600 + slack
```

The wrapper's derived timeout assumes 10 MiB/s, which is optimistic for small
values or slow commits. Pass `--timeout` explicitly for the 1 TiB run. If the
preload is slower than the timeout allows, `go test` panics before the report is
written.

### 4. Let the measured phase run

Start the run only after the preload completes and the harness logs
`SOAK preload: complete`. Do not restart the client, change the value size or
resize the cluster during the measured phase. Record any external event
(rolling restart, network change, competing load) in the evidence bundle: the
JSON cannot infer it.

Watch progress in the log:

```bash
tail -f /var/tmp/soak-evidence/soak-<run-id>.log
```

Every bucket logs elapsed time, ops, throughput, reads/writes, errors,
p99/all/read/write, heap, goroutines, RSS, WAL, disk and reachable nodes.

### 5. Review the result

The run exits non-zero when a configured gate fails. The JSON report is written
**before** gate failures are reported, so a failed gate still leaves evidence.
Setup or preload failures can terminate the test before a report is written;
in that case the log is the only artifact.

```bash
jq '.workload, .gates, .latency_drift, .memory, .storage, .cluster' \
  /var/tmp/soak-evidence/soak-72h.json
```

Do not quote the numbers as a production result until the environment,
client saturation, cluster events and the "what is not covered" list below have
been reviewed.

## 1 TiB staging math

`SOAK_PRELOAD_GB` is GiB of value payload (`1 GiB = 1<<30` bytes). It does not
include keys, transaction metadata, Raft logs, WAL, SST files or replication.

| Value size | 1 TiB payload keys | Notes |
|---|---:|---|
| 1 KiB | 1,073,741,824 | Default; smallest per-key overhead, largest key count. |
| 4 KiB | 268,435,456 | Fewer keys, more bytes per transaction batch. |
| 16 KiB | 67,108,864 | Useful when per-key overhead dominates. |

At RF=3, budget at least 3× the value payload in raw cluster capacity, plus
WAL/SST/compaction headroom. A safe staging rule is **4×** the logical payload
(or more for small values or high overwrite churn). For 1 TiB of values, stage
4 TiB or more of raw capacity and verify free space on every node immediately
before the run.

The preload is streamed in bounded batches and is never materialized in RAM.
The workload keyspace (`SOAK_KEYSPACE`) is separate from the preload dataset:
the measured workload reads only keys it initialized itself, while the preload
provides bulk occupied storage. Size `SOAK_KEYSPACE` for the measured working
set, not for the preload.

## Environment reference

Required:

| Variable | Meaning |
|---|---|
| `SOAK_RUN=1` | Gate. Any other value skips `TestSoakMultiHost`. The wrapper always sets it. |
| `SOAK_META` | Meta/TSO gRPC `host:port`. |
| `SOAK_NODES` | Comma-separated KV gRPC `host:port` list. |

Workload:

| Variable | Default | Meaning |
|---|---|---|
| `SOAK_DURATION` | `72h` | Measured duration, Go duration, seconds or a day component such as `3d`/`1d12h`. |
| `SOAK_CLIENTS` | `16` | Client goroutines (1–1024). |
| `SOAK_MAX_INFLIGHT` | clients | Global in-flight cap (1–8192). With synchronous workers, actual in-flight is at most clients. |
| `SOAK_READ_RATIO` | `0.8` | Fraction of reads, 0–1. |
| `SOAK_VALUE_SIZE` | `1024` | Value payload bytes (1–16 MiB). |
| `SOAK_KEYSPACE` | `10000000` | Logical key budget, partitioned by client. Must be ≥ clients. |
| `SOAK_OP_TIMEOUT` | `10s` | Per-operation timeout. |
| `SOAK_SAMPLE_INTERVAL` | `1m` | Bucket, memory and probe interval (5s–1h). |
| `SOAK_SEED` | time-derived | Reproducible RNG seed; recorded in the report. |
| `SOAK_VERIFY_RATE` | `0.001` | Fraction of reads that compare the full deterministic payload. |
| `SOAK_VERIFY_SAMPLE` | `64` | Post-run sampled reads from workload and preload keys (0–10000). |

Preload:

| Variable | Default | Meaning |
|---|---|---|
| `SOAK_PRELOAD_GB` | `0` | Value payload target in GiB; `1024` = 1 TiB. |
| `SOAK_PRELOAD_WORKERS` | `4` | Concurrent preload transactions (1–64). |
| `SOAK_PRELOAD_BATCH_KEYS` | `2048` | Maximum keys per preload transaction; the 4 MiB byte cap also applies. |

Gates (see "Gates" below):

| Variable | Default | Meaning |
|---|---|---|
| `SOAK_P99_GATE_MS` | `0` (off) | Fail if all/read/write p99 exceeds this. |
| `SOAK_MAX_ERROR_RATE` | `0.001` | Fail above this workload error ratio. |
| `SOAK_MEMORY_GROWTH_MIB_PER_HOUR` | `0` (off) | Fail above this client heap in-use slope after warmup. |
| `SOAK_WAL_GROWTH_MIB_PER_HOUR` | `0` (off) | Fail above this WAL slope; requires a WAL metric. |
| `SOAK_MIN_THROUGHPUT` | `0` (off) | Fail below this aggregate ops/s. |

Evidence and probes:

| Variable | Default | Meaning |
|---|---|---|
| `SOAK_REPORT_JSON` | none | Report path. Use an absolute path when invoking `go test` directly; `go test ./test` runs the test with `./test` as its working directory. The wrapper absolutizes it. |
| `SOAK_METRICS_URLS` | none | Comma-separated Prometheus text endpoints (`/metrics`). Values are summed across endpoints and series. |
| `SOAK_WAL_METRIC` | auto | Explicit WAL bytes metric name, overriding the alias list. |
| `SOAK_DISK_METRIC` | auto | Explicit disk bytes metric name, used only as a fallback when `RangeStats` is unavailable. |
| `SOAK_COMPACTION_METRIC` | auto | Explicit compaction counter metric name. |
| `SOAK_COMPACTION_BYTES_METRIC` | auto | Explicit compaction-bytes counter metric name. |
| `SOAK_DISK_PROBE` | `1` | Enable the `RangeStats` disk probe. |
| `SOAK_DISK_PROBE_INTERVAL` | `5m` | Disk probe interval (10s–24h). |

Wrapper-only variables: `SOAK_OUT_DIR`, `SOAK_RUN_ID`, `SOAK_TEST_TIMEOUT`,
`SOAK_ALLOW_SMALL_CLUSTER`, `SOAK_REPORT_JSON`. `SOAK_TEST_TIMEOUT` is passed
straight to `go test -timeout`.

## Metrics endpoint contract

When `SOAK_METRICS_URLS` is set, the harness scrapes each endpoint and sums all
series matching a metric name. The built-in aliases are:

- WAL bytes (gauge): `pebble_wal_physical_size_bytes`,
  `pebble_wal_size_bytes`, `rocksdb_wal_size_bytes`,
  `kvserver_wal_size_bytes`, `db_wal_size_bytes`, `wal_size_bytes`.
- Disk bytes (gauge, fallback only): `pebble_disk_space_usage_bytes`,
  `pebble_disk_usage_bytes`, `kvserver_disk_used_bytes`,
  `db_disk_size_bytes`, `disk_usage_bytes`.
- Compactions (counter): `pebble_compactions_total`,
  `pebble_compaction_count`, `kvserver_compactions_total`,
  `compactions_total`.
- Compaction bytes written (counter):
  `pebble_compaction_bytes_written_total`,
  `pebble_compact_bytes_written_total`,
  `compaction_bytes_written_total`.

The current `kvserver` binary does not export Pebble WAL/compaction metrics by
itself. Point `SOAK_METRICS_URLS` at an exporter that does, or use
`SOAK_WAL_METRIC` / `SOAK_COMPACTION_METRIC` with the names your exporter
provides. If no WAL metric is available and `SOAK_WAL_GROWTH_MIB_PER_HOUR` is
non-zero, the run **fails**: a configured gate is never silently skipped.

Multiple endpoints are treated as one fleet: values are summed. Do not include
the same series twice (for example a federation endpoint that already includes
a per-node endpoint). Keep the endpoint set stable for the whole run: if one
endpoint disappears for a scrape, a summed WAL gauge dips and a summed
compaction counter looks like it reset. Prefer one aggregated endpoint for
counters. Avoid embedding credentials in URLs. The wrapper redacts URL
userinfo in its log and the report stores only the target count, but a
credential in a URL is still a credential: use a metrics proxy or network
policy instead.

## Disk and compaction evidence

Disk growth is probed through the KV `RangeStats` RPC, which reports the
storage-engine disk usage for a hosted range. The harness lists ranges from the
meta service, tries the range leader first, accepts only a response whose
start/end match the descriptor, and sums `size_bytes` and `key_count`. The probe
is approximate:

- it counts each reported range once; the reported size includes that replica's
  engine usage, not filesystem free space;
- it can time out (20 s per probe) and produce a **partial** sample; the report
  marks `disk_partial=true` and records `disk_ranges_probed` / `disk_ranges_total`;
- it costs one RPC per range per probe interval, so lower the frequency on
  clusters with thousands of ranges;
- if the probe finds no ranges, the optional disk metric is used as a fallback.

Compaction evidence comes only from metrics. The report records the first and
last counter values, the monotonic delta, counter resets, and the compaction
bytes delta. A zero delta on a quiet dataset is not proof that compaction works;
it means no compaction counter movement was observed.

`Status` probes are read-only and cheap. They provide reachability, role, term,
commit index and leader per node, plus a cumulative leader-change count. They
do not expose storage state.

## Report layout

The JSON report is the primary evidence. Top-level sections:

- `harness`, `timestamp`, `client_host`, `build` — harness identity and VCS
  revision/Go version for reproducibility;
- `meta`, `nodes`, `config` — the cluster endpoints and every effective knob
  (including gates and seed);
- `preload` — target/actual bytes, keys, transactions, duration and MiB/s;
- `workload` — full-run ops, reads, writes, errors, error rate, throughput,
  bytes, MiB/s and latency for all/read/write;
- `buckets[]` — per-interval ops/errors/throughput, p50/p95/p99 for
  all/read/write, memory, cluster reachability, WAL/disk/compaction samples;
- `latency_drift` — p99 points, min/max/first/last and least-squares slope in
  ms/hour for all/read/write;
- `memory` — heap in-use/alloc slopes in MiB/hour, RSS slope when available,
  goroutine start/end/max, GC pause total and GC count;
- `storage` — disk/WAL start/end/max and growth slopes, compaction deltas,
  probe availability/partial state and last errors;
- `cluster` — final per-node status, range count and leader changes;
- `verification` — inline and post-run sampled payload checks;
- `gates` — each gate's limit, observed value, evaluation state, pass/fail and
  the `failures` list.

Latency quantiles are conservative histogram bucket upper bounds clamped to the
observed maximum, not exact order statistics. They are suitable for trend and
gate comparisons, not for claims finer than one bucket.

Memory and WAL slopes are ordinary least-squares fits over bucket samples.
Memory uses only samples after a warmup of `min(duration/10, 1h)`; when that
warmup is under one minute (configured durations below 10 minutes) no warmup is
skipped and all samples are used. A slope requires at least three points
spanning 60 s. WAL uses the full run. A slope can be negative; negative
growth passes a maximum-bound gate. Gates are evaluated on the client Go heap
in-use slope (`heap_inuse_slope_mib_per_hour`), not on RSS. RSS is reported for
correlation because it includes non-Go memory and can fluctuate with the OS.

## Gates

A run exits non-zero if any gate fails. Gate evaluation happens after the JSON
report is written.

| Gate | What is compared | Default |
|---|---|---|
| `p99` | Maximum of all/read/write p99 vs `SOAK_P99_GATE_MS`. | Off |
| `error_rate` | `errors / ops` vs `SOAK_MAX_ERROR_RATE`. | `0.001` |
| `memory_growth` | Client heap in-use slope (MiB/h) vs `SOAK_MEMORY_GROWTH_MIB_PER_HOUR`. | Off |
| `wal_growth` | WAL bytes slope (MiB/h) vs `SOAK_WAL_GROWTH_MIB_PER_HOUR`. | Off |
| `integrity` | Inline + post-run verification failures vs 0. | Always on |
| `throughput` | Aggregate ops/s vs `SOAK_MIN_THROUGHPUT`. | Off |

Rules that matter for interpreting results:

- **A configured gate that cannot be evaluated fails.** A WAL bound without a
  WAL metric, or a memory bound without enough samples, produces a failure and
  an explicit `detail`, not a pass.
- **Disabled gates report `configured=false` and pass.** They still appear in
  the report so the absence of a bound is visible.
- **Zero operations fail.** No measured operations is always a failure.
- **Integrity failures fail.** A read that misses a key this worker committed,
  returns the wrong value length, or mismatches the deterministic payload is
  treated as data loss. A run that disables verification entirely
  (`SOAK_VERIFY_RATE=0` and `SOAK_VERIFY_SAMPLE=0`) also fails the integrity
  gate: passing without checks would be a silent no-evidence result.
- **Gate failures do not suppress the report.** The test uses `t.Errorf` after
  writing the JSON.

Suggested starting bounds for a dedicated LAN cluster (adjust to the SLO being
tested, and record the choice before the run):

- `SOAK_P99_GATE_MS=250` for 1 KiB point operations;
- `SOAK_MAX_ERROR_RATE=0.001`;
- `SOAK_MEMORY_GROWTH_MIB_PER_HOUR=64` (about 4.5 GiB over 72 h; a flat Go
  heap after warmup is expected);
- `SOAK_WAL_GROWTH_MIB_PER_HOUR=128` only after establishing a baseline for the
  workload and compaction policy;
- `SOAK_MIN_THROUGHPUT` only after a shorter pilot establishes the expected
  rate; an arbitrary bound turns a healthy run into a false failure.

## Reproducibility

- Pass an explicit `SOAK_SEED` for runs that must be comparable. The report
  records the seed; if it was time-derived, the report still shows the value
  used.
- Keep `SOAK_CLIENTS`, `SOAK_MAX_INFLIGHT`, `SOAK_VALUE_SIZE`, `SOAK_KEYSPACE`,
  `SOAK_READ_RATIO`, `SOAK_SAMPLE_INTERVAL` and `SOAK_DURATION` identical when
  comparing two runs.
- Keep the same client host and Go build. The report's `build.go_version`,
  `build.revision`, `build.vcs_time` and `build.dirty` identify the binary.
- Record cluster topology, replication factor, storage engine settings,
  compaction settings, node counts and any node events in the evidence bundle.
  The harness cannot see server configuration.

## Evidence collection

Collect at minimum:

1. The JSON report and the full `tee` log.
2. The effective configuration (the wrapper writes it at the top of the log).
3. Cluster topology, RF, storage settings and node identities.
4. Client host resource data (`sar`, `iostat`, `vmstat`, `ss`, container
   metrics) for the run window.
5. Server metrics (CPU, disk, Raft, WAL, compaction) for the same timestamps.
6. A note of every external event: rolling restart, network change, competing
   workload, operator command.
7. The report's `build` section and a hash of the binary/checkout.

Useful `jq` commands:

```bash
jq '{harness, timestamp, build, meta, nodes, config}' soak.json
jq '{workload, gates}' soak.json
jq '.latency_drift' soak.json
jq '.memory, .storage, .cluster' soak.json
jq '[.buckets[] | {t: .elapsed_seconds, ops, p99: .latency.p99_ms,
                    heap_mib: (.memory.heap_inuse_bytes/1048576),
                    rss_mib: (.memory.rss_bytes/1048576),
                    wal: .storage.wal_bytes,
                    disk: .storage.disk_bytes}]' soak.json
jq '.buckets | length' soak.json
```

Checksum the artifacts and store them outside the client host:

```bash
sha256sum soak-72h.json soak-72h.log
```

## What is not covered

- **No claim of a completed 72 h or 1 TiB run.** This repository contains the
  harness and procedure; it does not contain that evidence.
- **Client-side view only.** The harness measures one client host against the
  cluster. It does not measure server Go heap, per-range latency, Raft apply lag,
  disk I/O latency, CPU saturation, network loss or filesystem-level growth.
- **No fault injection.** It does not kill nodes, partition the network, skew
  clocks, restart processes or exercise rolling upgrades. Use the chaos and
  Jepsen suites for those claims.
- **No durability/crash-consistency claim.** The sampled verification reads
  values after the run; it is not a crash test and does not prove that an
  acknowledged write survives a power loss.
- **WAL/compaction evidence is conditional.** It requires a metrics exporter
  that exposes the relevant counters. `Status` and `RangeStats` do not provide
  WAL bytes or compaction counters.
- **Disk evidence is approximate and can be partial.** `RangeStats` reports
  engine usage per range, not free filesystem space; the probe has a 20 s
  budget and records partial samples.
- **RSS is platform-dependent.** Linux `/proc` is supported; other platforms
  omit RSS rather than reporting a high-water mark as if it were current.
- **No TLS/mTLS in this harness.** The workload client and probe dial with
  plaintext gRPC, matching the v2 client used here. A TLS-only deployment needs
  a harness change; do not point this script at one and expect valid evidence.
- **No multi-client-host aggregation.** Running several clients gives several
  reports; combining them is an operator task and the p99s are not pooled.
- **Preload is value payload only.** Replication, WAL, SST, keys, MVCC versions
  and compaction overhead are additional and depend on workload/churn.
- **No automatic cleanup.** Preload keys (`soak/preload/`) and workload keys
  (`soak/workload/`) remain until deleted by an operator.

## Troubleshooting

| Symptom | Likely cause and action |
|---|---|
| `go test -timeout ... is too short` | The test refuses a timeout shorter than the measured duration plus slack. Pass `-timeout 96h` (or the wrapper's derived value) for 72 h. |
| `WAL growth gate configured but not evaluated` | No WAL metric reached the harness. Set `SOAK_METRICS_URLS` or `SOAK_WAL_METRIC`, or disable the WAL gate. |
| `memory growth gate ... insufficient samples` | The run was too short or sampling was interrupted. Use a longer run or lower sampling interval; do not lower the gate to hide it. |
| `verification failures=...` | Treat as potential data loss. Preserve the log/report, stop the run, and investigate before retrying. |
| `disk_partial=true` | The `RangeStats` probe could not cover every range in 20 s. Lower `SOAK_DISK_PROBE_INTERVAL`, or accept partial disk evidence and say so. |
| `rss_available=false` | The client host has no `/proc` (for example macOS). Heap slope is still available; run the 72 h job on Linux for RSS. |
| Preload transactions fail | The server may cap transaction size. Lower `SOAK_PRELOAD_BATCH_KEYS`; the 4 MiB payload cap already limits large values, but a value near the 16 MiB maximum can exceed the server's 16 MiB gRPC message cap including transaction overhead. Use smaller values for multi-TiB staging. |
| Error rate rises over the run | Check client CPU/FDs, `SOAK_OP_TIMEOUT`, cluster health and whether `SOAK_CLIENTS` is saturating the client. The harness does not auto-tune. |
| Report is missing | Setup/preload failed before the report point. The log is the artifact; the test writes the JSON only after a completed measured phase. |

## Acceptance checklist

- [ ] Cluster and meta are external; this repository started nothing.
- [ ] Node count, RF, capacity, free space and metrics exporter recorded.
- [ ] 1 TiB payload staged with at least 3–4× raw headroom at RF=3.
- [ ] `SOAK_SEED`, duration, clients, value size, keyspace and read ratio
      recorded and identical across compared runs.
- [ ] Preload completed at the target bytes; preload throughput recorded.
- [ ] Measured phase ran for the configured duration without an unexplained
      client restart.
- [ ] JSON report exists, parses, and contains buckets, drift, memory, storage,
      cluster and gates.
- [ ] Every configured gate is `evaluated=true` and `passed=true`, or the
      failure is explained.
- [ ] WAL and compaction evidence is present, or its absence is stated
      explicitly as a limitation.
- [ ] RSS is present, or the platform limitation is stated.
- [ ] Log, report, checksums, cluster metrics and external-event notes are
      archived together.
- [ ] No 72 h or 1 TiB claim is made unless the artifact set proves it.

## Related files

- `test/soak_test.go` — harness and cluster-free smoke test.
- `scripts/run-soak.sh` — validated wrapper, output capture and report path.
- `docs/SLO-RUNBOOK.md` — shorter multi-host SLO harness; useful for pilots.
- `docs/OPERATIONS.md` — cluster operations and disk/lifecycle context.
