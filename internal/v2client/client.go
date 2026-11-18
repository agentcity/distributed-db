// Package v2client is the transactional client for the v2 KV service.
//
// It discovers the shard leader by polling KVService.Status, caches it for a
// short time, and exposes a small Begin/Set/Get/Commit/Rollback transaction
// API. TSO comes from the meta service and is fetched in batches. Range
// descriptors are cached briefly so a retry storm does not turn into a meta
// RPC storm; both caches are invalidated when the meta leader moves.
//
// Every meta and KV connection also carries a wire-additive protocol version
// handshake: the client advertises its semantic version and feature bits on
// each RPC, records the server version from the response headers and exposes
// the result through ProtocolInfo, NegotiatedVersion and ProtocolSummary. A
// server without handshake support is treated as the legacy protocol version.
package v2client

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"distributed-db/internal/grpctls"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/version"
)

var (
	// ErrNotFound is returned by Txn.Get when no visible value exists.
	ErrNotFound = errors.New("v2client: key not found")
	// ErrNotLeader is returned when every attempt landed on a non-leader.
	ErrNotLeader = errors.New("v2client: not leader")
	// ErrConflict is returned for lock/write conflicts.
	ErrConflict = errors.New("v2client: transaction conflict")
	// ErrCommitIncomplete means the primary range committed (the transaction
	// is durable) but at least one secondary range did not. The transaction
	// must not be rolled back; calling Txn.Commit again retries the remaining
	// range commits idempotently at the same commitTS.
	ErrCommitIncomplete = errors.New("v2client: commit point reached but not all ranges committed")
	// ErrCommitUnknown means the primary range's commit outcome could not be
	// determined (for example the primary leader died). The transaction may
	// have committed, so Rollback is refused; retry Commit or resolve the
	// transaction through the primary lock.
	ErrCommitUnknown = errors.New("v2client: primary commit outcome unknown")
	// ErrLockWaitTimeout means a conflicting lock did not clear within the
	// configured wait budget. The error carries wait-for diagnostics.
	ErrLockWaitTimeout = errors.New("v2client: lock wait timed out")

	// errClientClosed is returned after Close.
	errClientClosed = errors.New("v2client: closed")

	// Server-reported conflict classes. They are wrapped together with
	// ErrConflict so callers can distinguish "still locked" (worth waiting
	// for) from "write conflict" (abort immediately).
	errServerLocked           = errors.New("v2client: key is locked by another transaction")
	errServerWriteConflict    = errors.New("v2client: write conflict")
	errServerAlreadyCommitted = errors.New("v2client: transaction already committed")
)

const (
	leaderTTL      = time.Second
	maxAttempts    = 8
	backoff        = 50 * time.Millisecond
	defaultTimeout = 5 * time.Second

	// defaultCommitRetryTimeout bounds secondary-commit retries after the
	// primary range has committed. It is deliberately short: an unresolved
	// transaction stays durably visible after recovery via its primary lock.
	defaultCommitRetryTimeout = 5 * time.Second
	// maxCommitRetryTimeout caps a caller-supplied retry budget.
	maxCommitRetryTimeout = 30 * time.Second
	// maxCommitRetryBackoff caps exponential backoff between retries.
	maxCommitRetryBackoff = 500 * time.Millisecond
	// maxResolveAttempts bounds prewrite/read conflict resolution loops.
	maxResolveAttempts = 8
)

// TxnOptions tunes conflict waiting and post-commit retry for one transaction.
// The zero value keeps the historical behavior: conflicts abort immediately and
// secondary commits are retried for up to five seconds.
type TxnOptions struct {
	// LockWaitTimeout bounds how long a Prewrite or Get waits for a conflicting
	// lock to be resolved (including cross-range resolution through the
	// primary lock). Zero means do not wait: an unresolved lock is reported
	// immediately with wait-for diagnostics.
	LockWaitTimeout time.Duration
	// CommitRetryTimeout bounds retries of a range commit after the primary
	// commit point. Zero means defaultCommitRetryTimeout. The effective budget
	// is additionally capped by the caller's context deadline.
	CommitRetryTimeout time.Duration
	// RetryBackoff is the initial delay between retries. Zero means 50ms; it
	// doubles up to 500ms.
	RetryBackoff time.Duration
}

func (o TxnOptions) retryBackoff() time.Duration {
	if o.RetryBackoff > 0 {
		return o.RetryBackoff
	}
	return backoff
}

func (o TxnOptions) commitRetryTimeout() time.Duration {
	if o.CommitRetryTimeout <= 0 {
		return defaultCommitRetryTimeout
	}
	if o.CommitRetryTimeout > maxCommitRetryTimeout {
		return maxCommitRetryTimeout
	}
	return o.CommitRetryTimeout
}

// WaitFor is one lock that blocked a transaction, reported by LockWaitError.
type WaitFor struct {
	Key         []byte
	Primary     []byte
	LockStartTS uint64
	Attempts    int
	Waited      time.Duration
	Reason      string
}

func (w WaitFor) String() string {
	return fmt.Sprintf("key=%q lock_ts=%d primary=%q attempts=%d waited=%s (%s)",
		w.Key, w.LockStartTS, w.Primary, w.Attempts, w.Waited.Round(time.Millisecond), w.Reason)
}

// LockWaitError reports a conflict that did not clear within the bounded wait
// budget. It is a bounded alternative to spinning on a lock forever.
type LockWaitError struct {
	Keys    []WaitFor
	Timeout time.Duration
	Cause   error
}

