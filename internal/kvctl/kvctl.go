// Package kvctl implements the kvctl command-line client for the v2 KV
// cluster. It is deliberately independent of gRPC types: Runner talks to a
// Backend interface, so command parsing, formatting and error behaviour can be
// tested without a cluster.
package kvctl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Version is printed by `kvctl version`.
const Version = "0.1.0"

// ErrNotFound is returned by Backend.Get when the key does not exist.
var ErrNotFound = errors.New("key not found")

// KV is one key/value pair returned by Scan.
type KV struct {
	Key   []byte
	Value []byte
}

// Op is one operation in an atomic transaction.
type Op struct {
	Delete bool
	Key    []byte
	Value  []byte
}

// RangeInfo describes one range in the meta range map. Start is inclusive and
// End is exclusive; nil boundaries mean the range is unbounded on that side.
//
// LeaderKnown, ReplicaCount, LiveVoters, Epoch and Stale are health fields.
// They are best-effort: LiveVoters is populated only when the backend can
// probe KV Status, and a leader is never synthesized when unknown.
type RangeInfo struct {
	ID       uint64
	Start    []byte
	End      []byte
	Replicas []string
	Leader   string
	Version  uint64

	LeaderKnown  bool
	ReplicaCount int
	LiveVoters   []string
	Epoch        uint64
	Stale        bool
}

// BackupOptions configures one logical backup.
type BackupOptions struct {
	OutDir     string
	ChunkKeys  int
	ChunkBytes int64
}

// BackupResult summarizes a completed logical backup.
type BackupResult struct {
	OutDir     string
	SnapshotTS uint64
	Keys       int64
	Bytes      int64
	Ranges     int
}

// Backend is the subset of the v2 client used by the CLI. The administrative
// methods only need a meta connection; data methods need KV nodes.
type Backend interface {
	Put(ctx context.Context, key, value []byte) error
	Get(ctx context.Context, key []byte) ([]byte, error)
	Delete(ctx context.Context, key []byte) error
	Scan(ctx context.Context, start, end []byte, limit int) ([]KV, error)
	Txn(ctx context.Context, ops []Op) error
	Ping(ctx context.Context) error
	Close() error

	ListRanges(ctx context.Context) ([]RangeInfo, error)
	SetRanges(ctx context.Context, ranges []RangeInfo) error
	SplitRange(ctx context.Context, rangeID uint64, splitKey []byte) (uint64, error)
	MergeRanges(ctx context.Context, leftID, rightID uint64) error
	MetaLeader(ctx context.Context) (string, uint64, error)
	Backup(ctx context.Context, opts BackupOptions) (BackupResult, error)
	Decommission(ctx context.Context, opts DecommissionOptions) (DecommissionResult, error)
}

// Config is the resolved global CLI configuration.
type Config struct {
	Meta    string
	Nodes   []string
	Timeout time.Duration
	Format  string // table, json, raw
	Hex     bool   // keys/values on input/output are hexadecimal
	Quiet   bool

	// TLS enables TLS/mTLS for meta and KV gRPC connections. An empty
	// TLSCA/cert/key/server-name leaves the connection plaintext (dev).
	TLSCA         string
	TLSCert       string
	TLSKey        string
	TLSServerName string

	// AdminToken is sent as a Bearer credential on meta admin RPCs
	// (SetRanges/SplitRange/MergeRanges) and on the KV admin RPCs driven by
	// decommission (TransferLeader/PromoteReplica/RemoveVoter/
	// UpdateRangeEpoch). It is required when the meta or KV server enforces
	// its Config.AdminToken; read-only commands (ranges, status) and data
	// commands never carry it. Empty means the server check is disabled
	// (development) or the command does not need it. The token is never
	// printed.
	AdminToken string
}

// Runner executes one kvctl invocation. Open creates a backend; tests replace
// it with an in-memory implementation.
type Runner struct {
	In   io.Reader
	Out  io.Writer
	Err  io.Writer
	Open func(context.Context, Config) (Backend, error)
}

// NewRunner returns a Runner wired to the real v2 client.
func NewRunner(in io.Reader, out, errOut io.Writer) *Runner {
	return &Runner{In: in, Out: out, Err: errOut, Open: openV2Backend}
}

