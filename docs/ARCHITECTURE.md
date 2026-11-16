# Architecture and package contracts

This document is the integration contract between packages. Keep it in sync
with the code. The project is a sharded, replicated key/value store:

- every shard is an independent Raft group with `N` replicas;
- a process runs either one node (member of exactly one shard) or the
  coordinator (stateless router);
- clients talk to the coordinator; the coordinator hashes the key to a shard,
  finds that shard's current leader and proxies the request;
- nodes also expose a direct HTTP/gRPC API for debugging and for Raft RPCs.

```
client ──HTTP──> coordinator ──gRPC──> node (leader of shard)
                                      │
                                      ├── Raft log + WAL
                                      └── KV state machine + WAL
```

## Module layout

```
cmd/db/               one binary: `db node ...` or `db coordinator ...`
internal/model/       Entry / CommandType, no project imports
internal/raft/        consensus core; depends only on model + stdlib
internal/rafttest/    in-memory network used by raft and integration tests
internal/kv/          state machine (map + WAL); implements raft.StateMachine
internal/pb/          generated gRPC code (do not edit)
internal/node/        node adapters: gRPC/HTTP servers, peer transport, metrics
internal/coordinator/ cluster entry point: shard map, routing, HTTP/gRPC
test/                 end-to-end tests
docker/, docs/        deployment and documentation
```

No package may import `internal/node` or `internal/coordinator` except
`cmd/db` and tests. `internal/raft` must never import `internal/node` or
`internal/kv`.

## Shared model (`internal/model`)

```go
type CommandType string
const (
    CommandNoop   CommandType = "noop"
    CommandPut    CommandType = "put"
    CommandDelete CommandType = "delete"
)
type Entry struct {
    Term  uint64      `json:"term"`
    Index uint64      `json:"index"` // 1-based
    Type  CommandType `json:"type"`
    Key   string      `json:"key,omitempty"`
    Value []byte      `json:"value,omitempty"`
}
```

## Raft core (`internal/raft`)

```go
type Role string
const (
    Follower  Role = "follower"
    Candidate Role = "candidate"
    Leader    Role = "leader"
)

type Config struct {
    ID                 string        // unique id == raft address
    Peers              []string      // other members, ids are addresses
    DataDir            string        // WAL directory
    ElectionTimeoutMin time.Duration // e.g. 300ms
    ElectionTimeoutMax time.Duration // e.g. 600ms
    HeartbeatInterval  time.Duration // e.g. 100ms
    Logger             *slog.Logger
}

type Status struct {
    ID          string
    Role        Role
    Term        uint64
    Leader      string
    CommitIndex uint64
    LastIndex   uint64
    LastApplied uint64
}

type RequestVoteRequest struct {
    Term         uint64
    CandidateID  string
    LastLogIndex uint64
    LastLogTerm  uint64
}
type RequestVoteResponse struct {
    Term        uint64
    VoteGranted bool
}

type AppendEntriesRequest struct {
    Term         uint64
    LeaderID     string
    PrevLogIndex uint64
    PrevLogTerm  uint64
    Entries      []model.Entry
    LeaderCommit uint64
}
type AppendEntriesResponse struct {
    Term          uint64
    Success       bool
    MatchIndex    uint64
    ConflictIndex uint64
}

type Transport interface {
    RequestVote(ctx context.Context, peer string, req *RequestVoteRequest) (*RequestVoteResponse, error)
    AppendEntries(ctx context.Context, peer string, req *AppendEntriesRequest) (*AppendEntriesResponse, error)
}

type StateMachine interface {
    Apply(entry model.Entry) error
}

type PersistentState struct {
    Term     uint64
    VotedFor string
    Entries  []model.Entry
}

// LogStore is durable: SaveMeta and Append must fsync before returning.
// Entries are 1-based and contiguous. Truncate removes Index >= fromIndex.
type LogStore interface {
    Load() (PersistentState, error)
    SaveMeta(term uint64, votedFor string) error
    Append(entries ...model.Entry) error
    Truncate(fromIndex uint64) error
}

func New(cfg Config, store LogStore, sm StateMachine, tr Transport) (*Node, error)

// Start launches background goroutines and returns immediately. Cancelling
// ctx is equivalent to Stop.
func (n *Node) Start(ctx context.Context) error
func (n *Node) Stop()

// Propose appends cmd on the leader, replicates it and returns only after the
// entry is committed and applied. Term/Index are filled by Raft.
// Returns ErrNotLeader when this node is not the leader, ErrTimeout when the
// entry could not be committed before ctx deadline.
func (n *Node) Propose(ctx context.Context, cmd model.Entry) (uint64, error)

// ReadBarrier checks that this node is still a leader with a valid quorum
// lease and waits until the state machine has applied at least the commit
// index (and the current leader's no-op) observed at the start. This makes a
// following local read linearizable, including right after a failover.
func (n *Node) ReadBarrier(ctx context.Context) error

func (n *Node) Status() Status
func (n *Node) Leader() string
func (n *Node) IsLeader() bool

func (n *Node) HandleRequestVote(ctx context.Context, req *RequestVoteRequest) (*RequestVoteResponse, error)
func (n *Node) HandleAppendEntries(ctx context.Context, req *AppendEntriesRequest) (*AppendEntriesResponse, error)

var (
    ErrNotLeader = errors.New("raft: not leader")
    ErrTimeout   = errors.New("raft: proposal timed out")
    ErrStopped   = errors.New("raft: stopped")
)

// FileStore is the production LogStore. It keeps a single append-only WAL in
// DataDir with meta records and entry/truncate records; Load replays it.
func NewFileStore(dir string) (*FileStore, error)
```