func (e *LockWaitError) Error() string {
	var b strings.Builder
	if e.Timeout > 0 {
		fmt.Fprintf(&b, "%s after %s", ErrLockWaitTimeout.Error(), e.Timeout)
	} else {
		fmt.Fprintf(&b, "%s: lock is still held", ErrConflict.Error())
	}
	for _, w := range e.Keys {
		b.WriteString("; wait-for: ")
		b.WriteString(w.String())
	}
	if e.Cause != nil {
		b.WriteString("; last error: ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

// Unwrap lets callers use errors.Is(err, ErrConflict); a waited conflict also
// matches ErrLockWaitTimeout.
func (e *LockWaitError) Unwrap() []error {
	if e.Timeout > 0 {
		return []error{ErrConflict, ErrLockWaitTimeout}
	}
	return []error{ErrConflict}
}

// CommitError reports a cross-range commit failure. OutcomeUnknown is set when
// the primary range's commit result could not be determined; otherwise the
// transaction reached its commit point and only secondary ranges are missing.
type CommitError struct {
	StartTS        uint64
	CommitTS       uint64
	Primary        []byte
	PrimaryRange   uint64
	FailedRange    uint64
	Keys           [][]byte
	Attempts       int
	CommitPoint    bool
	OutcomeUnknown bool
	Cause          error
}

func (e *CommitError) Error() string {
	var b strings.Builder
	if e.OutcomeUnknown {
		fmt.Fprintf(&b, "%s: start_ts=%d primary_range=%d failed_range=%d attempts=%d",
			ErrCommitUnknown.Error(), e.StartTS, e.PrimaryRange, e.FailedRange, e.Attempts)
	} else {
		fmt.Fprintf(&b, "%s: start_ts=%d commit_ts=%d primary_range=%d failed_range=%d attempts=%d",
			ErrCommitIncomplete.Error(), e.StartTS, e.CommitTS, e.PrimaryRange, e.FailedRange, e.Attempts)
	}
	b.WriteString("; do not roll back; retry Commit to finish secondary ranges")
	if len(e.Keys) > 0 {
		fmt.Fprintf(&b, "; keys=%q", e.Keys)
	}
	if e.Cause != nil {
		b.WriteString("; last error: ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

func (e *CommitError) Unwrap() error {
	if e.OutcomeUnknown {
		return ErrCommitUnknown
	}
	return ErrCommitIncomplete
}

// Client is a transactional v2 KV client. It is safe for concurrent use.
type Client struct {
	// KV node connections and leader hints.
	mu      sync.Mutex
	clients map[string]pbv2.KVServiceClient
	conns   map[string]*grpc.ClientConn
	leaders map[uint64]leaderEntry // range ID -> cached leader
	closed  bool

	addrs []string
	// adminToken is sent as a Bearer credential on meta admin RPCs only. It
	// is empty when the meta server has the check disabled (development) or
	// when the client only performs read-only/data work.
	adminToken string
	// transportCreds is the credentials used for KV node connections. It is
	// nil only for a Client built directly by tests, which keeps the plaintext
	// default; New/NewWithOptions/NewWithTLS always set it.
	transportCreds credentials.TransportCredentials
	// dialOpts are applied to KV node connections. They are useful for
	// custom timeouts, test interceptors and advanced transport settings;
	// transport security itself is set by Options.TLS or NewWithTLS.
	dialOpts []grpc.DialOption
	// protocol records the negotiated protocol version of every meta and KV
	// peer. It is populated by the handshake interceptors in protocolOpts.
	protocol *version.Registry
	// protocolOpts attach the version handshake to every dial. They are kept
	// separate from dialOpts so public Options.DialOptions stays exactly what
	// the caller supplied.
	protocolOpts []grpc.DialOption

	// Meta service connection pool and leader discovery. Connections are
	// pooled by address so repeated failover does not create a new gRPC
	// connection per call.
	metaMu          sync.Mutex
	meta            metaAPI
	metaAddr        string
	metaSeeds       []string
	metaPool        map[string]*metaEntry
	metaDial        func(string) (*grpc.ClientConn, metaAPI, error)
	metaLeaderAddr  string
	metaLeaderUntil time.Time
	metaGen         uint64
	metaClosed      bool

	// TSO batch. All fields are guarded by tso.mu.
	tso             tsoBatcher
	tsBatchSize     int32
	tsBatchTTL      time.Duration
	tsIdleCheck     time.Duration
	tsValidateEvery uint64

	// Range descriptor cache.
	rangeMu  sync.Mutex
	ranges   map[uint64]*rangeEntry
	rangeSeq uint64
	rangeTTL time.Duration
	rangeCap int
}

type leaderEntry struct {
	addr  string
	until time.Time
}

// Options configures a Client.
//
// TLS, when non-nil, secures every connection the client opens: the initial
// meta connection, every meta failover dial and every KV node dial. A nil TLS
// keeps the historical plaintext transport, which is a development default
// and not suitable for production.
//
// When TLS is set there is no plaintext fallback: an invalid CA, certificate
// or key makes the constructor fail, and an unverifiable server makes the RPC
// fail.
type Options struct {
	// TLS enables TLS (and mTLS when a client certificate is set) for both
	// meta and KV dials.
	TLS *grpctls.ClientOptions
	// DialOptions are extra gRPC options applied to KV node connections in
	// addition to the transport credentials. They are useful for keepalive
	// settings and test interceptors.
	DialOptions []grpc.DialOption
	// AdminToken is sent as "authorization: Bearer <token>" on meta admin
	// RPCs (SetRanges, SplitRange, MergeRanges). It is required when the meta
	// server runs with metaserver.Config.AdminToken set and is empty for
	// development clusters with the check disabled.
	//
	// Only admin RPCs carry the token; data-plane operations and the
	// read-only meta RPCs (ListRanges, GetRange, AllocTS, Leader) never send
	// it. The token is not included in errors or logs.
	AdminToken string
}

// New connects to the meta service and one or more KV nodes over plaintext. It
// does not dial KV nodes until the first operation. Use NewWithTLS or
// NewWithOptions for a TLS cluster.
func New(metaAddr string, addrs []string) (*Client, error) {
	return NewWithDialOptions(metaAddr, addrs)
}

// NewWithDialOptions is New with additional gRPC dial options applied to KV
// node connections. Meta connections use the default options. It is retained
// for compatibility; new code that needs transport security should use
// NewWithOptions/NewWithTLS so that meta connections are covered too.
func NewWithDialOptions(metaAddr string, addrs []string, opts ...grpc.DialOption) (*Client, error) {
	if metaAddr == "" {
		return nil, errors.New("v2client: meta address is required")
	}
	if len(addrs) == 0 {
		return nil, errors.New("v2client: at least one node address is required")
	}
	return newClient(metaAddr, addrs, nil, "", opts...)
}

// NewWithOptions is New with explicit transport options. When opts.TLS is set,
// both meta and KV connections use it. opts.AdminToken, when set, is sent only
// on meta admin RPCs.
func NewWithOptions(metaAddr string, addrs []string, opts Options) (*Client, error) {
	if metaAddr == "" {
		return nil, errors.New("v2client: meta address is required")
	}
	if len(addrs) == 0 {
		return nil, errors.New("v2client: at least one node address is required")
	}
	return newClient(metaAddr, addrs, opts.TLS, opts.AdminToken, opts.DialOptions...)
}

// NewWithTLS is New with TLS/mTLS enabled for both meta and KV connections.
// The TLS options are validated before any connection is created; an error is
// returned instead of falling back to plaintext.
func NewWithTLS(metaAddr string, addrs []string, clientTLS *grpctls.ClientOptions) (*Client, error) {
	if metaAddr == "" {
		return nil, errors.New("v2client: meta address is required")
	}
	if len(addrs) == 0 {
		return nil, errors.New("v2client: at least one node address is required")
	}
	return newClient(metaAddr, addrs, clientTLS, "")
}

// NewMetaOnly connects only to the meta service over plaintext. It is intended
// for administrative operations that read or change the range map (listing,
// splitting and merging ranges, or asking for the meta leader) where no KV
// node address is needed. Data operations are intended for a client created
// with New.
func NewMetaOnly(metaAddr string) (*Client, error) {
	return newClient(metaAddr, nil, nil, "")
}

// NewMetaOnlyWithTLS is NewMetaOnly with TLS/mTLS for the meta connection.
func NewMetaOnlyWithTLS(metaAddr string, clientTLS *grpctls.ClientOptions) (*Client, error) {
	return newClient(metaAddr, nil, clientTLS, "")
}

// NewMetaOnlyWithOptions is NewMetaOnly with explicit TLS, dial and admin-token
// options. Administrative tools that run against a meta server with
// Config.AdminToken set should use this constructor so that SetRanges,
// SplitRange and MergeRanges carry the Bearer credential.
func NewMetaOnlyWithOptions(metaAddr string, opts Options) (*Client, error) {
	if metaAddr == "" {
		return nil, errors.New("v2client: meta address is required")
	}
	return newClient(metaAddr, nil, opts.TLS, opts.AdminToken, opts.DialOptions...)
}

// newClient builds a Client. clientTLS nil means plaintext; a non-nil value is
// validated and used for every meta and KV dial.
func newClient(metaAddr string, addrs []string, clientTLS *grpctls.ClientOptions, adminToken string, opts ...grpc.DialOption) (*Client, error) {
	if metaAddr == "" {
		return nil, errors.New("v2client: meta address is required")
	}
	transportCreds := insecure.NewCredentials()
	if clientTLS != nil {
		creds, err := grpctls.ClientCredentials(*clientTLS)
		if err != nil {
			return nil, fmt.Errorf("v2client: TLS: %w", err)
		}
		transportCreds = creds
	}
	metaOpts := []grpc.DialOption{grpc.WithTransportCredentials(transportCreds)}
	// The handshake interceptors are added to every meta and KV connection.
	// They only add version metadata; transport security and any caller
	// dial options are untouched.
	protocol := version.NewRegistry()
	handshake := version.NewClientHandshake(protocol)
	protocolOpts := []grpc.DialOption{
		grpc.WithChainUnaryInterceptor(handshake.Unary()),
		grpc.WithChainStreamInterceptor(handshake.Stream()),
	}
	metaOpts = append(metaOpts, protocolOpts...)
	cc, err := grpc.NewClient(metaAddr, metaOpts...)
	if err != nil {
		return nil, fmt.Errorf("v2client: dial meta: %w", err)
	}
	mc := pbv2.NewMetaServiceClient(cc)
	return &Client{
		adminToken: adminToken,
		meta:       mc,
		metaAddr:   metaAddr,
		metaSeeds:  []string{metaAddr},
		metaPool:   map[string]*metaEntry{metaAddr: {cc: cc, mc: mc}},
		metaDial: func(addr string) (*grpc.ClientConn, metaAPI, error) {
			return dialMeta(addr, metaOpts...)
		},
		addrs:           append([]string(nil), addrs...),
		transportCreds:  transportCreds,
		dialOpts:        append([]grpc.DialOption(nil), opts...),
		protocol:        protocol,
		protocolOpts:    protocolOpts,
		clients:         make(map[string]pbv2.KVServiceClient),
		conns:           make(map[string]*grpc.ClientConn),
		leaders:         make(map[uint64]leaderEntry),
		tsBatchSize:     defaultTSOBatchSize,
		tsBatchTTL:      defaultTSOBatchTTL,
		tsIdleCheck:     defaultTSOIdleCheck,
		tsValidateEvery: defaultTSOValidateEvery,
		ranges:          make(map[uint64]*rangeEntry),
		rangeTTL:        defaultRangeTTL,
		rangeCap:        defaultRangeCacheMax,
	}, nil
}

// Close closes all gRPC connections.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	conns := c.conns
	c.conns = nil
	c.clients = nil
	c.mu.Unlock()

	c.metaMu.Lock()
	metaClosed := c.metaClosed
	c.metaClosed = true
	// Invalidating the generation makes any in-flight TSO/range cache entry
	// unusable after Close.
	c.metaGen++
	pool := c.metaPool
	c.metaPool = nil
	c.meta = nil
	c.metaMu.Unlock()
	c.clearRangeCache()

	var errs []error
	for _, cc := range conns {
		errs = append(errs, cc.Close())
	}
	if !metaClosed {
		for _, e := range pool {
			if e != nil && e.cc != nil {
				errs = append(errs, e.cc.Close())
			}
		}
	}
	return errors.Join(errs...)
}

func (c *Client) node(addr string) (pbv2.KVServiceClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errClientClosed
	}
	if cl, ok := c.clients[addr]; ok {
		return cl, nil
	}
	creds := c.transportCreds
	if creds == nil {
		// Only a Client assembled directly by tests has nil credentials; the
		// public constructors always set the configured transport security.
		creds = insecure.NewCredentials()
	}
	opts := make([]grpc.DialOption, 0, len(c.dialOpts)+len(c.protocolOpts)+1)
	opts = append(opts, grpc.WithTransportCredentials(creds))
	opts = append(opts, c.dialOpts...)
	opts = append(opts, c.protocolOpts...)
	cc, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, err
	}
	cl := pbv2.NewKVServiceClient(cc)
	c.conns[addr] = cc
	c.clients[addr] = cl
	return cl, nil
}

func (c *Client) cachedLeader(rangeID uint64) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.leaders[rangeID]; ok && e.addr != "" && time.Now().Before(e.until) {
		return e.addr
	}
	return ""
}

func (c *Client) setLeader(rangeID uint64, addr string) {
	c.mu.Lock()
	c.leaders[rangeID] = leaderEntry{addr: addr, until: time.Now().Add(leaderTTL)}
	c.mu.Unlock()
}

func (c *Client) invalidateLeader(rangeID uint64) {
	c.mu.Lock()
	delete(c.leaders, rangeID)
	c.mu.Unlock()
}

// discoverLeader probes the meta range's replicas (leader hint first) and
// returns the first node that reports leadership, together with the range
// descriptor version used for the probe.
func (c *Client) discoverLeader(ctx context.Context, key []byte) (string, uint64, uint64, error) {
	var candidates []string
	var rangeID, version uint64
	if r, err := c.rangeFor(ctx, key); err == nil {
		rangeID = r.GetId()
		version = r.GetVersion()
		if r.GetLeader() != "" {
			candidates = append(candidates, r.GetLeader())
		}
		candidates = append(candidates, r.GetReplicas()...)
	}
	candidates = append(candidates, c.addrs...)

	seen := make(map[string]struct{}, len(candidates))
	var lastErr error
	for _, addr := range candidates {
		if addr == "" {
			continue
		}
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		cl, err := c.node(addr)
		if err != nil {
			lastErr = err
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		resp, err := cl.Status(probeCtx, &pbv2.StatusRequest{RangeId: rangeID})
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.GetLeader() != "" {
			c.setLeader(rangeID, resp.GetLeader())
			return resp.GetLeader(), rangeID, version, nil
		}
		if resp.GetRole() == "leader" {
			c.setLeader(rangeID, addr)
			return addr, rangeID, version, nil
		}
	}
	if lastErr == nil {
		lastErr = ErrNotLeader
	}
	return "", rangeID, version, fmt.Errorf("v2client: discover leader: %w", lastErr)
}

type callFunc func(ctx context.Context, cl pbv2.KVServiceClient) (leaderHint, errMsg string, err error)

func (c *Client) retry(ctx context.Context, key []byte, call callFunc) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	var rangeID, rangeEpoch uint64
	if r, err := c.rangeFor(ctx, key); err == nil {
		rangeID = r.GetId()
		rangeEpoch = r.GetVersion()
	}
	var lastErr error
	refreshedRange := false
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * backoff):
			}
		}
		addr := c.cachedLeader(rangeID)
		if addr == "" {
			var err error
			var rid, version uint64
			addr, rid, version, err = c.discoverLeader(ctx, key)
			if err != nil {
				lastErr = err
				continue
			}
			if rid != 0 {
				rangeID, rangeEpoch = rid, version
			}
		}
		cl, err := c.node(addr)
		if err != nil {
			lastErr = err
			continue
		}
		callCtx := ctx
		if rangeEpoch != 0 {
			callCtx = metadata.AppendToOutgoingContext(ctx, "x-range-epoch", strconv.FormatUint(rangeEpoch, 10))
		}
		hint, msg, err := call(callCtx, cl)
		if err != nil {
			lastErr = err
			c.invalidateLeader(rangeID)
			continue
		}
		switch msg {
		case "":
			return nil
		case "not leader":
			c.invalidateLeader(rangeID)
			lastErr = ErrNotLeader
			if hint != "" {
				c.setLeader(rangeID, hint)
			}
			continue
		case "range frozen":
			lastErr = ErrConflict
			continue
		case "locked":
			// A live lock is a conflict, not a transport error. Callers that
			// can wait inspect the primary lock and resolve it explicitly.
			return fmt.Errorf("%w: %w", ErrConflict, errServerLocked)
		case "write conflict":
			return fmt.Errorf("%w: %w", ErrConflict, errServerWriteConflict)
		case "already committed":
			return fmt.Errorf("%w: %w", ErrConflict, errServerAlreadyCommitted)
		case "stale epoch", "not hosted":
			// The cached descriptor is no longer valid (split/merge or the
			// range moved). Drop everything for this key and re-resolve it
			// through meta exactly once before retrying the RPC.
			c.invalidateLeader(rangeID)
			c.invalidateRangeForKey(rangeID, key)
			if refreshedRange {
				return fmt.Errorf("v2client: %s", msg)
			}
			refreshedRange = true
			nr, rerr := c.rangeForFresh(ctx, key)
			if rerr != nil {
				return fmt.Errorf("v2client: refresh range after %q: %w", msg, rerr)
			}
			rangeID, rangeEpoch = nr.GetId(), nr.GetVersion()
			lastErr = fmt.Errorf("%s", msg)
			// The meta round-trip already provided the retry opportunity, so
			// it does not consume the attempt budget.
			attempt--
			continue
		default:
			return fmt.Errorf("v2client: %s", msg)
		}
	}
	if lastErr == nil {
		lastErr = ErrNotLeader
	}
	return fmt.Errorf("v2client: %w", lastErr)
}

