// Command edgehash builds, serves and queries immutable edge hash sets.
//
//	edgehash build  -in hashes.txt -out DIR [-prefix-bytes 2] [-fp 0.01] [-key HEX] [-ed25519-key FILE] [-compression auto|none|zstd]
//	edgehash serve  -dir DIR [-addr :8080] [-key HEX] [-verify-ed25519 PUBKEY] [-tls-cert FILE] [-tls-key FILE] [-auth-token TOKEN] [-rate-limit N] [-rate-burst N]
//	edgehash lookup -server URL -hash HEX [-key HEX] [-verify-ed25519 PUBKEY] [-timeout D]
//	edgehash sync   -server URL -dir CACHE [-key HEX] [-verify-ed25519 PUBKEY] [-prefix P1,P2] [-max-bytes N] [-max-segments N] [-ttl D] [-timeout D]
//
// The build input is one 64-character hexadecimal SHA-256 per line. Blank
// lines and lines beginning with '#' are ignored; "-" reads standard input.
//
// Manifests can be signed with a legacy HMAC-SHA256 key (-key HEX) or Ed25519
// (-ed25519-key for build, -verify-ed25519 for serve/lookup/sync). The Ed25519
// private key file may be PEM PKCS#8, OpenSSH PEM, or raw 64 bytes (or a 32-byte
// seed). The public key file may be PEM SPKI, a PEM certificate, or raw 32
// bytes.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
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

	"golang.org/x/crypto/ssh"

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
  edgehash build  -in FILE -out DIR [-prefix-bytes 2] [-fp 0.01] [-key HEX] [-ed25519-key FILE] [-compression auto|none|zstd]
  edgehash serve  -dir DIR [-addr :8080] [-key HEX] [-verify-ed25519 PUBKEY] [-tls-cert FILE] [-tls-key FILE] [-auth-token TOKEN] [-rate-limit N] [-rate-burst N]
  edgehash lookup -server URL -hash HEX [-key HEX] [-verify-ed25519 PUBKEY] [-auth-token TOKEN] [-timeout D]
  edgehash sync   -server URL -dir CACHE [-key HEX] [-verify-ed25519 PUBKEY] [-prefix P1,P2] [-max-bytes N] [-max-segments N] [-ttl D] [-timeout D]

build reads one 64-character hexadecimal SHA-256 per line; blank lines and
lines starting with '#' are ignored. Use -in - to read standard input.
-compression auto selects zstd for segments of 4 KiB or larger, none disables
compression and zstd always compresses.

serve exposes /healthz, /v1/manifest, /v1/manifest/version, /v1/segment/{prefix}
and /v1/lookup/{hash}. Segment responses are CDN-friendly (strong SHA-256 ETag,
Range support, one-year immutable Cache-Control). -auth-token enables Bearer
authentication, -rate-limit/-rate-burst a per-IP token bucket, and
-tls-cert/-tls-key TLS. Without -tls-cert the server speaks plain HTTP.

lookup asks the server directly and verifies the manifest signature and the
returned Merkle proof (including absence proofs returned by ?absence=1). sync
materializes a crash-atomic local cache for offline Bloom lookups; the optional
-prefix list (comma-separated 4-hex segment ids) caches only selected segments
and resolves other prefixes remotely. -max-bytes, -max-segments and -ttl bound
the cached segment set; evicted segments fall back to the server.
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
	ed25519KeyFile := fs.String("ed25519-key", "", "Ed25519 private key file (PEM/PKCS8, OpenSSH PEM or raw 64 bytes)")
	compression := fs.String("compression", "auto", "segment compression: auto, none or zstd")
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
	if *ed25519KeyFile != "" && len(key) > 0 {
		fmt.Fprintln(stderr, "edgehash build: -key and -ed25519-key are mutually exclusive")
		return 2
	}
	var privateKey ed25519.PrivateKey
	if *ed25519KeyFile != "" {
		privateKey, err = loadEd25519PrivateKey(*ed25519KeyFile)
		if err != nil {
			fmt.Fprintf(stderr, "edgehash build: %v\n", err)
			return 2
		}
	}
	compressionValue, err := parseCompressionFlag(*compression)
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
		Ed25519PrivateKey: privateKey,
		Compression:       compressionValue,
	})
	if err != nil {
		fmt.Fprintf(stderr, "edgehash build: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "built %d hashes in %d segments at %s (manifest v%d, %s)\n",
		store.TotalCount(), store.SegmentCount(), store.Dir(), manifest.Version, describeSignature(manifest))
	return 0
}