const usageText = `kvctl — terminal client for the v2 distributed KV cluster

Usage:
  kvctl [global flags] <command> [args]

Commands:
  get <key>                 read one key
  put <key> <value|->       write one key; "-" reads stdin, "@file" reads a file
  delete <key>              delete one key (alias: del)
  scan [start] [end]        scan a range (flags: -start -end -prefix -limit)
  txn [-file ops.txt]       atomic transaction; stdin: put/delete lines
  shell                     interactive shell (data commands only)
  ping                      check meta/TSO connectivity
  ranges                    list meta ranges with leader and live health (meta only)
  init-ranges               bootstrap a fresh single-range meta with KV replicas
  status                    show meta leader, term and range counts (meta only)
  split <range-id> <key>    split a range at key (meta only)
  merge <left-id> <right-id>  merge two adjacent ranges (meta only)
  decommission -range <id> -target <addr>  remove one replica from a range safely
  backup -out DIR           logical backup (flags: -chunk-keys -chunk-bytes)
  version                   print version
  help                      this help

Global flags:
  -meta ADDR                meta/TSO gRPC address (env KVCTL_META; default 127.0.0.1:9090)
  -nodes A,B                KV node gRPC addresses (env KVCTL_NODES; required for data/decommission)
  -timeout D                per-command timeout (default 10s; backup 10m; decommission 2m)
  -format table|json|raw    output format (default table)
  -hex                      treat keys/values as hex on input and output
  -quiet                    suppress success messages
  -tls-ca FILE              PEM CA bundle for meta/KV server verification
  -tls-cert FILE            PEM client certificate for mTLS
  -tls-key FILE             PEM client private key (with -tls-cert)
  -tls-server-name NAME     expected server name for certificate verification
  -admin-token TOKEN        Bearer token for meta/KV admin RPCs (env KVCTL_ADMIN_TOKEN; never printed)

  ranges and status are read-only and never send the admin token. ranges also
  probes each replica's read-only KVService.Status (best-effort, bounded) to
  fill LEADER, LEADER_KNOWN, LIVE_VOTERS and STALE. Unreachable replicas leave
  those columns "-" and never fabricate a leader. init-ranges,
  split, merge and decommission are admin operations; when the meta or KV
  server was started with -admin-token, pass -admin-token or set
  KVCTL_ADMIN_TOKEN. decommission sends it on TransferLeader, PromoteReplica,
  RemoveVoter, UpdateRangeEpoch and SetRanges; Status stays read-only. The
  token is never printed.

  init-ranges is a one-time, fail-closed bootstrap for a FRESH meta whose
  single range has no replicas. It sets range 1's replicas to -replicas (or
  -nodes when -replicas is omitted). It refuses to touch a map with more than
  one range or with a different existing replica set, because SetRanges
  replaces the whole range map and can orphan split ranges and data. Running it
  twice with the same replica set is a no-op.

  ranges, status, init-ranges, split and merge only need -meta. get, put,
  delete, scan, txn, shell, ping and backup also require -nodes. Backup
  defaults to a 10m timeout; raise -timeout for very large data sets.

  decommission requires both -meta and -nodes: it reads the range map from
  meta, then drives Status/TransferLeader/PromoteReplica/RemoveVoter directly
  on the KV replicas and publishes the resulting voter set with SetRanges.
  -target is the replica address to remove; -replicas optionally overrides
  -nodes as the replica hint. It is idempotent: a target that is already absent
  is reported as already_removed and the range map is re-checked. If both meta
  and KV admin auth are enabled, the one -admin-token must be valid on both.
  Use -catch-up-timeout/-step-timeout only to tune the bounded waits. The
  command defaults to a 2m timeout.

Examples:
  kvctl -nodes 127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003 put user:1 alice
  kvctl -nodes ... get user:1
  kvctl -nodes ... scan -prefix user: -limit 100 -format json
  printf 'put a 1\nput b 2\ndelete a\n' | kvctl -nodes ... txn
  kvctl -nodes ... -hex put 6b 76   # key "k", value "v"
  kvctl -format json -meta 127.0.0.1:9090 ranges
  kvctl -meta 127.0.0.1:9090 init-ranges \
    -replicas 127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003
  kvctl -meta 127.0.0.1:9090 status
  kvctl -meta 127.0.0.1:9090 split 1 m
  kvctl -meta 127.0.0.1:9090 merge 2 3
  kvctl -meta 127.0.0.1:9090 -nodes 127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003 \
    -admin-token "$KVCTL_ADMIN_TOKEN" decommission -range 1 -target 127.0.0.1:7003
  kvctl -nodes ... backup -out ./backup
`

func (r *Runner) out() io.Writer {
	if r.Out == nil {
		return io.Discard
	}
	return r.Out
}

func (r *Runner) errOut() io.Writer {
	if r.Err == nil {
		return io.Discard
	}
	return r.Err
}

func (r *Runner) input() io.Reader {
	if r.In == nil {
		return strings.NewReader("")
	}
	return r.In
}

// Run parses args and executes one command. It returns a process exit code:
// 0 success, 1 operational error, 2 usage error.
func (r *Runner) Run(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("kvctl", flag.ContinueOnError)
	fs.SetOutput(r.errOut())
	meta := fs.String("meta", envOr("KVCTL_META", "127.0.0.1:9090"), "meta/TSO gRPC address")
	nodes := fs.String("nodes", envOr("KVCTL_NODES", ""), "comma-separated KV node addresses")
	timeout := fs.Duration("timeout", 10*time.Second, "per-command timeout")
	format := fs.String("format", envOr("KVCTL_FORMAT", "table"), "table, json or raw")
	hexMode := fs.Bool("hex", false, "hex keys/values")
	quiet := fs.Bool("quiet", false, "suppress success messages")
	tlsCA := fs.String("tls-ca", "", "PEM CA bundle for meta/KV servers")
	tlsCert := fs.String("tls-cert", "", "PEM client cert for mTLS")
	tlsKey := fs.String("tls-key", "", "PEM client key for mTLS")
	tlsServerName := fs.String("tls-server-name", "", "expected server name")
	adminToken := fs.String("admin-token", envOr("KVCTL_ADMIN_TOKEN", ""),
		"Bearer token for meta admin RPCs (SetRanges/SplitRange/MergeRanges) "+
			"and KV admin RPCs driven by decommission")
	fs.Usage = func() { fmt.Fprint(r.errOut(), usageText) }

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(r.out(), usageText)
			return 0
		}
		return 2
	}
	timeoutSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "timeout" {
			timeoutSet = true
		}
	})
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprint(r.errOut(), usageText)
		return 2
	}
	cmd := rest[0]
	if cmd == "help" || cmd == "version" {
		if cmd == "help" {
			fmt.Fprint(r.out(), usageText)
		} else {
			fmt.Fprintln(r.out(), Version)
		}
		return 0
	}
	cfg := Config{
		Meta:          *meta,
		Nodes:         splitList(*nodes),
		Timeout:       *timeout,
		Format:        strings.ToLower(*format),
		Hex:           *hexMode,
		Quiet:         *quiet,
		TLSCA:         *tlsCA,
		TLSCert:       *tlsCert,
		TLSKey:        *tlsKey,
		TLSServerName: *tlsServerName,
		AdminToken:    *adminToken,
	}
	if cfg.Timeout <= 0 {
		fmt.Fprintln(r.errOut(), "kvctl: -timeout must be > 0")
		return 2
	}
	if cmd == "backup" && !timeoutSet {
		// A backup of more than a handful of keys easily exceeds the
		// interactive default. Keep a bound, but make it generous.
		cfg.Timeout = 10 * time.Minute
	}
	if cmd == "decommission" && !timeoutSet {
		// Catch-up and leadership transfer are bounded inside the
		// orchestrator; the interactive 10s default is too short for them.
		cfg.Timeout = 2 * time.Minute
	}
	switch cfg.Format {
	case "table", "json", "raw":
	default:
		fmt.Fprintf(r.errOut(), "kvctl: unknown -format %q (want table, json or raw)\n", cfg.Format)
		return 2
	}
	if cfg.Meta == "" {
		fmt.Fprintln(r.errOut(), "kvctl: -meta or KVCTL_META is required")
		return 2
	}
	// Administrative range commands talk only to the meta service. Everything
	// else needs at least one KV node.
	if !metaOnlyCommand(cmd) && len(cfg.Nodes) == 0 {
		fmt.Fprintln(r.errOut(), "kvctl: -nodes or KVCTL_NODES is required (comma-separated KV addresses)")
		return 2
	}

	open := r.Open
	if open == nil {
		open = openV2Backend
	}
	runCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	b, err := open(runCtx, cfg)
	if err != nil {
		fmt.Fprintf(r.errOut(), "kvctl: connect: %v\n", err)
		return 1
	}
	defer b.Close()

	if cmd == "shell" {
		return r.runShell(runCtx, b, cfg)
	}
	return r.execute(runCtx, b, cfg, cmd, rest[1:])
}