// KV is one row returned by Txn.Scan.
type KV struct {
	Key   []byte
	Value []byte
}

// CheckTxnStatus inspects the lock on key and reports committed/rolled-back/
// live/expired status. startTS may be zero when the caller wants the lock's
// own timestamp.
func (c *Client) CheckTxnStatus(ctx context.Context, key []byte, startTS uint64) (*pbv2.CheckTxnStatusResponse, error) {
	var out *pbv2.CheckTxnStatusResponse
	if err := c.retry(ctx, key, func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
		resp, err := cl.CheckTxnStatus(ctx, &pbv2.CheckTxnStatusRequest{Key: key, StartTs: startTS})
		if err != nil {
			return "", "", err
		}
		out = resp
		return resp.GetLeader(), resp.GetError(), nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// ResolveLock commits (commitTS > 0) or rolls back (commitTS == 0) a
// transaction's keys.
func (c *Client) ResolveLock(ctx context.Context, keys [][]byte, startTS, commitTS uint64) error {
	if len(keys) == 0 {
		return errors.New("v2client: ResolveLock requires keys")
	}
	return c.retry(ctx, keys[0], func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
		resp, err := cl.ResolveLock(ctx, &pbv2.ResolveLockRequest{Keys: keys, StartTs: startTS, CommitTs: commitTS})
		if err != nil {
			return "", "", err
		}
		return resp.GetLeader(), resp.GetError(), nil
	})
}

// rangeBatch groups mutations that route to one range for cross-range 2PC.
type rangeBatch struct {
	id   uint64
	keys [][]byte
	muts []*pbv2.Mutation
}

// Txn is a single optimistic transaction. It is not safe for concurrent use.
type Txn struct {
	c       *Client
	startTS uint64
	order   []string
	writes  map[string]*pbv2.Mutation
	opts    TxnOptions
	done    bool

	// Cross-range commit state. Once commitStarted is set the plan is frozen
	// and Commit can be retried idempotently at the same commitTS without
	// re-prewriting or rolling back.
	commitStarted   bool
	commitPoint     bool // primary range committed: rollback is forbidden
	commitAmbiguous bool // primary commit result unknown: rollback is forbidden
	commitTS        uint64
	primary         []byte
	ordered         []*rangeBatch
}

// Begin allocates a start timestamp and returns a transaction.
func (c *Client) Begin(ctx context.Context) (*Txn, error) {
	return c.BeginWithOptions(ctx, TxnOptions{})
}

// BeginWithOptions is Begin with per-transaction retry and lock-wait options.
func (c *Client) BeginWithOptions(ctx context.Context, opts TxnOptions) (*Txn, error) {
	ts, err := c.AllocTS(ctx)
	if err != nil {
		return nil, err
	}
	return &Txn{c: c, startTS: ts, writes: make(map[string]*pbv2.Mutation), opts: opts}, nil
}

// StartTS returns the transaction's start timestamp.
func (t *Txn) StartTS() uint64 { return t.startTS }

// CommitTS returns the timestamp chosen for a commit that has started, or 0
// before the commit phase. It is stable across idempotent Commit retries.
func (t *Txn) CommitTS() uint64 { return t.commitTS }

// Set stages a put.
func (t *Txn) Set(key, value []byte) {
	k := string(key)
	if _, ok := t.writes[k]; !ok {
		t.order = append(t.order, k)
	}
	t.writes[k] = &pbv2.Mutation{Key: append([]byte(nil), key...), Value: append([]byte(nil), value...)}
}

// Delete stages a delete.
func (t *Txn) Delete(key []byte) {
	k := string(key)
	if _, ok := t.writes[k]; !ok {
		t.order = append(t.order, k)
	}
	t.writes[k] = &pbv2.Mutation{Key: append([]byte(nil), key...), Delete: true}
}

// Get reads through the transaction snapshot: staged writes first, then the
// committed value at startTS. If the key is blocked by a lock, Get resolves the
// lock through its primary: a committed primary means the secondary lock is
// committed, a rolled-back primary means it is rolled back. When the primary is
// still live, the read waits for at most TxnOptions.LockWaitTimeout and then
// fails with wait-for diagnostics.
func (t *Txn) Get(ctx context.Context, key []byte) ([]byte, error) {
	if m, ok := t.writes[string(key)]; ok {
		if m.GetDelete() {
			return nil, ErrNotFound
		}
		return append([]byte(nil), m.GetValue()...), nil
	}
	wait := t.opts.LockWaitTimeout
	if wait < 0 {
		wait = 0
	}
	deadline := time.Now().Add(wait)
	for attempt := 0; attempt < maxResolveAttempts; attempt++ {
		var out []byte
		var found bool
		var msg string
		err := t.c.retry(ctx, key, func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
			resp, err := cl.Get(ctx, &pbv2.GetRequest{Key: key, Ts: t.startTS})
			if err != nil {
				return "", "", err
			}
			out, found, msg = resp.GetValue(), resp.GetFound(), resp.GetError()
			return resp.GetLeader(), resp.GetError(), nil
		})
		if err == nil {
			if msg != "" {
				return nil, fmt.Errorf("v2client: get: %s", msg)
			}
			if !found {
				return nil, ErrNotFound
			}
			return out, nil
		}
		if !errors.Is(err, errServerLocked) {
			return nil, err
		}
		resolved, wf, rerr := t.c.resolveLockedKey(ctx, key, deadline, t.opts.retryBackoff())
		if rerr != nil {
			return nil, rerr
		}
		if resolved {
			continue
		}
		waits := make([]WaitFor, 0, 1)
		if wf != nil {
			waits = append(waits, *wf)
		}
		return nil, &LockWaitError{Keys: waits, Timeout: wait, Cause: err}
	}
	return nil, &LockWaitError{
		Keys:    []WaitFor{{Key: append([]byte(nil), key...), Attempts: maxResolveAttempts, Reason: "conflict persisted"}},
		Timeout: wait,
		Cause:   ErrConflict,
	}
}