func describeSignature(manifest *edgeset.Manifest) string {
	if manifest == nil || len(manifest.Signature) == 0 {
		return "unsigned"
	}
	switch manifest.SignatureAlg {
	case edgeset.SignatureAlgEd25519:
		return "signed (ed25519)"
	case "", edgeset.SignatureAlgHMACSHA256:
		return "signed (hmac-sha256)"
	default:
		return "signed"
	}
}

func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "store directory")
	addr := fs.String("addr", defaultAddr, "listen address")
	keyHex := fs.String("key", "", "manifest HMAC-SHA256 key as hex")
	ed25519KeyFile := fs.String("verify-ed25519", "", "Ed25519 public key file (PEM SPKI, certificate or raw 32 bytes)")
	tlsCert := fs.String("tls-cert", "", "TLS certificate PEM file")
	tlsKey := fs.String("tls-key", "", "TLS private key PEM file")
	authToken := fs.String("auth-token", "", "Bearer token required on API endpoints")
	rateLimit := fs.Float64("rate-limit", 0, "per-IP requests per second (0 disables)")
	rateBurst := fs.Int("rate-burst", 0, "per-IP token bucket burst (0 defaults to ceil(rate))")
	readHeaderTimeout := fs.Duration("read-header-timeout", 10*time.Second, "maximum time to read request headers")
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
	verifier, err := verifierFromFlags(*keyHex, *ed25519KeyFile)
	if err != nil {
		fmt.Fprintf(stderr, "edgehash serve: %v\n", err)
		return 2
	}

	server, err := edgehttp.NewServerWithOptions(*dir, edgehttp.Options{
		Verifier:          verifier,
		TLSCertFile:       *tlsCert,
		TLSKeyFile:        *tlsKey,
		AuthToken:         *authToken,
		RateLimit:         *rateLimit,
		RateBurst:         *rateBurst,
		ReadHeaderTimeout: *readHeaderTimeout,
	})
	if err != nil {
		fmt.Fprintf(stderr, "edgehash serve: %v\n", err)
		return 1
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(stderr, "edgehash serve: listen %s: %v\n", *addr, err)
		return 1
	}
	httpServer := server.HTTPHandlerServer()
	serveErr := make(chan error, 1)
	if *tlsCert != "" || *tlsKey != "" {
		// Use the same http.Server instance for serving and Shutdown.
		go func() { serveErr <- httpServer.ServeTLS(listener, *tlsCert, *tlsKey) }()
		fmt.Fprintf(stdout, "edgehash: serving %s on %s (tls)\n", server.Dir(), listener.Addr())
	} else {
		go func() { serveErr <- httpServer.Serve(listener) }()
		fmt.Fprintf(stdout, "edgehash: serving %s on %s\n", server.Dir(), listener.Addr())
	}

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
	ed25519KeyFile := fs.String("verify-ed25519", "", "Ed25519 public key file (PEM SPKI, certificate or raw 32 bytes)")
	authToken := fs.String("auth-token", "", "Bearer token for the edge server")
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
	verifier, err := verifierFromFlags(*keyHex, *ed25519KeyFile)
	if err != nil {
		fmt.Fprintf(stderr, "edgehash lookup: %v\n", err)
		return 2
	}
	h, err := edgeset.ParseHash(strings.TrimSpace(*hashHex))
	if err != nil {
		fmt.Fprintf(stderr, "edgehash lookup: %v\n", err)
		return 2
	}
	result, err := edgecache.RemoteLookup(ctx, *serverURL, nil, h, edgecache.Options{
		Timeout:   *timeout,
		Verifier:  verifier,
		AuthToken: *authToken,
	})
	if err != nil {
		fmt.Fprintf(stderr, "edgehash lookup: %v\n", err)
		return 1
	}
	if result.Present {
		fmt.Fprintf(stdout, "present: true\nprefix: %04x\nroot: %s\nproof: verified\n",
			result.Prefix, result.Root)
		return 0
	}
	fmt.Fprintln(stdout, "present: false")
	if result.AbsenceProof != nil {
		fmt.Fprintf(stdout, "absence_proof: verified (absence proof)\nprefix: %04x\nroot: %s\n",
			result.Prefix, result.Root)
	}
	return 0
}

