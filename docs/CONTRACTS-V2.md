# V2 package contracts (M0/M1)

This file is the integration contract for the KV v2 packages. Implement your
package exactly against these signatures; do not edit files outside your
ownership. If a signature is genuinely wrong, report it instead of changing a
shared contract.

Go environment for every command:

```bash
export GOPATH=/tmp/dsh-gopath GOMODCACHE=/tmp/dsh-modcache \
       GOCACHE=/tmp/dsh-gocache GOSUMDB=off
```

Rules:

- `gofmt -w` your files; `go test -race ./<your package>/...` must pass.
- Do not edit `go.mod`, `go.sum`, `proto/`, `internal/pb/`, `internal/model/`,
  `docs/`, `ai/tasks/`, or another team's package.
- stdlib only, except where a package explicitly uses an existing dependency
  (`storage` uses `github.com/cockroachdb/pebble`, already in `go.mod`).
- Tests use `testing` only; no testify.
- Do not touch the working v1 cluster (`cmd/db`, `internal/node`,
  `internal/coordinator`, `internal/kv`).

## 1. `internal/storage` — Pebble engine

`internal/storage/api.go` is **fixed** and already contains the interfaces.
Implement in new files (`db.go`, `batch.go`, `iter.go`, `stats.go`, tests):

```go
func Open(dir string, opts Options) (Engine, error)
```

Requirements:

- Pebble v1.1.5; map `Options` to Pebble options (`Cache`, `MemTableSize`,
  `MaxOpenFiles`, `Compression` snappy/zstd, `WALBytesPerSync`, `BytesPerSync`).
- `SyncWrites=true` → batch/Set/Delete use `pebble.Sync`, otherwise
  `pebble.NoSync`; document the tradeoff.
- `Get` returns a copy and `ErrNotFound` for missing keys.
- `NewIter` honours `[start,end)`, nil bounds, and ascending order.
- `Snapshot` is consistent and independent of later writes.
- `Batch` is atomic; `Commit` is durable per `SyncWrites`; `Close` idempotent.
- `Stats` from Pebble metrics (disk size, memtable, counters).
- `Close` flushes and is idempotent.
- Tests: CRUD, missing, batch atomic/rollback on close, iterator bounds,
  snapshot isolation, reopen persistence, concurrent access, large values,
  sync mode.
- Benchmarks: `BenchmarkSet`, `BenchmarkGet`, `BenchmarkScan`.

## 2. `internal/mvcc` — versioned KV and optimistic transactions

Import `distributed-db/internal/storage` for the engine. Key layout:

```
data:  'd' | userKey | bigEndian(commitTS, 8)
lock:  'l' | userKey
write: 'w' | userKey | bigEndian(commitTS, 8)
```

Public API:

```go
type Store struct{}
func New(engine storage.Engine) *Store

func (s *Store) Get(key []byte, ts uint64) ([]byte, error) // ErrKeyNotFound / ErrLocked
func (s *Store) Scan(start, end []byte, ts uint64, limit int) ([]KV, error)
func (s *Store) Prewrite(mutations []Mutation, primary []byte, startTS uint64) error
func (s *Store) Commit(keys [][]byte, startTS, commitTS uint64) error
func (s *Store) Rollback(keys [][]byte, startTS uint64) error
func (s *Store) GC(safePoint uint64) error

type KV struct { Key, Value []byte }
type Mutation struct { Key, Value []byte; Delete bool }
var ErrKeyNotFound, ErrLocked, ErrWriteConflict, ErrAlreadyCommitted error
```

Requirements:

- MVCC read: find the newest data version with `commitTS <= ts`; ignore
  rollbacks; return the write record for `commitTS` if present.
- Locks: persisted `lock` record with primary key, startTS, kind, value, TTL.
  `Prewrite` writes locks and data; primary lock marks the transaction.