func (r *Runner) execute(ctx context.Context, b Backend, cfg Config, cmd string, args []string) int {
	switch cmd {
	case "get":
		if len(args) != 1 {
			return r.usage("get requires exactly one key")
		}
		key, err := decodeArg(args[0], cfg.Hex)
		if err != nil {
			return r.fail("invalid key", err)
		}
		value, err := b.Get(ctx, key)
		if errors.Is(err, ErrNotFound) {
			fmt.Fprintf(r.errOut(), "kvctl: key %s not found\n", display(key, cfg.Hex))
			return 1
		}
		if err != nil {
			return r.fail("get", err)
		}
		return r.printValue(key, value, cfg)

	case "put":
		if len(args) < 2 {
			return r.usage("put requires <key> <value|-> (use @file for a file)")
		}
		if len(args) > 2 {
			return r.usage("put accepts exactly two arguments; quote values with spaces")
		}
		key, err := decodeArg(args[0], cfg.Hex)
		if err != nil {
			return r.fail("invalid key", err)
		}
		value, err := r.readValue(args[1], cfg.Hex)
		if err != nil {
			return r.fail("read value", err)
		}
		if err := b.Put(ctx, key, value); err != nil {
			return r.fail("put", err)
		}
		return r.printOK("put", key, cfg)

	case "delete", "del":
		if len(args) != 1 {
			return r.usage("delete requires exactly one key")
		}
		key, err := decodeArg(args[0], cfg.Hex)
		if err != nil {
			return r.fail("invalid key", err)
		}
		if err := b.Delete(ctx, key); err != nil {
			return r.fail("delete", err)
		}
		return r.printOK("delete", key, cfg)

	case "scan":
		return r.executeScan(ctx, b, cfg, args)

	case "txn":
		return r.executeTxn(ctx, b, cfg, args)

	case "ping":
		if len(args) != 0 {
			return r.usage("ping takes no arguments")
		}
		if err := b.Ping(ctx); err != nil {
			return r.fail("ping", err)
		}
		if cfg.Format == "json" {
			fmt.Fprintln(r.out(), `{"status":"ok"}`)
		} else {
			fmt.Fprintln(r.out(), "pong")
		}
		return 0

	case "ranges":
		return r.executeRanges(ctx, b, cfg, args)

	case "init-ranges":
		return r.executeInitRanges(ctx, b, cfg, args)

	case "status":
		return r.executeStatus(ctx, b, cfg, args)

	case "split":
		return r.executeSplit(ctx, b, cfg, args)

	case "merge":
		return r.executeMerge(ctx, b, cfg, args)

	case "decommission":
		return r.executeDecommission(ctx, b, cfg, args)

	case "backup":
		return r.executeBackup(ctx, b, cfg, args)

	default:
		return r.usage(fmt.Sprintf("unknown command %q", cmd))
	}
}

func (r *Runner) executeScan(ctx context.Context, b Backend, cfg Config, args []string) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(r.errOut())
	startFlag := fs.String("start", "", "start key (inclusive)")
	endFlag := fs.String("end", "", "end key (exclusive)")
	prefixFlag := fs.String("prefix", "", "prefix shortcut")
	limit := fs.Int("limit", 100, "maximum rows (1..10000)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 2 {
		return r.usage("scan accepts at most [start] [end]")
	}
	if *prefixFlag != "" && (*startFlag != "" || *endFlag != "") {
		return r.usage("scan: -prefix cannot be combined with -start/-end")
	}
	if *prefixFlag != "" && fs.NArg() > 0 {
		return r.usage("scan: -prefix cannot be combined with positional keys")
	}
	var start, end []byte
	var err error
	if *prefixFlag != "" {
		start, err = decodeArg(*prefixFlag, cfg.Hex)
		if err != nil {
			return r.fail("invalid prefix", err)
		}
		end = prefixEnd(start)
	} else {
		if fs.NArg() > 0 && *startFlag == "" {
			*startFlag = fs.Arg(0)
		}
		if fs.NArg() > 1 && *endFlag == "" {
			*endFlag = fs.Arg(1)
		}
		if *startFlag != "" {
			start, err = decodeArg(*startFlag, cfg.Hex)
			if err != nil {
				return r.fail("invalid start key", err)
			}
		}
		if *endFlag != "" {
			end, err = decodeArg(*endFlag, cfg.Hex)
			if err != nil {
				return r.fail("invalid end key", err)
			}
		}
	}
	if *limit < 1 || *limit > 10000 {
		return r.usage("scan: -limit must be 1..10000")
	}
	items, err := b.Scan(ctx, start, end, *limit)
	if err != nil {
		return r.fail("scan", err)
	}
	return r.printScan(items, cfg)
}