func runSync(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	serverURL := fs.String("server", "", "edgehash server base URL")
	dir := fs.String("dir", "", "cache directory")
	keyHex := fs.String("key", "", "manifest HMAC-SHA256 key as hex")
	ed25519KeyFile := fs.String("verify-ed25519", "", "Ed25519 public key file (PEM SPKI, certificate or raw 32 bytes)")
	prefixes := fs.String("prefix", "", "comma-separated segment prefixes to cache")
	timeout := fs.Duration("timeout", defaultLookupTimeout, "HTTP timeout")
	maxBytes := fs.Int64("max-bytes", 0, "maximum cached segment bytes (0 unlimited)")
	maxSegments := fs.Int("max-segments", 0, "maximum cached segments (0 unlimited)")
	ttl := fs.Duration("ttl", 0, "evict segments unused for this duration (0 disables)")
	evictInterval := fs.Duration("evict-interval", 0, "background eviction interval (0 disables)")
	authToken := fs.String("auth-token", "", "Bearer token for the edge server")
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
	verifier, err := verifierFromFlags(*keyHex, *ed25519KeyFile)
	if err != nil {
		fmt.Fprintf(stderr, "edgehash sync: %v\n", err)
		return 2
	}
	filter, err := parsePrefixList(*prefixes)
	if err != nil {
		fmt.Fprintf(stderr, "edgehash sync: %v\n", err)
		return 2
	}

	cache, err := edgecache.New(*serverURL, *dir, nil, edgecache.Options{
		Timeout:       *timeout,
		Prefixes:      filter,
		Verifier:      verifier,
		MaxCacheBytes: *maxBytes,
		MaxSegments:   *maxSegments,
		SegmentTTL:    *ttl,
		EvictInterval: *evictInterval,
		AuthToken:     *authToken,
	})
	if err != nil {
		fmt.Fprintf(stderr, "edgehash sync: %v\n", err)
		return 1
	}
	defer func() { _ = cache.Close() }()
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
	totalSegments := 0
	if full != nil {
		totalSegments = len(full.Segments)
	}
	fmt.Fprintf(stdout, "synced %d/%d segments (%d hashes) into %s\n",
		cachedSegments, totalSegments, cachedHashes, cache.Dir())
	stats := cache.Stats()
	fmt.Fprintf(stdout, "cache: bytes=%d segments=%d evictions=%d generations=%d generation=%s\n",
		stats.Bytes, stats.Segments, stats.Evictions, stats.Generations, cache.CurrentGeneration())
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

func verifierFromFlags(keyHex, ed25519KeyFile string) (edgeset.Verifier, error) {
	key, err := parseKey(keyHex)
	if err != nil {
		return nil, err
	}
	if ed25519KeyFile != "" {
		if len(key) > 0 {
			return nil, errors.New("-key and -verify-ed25519 are mutually exclusive")
		}
		publicKey, err := loadEd25519PublicKey(ed25519KeyFile)
		if err != nil {
			return nil, err
		}
		return edgeset.NewEd25519Verifier(publicKey), nil
	}
	return edgeset.NewHMACVerifier(key), nil
}

func parseCompressionFlag(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "auto":
		return "", nil
	case edgeset.CompressionNone:
		return edgeset.CompressionNone, nil
	case edgeset.CompressionZstd:
		return edgeset.CompressionZstd, nil
	default:
		return "", fmt.Errorf("invalid compression %q: want auto, none or zstd", raw)
	}
}

// loadEd25519PrivateKey accepts a PEM PKCS#8/OpenSSH private key, raw 64-byte
// private key bytes, or a raw 32-byte seed. Hex text is accepted as a
// convenience for generated test keys.
func loadEd25519PrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Ed25519 private key: %w", err)
	}
	if block, _ := pem.Decode(raw); block != nil {
		switch block.Type {
		case "OPENSSH PRIVATE KEY":
			key, err := ssh.ParseRawPrivateKey(raw)
			if err != nil {
				return nil, fmt.Errorf("parse OpenSSH private key: %w", err)
			}
			return asEd25519PrivateKey(key)
		default:
			key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				if len(block.Bytes) == ed25519.PrivateKeySize {
					return append(ed25519.PrivateKey(nil), block.Bytes...), nil
				}
				if len(block.Bytes) == ed25519.SeedSize {
					return ed25519.NewKeyFromSeed(block.Bytes), nil
				}
				return nil, fmt.Errorf("parse Ed25519 PKCS#8 private key: %w", err)
			}
			return asEd25519PrivateKey(key)
		}
	}
	return decodeEd25519PrivateBytes(raw)
}