Correctness requirements:

1. `term`/`votedFor` are persisted before a vote is granted or a term is
   adopted; log entries are persisted before they are acknowledged.
2. At most one leader per term: a candidate needs a strict majority of the
   configured cluster (including itself).
3. Leader commits only when an entry from its own term is stored on a majority
   (`matchIndex >= N`). Later entries are committed by induction.
4. Followers truncate conflicting suffixes and never commit entries the
   leader does not have.
5. A newly elected leader appends a no-op entry to commit entries from older
   terms.
6. `ReadBarrier` returns an error unless the node is leader and has heard from
   a majority within `ElectionTimeoutMin`; it then waits until `lastApplied`
   covers the commit index and the current leader's no-op. Otherwise the node
   steps down. This prevents stale reads from a freshly elected leader.
7. Apply is in index order and exactly once per index. Entries are 1-based;
   `Entries[0].Index == 1`.
8. `AppendEntries` with `PrevLogIndex=0` means "from the beginning" and always
   matches an empty prefix.

## In-memory test network (`internal/rafttest`)

```go
type Handler interface {
    HandleRequestVote(ctx context.Context, req *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error)
    HandleAppendEntries(ctx context.Context, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error)
}
type Network struct{ /* ... */ }
func NewNetwork() *Network
func (n *Network) Register(id string, h Handler)
func (n *Network) Transport(id string) raft.Transport
func (n *Network) Partition(id string, blocked ...string) // one-way block id -> blocked
func (n *Network) Heal(id string)
func (n *Network) Drop(from, to string, drop bool) // optional fine-grained control
```

Raft tests must not sleep for seconds: use election timeouts of 30–80 ms and
`require.Eventually`-style polling with a 2 s deadline. Always `-race`.

## KV state machine (`internal/kv`)

```go
func Open(dir string) (*Store, error) // creates dir, replays WAL
func (s *Store) Apply(entry model.Entry) error // put/delete/noop; fsyncs
func (s *Store) Get(key string) ([]byte, bool) // returns a copy
func (s *Store) Len() int
func (s *Store) Close() error
```

- Implements `raft.StateMachine`.
- `put` stores a copy of `Value`; `delete` is idempotent; `noop` is a no-op.
- WAL format: newline-delimited JSON records; `put` values are base64 by
  `encoding/json`. Every record is fsynced. On open, replay; compact when the
  WAL has more than 10k records by rewriting it from the current map.
- Safe for concurrent `Get` and `Apply`.

## Node (`internal/node`)

```go
type Config struct {
    ID          string   // defaults to RaftAddr
    Shard       string
    RaftAddr    string   // gRPC listen address for NodeService
    HTTPAddr    string
    Peers       []string // raft peer addresses
    DataDir     string
    CoordinatorAddr string // optional: periodic load report
    AuthUser    string
    AuthPass    string
    TLSCert     string
    TLSKey      string
    TLSCA       string   // if empty and TLS enabled, peer clients skip verify
    RateLimit   float64  // requests/sec, 0 disables
    RateBurst   int
    Logger      *slog.Logger
}

func New(cfg Config) (*Node, error)
func (n *Node) Start(ctx context.Context) error
func (n *Node) Stop(ctx context.Context) error // graceful: HTTP, gRPC, raft, kv
```

- `New` opens `raft.NewFileStore(filepath.Join(DataDir,"raft"))`, `kv.Open(filepath.Join(DataDir,"kv"))`,
  builds the gRPC peer transport and `raft.New`.