func (r *Runner) executeTxn(ctx context.Context, b Backend, cfg Config, args []string) int {
	fs := flag.NewFlagSet("txn", flag.ContinueOnError)
	fs.SetOutput(r.errOut())
	file := fs.String("file", "", "operations file (default stdin)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return r.usage("txn takes no positional arguments")
	}
	in := r.input()
	var closeFile func() error
	if *file != "" {
		f, err := os.Open(*file)
		if err != nil {
			return r.fail("open operations file", err)
		}
		in = f
		closeFile = f.Close
	}
	if closeFile != nil {
		defer closeFile()
	}
	ops, err := parseOps(in, cfg.Hex)
	if err != nil {
		return r.fail("parse transaction", err)
	}
	if len(ops) == 0 {
		fmt.Fprintln(r.errOut(), "kvctl: empty transaction")
		return 1
	}
	if err := b.Txn(ctx, ops); err != nil {
		return r.fail("commit transaction", err)
	}
	if cfg.Format == "json" {
		fmt.Fprintf(r.out(), "{\"status\":\"ok\",\"operations\":%d}\n", len(ops))
	} else if !cfg.Quiet {
		fmt.Fprintf(r.out(), "committed %d operations\n", len(ops))
	}
	return 0
}

// rangeJSON keeps the original id/start/end/leader/replicas/version fields
// unchanged for backward compatibility and adds health fields. leader is still
// emitted even when unknown (empty), while leader_known distinguishes an
// unknown leader from a genuinely empty one.
type rangeJSON struct {
	ID           uint64   `json:"id"`
	Start        string   `json:"start"`
	End          string   `json:"end"`
	Leader       string   `json:"leader"`
	LeaderKnown  bool     `json:"leader_known"`
	Replicas     []string `json:"replicas"`
	ReplicaCount int      `json:"replica_count"`
	LiveVoters   []string `json:"live_voters"`
	Version      uint64   `json:"version"`
	Epoch        uint64   `json:"epoch"`
	Stale        bool     `json:"stale"`
}

