// Command kvbackup creates, restores and verifies logical backups of the v2
// KV plane.
//
//	kvbackup backup  -meta 127.0.0.1:9090 -nodes 127.0.0.1:7001 -out ./backup
//	kvbackup restore -meta 127.0.0.1:9090 -nodes 127.0.0.1:7001 -in ./backup
//	kvbackup verify  -in ./backup
//
// backup and restore accept -tls-ca, -tls-cert, -tls-key, -tls-server-name and
// -admin-token (env KVBACKUP_ADMIN_TOKEN) for clusters that require TLS/mTLS
// and the meta admin token. The token is required by SetRanges and is never
// logged. A token without TLS is rejected unless -allow-plaintext-token is
// set, because it would cross the network in clear text.
//
// A backup is a directory with a manifest and checksummed chunk files. It is
// point-in-time at one TSO but is not a continuous/PITR backup: restoring
// overwrites the keys that exist in the backup and does not delete other keys.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"distributed-db/internal/backup"
	"distributed-db/internal/grpctls"
	"distributed-db/internal/v2client"
)

const (
	defaultTimeout = 10 * time.Minute
	// adminTokenEnv supplies the meta admin token when -admin-token is not
	// given on the command line. An explicit flag wins.
	adminTokenEnv = "KVBACKUP_ADMIN_TOKEN"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "backup":
		return runBackup(ctx, args[1:], stdout, stderr)
	case "restore":
		return runRestore(ctx, args[1:], stdout, stderr)
	case "verify":
		return runVerify(ctx, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "kvbackup: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage:
  kvbackup backup  -meta ADDR -nodes ADDR,ADDR -out DIR [-chunk-keys N] [-chunk-bytes BYTES] [-timeout D] [TLS flags]
  kvbackup restore -meta ADDR -nodes ADDR,ADDR -in DIR [-ensure-ranges] [-replicas ADDR,ADDR] [-strict-epoch] [-check] [-timeout D] [TLS flags]
  kvbackup verify  -in DIR

backup captures all ranges at one TSO and writes DIR atomically.
restore verifies the backup, checks or replaces the target range map, then
writes one transaction per chunk. -ensure-ranges uses meta SetRanges and
requires the target KV nodes to already host the same range boundaries.
verify validates the manifest, checksums, framing and totals offline.

TLS and admin auth (backup and restore only):
  -tls-ca FILE             PEM CA bundle for meta/KV server verification (default: system roots)
  -tls-cert FILE           PEM client certificate for mTLS (requires -tls-key)
  -tls-key FILE            PEM client private key for -tls-cert
  -tls-server-name NAME    expected server name for certificate verification
  -admin-token TOKEN       Bearer token for the meta admin RPC (env KVBACKUP_ADMIN_TOKEN)
  -allow-plaintext-token   allow -admin-token without TLS (development only; the token is sent in clear text)

Setting -tls-ca, -tls-cert/-tls-key or -tls-server-name enables cluster TLS.
There is no plaintext fallback: a TLS configuration or verification error fails
the command. A token without TLS is rejected unless -allow-plaintext-token is
passed. Tokens are never printed, and the token is only sent on SetRanges.
`)
}

func runBackup(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	metaAddr := fs.String("meta", "", "meta service address(es), comma-separated")
	nodes := fs.String("nodes", "", "KV node address(es), comma-separated")
	out := fs.String("out", "", "backup directory to create")
	chunkKeys := fs.Int("chunk-keys", backup.DefaultChunkKeys, "maximum keys per chunk")
	chunkBytes := fs.Int64("chunk-bytes", backup.DefaultChunkBytes, "maximum key+value payload bytes per chunk")
	timeout := fs.Duration("timeout", defaultTimeout, "overall operation timeout")
	cf := addClientFlags(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	cf.resolve(fs)
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "kvbackup backup: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	if *metaAddr == "" || *nodes == "" || *out == "" {
		fmt.Fprintln(stderr, "kvbackup backup: -meta, -nodes and -out are required")
		return 2
	}
	if err := cf.validate(); err != nil {
		fmt.Fprintf(stderr, "kvbackup backup: %v\n", err)
		return 2
	}
	clientTLS, err := cf.clientOptions()
	if err != nil {
		fmt.Fprintf(stderr, "kvbackup backup: %v\n", err)
		return 2
	}
	opCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	client, meta, err := openClients(*metaAddr, *nodes, clientTLS, cf.adminToken)
	if err != nil {
		fmt.Fprintf(stderr, "kvbackup backup: %v\n", err)
		return 1
	}
	defer client.Close()
	defer meta.Close()

	m, err := backup.Backup(opCtx, backup.BackupConfig{
		Client: client, Meta: meta, OutDir: *out,
		ChunkKeys: *chunkKeys, ChunkBytes: *chunkBytes,
	})
	if err != nil {
		fmt.Fprintf(stderr, "kvbackup backup: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "backup complete: %s\nranges=%d chunks=%d keys=%d bytes=%d snapshot_ts=%d\n",
		*out, len(m.Ranges), len(m.Chunks), m.TotalKeys, m.TotalBytes, m.SnapshotTS)
	return 0
}

func runRestore(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	metaAddr := fs.String("meta", "", "meta service address(es), comma-separated")
	nodes := fs.String("nodes", "", "KV node address(es), comma-separated")
	in := fs.String("in", "", "backup directory to restore")
	ensure := fs.Bool("ensure-ranges", false, "replace the meta range map with the backup layout")
	replicas := fs.String("replicas", "", "replica addresses to use with -ensure-ranges (comma-separated)")
	strictEpoch := fs.Bool("strict-epoch", false, "require target range versions to match the backup")
	check := fs.Bool("check", false, "read every restored key back and compare values")
	timeout := fs.Duration("timeout", defaultTimeout, "overall operation timeout")
	cf := addClientFlags(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	cf.resolve(fs)
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "kvbackup restore: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	if *metaAddr == "" || *nodes == "" || *in == "" {
		fmt.Fprintln(stderr, "kvbackup restore: -meta, -nodes and -in are required")
		return 2
	}
	if err := cf.validate(); err != nil {
		fmt.Fprintf(stderr, "kvbackup restore: %v\n", err)
		return 2
	}
	clientTLS, err := cf.clientOptions()
	if err != nil {
		fmt.Fprintf(stderr, "kvbackup restore: %v\n", err)
		return 2
	}
	opCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	client, meta, err := openClients(*metaAddr, *nodes, clientTLS, cf.adminToken)
	if err != nil {
		fmt.Fprintf(stderr, "kvbackup restore: %v\n", err)
		return 1
	}
	defer client.Close()
	defer meta.Close()

	rep, err := backup.Restore(opCtx, backup.RestoreConfig{
		Client: client, Meta: meta, InDir: *in,
		EnsureRanges: *ensure, Replicas: splitCSV(*replicas),
		StrictEpoch: *strictEpoch, Check: *check,
	})
	if err != nil {
		fmt.Fprintf(stderr, "kvbackup restore: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "restore complete: %s\nranges=%d chunks=%d keys=%d bytes=%d snapshot_ts=%d checked=%v\n",
		*in, rep.Ranges, rep.Chunks, rep.Keys, rep.PayloadBytes, rep.SnapshotTS, rep.Checked)
	return 0
}

func runVerify(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	in := fs.String("in", "", "backup directory to verify")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "kvbackup verify: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	if *in == "" {
		fmt.Fprintln(stderr, "kvbackup verify: -in is required")
		return 2
	}
	rep, err := backup.Verify(ctx, *in)
	if err != nil {
		fmt.Fprintf(stderr, "kvbackup verify: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "verify ok: %s\nranges=%d chunks=%d keys=%d bytes=%d snapshot_ts=%d\n",
		*in, rep.Ranges, rep.Chunks, rep.Keys, rep.PayloadBytes, rep.SnapshotTS)
	return 0
}

func openClients(metaFlag, nodesFlag string, clientTLS *grpctls.ClientOptions, adminToken string) (*v2client.Client, *backup.MetaClient, error) {
	metas := splitCSV(metaFlag)
	nodes := splitCSV(nodesFlag)
	if len(metas) == 0 {
		return nil, nil, errors.New("-meta is required")
	}
	if len(nodes) == 0 {
		return nil, nil, errors.New("-nodes is required")
	}
	client, err := v2client.NewWithOptions(metas[0], nodes, v2client.Options{
		TLS:        clientTLS,
		AdminToken: adminToken,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("open v2 client: %w", err)
	}
	meta, err := backup.OpenMetaWithOptions(metas, backup.MetaOptions{
		TLS:        clientTLS,
		AdminToken: adminToken,
	})
	if err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	return client, meta, nil
}

// clientFlags holds the cluster transport and admin-auth flags shared by the
// backup and restore commands. verify never contacts a cluster.
type clientFlags struct {
	tlsCA               string
	tlsCert             string
	tlsKey              string
	tlsServerName       string
	adminToken          string
	allowPlaintextToken bool
}

// addClientFlags registers the shared TLS/admin flags on fs. The admin-token
// default is deliberately empty: resolve reads the environment only after a
// successful parse, so an env token never appears in -help output or a flag
// parse error.
func addClientFlags(fs *flag.FlagSet) *clientFlags {
	f := &clientFlags{}
	fs.StringVar(&f.tlsCA, "tls-ca", "", "PEM CA bundle for meta/KV server verification (default: system roots)")
	fs.StringVar(&f.tlsCert, "tls-cert", "", "PEM client certificate for mTLS (requires -tls-key)")
	fs.StringVar(&f.tlsKey, "tls-key", "", "PEM client private key for -tls-cert")
	fs.StringVar(&f.tlsServerName, "tls-server-name", "", "expected server name for certificate verification")
	fs.StringVar(&f.adminToken, "admin-token", "", "Bearer token for the meta admin RPC (env "+adminTokenEnv+"); requires TLS")
	fs.BoolVar(&f.allowPlaintextToken, "allow-plaintext-token", false, "allow -admin-token without TLS (development only; the token is sent in clear text)")
	return f
}

// resolve applies the environment fallback unless -admin-token was set
// explicitly on the command line.
func (f *clientFlags) resolve(fs *flag.FlagSet) {
	explicit := false
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "admin-token" {
			explicit = true
		}
	})
	if !explicit {
		f.adminToken = os.Getenv(adminTokenEnv)
	}
}

func (f *clientFlags) tlsEnabled() bool {
	return f.tlsCA != "" || f.tlsCert != "" || f.tlsKey != "" || f.tlsServerName != ""
}

// validate rejects a token that would cross a plaintext connection. The
// errors never contain the token.
func (f *clientFlags) validate() error {
	if (f.tlsCert == "") != (f.tlsKey == "") {
		return errors.New("-tls-cert and -tls-key must be provided together")
	}
	if f.adminToken != "" && !f.tlsEnabled() && !f.allowPlaintextToken {
		return errors.New("refusing to send -admin-token without TLS: the token would cross the network in clear text " +
			"(set -tls-ca/-tls-server-name and/or -tls-cert/-tls-key, or pass -allow-plaintext-token for a trusted development cluster)")
	}
	return nil
}

// clientOptions returns the client TLS configuration. A nil result means
// plaintext; partial or unreadable TLS material is an error, never a silent
// fallback.
func (f *clientFlags) clientOptions() (*grpctls.ClientOptions, error) {
	return grpctls.NewClientOptions(f.tlsCA, f.tlsCert, f.tlsKey, f.tlsServerName)
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