func asEd25519PrivateKey(key any) (ed25519.PrivateKey, error) {
	switch value := key.(type) {
	case ed25519.PrivateKey:
		return append(ed25519.PrivateKey(nil), value...), nil
	case *ed25519.PrivateKey:
		return append(ed25519.PrivateKey(nil), (*value)...), nil
	default:
		return nil, fmt.Errorf("key is %T, want ed25519.PrivateKey", key)
	}
}

func decodeEd25519PrivateBytes(raw []byte) (ed25519.PrivateKey, error) {
	if len(raw) == ed25519.PrivateKeySize {
		return append(ed25519.PrivateKey(nil), raw...), nil
	}
	trimmed := bytes.TrimRight(raw, "\r\n")
	if len(trimmed) == ed25519.PrivateKeySize {
		return append(ed25519.PrivateKey(nil), trimmed...), nil
	}
	if len(trimmed) == ed25519.SeedSize {
		return ed25519.NewKeyFromSeed(trimmed), nil
	}
	if spaced := bytes.TrimSpace(raw); len(spaced) == ed25519.PrivateKeySize {
		return append(ed25519.PrivateKey(nil), spaced...), nil
	} else if len(spaced) == ed25519.SeedSize {
		return ed25519.NewKeyFromSeed(spaced), nil
	}
	text := strings.TrimSpace(string(raw))
	if decoded, err := hex.DecodeString(text); err == nil {
		return decodeEd25519PrivateBytes(decoded)
	}
	return nil, fmt.Errorf("Ed25519 private key must be PEM/PKCS#8, raw %d bytes or a %d-byte seed",
		ed25519.PrivateKeySize, ed25519.SeedSize)
}

// loadEd25519PublicKey accepts PEM SPKI, a PEM certificate, raw 32 bytes or hex.
func loadEd25519PublicKey(path string) (ed25519.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Ed25519 public key: %w", err)
	}
	if key, _, _, _, err := ssh.ParseAuthorizedKey(raw); err == nil {
		if cryptoKey, ok := key.(ssh.CryptoPublicKey); ok {
			if publicKey, ok := cryptoKey.CryptoPublicKey().(ed25519.PublicKey); ok {
				return append(ed25519.PublicKey(nil), publicKey...), nil
			}
		}
	}
	if block, _ := pem.Decode(raw); block != nil {
		switch block.Type {
		case "CERTIFICATE":
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("parse certificate: %w", err)
			}
			publicKey, ok := cert.PublicKey.(ed25519.PublicKey)
			if !ok {
				return nil, fmt.Errorf("certificate public key is %T, want ed25519.PublicKey", cert.PublicKey)
			}
			return append(ed25519.PublicKey(nil), publicKey...), nil
		default:
			if sshKey, err := ssh.ParsePublicKey(block.Bytes); err == nil {
				if cryptoKey, ok := sshKey.(ssh.CryptoPublicKey); ok {
					if publicKey, ok := cryptoKey.CryptoPublicKey().(ed25519.PublicKey); ok {
						return append(ed25519.PublicKey(nil), publicKey...), nil
					}
				}
			}
			key, err := x509.ParsePKIXPublicKey(block.Bytes)
			if err != nil {
				if len(block.Bytes) == ed25519.PublicKeySize {
					return append(ed25519.PublicKey(nil), block.Bytes...), nil
				}
				return nil, fmt.Errorf("parse Ed25519 public key: %w", err)
			}
			publicKey, ok := key.(ed25519.PublicKey)
			if !ok {
				return nil, fmt.Errorf("public key is %T, want ed25519.PublicKey", key)
			}
			return append(ed25519.PublicKey(nil), publicKey...), nil
		}
	}
	if len(raw) == ed25519.PublicKeySize {
		return append(ed25519.PublicKey(nil), raw...), nil
	}
	trimmed := bytes.TrimRight(raw, "\r\n")
	if len(trimmed) == ed25519.PublicKeySize {
		return append(ed25519.PublicKey(nil), trimmed...), nil
	}
	if spaced := bytes.TrimSpace(raw); len(spaced) == ed25519.PublicKeySize {
		return append(ed25519.PublicKey(nil), spaced...), nil
	}
	if decoded, err := hex.DecodeString(strings.TrimSpace(string(raw))); err == nil && len(decoded) == ed25519.PublicKeySize {
		return ed25519.PublicKey(decoded), nil
	}
	return nil, fmt.Errorf("Ed25519 public key must be PEM SPKI, a certificate, raw %d bytes or hex", ed25519.PublicKeySize)
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