func (r *Runner) executeRanges(ctx context.Context, b Backend, cfg Config, args []string) int {
	if len(args) != 0 {
		return r.usage("ranges takes no arguments")
	}
	if cfg.Format == "raw" {
		return r.usage("ranges: -format raw is not supported; use table or json")
	}
	infos, err := b.ListRanges(ctx)
	if err != nil {
		return r.fail("ranges", err)
	}
	sort.SliceStable(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	if cfg.Format == "json" {
		out := make([]rangeJSON, 0, len(infos))
		for _, info := range infos {
			out = append(out, rangeJSONFromInfo(info, cfg))
		}
		return r.writeJSON(out)
	}
	fmt.Fprintln(r.out(), "ID\tSTART\tEND\tLEADER\tLEADER_KNOWN\tREPLICAS\tLIVE_VOTERS\tVERSION\tSTALE")
	for _, info := range infos {
		v := rangeHealth(info)
		leader := info.Leader
		if leader == "" {
			leader = "-"
		}
		live := "-"
		if v.liveVoters != nil {
			live = strings.Join(v.liveVoters, ",")
			if live == "" {
				live = "-"
			}
		}
		fmt.Fprintf(r.out(), "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			info.ID, display(info.Start, cfg.Hex), display(info.End, cfg.Hex),
			leader, yesNo(v.leaderKnown), strings.Join(info.Replicas, ","),
			live, v.epoch, yesNo(v.stale))
	}
	return 0
}

// rangeHealthValues is the normalized health view used by both output formats.
// Missing backend health fields fall back to values derivable from the
// descriptor, so a backend that cannot probe still reports replica count,
// leader-known and epoch truthfully.
type rangeHealthValues struct {
	leaderKnown  bool
	replicaCount int
	liveVoters   []string
	epoch        uint64
	stale        bool
}

func rangeHealth(info RangeInfo) rangeHealthValues {
	leaderKnown := info.LeaderKnown || info.Leader != ""
	replicaCount := info.ReplicaCount
	if replicaCount == 0 {
		replicaCount = len(info.Replicas)
	}
	epoch := info.Epoch
	if epoch == 0 {
		epoch = info.Version
	}
	return rangeHealthValues{
		leaderKnown:  leaderKnown,
		replicaCount: replicaCount,
		liveVoters:   info.LiveVoters,
		epoch:        epoch,
		stale:        info.Stale,
	}
}

func rangeJSONFromInfo(info RangeInfo, cfg Config) rangeJSON {
	v := rangeHealth(info)
	replicas := info.Replicas
	if replicas == nil {
		replicas = []string{}
	}
	live := v.liveVoters
	if live == nil {
		live = []string{}
	}
	return rangeJSON{
		ID:           info.ID,
		Start:        display(info.Start, cfg.Hex),
		End:          display(info.End, cfg.Hex),
		Leader:       info.Leader,
		LeaderKnown:  v.leaderKnown,
		Replicas:     replicas,
		ReplicaCount: v.replicaCount,
		LiveVoters:   live,
		Version:      info.Version,
		Epoch:        v.epoch,
		Stale:        v.stale,
	}
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

type initRangesJSON struct {
	Status             string   `json:"status"`
	RangeID            uint64   `json:"range_id"`
	Replicas           []string `json:"replicas"`
	AlreadyInitialized bool     `json:"already_initialized"`
}

// executeInitRanges bootstraps a fresh meta range map. It is deliberately
// narrow: only a single existing range with no replicas may be changed, and a
// map that is already initialized is a no-op only when the replica set matches
// exactly. SetRanges replaces the whole map, so rewriting an unknown or split
// layout could orphan ranges and data.
func (r *Runner) executeInitRanges(ctx context.Context, b Backend, cfg Config, args []string) int {
	fs := flag.NewFlagSet("init-ranges", flag.ContinueOnError)
	fs.SetOutput(r.errOut())
	replicasFlag := fs.String("replicas", "",
		"comma-separated KV replica addresses (default: -nodes)")
	versionFlag := fs.Uint64("version", 0,
		"range descriptor version to publish (0 keeps the existing version)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return r.usage("init-ranges takes no positional arguments")
	}
	if cfg.Format == "raw" {
		return r.usage("init-ranges: -format raw is not supported; use table or json")
	}

	replicas := splitList(*replicasFlag)
	if len(replicas) == 0 {
		replicas = append([]string(nil), cfg.Nodes...)
	}
	if len(replicas) == 0 {
		return r.usage("init-ranges requires -replicas or -nodes (comma-separated KV addresses)")
	}
	if len(replicas) > 5 {
		return r.usage(fmt.Sprintf("init-ranges: at most 5 replicas are supported (got %d)", len(replicas)))
	}
	seen := make(map[string]struct{}, len(replicas))
	clean := make([]string, 0, len(replicas))
	for _, addr := range replicas {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			return r.usage("init-ranges: replica addresses must not be empty")
		}
		if _, ok := seen[addr]; ok {
			return r.usage(fmt.Sprintf("init-ranges: duplicate replica address %q", addr))
		}
		seen[addr] = struct{}{}
		clean = append(clean, addr)
	}
	replicas = clean

	infos, err := b.ListRanges(ctx)
	if err != nil {
		return r.fail("init-ranges: list ranges", err)
	}
	switch len(infos) {
	case 0:
		return r.fail("init-ranges", errors.New("meta returned no ranges"))
	case 1:
		// The expected fresh state.
	default:
		return r.fail("init-ranges", fmt.Errorf(
			"refusing to replace a map with %d ranges: init-ranges only bootstraps a fresh cluster", len(infos)))
	}
	current := infos[0]
	if current.ID == 0 {
		return r.fail("init-ranges", errors.New("meta returned a range with id 0"))
	}
	if len(current.Replicas) > 0 {
		if !sameReplicaSet(current.Replicas, replicas) {
			return r.fail("init-ranges", fmt.Errorf(
				"range %d already has replicas %s; refusing to replace it with %s (SetRanges replaces the whole map)",
				current.ID, strings.Join(current.Replicas, ","), strings.Join(replicas, ",")))
		}
		return r.printInitRanges(cfg, current.ID, replicas, true)
	}

	version := current.Version
	if *versionFlag != 0 {
		version = *versionFlag
	}
	desired := RangeInfo{
		ID:       current.ID,
		Start:    current.Start,
		End:      current.End,
		Replicas: replicas,
		Version:  version,
	}
	if err := b.SetRanges(ctx, []RangeInfo{desired}); err != nil {
		return r.fail("init-ranges: set ranges", err)
	}

	// Verify rather than trusting a successful RPC: this is the one command
	// that rewrites the whole range map, and a split/merge tool must never see
	// an unverified descriptor.
	after, err := b.ListRanges(ctx)
	if err != nil {
		return r.fail("init-ranges: verify ranges", err)
	}
	if len(after) != 1 || after[0].ID != desired.ID || !sameReplicaSet(after[0].Replicas, replicas) {
		return r.fail("init-ranges: verify ranges", fmt.Errorf(
			"range map did not converge to the requested single range %d with replicas %s",
			desired.ID, strings.Join(replicas, ",")))
	}
	return r.printInitRanges(cfg, desired.ID, replicas, false)
}

func (r *Runner) printInitRanges(cfg Config, rangeID uint64, replicas []string, already bool) int {
	if cfg.Format == "json" {
		return r.writeJSON(initRangesJSON{
			Status:             "ok",
			RangeID:            rangeID,
			Replicas:           replicas,
			AlreadyInitialized: already,
		})
	}
	if cfg.Quiet {
		return 0
	}
	if already {
		fmt.Fprintf(r.out(), "range %d already initialized with %d replicas: %s\n",
			rangeID, len(replicas), strings.Join(replicas, ","))
		return 0
	}
	fmt.Fprintf(r.out(), "initialized range %d with %d replicas: %s\n",
		rangeID, len(replicas), strings.Join(replicas, ","))
	return 0
}

type statusJSON struct {
	MetaLeader string         `json:"meta_leader"`
	MetaTerm   uint64         `json:"meta_term"`
	Ranges     int            `json:"ranges"`
	Leaders    map[string]int `json:"leaders"`
	Meta       string         `json:"meta"`
	Nodes      []string       `json:"nodes"`
}

func (r *Runner) executeStatus(ctx context.Context, b Backend, cfg Config, args []string) int {
	if len(args) != 0 {
		return r.usage("status takes no arguments")
	}
	if cfg.Format == "raw" {
		return r.usage("status: -format raw is not supported; use table or json")
	}
	leader, term, err := b.MetaLeader(ctx)
	if err != nil {
		return r.fail("status", err)
	}
	infos, err := b.ListRanges(ctx)
	if err != nil {
		return r.fail("status", err)
	}
	counts := make(map[string]int)
	for _, info := range infos {
		if info.Leader != "" {
			counts[info.Leader]++
		}
	}
	if cfg.Format == "json" {
		nodes := append([]string{}, cfg.Nodes...)
		return r.writeJSON(statusJSON{
			MetaLeader: leader,
			MetaTerm:   term,
			Ranges:     len(infos),
			Leaders:    counts,
			Meta:       cfg.Meta,
			Nodes:      nodes,
		})
	}
	nodes := append([]string{}, cfg.Nodes...)
	fmt.Fprintln(r.out(), "META_LEADER\tTERM\tRANGES\tMETA\tNODES")
	fmt.Fprintf(r.out(), "%s\t%d\t%d\t%s\t%s\n",
		leader, term, len(infos), cfg.Meta, strings.Join(nodes, ","))
	fmt.Fprintln(r.out(), "LEADER\tRANGES")
	for _, l := range sortedKeys(counts) {
		fmt.Fprintf(r.out(), "%s\t%d\n", l, counts[l])
	}
	return 0
}

type splitJSON struct {
	Status     string `json:"status"`
	RangeID    uint64 `json:"range_id"`
	SplitKey   string `json:"split_key"`
	NewRangeID uint64 `json:"new_range_id"`
}

func (r *Runner) executeSplit(ctx context.Context, b Backend, cfg Config, args []string) int {
	if len(args) != 2 {
		return r.usage("split requires <range-id> <split-key>")
	}
	if cfg.Format == "raw" {
		return r.usage("split: -format raw is not supported; use table or json")
	}
	rangeID, err := parseRangeID(args[0])
	if err != nil {
		return r.usage(err.Error())
	}
	splitKey, err := decodeArg(args[1], cfg.Hex)
	if err != nil {
		return r.fail("invalid split key", err)
	}
	if len(splitKey) == 0 {
		return r.usage("split key must not be empty")
	}
	newRangeID, err := b.SplitRange(ctx, rangeID, splitKey)
	if err != nil {
		return r.fail("split", err)
	}
	if cfg.Format == "json" {
		return r.writeJSON(splitJSON{
			Status:     "ok",
			RangeID:    rangeID,
			SplitKey:   display(splitKey, cfg.Hex),
			NewRangeID: newRangeID,
		})
	}
	if !cfg.Quiet {
		fmt.Fprintf(r.out(), "new range id: %d\n", newRangeID)
	}
	return 0
}

type mergeJSON struct {
	Status       string `json:"status"`
	LeftRangeID  uint64 `json:"left_range_id"`
	RightRangeID uint64 `json:"right_range_id"`
}

func (r *Runner) executeMerge(ctx context.Context, b Backend, cfg Config, args []string) int {
	if len(args) != 2 {
		return r.usage("merge requires <left-range-id> <right-range-id>")
	}
	if cfg.Format == "raw" {
		return r.usage("merge: -format raw is not supported; use table or json")
	}
	leftID, err := parseRangeID(args[0])
	if err != nil {
		return r.usage(err.Error())
	}
	rightID, err := parseRangeID(args[1])
	if err != nil {
		return r.usage(err.Error())
	}
	if err := b.MergeRanges(ctx, leftID, rightID); err != nil {
		return r.fail("merge", err)
	}
	if cfg.Format == "json" {
		return r.writeJSON(mergeJSON{
			Status:       "ok",
			LeftRangeID:  leftID,
			RightRangeID: rightID,
		})
	}
	if !cfg.Quiet {
		fmt.Fprintln(r.out(), "OK")
	}
	return 0
}

type decommissionJSON struct {
	Status         string   `json:"status"`
	RangeID        uint64   `json:"range_id"`
	Target         string   `json:"target"`
	Leader         string   `json:"leader"`
	Voters         []string `json:"voters"`
	Learners       []string `json:"learners"`
	Transferred    bool     `json:"transferred"`
	Promoted       bool     `json:"promoted"`
	AlreadyRemoved bool     `json:"already_removed"`
	LagEntries     uint64   `json:"lag_entries"`
}

// executeDecommission removes one replica from a range. Parsing and output live
// here; the RPC adapter and meta range-map publication live in decommission.go.
func (r *Runner) executeDecommission(ctx context.Context, b Backend, cfg Config, args []string) int {
	fs := flag.NewFlagSet("decommission", flag.ContinueOnError)
	fs.SetOutput(r.errOut())
	rangeID := fs.Uint64("range", 0, "range ID to change")
	target := fs.String("target", "", "replica address to remove")
	replicasFlag := fs.String("replicas", "",
		"comma-separated replica addresses (default: -nodes)")
	catchUp := fs.Duration("catch-up-timeout", 0,
		"learner catch-up bound (0 uses the library default)")
	step := fs.Duration("step-timeout", 0,
		"per-step bound (0 uses the library default)")
	poll := fs.Duration("poll-interval", 0,
		"status poll interval (0 uses the library default)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return r.usage("decommission takes no positional arguments")
	}
	if cfg.Format == "raw" {
		return r.usage("decommission: -format raw is not supported; use table or json")
	}
	if *rangeID == 0 {
		return r.usage("decommission requires -range <id> (greater than zero)")
	}
	targetAddr := strings.TrimSpace(*target)
	if targetAddr == "" {
		return r.usage("decommission requires -target <replica-address>")
	}
	if *catchUp < 0 || *step < 0 || *poll < 0 {
		return r.usage("decommission: timeouts and poll interval must be >= 0")
	}
	replicas := splitList(*replicasFlag)
	if len(replicas) == 0 {
		replicas = append([]string(nil), cfg.Nodes...)
	}
	if len(replicas) == 0 {
		return r.usage("decommission requires -replicas or -nodes (comma-separated KV addresses)")
	}
	// The target must be reachable even if the caller forgot to list it.
	if !containsString(replicas, targetAddr) {
		replicas = append(replicas, targetAddr)
	}

	result, err := b.Decommission(ctx, DecommissionOptions{
		RangeID:        *rangeID,
		Target:         targetAddr,
		Replicas:       replicas,
		CatchUpTimeout: *catchUp,
		StepTimeout:    *step,
		PollInterval:   *poll,
	})
	if err != nil {
		return r.fail("decommission", err)
	}
	return r.printDecommission(cfg, result)
}

func (r *Runner) printDecommission(cfg Config, result DecommissionResult) int {
	voters := result.Voters
	if voters == nil {
		voters = []string{}
	}
	learners := result.Learners
	if learners == nil {
		learners = []string{}
	}
	if cfg.Format == "json" {
		return r.writeJSON(decommissionJSON{
			Status:         "ok",
			RangeID:        result.RangeID,
			Target:         result.Target,
			Leader:         result.Leader,
			Voters:         voters,
			Learners:       learners,
			Transferred:    result.Transferred,
			Promoted:       result.Promoted,
			AlreadyRemoved: result.AlreadyRemoved,
			LagEntries:     result.LagEntries,
		})
	}
	if cfg.Quiet {
		return 0
	}
	if result.AlreadyRemoved {
		fmt.Fprintf(r.out(), "range %d: %s already decommissioned; leader=%s voters=%s\n",
			result.RangeID, result.Target, result.Leader, strings.Join(voters, ","))
		return 0
	}
	fmt.Fprintf(r.out(), "range %d: decommissioned %s; leader=%s voters=%s",
		result.RangeID, result.Target, result.Leader, strings.Join(voters, ","))
	if len(learners) > 0 {
		fmt.Fprintf(r.out(), " learners=%s", strings.Join(learners, ","))
	}
	if result.Transferred {
		fmt.Fprint(r.out(), " leadership_transferred=true")
	}
	if result.Promoted {
		fmt.Fprint(r.out(), " learner_promoted=true")
	}
	fmt.Fprintln(r.out())
	return 0
}

type backupJSON struct {
	Dir        string `json:"dir"`
	SnapshotTS uint64 `json:"snapshot_ts"`
	Keys       int64  `json:"keys"`
	Bytes      int64  `json:"bytes"`
	Ranges     int    `json:"ranges"`
}

func (r *Runner) executeBackup(ctx context.Context, b Backend, cfg Config, args []string) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(r.errOut())
	outDir := fs.String("out", "", "backup directory to create")
	chunkKeys := fs.Int("chunk-keys", 0, "maximum keys per chunk (0 uses the backend default)")
	chunkBytes := fs.Int64("chunk-bytes", 0, "maximum key+value bytes per chunk (0 uses the backend default)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return r.usage("backup takes no positional arguments")
	}
	if cfg.Format == "raw" {
		return r.usage("backup: -format raw is not supported; use table or json")
	}
	if *outDir == "" {
		return r.usage("backup requires -out DIR")
	}
	if *chunkKeys < 0 {
		return r.usage("backup: -chunk-keys must be >= 0")
	}
	if *chunkBytes < 0 {
		return r.usage("backup: -chunk-bytes must be >= 0")
	}
	result, err := b.Backup(ctx, BackupOptions{
		OutDir:     *outDir,
		ChunkKeys:  *chunkKeys,
		ChunkBytes: *chunkBytes,
	})
	if err != nil {
		return r.fail("backup", err)
	}
	if cfg.Format == "json" {
		return r.writeJSON(backupJSON{
			Dir:        result.OutDir,
			SnapshotTS: result.SnapshotTS,
			Keys:       result.Keys,
			Bytes:      result.Bytes,
			Ranges:     result.Ranges,
		})
	}
	if !cfg.Quiet {
		fmt.Fprintf(r.out(), "backup complete: %s\n", result.OutDir)
		fmt.Fprintf(r.out(), "snapshot_ts=%d keys=%d bytes=%d ranges=%d\n",
			result.SnapshotTS, result.Keys, result.Bytes, result.Ranges)
	}
	return 0
}