// resolveLockedKey inspects key's lock and tries to resolve it through the
// transaction's primary lock, which lives in the primary range for cross-range
// transactions. It returns resolved=true when the lock is gone (or was
// committed/rolled back by this call), or resolved=false with wait-for
// diagnostics when the primary transaction is still live at deadline.
func (c *Client) resolveLockedKey(ctx context.Context, key []byte, deadline time.Time, backoff time.Duration) (bool, *WaitFor, error) {
	if backoff <= 0 {
		backoff = 50 * time.Millisecond
	}
	start := time.Now()
	attempts := 0
	var last WaitFor
	for {
		attempts++
		st, err := c.CheckTxnStatus(ctx, key, 0)
		if err != nil {
			last = WaitFor{
				Key: append([]byte(nil), key...), Attempts: attempts,
				Waited: time.Since(start), Reason: "check lock status: " + err.Error(),
			}
		} else if st.GetLockTs() == 0 && len(st.GetLockPrimary()) == 0 {
			// No lock remains; a commit/rollback record (if any) is handled by
			// the normal read path.
			return true, nil, nil
		} else {
			lockTS := st.GetLockTs()
			primary := append([]byte(nil), st.GetLockPrimary()...)
			if len(primary) == 0 {
				primary = append([]byte(nil), key...)
			}
			last = WaitFor{
				Key: append([]byte(nil), key...), Primary: primary, LockStartTS: lockTS,
				Attempts: attempts, Waited: time.Since(start),
			}
			pst, perr := c.CheckTxnStatus(ctx, primary, lockTS)
			keys := st.GetKeys()
			if len(keys) == 0 {
				keys = [][]byte{append([]byte(nil), key...)}
			}
			switch {
			case perr != nil:
				last.Reason = "check primary lock: " + perr.Error()
			case pst.GetCommitted():
				if rerr := c.ResolveLock(ctx, keys, lockTS, pst.GetCommitTs()); rerr != nil {
					last.Reason = "resolve committed lock: " + rerr.Error()
				} else {
					return true, nil, nil
				}
			case pst.GetRolledBack():
				if rerr := c.ResolveLock(ctx, keys, lockTS, 0); rerr != nil {
					last.Reason = "resolve rolled-back lock: " + rerr.Error()
				} else {
					return true, nil, nil
				}
			default:
				last.Reason = "primary transaction is still live"
			}
		}
		if !time.Now().Before(deadline) {
			return false, &last, nil
		}
		pause := time.Until(deadline)
		if pause > backoff {
			pause = backoff
		}
		select {
		case <-ctx.Done():
			return false, &last, ctx.Err()
		case <-time.After(pause):
		}
		backoff *= 2
		if backoff > maxCommitRetryBackoff {
			backoff = maxCommitRetryBackoff
		}
	}
}