- `Commit`: verify locks, write `write` records at `commitTS`, delete locks;
  idempotent for already committed.
- `Rollback`: delete locks and write a rollback record; idempotent.
- `Prewrite` fails with `ErrWriteConflict` if a conflicting commit exists after
  `startTS`; `ErrLocked` if another transaction holds the lock.
- `Scan` merges visible versions in key order, deduplicates by user key,
  respects `limit`; skips locks/rollbacks.
- `GC` removes data/write versions older than the safe point and expired locks
  (basic implementation is fine; document what is left).
- Tests: snapshot isolation, read-your-writes via locks, prewrite/commit,
  rollback, write conflict, lock conflict, scan versions, GC, reopen.
- Use a fake storage engine in tests? Prefer a temp Pebble engine via
  `storage.Open`; if that package is still being written, use an in-package
  fake implementing `storage.Engine` and keep it in `_test.go`.

## 3. `internal/raft` — snapshots and log compaction

Extend the existing package; keep all current public API and tests green.

Add:

```go
type Snapshot struct {
    Index uint64
    Term  uint64
    Data  []byte
}

// Snapshot asks the state machine for a snapshot, persists it and truncates
// the local log prefix.
func (n *Node) Snapshot() (Snapshot, error)

// RestoreSnapshot installs a snapshot received from a leader.
func (n *Node) RestoreSnapshot(s Snapshot) error
```

Requirements:

- `StateMachine` may optionally implement `interface{ Snapshot() ([]byte,uint64,error) }`;
  if not, `Node.Snapshot` returns `ErrSnapshotUnsupported`.
- Persist snapshot metadata and data through a new optional `LogStore`
  capability (`SaveSnapshot`, `LoadSnapshot`, `TruncatePrefix`); keep backward
  compatibility by type-asserting, so old stores still work.
- `FileStore` implements the new methods with a `snapshot/` subdirectory;
  `Load` starts after the snapshot index.
- Add `InstallSnapshotRequest/Response` and `HandleInstallSnapshot`; a follower
  restores the snapshot, truncates its log, sets commit/applied.
- Leader replication: when `nextIndex[peer] <= snapshotIndex`, send a snapshot
  instead of entries; after success set `nextIndex = snapshotIndex+1`.
- Tests: snapshot/restore round-trip, FileStore reload after truncation,
  lagging follower catches up via snapshot, existing election/replication/
  failover tests still pass.
- Do not change `Propose`/`ReadBarrier` semantics.

## 4. `internal/multiraft` — many Raft groups per process

```go
type GroupID uint64

type Status struct {
    ID GroupID
    Leader string
    Term uint64
    CommitIndex uint64
    LastApplied uint64
}

type Group interface {
    ID() GroupID
    Propose(ctx context.Context, cmd model.Entry) (uint64, error)
    Status() Status
    Stop()
}

type Factory interface {
    NewGroup(id GroupID, peers []string) (Group, error)
}

type Host struct{}
func NewHost(f Factory) *Host
func (h *Host) Create(id GroupID, peers []string) error
func (h *Host) Get(id GroupID) (Group, bool)
func (h *Host) Propose(ctx context.Context, id GroupID, cmd model.Entry) (uint64, error)
func (h *Host) Remove(id GroupID) error
func (h *Host) Stop()
```

Requirements:

- Concurrent-safe map of groups; duplicate create is an error; unknown id is
  `ErrGroupNotFound`.
- `Stop` stops all groups and is idempotent; no creation after stop.
- Tests with a fake `Factory`/`Group`: create/get/propose routing, duplicate,
  remove, stop, concurrent access.
- Do not import `internal/node` or `internal/coordinator`.

## 5. `internal/meta` — range map, placement, TSO