func (r *Runner) runShell(ctx context.Context, b Backend, cfg Config) int {
	if cfg.Format == "json" {
		fmt.Fprintln(r.errOut(), "kvctl: shell is not available with -format json")
		return 2
	}
	fmt.Fprintln(r.out(), "kvctl shell; type help or quit")
	sc := bufio.NewScanner(r.input())
	for {
		fmt.Fprint(r.out(), "kvctl> ")
		if !sc.Scan() {
			break
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		cmd, rest, _ := strings.Cut(line, " ")
		rest = strings.TrimSpace(rest)
		switch cmd {
		case "quit", "exit", "\\q":
			return 0
		case "help", "?":
			fmt.Fprintln(r.out(), "get <key> | put <key> <value> | delete <key> | scan [-prefix P] [-limit N] | quit")
			continue
		}
		if adminCommand(cmd) {
			fmt.Fprintf(r.errOut(), "kvctl shell: %s is not available in the shell; run it directly\n", cmd)
			continue
		}
		var args []string
		if rest != "" {
			if cmd == "put" {
				key, value, ok := strings.Cut(rest, " ")
				if !ok {
					fmt.Fprintln(r.errOut(), "put requires key and value")
					continue
				}
				args = []string{key, strings.TrimSpace(value)}
			} else {
				args = strings.Fields(rest)
			}
		}
		cmdCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		code := r.execute(cmdCtx, b, cfg, cmd, args)
		cancel()
		// Interactive shell: a bad command must not close the session. The
		// command already printed its error; keep the prompt alive.
		if code != 0 {
			continue
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(r.errOut(), "kvctl shell: %v\n", err)
		return 1
	}
	return 0
}

func (r *Runner) printValue(key, value []byte, cfg Config) int {
	switch cfg.Format {
	case "raw":
		if cfg.Hex {
			_, _ = r.out().Write([]byte(hex.EncodeToString(value)))
		} else {
			_, _ = r.out().Write(value)
		}
		return 0
	case "json":
		return r.writeJSON(map[string]any{
			"key":   display(key, cfg.Hex),
			"value": display(value, cfg.Hex),
		})
	default:
		fmt.Fprintf(r.out(), "%s\t%s\n", display(key, cfg.Hex), display(value, cfg.Hex))
		return 0
	}
}

func (r *Runner) printScan(items []KV, cfg Config) int {
	switch cfg.Format {
	case "json":
		out := make([]map[string]string, 0, len(items))
		for _, kv := range items {
			out = append(out, map[string]string{
				"key":   display(kv.Key, cfg.Hex),
				"value": display(kv.Value, cfg.Hex),
			})
		}
		return r.writeJSON(out)
	case "raw":
		for _, kv := range items {
			fmt.Fprintf(r.out(), "%s\t%s\n", display(kv.Key, cfg.Hex), display(kv.Value, cfg.Hex))
		}
		return 0
	default:
		fmt.Fprintln(r.out(), "KEY\tVALUE")
		for _, kv := range items {
			fmt.Fprintf(r.out(), "%s\t%s\n", display(kv.Key, cfg.Hex), display(kv.Value, cfg.Hex))
		}
		return 0
	}
}

func (r *Runner) printOK(action string, key []byte, cfg Config) int {
	if cfg.Quiet {
		return 0
	}
	if cfg.Format == "json" {
		return r.writeJSON(map[string]string{"status": "ok", "action": action, "key": display(key, cfg.Hex)})
	}
	fmt.Fprintln(r.out(), "OK")
	return 0
}

func (r *Runner) writeJSON(v any) int {
	enc := json.NewEncoder(r.out())
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(r.errOut(), "kvctl: write output: %v\n", err)
		return 1
	}
	return 0
}

func (r *Runner) usage(msg string) int {
	fmt.Fprintf(r.errOut(), "kvctl: %s\n\n%s", msg, usageText)
	return 2
}

func (r *Runner) fail(what string, err error) int {
	fmt.Fprintf(r.errOut(), "kvctl: %s: %v\n", what, err)
	return 1
}

func (r *Runner) readValue(arg string, hexMode bool) ([]byte, error) {
	var data []byte
	var err error
	switch {
	case arg == "-":
		data, err = io.ReadAll(r.input())
	case strings.HasPrefix(arg, "@"):
		data, err = os.ReadFile(strings.TrimPrefix(arg, "@"))
	default:
		data = []byte(arg)
	}
	if err != nil {
		return nil, err
	}
	if hexMode {
		return decodeHex(string(data))
	}
	return data, nil
}

func parseOps(r io.Reader, hexMode bool) ([]Op, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var ops []Op
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cmd, rest, ok := strings.Cut(line, " ")
		if !ok {
			rest = ""
		}
		rest = strings.TrimSpace(rest)
		switch cmd {
		case "put", "set":
			keyStr, valueStr, ok := strings.Cut(rest, " ")
			if !ok || keyStr == "" {
				return nil, fmt.Errorf("line %d: want put <key> <value>", lineNo)
			}
			key, err := decodeArg(keyStr, hexMode)
			if err != nil {
				return nil, fmt.Errorf("line %d: key: %w", lineNo, err)
			}
			value, err := decodeArg(strings.TrimSpace(valueStr), hexMode)
			if err != nil {
				return nil, fmt.Errorf("line %d: value: %w", lineNo, err)
			}
			ops = append(ops, Op{Key: key, Value: value})
		case "delete", "del":
			if rest == "" || strings.Contains(rest, " ") {
				return nil, fmt.Errorf("line %d: want delete <key>", lineNo)
			}
			key, err := decodeArg(rest, hexMode)
			if err != nil {
				return nil, fmt.Errorf("line %d: key: %w", lineNo, err)
			}
			ops = append(ops, Op{Delete: true, Key: key})
		default:
			return nil, fmt.Errorf("line %d: unknown operation %q", lineNo, cmd)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return ops, nil
}

func decodeArg(s string, hexMode bool) ([]byte, error) {
	if !hexMode {
		return []byte(s), nil
	}
	return decodeHex(s)
}

func decodeHex(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		default:
			return r
		}
	}, s)
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("odd-length hex")
	}
	out, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("invalid hex: %w", err)
	}
	return out, nil
}