// Scan reads a range at the transaction snapshot. It is routed by start key.
func (t *Txn) Scan(ctx context.Context, start, end []byte, limit int) ([]KV, error) {
	var rows []KV
	var msg string
	if err := t.c.retry(ctx, start, func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
		resp, err := cl.Scan(ctx, &pbv2.ScanRequest{Start: start, End: end, Ts: t.startTS, Limit: int32(limit)})
		if err != nil {
			return "", "", err
		}
		msg = resp.GetError()
		rows = rows[:0]
		for _, kv := range resp.GetKvs() {
			rows = append(rows, KV{Key: kv.GetKey(), Value: kv.GetValue()})
		}
		return resp.GetLeader(), resp.GetError(), nil
	}); err != nil {
		return nil, err
	}
	if msg != "" {
		return nil, fmt.Errorf("v2client: scan: %s", msg)
	}
	return rows, nil
}

// Commit prewrites all staged mutations and commits them.
//
// A cross-range transaction uses primary-first 2PC: the primary range is
// prewritten first, the remaining ranges are prewritten concurrently, the
// primary range commits first, and then every secondary range commits
// concurrently. Once the primary range's commit succeeds the transaction is
// durable (the commit point) and Commit will never roll back secondary ranges:
// if a secondary fails, Commit returns an error matching ErrCommitIncomplete
// and a later Commit call retries the same commitTS idempotently. If the
// primary's commit outcome cannot be determined, the error matches
// ErrCommitUnknown and Rollback is refused for the same reason.
func (t *Txn) Commit(ctx context.Context) error {
	if t.done {
		return errors.New("v2client: transaction is finished")
	}
	if t.commitStarted {
		return t.finishCommit(ctx)
	}
	if len(t.writes) == 0 {
		t.done = true
		return nil
	}
	keys := make([]string, 0, len(t.order))
	for _, k := range t.order {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	primary := []byte(keys[0])
	mutations := make([]*pbv2.Mutation, 0, len(keys))
	for _, k := range keys {
		mutations = append(mutations, t.writes[k])
	}

	// Group mutations by range. A single-range transaction uses 1PC; a
	// cross-range transaction uses primary-first 2PC: prewrite the primary
	// range first and the rest concurrently, then commit the primary's range
	// first (so a reader can resolve secondary locks by inspecting the
	// primary) before committing the remaining ranges concurrently.
	batches := make([]*rangeBatch, 0, 2)
	byRange := make(map[uint64]*rangeBatch)
	for i, k := range keys {
		ri, err := t.c.rangeFor(ctx, []byte(k))
		if err != nil {
			return err
		}
		b := byRange[ri.GetId()]
		if b == nil {
			b = &rangeBatch{id: ri.GetId()}
			byRange[ri.GetId()] = b
			batches = append(batches, b)
		}
		b.keys = append(b.keys, []byte(k))
		b.muts = append(b.muts, mutations[i])
	}

	commitTS, err := t.c.AllocTS(ctx)
	if err != nil {
		return err
	}
	for commitTS <= t.startTS {
		commitTS, err = t.c.AllocTS(ctx)
		if err != nil {
			return err
		}
	}

	if len(batches) == 1 {
		var commitMsg string
		err := t.c.retry(ctx, primary, func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
			resp, err := cl.CommitSingle(ctx, &pbv2.CommitSingleRequest{
				StartTs: t.startTS, CommitTs: commitTS, Primary: primary, Mutations: mutations,
			})
			if err != nil {
				return "", "", err
			}
			commitMsg = resp.GetError()
			return resp.GetLeader(), resp.GetError(), nil
		})
		if err != nil && errors.Is(err, errServerWriteConflict) {
			// A retry after a lost success response reports a write conflict
			// because the transaction is already committed. Confirm through
			// the committed record before surfacing an error.
			if st, serr := t.c.CheckTxnStatus(ctx, primary, t.startTS); serr == nil && st.GetCommitted() {
				t.commitTS = st.GetCommitTs()
				t.done = true
				return nil
			}
		}
		if err != nil {
			return err
		}
		if commitMsg != "" {
			return fmt.Errorf("v2client: commit: %s", commitMsg)
		}
		t.commitTS = commitTS
		t.done = true
		return nil
	}

	// Put the primary's range first for both prewrite and commit.
	primaryRange, err := t.c.rangeFor(ctx, primary)
	if err != nil {
		return err
	}
	ordered := make([]*rangeBatch, 0, len(batches))
	for _, b := range batches {
		if b.id == primaryRange.GetId() {
			ordered = append(ordered, b)
		}
	}
	for _, b := range batches {
		if b.id != primaryRange.GetId() {
			ordered = append(ordered, b)
		}
	}

	// Prewrite the primary range first: its lock must exist before any
	// secondary commit can be attempted. Then prewrite every secondary range
	// concurrently, waiting out conflicting locks up to LockWaitTimeout. Any
	// failure before the commit point rolls back every range that was
	// successfully prewritten, including concurrent results.
	prewritten := make([]*rangeBatch, 0, len(ordered))
	if err := t.prewriteBatch(ctx, ordered[0], primary); err != nil {
		return err
	}
	prewritten = append(prewritten, ordered[0])
	if err := t.prewriteSecondaries(ctx, ordered[1:], primary, &prewritten); err != nil {
		t.rollbackBatches(ctx, prewritten)
		return err
	}

	// Freeze the plan before the first commit RPC. From this point on a retry
	// reuses the same startTS/commitTS and only replays idempotent commits.
	t.commitStarted = true
	t.commitTS = commitTS
	t.primary = primary
	t.ordered = ordered
	return t.finishCommit(ctx)
}

