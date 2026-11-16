// Command edgehash builds, serves and queries immutable edge hash sets.
//
//	edgehash build  -in hashes.txt -out DIR [-prefix-bytes 2] [-fp 0.01] [-key HEX]
//	edgehash serve  -dir DIR [-addr :8080] [-key HEX]
//	edgehash lookup -server URL -hash HEX [-key HEX] [-timeout D]
//	edgehash sync   -server URL -dir CACHE [-key HEX] [-prefix P1,P2] [-timeout D]
//
// The build input is one 64-character hexadecimal SHA-256 per line. Blank
// lines and lines beginning with '#' are ignored; "-" reads standard input.
package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"distributed-db/internal/edgecache"
	"distributed-db/internal/edgehttp"
	"distributed-db/internal/edgeset"
)

const (
	defaultAddr          = ":8080"
	defaultLookupTimeout = 15 * time.Second
	shutdownTimeout      = 5 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "build":
		return runBuild(args[1:], stdin, stdout, stderr)
	case "serve":
		return runServe(ctx, args[1:], stdout, stderr)
	case "lookup":
		return runLookup(ctx, args[1:], stdout, stderr)
	case "sync":
		return runSync(ctx, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "edgehash: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage:
  edgehash build  -in FILE -out DIR [-prefix-bytes 2] [-fp 0.01] [-key HEX]
  edgehash serve  -dir DIR [-addr :8080] [-key HEX]
  edgehash lookup -server URL -hash HEX [-key HEX] [-timeout D]
  edgehash sync   -server URL -dir CACHE [-key HEX] [-prefix P1,P2] [-timeout D]

build reads one 64-character hexadecimal SHA-256 per line; blank lines and
lines starting with '#' are ignored. Use -in - to read standard input.

lookup asks the server directly and verifies the manifest HMAC and the returned
Merkle proof. sync materializes a local cache for offline Bloom lookups; the
optional -prefix list (comma-separated 4-hex segment ids) caches only selected
segments and resolves other prefixes remotely.
`)
}

func runBuild(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	in := fs.String("in", "", "hashes file, or - for stdin")
	out := fs.String("out", "", "output store directory")
	prefixBytes := fs.Int("prefix-bytes", 2, "high hash bytes used for segment routing (1..4)")
	falsePositive := fs.Float64("fp", 0.01, "Bloom false-positive target in (0,1)")
	keyHex := fs.String("key", "", "manifest HMAC-SHA256 key as hex")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "edgehash build: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	if *in == "" || *out == "" {
		fmt.Fprintln(stderr, "edgehash build: -in and -out are required")
		return 2
	}
	key, err := parseKey(*keyHex)
	if err != nil {
		fmt.Fprintf(stderr, "edgehash build: %v\n", err)
		return 2
	}

	reader := stdin
	var closeInput func() error
	if *in != "-" {
		file, err := os.Open(*in)
		if err != nil {
			fmt.Fprintf(stderr, "edgehash build: open input: %v\n", err)
			return 1
		}
		reader = file
		closeInput = file.Close
	}
	hashes, err := readHashes(reader)
	if closeInput != nil {
		_ = closeInput()
	}
	if err != nil {
		fmt.Fprintf(stderr, "edgehash build: %v\n", err)
		return 1
	}

	store, manifest, err := edgeset.Build(hashes, edgeset.BuildOptions{
		SegmentDir:        *out,
		PrefixBytes:       *prefixBytes,
		FalsePositiveRate: *falsePositive,
		SigningKey:        key,
	})
	if err != nil {
		fmt.Fprintf(stderr, "edgehash build: %v\n", err)
		return 1
	}
	signature := "unsigned"
	if len(manifest.Signature) > 0 {
		signature = "signed"
	}
	fmt.Fprintf(stdout, "built %d hashes in %d segments at %s (manifest v%d, %s)\n",
		store.TotalCount(), store.SegmentCount(), store.Dir(), manifest.Version, signature)
	return 0
}

func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "store directory")
	addr := fs.String("addr", defaultAddr, "listen address")
	keyHex := fs.String("key", "", "manifest HMAC-SHA256 key as hex")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "edgehash serve: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	if *dir == "" {
		fmt.Fprintln(stderr, "edgehash serve: -dir is required")
		return 2
	}
	key, err := parseKey(*keyHex)
	if err != nil {
		fmt.Fprintf(stderr, "edgehash serve: %v\n", err)
		return 2
	}

	server, err := edgehttp.NewServer(*dir, key)
	if err != nil {
		fmt.Fprintf(stderr, "edgehash serve: %v\n", err)
		return 1
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(stderr, "edgehash serve: listen %s: %v\n", *addr, err)
		return 1
	}
	httpServer := &http.Server{
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(listener) }()
	fmt.Fprintf(stdout, "edgehash: serving %s on %s\n", server.Dir(), listener.Addr())

	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "edgehash serve: %v\n", err)
			return 1
		}
		return 0
	case <-ctx.Done():
		fmt.Fprintln(stdout, "edgehash: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			_ = httpServer.Close()
			fmt.Fprintf(stderr, "edgehash serve: shutdown: %v\n", err)
			return 1
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "edgehash serve: %v\n", err)
			return 1
		}
		return 0
	}
}

func runLookup(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lookup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	serverURL := fs.String("server", "", "edgehash server base URL")
	hashHex := fs.String("hash", "", "hash to look up (64 hex characters)")
	keyHex := fs.String("key", "", "manifest HMAC-SHA256 key as hex")
	timeout := fs.Duration("timeout", defaultLookupTimeout, "HTTP timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "edgehash lookup: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	if *serverURL == "" || *hashHex == "" {
		fmt.Fprintln(stderr, "edgehash lookup: -server and -hash are required")
		return 2
	}
	key, err := parseKey(*keyHex)
	if err != nil {
		fmt.Fprintf(stderr, "edgehash lookup: %v\n", err)
		return 2
	}
	h, err := edgeset.ParseHash(strings.TrimSpace(*hashHex))
	if err != nil {
		fmt.Fprintf(stderr, "edgehash lookup: %v\n", err)
		return 2
	}
	result, err := edgecache.RemoteLookup(ctx, *serverURL, key, h, edgecache.Options{Timeout: *timeout})
	if err != nil {
		fmt.Fprintf(stderr, "edgehash lookup: %v\n", err)
		return 1
	}
	if result.Present {
		fmt.Fprintf(stdout, "present: true\nprefix: %04x\nroot: %s\nproof: verified\n",
			result.Prefix, result.Root)
	} else {
		fmt.Fprintln(stdout, "present: false")
	}
	return 0
}

func runSync(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	serverURL := fs.String("server", "", "edgehash server base URL")
	dir := fs.String("dir", "", "cache directory")
	keyHex := fs.String("key", "", "manifest HMAC-SHA256 key as hex")
	prefixes := fs.String("prefix", "", "comma-separated segment prefixes to cache")
	timeout := fs.Duration("timeout", defaultLookupTimeout, "HTTP timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "edgehash sync: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	if *serverURL == "" || *dir == "" {
		fmt.Fprintln(stderr, "edgehash sync: -server and -dir are required")
		return 2
	}
	key, err := parseKey(*keyHex)
	if err != nil {
		fmt.Fprintf(stderr, "edgehash sync: %v\n", err)
		return 2
	}
	filter, err := parsePrefixList(*prefixes)
	if err != nil {
		fmt.Fprintf(stderr, "edgehash sync: %v\n", err)
		return 2
	}

	cache, err := edgecache.New(*serverURL, *dir, key, edgecache.Options{
		Timeout:  *timeout,
		Prefixes: filter,
	})
	if err != nil {
		fmt.Fprintf(stderr, "edgehash sync: %v\n", err)
		return 1
	}
	if err := cache.Sync(ctx); err != nil {
		fmt.Fprintf(stderr, "edgehash sync: %v\n", err)
		return 1
	}
	full := cache.Manifest()
	store := cache.Store()
	cachedSegments, cachedHashes := 0, 0
	if store != nil {
		cachedSegments = store.SegmentCount()
		cachedHashes = store.TotalCount()
	}
	fmt.Fprintf(stdout, "synced %d/%d segments (%d hashes) into %s\n",
		cachedSegments, len(full.Segments), cachedHashes, cache.Dir())
	return 0
}

func readHashes(r io.Reader) ([]edgeset.Hash, error) {
	if r == nil {
		return nil, errors.New("no hash input")
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var hashes []edgeset.Hash
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Permit a trailing whitespace-separated comment.
		if fields := strings.Fields(line); len(fields) > 0 {
			line = fields[0]
		}
		h, err := edgeset.ParseHash(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		hashes = append(hashes, h)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read input: %w", err)
	}
	return hashes, nil
}

func parseKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if strings.HasPrefix(raw, "0x") || strings.HasPrefix(raw, "0X") {
		raw = raw[2:]
	}
	key, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid hex key: %w", err)
	}
	return key, nil
}

func parsePrefixList(raw string) ([]uint16, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	prefixes := make([]uint16, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if len(part) != 4 {
			return nil, fmt.Errorf("invalid prefix %q: exactly 4 hex characters required", part)
		}
		value, err := strconv.ParseUint(part, 16, 16)
		if err != nil {
			return nil, fmt.Errorf("invalid prefix %q: exactly 4 hex characters required", part)
		}
		prefixes = append(prefixes, uint16(value))
	}
	return prefixes, nil
}