func display(b []byte, hexMode bool) string {
	if hexMode {
		return hex.EncodeToString(b)
	}
	return printable(b)
}

// printable keeps ordinary text readable and escapes control bytes so table
// output cannot corrupt a terminal.
func printable(b []byte) string {
	if bytes.IndexFunc(b, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0 {
		return string(b)
	}
	var sb strings.Builder
	for _, c := range b {
		if c < 0x20 || c == 0x7f {
			fmt.Fprintf(&sb, "\\x%02x", c)
		} else {
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

func prefixEnd(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] != 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}

// metaOnlyCommand reports whether cmd talks only to the meta service and can
// therefore run without -nodes.
func metaOnlyCommand(cmd string) bool {
	switch cmd {
	case "ranges", "status", "init-ranges", "split", "merge":
		return true
	default:
		return false
	}
}

// sameReplicaSet compares replica addresses as a set. The order presented by
// meta is not contractual, so idempotence must not depend on it.
func sameReplicaSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// adminCommand reports whether cmd is an administrative command that is not
// available in the interactive shell.
func adminCommand(cmd string) bool {
	return metaOnlyCommand(cmd) || cmd == "backup" || cmd == "decommission"
}

func parseRangeID(s string) (uint64, error) {
	id, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid range id %q", s)
	}
	if id == 0 {
		return 0, errors.New("range id must be greater than zero")
	}
	return id, nil
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
