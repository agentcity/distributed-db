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
type RangeInfo struct {
	ID       uint64
	Start    []byte
	End      []byte
	Replicas []string
	Leader   string
	Version  uint64
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
	SplitRange(ctx context.Context, rangeID uint64, splitKey []byte) (uint64, error)
	MergeRanges(ctx context.Context, leftID, rightID uint64) error
	MetaLeader(ctx context.Context) (string, uint64, error)
	Backup(ctx context.Context, opts BackupOptions) (BackupResult, error)
}

// Config is the resolved global CLI configuration.
type Config struct {
	Meta    string
	Nodes   []string
	Timeout time.Duration
	Format  string // table, json, raw
	Hex     bool   // keys/values on input/output are hexadecimal
	Quiet   bool
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
  ranges                    list meta ranges (meta only)
  status                    show meta leader, term and range counts (meta only)
  split <range-id> <key>    split a range at key (meta only)
  merge <left-id> <right-id>  merge two adjacent ranges (meta only)
  backup -out DIR           logical backup (flags: -chunk-keys -chunk-bytes)
  version                   print version
  help                      this help

Global flags:
  -meta ADDR                meta/TSO gRPC address (env KVCTL_META; default 127.0.0.1:9090)
  -nodes A,B                KV node gRPC addresses (env KVCTL_NODES; required for data)
  -timeout D                per-command timeout (default 10s; backup 10m)
  -format table|json|raw    output format (default table)
  -hex                      treat keys/values as hex on input and output
  -quiet                    suppress success messages

  ranges, status, split and merge only need -meta. get, put, delete, scan,
  txn, shell, ping and backup also require -nodes. Backup defaults to a 10m
  timeout; raise -timeout for very large data sets.

Examples:
  kvctl -nodes 127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003 put user:1 alice
  kvctl -nodes ... get user:1
  kvctl -nodes ... scan -prefix user: -limit 100 -format json
  printf 'put a 1\nput b 2\ndelete a\n' | kvctl -nodes ... txn
  kvctl -nodes ... -hex put 6b 76   # key "k", value "v"
  kvctl -format json -meta 127.0.0.1:9090 ranges
  kvctl -meta 127.0.0.1:9090 status
  kvctl -meta 127.0.0.1:9090 split 1 m
  kvctl -meta 127.0.0.1:9090 merge 2 3
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
		Meta:    *meta,
		Nodes:   splitList(*nodes),
		Timeout: *timeout,
		Format:  strings.ToLower(*format),
		Hex:     *hexMode,
		Quiet:   *quiet,
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

	case "status":
		return r.executeStatus(ctx, b, cfg, args)

	case "split":
		return r.executeSplit(ctx, b, cfg, args)

	case "merge":
		return r.executeMerge(ctx, b, cfg, args)

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

type rangeJSON struct {
	ID       uint64   `json:"id"`
	Start    string   `json:"start"`
	End      string   `json:"end"`
	Leader   string   `json:"leader"`
	Replicas []string `json:"replicas"`
	Version  uint64   `json:"version"`
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
			replicas := info.Replicas
			if replicas == nil {
				replicas = []string{}
			}
			out = append(out, rangeJSON{
				ID:       info.ID,
				Start:    display(info.Start, cfg.Hex),
				End:      display(info.End, cfg.Hex),
				Leader:   info.Leader,
				Replicas: replicas,
				Version:  info.Version,
			})
		}
		return r.writeJSON(out)
	}
	fmt.Fprintln(r.out(), "ID\tSTART\tEND\tLEADER\tREPLICAS\tVERSION")
	for _, info := range infos {
		fmt.Fprintf(r.out(), "%d\t%s\t%s\t%s\t%s\t%d\n",
			info.ID, display(info.Start, cfg.Hex), display(info.End, cfg.Hex),
			info.Leader, strings.Join(info.Replicas, ","), info.Version)
	}
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
	case "ranges", "status", "split", "merge":
		return true
	default:
		return false
	}
}

// adminCommand reports whether cmd is an administrative command that is not
// available in the interactive shell.
func adminCommand(cmd string) bool {
	return metaOnlyCommand(cmd) || cmd == "backup"
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