// prewriteBatch prewrites one range, resolving (or waiting for) conflicting
// locks through the transaction's primary lock.
func (t *Txn) prewriteBatch(ctx context.Context, b *rangeBatch, primary []byte) error {
	wait := t.opts.LockWaitTimeout
	if wait < 0 {
		wait = 0
	}
	deadline := time.Now().Add(wait)
	var last WaitFor
	for attempt := 0; attempt < maxResolveAttempts; attempt++ {
		var msg string
		err := t.c.retry(ctx, b.keys[0], func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
			resp, err := cl.Prewrite(ctx, &pbv2.PrewriteRequest{
				StartTs: t.startTS, Primary: primary, Mutations: b.muts,
			})
			if err != nil {
				return "", "", err
			}
			msg = resp.GetError()
			return resp.GetLeader(), resp.GetError(), nil
		})
		if err == nil && msg == "" {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("%s", msg)
		}
		if !errors.Is(err, errServerLocked) {
			return fmt.Errorf("v2client: prewrite range %d: %w", b.id, err)
		}
		resolved, wf, rerr := t.c.resolveLockedKey(ctx, b.keys[0], deadline, t.opts.retryBackoff())
		if wf != nil {
			last = *wf
		}
		if rerr != nil {
			return fmt.Errorf("v2client: prewrite range %d: resolve lock: %w", b.id, rerr)
		}
		if resolved {
			continue
		}
		return &LockWaitError{Keys: []WaitFor{last}, Timeout: wait, Cause: err}
	}
	return &LockWaitError{Keys: []WaitFor{last}, Timeout: wait, Cause: ErrConflict}
}