```go
type Range struct {
    ID uint64
    Start, End []byte // [Start, End), End=nil means +inf
    Replicas []string
    Leader string
    Version uint64
}

type Cluster struct{}
func New() *Cluster
func (c *Cluster) RangeForKey(key []byte) (*Range, error)
func (c *Cluster) Split(id uint64, at []byte) (*Range, *Range, error)
func (c *Cluster) Merge(left, right uint64) (*Range, error)
func (c *Cluster) UpdateLeader(id uint64, leader string) error
func (c *Cluster) AllocID() uint64
func (c *Cluster) AllocTS(n int) (start uint64, count int)
func (c *Cluster) Heartbeat(node string, ranges []uint64) error
func (c *Cluster) Snapshot() []Range
```

Requirements:

- Initial cluster has one range `[nil,nil)` with ID 1.
- Ranges sorted by `Start`; no overlaps/gaps; `RangeForKey` handles nil
  boundaries and `[]byte` lexicographic order.
- `Split` validates `at` is strictly inside the range; the left range keeps the
  original ID, the right range gets a fresh ID (from `AllocID`, which starts at
  2 so ID 1 is never reissued); both get a version bump. `Merge` requires
  adjacent ranges and same replicas.
- TSO: physical milliseconds << 18 | logical; monotonic under concurrency;
  `AllocTS(n)` reserves n consecutive timestamps and returns `(start, n)`;
  `n <= 0` returns `(0, 0)`.
- Heartbeat records last-seen; no scheduler yet.
- Tests: lookup, split/merge, invalid boundaries, concurrent TSO monotonic,
  AllocID uniqueness, snapshot isolation of returned slices.

## 6. `internal/client` — smart KV client

```go
type KV struct { Key, Value []byte }

type Router interface {
    Leader(ctx context.Context, key []byte) (addr string, err error)
}
type Transport interface {
    Get(ctx context.Context, addr string, key []byte) ([]byte, error)
    Put(ctx context.Context, addr string, key, value []byte) error
    Delete(ctx context.Context, addr string, key []byte) error
    Scan(ctx context.Context, addr string, start, end []byte, limit int) ([]KV, error)
}

type Options struct {
    MaxRetries int
    Backoff time.Duration
    Timeout time.Duration
}

type Client struct{}
func New(router Router, transport Transport, opts Options) *Client
func (c *Client) Get(ctx context.Context, key []byte) ([]byte, error)
func (c *Client) Put(ctx context.Context, key, value []byte) error
func (c *Client) Delete(ctx context.Context, key []byte) error
func (c *Client) Scan(ctx context.Context, start, end []byte, limit int) ([]KV, error)
```

Requirements:

- On `ErrNotLeader`/`ErrUnavailable` (define sentinels) refresh the router and
  retry with exponential backoff up to `MaxRetries`.
- Cache the last leader per key prefix in a small LRU (e.g. 1024 entries).
- Respect ctx; no retry after context cancellation.
- Tests with fake router/transport: retry, cache invalidation, timeout,
  no-retry on not-found, concurrency.

## 7. `bench/` — workload generator

- CLI `go run ./bench -workload uniform|zipfian|sequential -keys 100000 -ops 1000000`.
- Engine interface `Get/Put/Scan`; in-memory implementation for the harness,
  later server adapter.
- Metrics: ops/sec, p50/p95/p99/p999, errors, total bytes; output table + JSON.
- Tests for percentile math, workload key generation, deterministic seed.
- No production imports; keep it runnable without a cluster.

## 8. SQL ADR (`docs/adr/0004-sql-layer.md`)

Research and write an ADR for the SQL layer on top of transactional KV:

- parser/AST/planner options (build vs libraries; license/Go compatibility);
- schema/catalog encoding; row and secondary-index key layout;
- distributed execution model; joins/aggregates/order/limit;
- transaction integration and isolation;
- PostgreSQL wire compatibility scope;
- phased plan and risks;
- recommendation: build a minimal SQL subset first, borrow a parser if licensing
  allows.

No code. Only `docs/adr/0004-sql-layer.md`.