- gRPC `NodeService`:
  - `RequestVote`/`AppendEntries` call the Raft handlers;
  - `Put` proposes `CommandPut` and returns `{success, leader, error, index}`;
    not leader is a normal response (not a gRPC error);
  - `Get` calls `ReadBarrier`; if that fails returns `{leader, error}`; else
    reads the local KV and returns `{found, value}`;
  - `Delete` proposes `CommandDelete`;
  - `Status` returns the Raft status plus shard.
- HTTP:
  - `PUT /v1/kv/{key}` (body = raw value), `GET /v1/kv/{key}`,
    `DELETE /v1/kv/{key}`; JSON error body `{"error":"..."}`;
  - `GET /health` (always 200 when the process is up),
    `GET /v1/status`, `GET /metrics`; when `TLSCert` is set the HTTP server
    speaks HTTPS with the same certificate as gRPC;
  - Basic Auth on `/v1/kv/*` when `AuthUser != ""`; rate limit globally when
    `RateLimit > 0`; requests over the limit get 429;
  - non-leader KV requests get 503 with `X-Leader: <addr>`.
- Metrics (own `prometheus.Registry`, not global): `db_put_total`,
  `db_get_total`, `db_delete_total`, `db_errors_total`,
  `db_http_request_duration_seconds`, `db_raft_term`,
  `db_raft_commit_index`, `db_raft_last_applied`, `db_raft_is_leader`.
- Peer transport: lazy `grpc.NewClient`/`grpc.Dial` per peer, TLS credentials
  when `TLSCert` is set, close all on stop.
- Load report: if `CoordinatorAddr != ""`, every 5 s call
  `CoordinatorService.ReportLoad` with `Load` = operations/sec since the
  previous sample and `Ops` = total KV operations.

## Coordinator (`internal/coordinator`)

```go
type Config struct {
    HTTPAddr   string
    GRPCAddr   string
    Shards     map[string][]string // shard -> node raft addresses
    AuthUser   string
    AuthPass   string
    TLSCert    string
    TLSKey     string
    TLSCA      string
    RateLimit  float64
    RateBurst  int
    Logger     *slog.Logger
}

func New(cfg Config) (*Coordinator, error)
func (c *Coordinator) Start(ctx context.Context) error
func (c *Coordinator) Stop(ctx context.Context) error
```

- Shard selection: sorted shard names, `xxhash.Sum64String(key) % len(shards)`.
- Leader discovery: `Status` on each shard member; cache leader for 2 s; on a
  not-leader response or an RPC error, refresh and retry up to 3 times.
- HTTP: same KV/health/status surface as the node; `/v1/status` includes the
  shard map, cached leaders and reported loads.
- gRPC `CoordinatorService`: `Get`/`Put`/`Delete` proxy to the shard leader;
  `Status` returns coordinator status; `ReportLoad` stores a load sample.
- Auth/rate limit/TLS/metrics mirror the node. Metrics: `db_coordinator_requests_total{op,shard,result}`,
  `db_coordinator_request_duration_seconds`, `db_coordinator_load{node,shard}`.

## CLI (`cmd/db`)

```
db node -id <addr> -shard <name> -raft-addr :7001 -http-addr :8001 \
        -peers addr2,addr3 -data-dir ./data/node1 \
        [-coordinator addr] [-auth-user u -auth-pass p] \
        [-tls-cert f -tls-key f -tls-ca f] [-rate-limit 100 -rate-burst 200]

db coordinator -http-addr :8080 -grpc-addr :9090 \
        -shards "shard-1=node1:7001,node2:7001;shard-2=node4:7001,node5:7001" \
        [-auth-user u -auth-pass p] [-tls-cert f -tls-key f -tls-ca f] \
        [-rate-limit 1000 -rate-burst 2000]
```

- `-log-level debug|info|warn|error`, `-log-format text|json`.
- Graceful shutdown on SIGINT/SIGTERM; exit within 10 s.
- `-id` defaults to `-raft-addr`; peers are full addresses.

## Definition of done

- `go build ./...`, `go vet ./...`, `go test -race ./...` pass.
- 3-node Raft test: leader elected, value replicated to all, leader killed,
  new leader elected, committed value survives, restarted node catches up.
- 2 shards × 3 replicas: coordinator routes a key to the right shard; killing
  the leader does not lose a committed write and coordinator retries.
- `docker compose up -d` starts 6 nodes + coordinator; the smoke script in
  README writes and reads a key through the coordinator.