// prewriteSecondaries prewrites every secondary range concurrently. The primary
// range is deliberately not included: it was already prewritten, so its lock
// exists before any secondary commit can be attempted. Every successful result
// is appended to prewritten even when a sibling fails, so the caller can roll
// the whole set back. The returned error is the first failure in plan order,
// which keeps error reporting deterministic.
func (t *Txn) prewriteSecondaries(ctx context.Context, batches []*rangeBatch, primary []byte, prewritten *[]*rangeBatch) error {
	if len(batches) == 0 {
		return nil
	}
	errs := make([]error, len(batches))
	var wg sync.WaitGroup
	wg.Add(len(batches))
	for i, b := range batches {
		go func(i int, b *rangeBatch) {
			defer wg.Done()
			errs[i] = t.prewriteBatch(ctx, b, primary)
		}(i, b)
	}
	wg.Wait()

	var firstErr error
	for i, b := range batches {
		if errs[i] != nil {
			if firstErr == nil {
				firstErr = errs[i]
			}
			continue
		}
		*prewritten = append(*prewritten, b)
	}
	return firstErr
}

// finishCommit drives the frozen cross-range plan. The primary range commits
// first and determines the transaction outcome. Only once the commit point is
// established are secondary ranges committed, all of them concurrently and each
// with bounded retries. It is safe to call more than once: each range commit is
// idempotent at the stored commitTS.
func (t *Txn) finishCommit(ctx context.Context) error {
	deadline := time.Now().Add(t.opts.commitRetryTimeout())
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	if !t.commitPoint {
		b := t.ordered[0]
		attempts, err := t.commitBatch(ctx, b, deadline)
		if err == nil {
			t.commitPoint = true
		} else {
			// The primary commit failed. Determine whether it nevertheless
			// became durable; if that cannot be established, rollback would
			// be unsafe. No secondary commit may start before the primary's
			// outcome is known to be committed.
			st, serr := t.c.CheckTxnStatus(ctx, t.primary, t.startTS)
			switch {
			case serr == nil && st.GetCommitted():
				t.commitPoint = true
				t.commitAmbiguous = false
			case serr == nil && st.GetRolledBack() && st.GetLockTs() == 0:
				return fmt.Errorf("v2client: commit primary range %d: %w", b.id, err)
			default:
				t.commitAmbiguous = true
				return &CommitError{
					StartTS: t.startTS, CommitTS: t.commitTS, Primary: t.primary,
					PrimaryRange: b.id, FailedRange: b.id, Keys: b.keys,
					Attempts: attempts, OutcomeUnknown: true, Cause: err,
				}
			}
		}
	}

	if err := t.commitSecondaries(ctx, deadline); err != nil {
		return err
	}
	t.done = true
	return nil
}

// commitSecondaries commits every secondary range concurrently, each bounded by
// the same deadline. All results are collected before returning: after the
// commit point a failure in one range must never cancel an in-flight commit in
// another. If any range remains uncommitted, the returned *CommitError matches
// ErrCommitIncomplete and reports the first failed range in plan order (with all
// secondary causes joined) so a later Commit can replay the plan idempotently.
func (t *Txn) commitSecondaries(ctx context.Context, deadline time.Time) error {
	batches := t.ordered[1:]
	if len(batches) == 0 {
		return nil
	}
	type result struct {
		batch    *rangeBatch
		attempts int
		err      error
	}
	results := make([]result, len(batches))
	var wg sync.WaitGroup
	wg.Add(len(batches))
	for i, b := range batches {
		go func(i int, b *rangeBatch) {
			defer wg.Done()
			attempts, err := t.commitBatch(ctx, b, deadline)
			results[i] = result{batch: b, attempts: attempts, err: err}
		}(i, b)
	}
	wg.Wait()

	var first *result
	var causes []error
	for i := range results {
		if results[i].err == nil {
			continue
		}
		if first == nil {
			first = &results[i]
		}
		causes = append(causes, results[i].err)
	}
	if first == nil {
		return nil
	}
	cause := first.err
	if len(causes) > 1 {
		cause = errors.Join(causes...)
	}
	return &CommitError{
		StartTS: t.startTS, CommitTS: t.commitTS, Primary: t.primary,
		PrimaryRange: t.ordered[0].id, FailedRange: first.batch.id, Keys: first.batch.keys,
		Attempts: first.attempts, CommitPoint: true, Cause: cause,
	}
}

// commitBatch commits one range, retrying transient failures until deadline.
// Conflict errors are permanent. Every retry uses the same commitTS, so a range
// that already applied the commit treats the replay as a no-op.
func (t *Txn) commitBatch(ctx context.Context, b *rangeBatch, deadline time.Time) (int, error) {
	attempts := 0
	wait := t.opts.retryBackoff()
	var lastErr error
	for {
		attempts++
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if lastErr == nil {
				lastErr = context.DeadlineExceeded
			}
			return attempts, lastErr
		}
		callCtx, cancel := context.WithTimeout(ctx, remaining)
		var msg string
		err := t.c.retry(callCtx, b.keys[0], func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
			resp, err := cl.Commit(ctx, &pbv2.CommitRequest{
				StartTs: t.startTS, CommitTs: t.commitTS, Keys: b.keys,
			})
			if err != nil {
				return "", "", err
			}
			msg = resp.GetError()
			return resp.GetLeader(), resp.GetError(), nil
		})
		cancel()
		if err == nil && msg == "" {
			return attempts, nil
		}
		if err == nil {
			err = fmt.Errorf("v2client: commit range %d: %s", b.id, msg)
		}
		lastErr = err
		if errors.Is(err, ErrConflict) || ctx.Err() != nil {
			return attempts, err
		}
		if !time.Now().Before(deadline) {
			return attempts, lastErr
		}
		pause := wait
		if d := time.Until(deadline); d < pause {
			pause = d
		}
		select {
		case <-ctx.Done():
			return attempts, ctx.Err()
		case <-time.After(pause):
		}
		wait *= 2
		if wait > maxCommitRetryBackoff {
			wait = maxCommitRetryBackoff
		}
	}
}

// rollbackKeys rolls back one range's keys with the transaction's startTS.
func (c *Client) rollbackKeys(ctx context.Context, keys [][]byte, startTS uint64) error {
	var msg string
	if err := c.retry(ctx, keys[0], func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
		resp, err := cl.Rollback(ctx, &pbv2.RollbackRequest{StartTs: startTS, Keys: keys})
		if err != nil {
			return "", "", err
		}
		msg = resp.GetError()
		return resp.GetLeader(), resp.GetError(), nil
	}); err != nil {
		return err
	}
	if msg != "" {
		return fmt.Errorf("v2client: rollback: %s", msg)
	}
	return nil
}

// rollbackBatches is a best-effort cleanup after a failed cross-range
// prewrite. A rollback failure leaves the lock for the background resolver.
func (t *Txn) rollbackBatches(ctx context.Context, batches []*rangeBatch) {
	for _, b := range batches {
		_ = t.c.rollbackKeys(ctx, b.keys, t.startTS)
	}
}

// Rollback aborts the transaction. It is idempotent. After the primary range's
// commit has succeeded (or may have succeeded) Rollback refuses to run: rolling
// back secondary ranges would make the transaction partially visible.
func (t *Txn) Rollback(ctx context.Context) error {
	if t.done || len(t.writes) == 0 {
		t.done = true
		return nil
	}
	if t.commitPoint {
		return fmt.Errorf("%w: start_ts=%d already reached its commit point; retry Commit instead of Rollback", ErrCommitIncomplete, t.startTS)
	}
	if t.commitAmbiguous {
		return fmt.Errorf("%w: start_ts=%d primary commit outcome is unknown; retry Commit instead of Rollback", ErrCommitUnknown, t.startTS)
	}
	keys := make([][]byte, 0, len(t.order))
	for _, k := range t.order {
		keys = append(keys, []byte(k))
	}
	var msg string
	if err := t.c.retry(ctx, keys[0], func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
		resp, err := cl.Rollback(ctx, &pbv2.RollbackRequest{StartTs: t.startTS, Keys: keys})
		if err != nil {
			return "", "", err
		}
		msg = resp.GetError()
		return resp.GetLeader(), resp.GetError(), nil
	}); err != nil {
		return err
	}
	if msg != "" {
		return fmt.Errorf("v2client: rollback: %s", msg)
	}
	t.done = true
	return nil
}
