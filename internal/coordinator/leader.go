package coordinator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"distributed-db/internal/pb"
)

const (
	// leaderCacheTTL is how long a discovered leader is trusted before the
	// coordinator asks the shard members again.
	leaderCacheTTL = 2 * time.Second
	// maxLeaderAttempts is the total number of operation attempts: the first
	// try plus up to three retries.
	maxLeaderAttempts = 4
	// leaderRetryBackoff is multiplied by the retry number: 25ms, 50ms, 75ms.
	leaderRetryBackoff = 25 * time.Millisecond
	// requestTimeout bounds one proxied operation, retries included.
	requestTimeout = 5 * time.Second
	// leaderProbeTimeout bounds a single Status probe during discovery.
	leaderProbeTimeout = 1 * time.Second
)

// cachedLeader is a leader address plus the instant it stops being trusted.
type cachedLeader struct {
	addr      string
	expiresAt time.Time
}

// notLeaderError reports that no shard member accepted the operation as
// leader. leader is the last leader hint reported by a node, if any.
type notLeaderError struct {
	leader string
}

func (e *notLeaderError) Error() string {
	if e.leader != "" {
		return fmt.Sprintf("coordinator: no leader available (last known leader %s)", e.leader)
	}
	return "coordinator: no leader available"
}

// Leader returns the last known leader address, or "".
func (e *notLeaderError) Leader() string { return e.leader }

// rpcCall performs one node RPC. It returns the leader hint and the error
// string carried by the response (both empty on success).
type rpcCall func(ctx context.Context, client pb.NodeServiceClient, addr string) (leaderHint, errMsg string, err error)

// leaderFor returns the cached leader of shard if the cache entry is fresh,
// otherwise it discovers the leader and caches it.
func (c *Coordinator) leaderFor(ctx context.Context, shard string) (string, error) {
	c.leadersMu.Lock()
	if entry, ok := c.leaders[shard]; ok && time.Now().Before(entry.expiresAt) {
		addr := entry.addr
		c.leadersMu.Unlock()
		return addr, nil
	}
	c.leadersMu.Unlock()

	addr, err := c.discoverLeader(ctx, shard)
	if err != nil {
		return "", err
	}
	c.leadersMu.Lock()
	c.leaders[shard] = cachedLeader{addr: addr, expiresAt: time.Now().Add(leaderCacheTTL)}
	c.leadersMu.Unlock()
	return addr, nil
}

// discoverLeader probes the shard members with NodeService.Status and returns
// the first leader address reported. A node that reports no leader but whose
// own role is "leader" is used directly.
func (c *Coordinator) discoverLeader(ctx context.Context, shard string) (string, error) {
	members := c.shards[shard]
	if len(members) == 0 {
		return "", fmt.Errorf("coordinator: shard %q has no members", shard)
	}
	var lastErr error
	for _, addr := range members {
		client, err := c.nodeClient(addr)
		if err != nil {
			lastErr = err
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, leaderProbeTimeout)
		resp, err := client.Status(probeCtx, &pb.StatusRequest{})
		cancel()
		if err != nil {
			lastErr = fmt.Errorf("coordinator: status %s: %w", addr, err)
			continue
		}
		if leader := resp.GetLeader(); leader != "" {
			return leader, nil
		}
		if strings.EqualFold(resp.GetRole(), "leader") {
			return addr, nil
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no member reported a leader")
	}
	return "", fmt.Errorf("coordinator: discover leader for shard %q: %w", shard, lastErr)
}

// invalidateLeader drops the cached leader of shard, forcing rediscovery on
// the next attempt.
func (c *Coordinator) invalidateLeader(shard string) {
	c.leadersMu.Lock()
	delete(c.leaders, shard)
	c.leadersMu.Unlock()
}

// cachedLeaders returns a copy of the unexpired leader cache.
func (c *Coordinator) cachedLeaders() map[string]string {
	now := time.Now()
	out := make(map[string]string)
	c.leadersMu.Lock()
	for shard, entry := range c.leaders {
		if now.Before(entry.expiresAt) {
			out[shard] = entry.addr
		}
	}
	c.leadersMu.Unlock()
	return out
}

// nodeClient returns a client for addr. Tests replace c.newNodeClient to
// inject fakes; production uses the connection pool.
func (c *Coordinator) nodeClient(addr string) (pb.NodeServiceClient, error) {
	return c.newNodeClient(addr)
}

// dialNodeClient returns a client backed by a pooled, lazily dialed connection.
func (c *Coordinator) dialNodeClient(addr string) (pb.NodeServiceClient, error) {
	cc, err := c.conns.get(addr)
	if err != nil {
		return nil, err
	}
	return pb.NewNodeServiceClient(cc), nil
}

// proxy runs call against the leader of key's shard. On an RPC error or a
// not-leader response it invalidates the cached leader and retries, up to
// maxLeaderAttempts attempts in total. It returns the address of the node that
// accepted the call.
func (c *Coordinator) proxy(ctx context.Context, op, key string, call rpcCall) (string, error) {
	shard, err := c.shardForKey(key)
	if err != nil {
		return "", err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, requestTimeout)
		defer cancel()
	}

	var lastErr error
	var lastHint string
	var notLeader bool

	for attempt := 0; attempt < maxLeaderAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, time.Duration(attempt)*leaderRetryBackoff); err != nil {
				return "", err
			}
		}

		leader, err := c.leaderFor(ctx, shard)
		if err != nil {
			lastErr = err
			lastHint = ""
			notLeader = false
			continue
		}

		client, err := c.nodeClient(leader)
		if err != nil {
			c.invalidateLeader(shard)
			lastErr = err
			lastHint = ""
			notLeader = false
			continue
		}

		hint, errMsg, err := call(ctx, client, leader)
		if err != nil {
			c.invalidateLeader(shard)
			lastErr = fmt.Errorf("coordinator: %s on %s: %w", op, leader, err)
			lastHint = ""
			notLeader = false
			continue
		}
		if isNotLeader(errMsg, hint, leader) {
			c.invalidateLeader(shard)
			lastHint = hint
			notLeader = true
			lastErr = fmt.Errorf("coordinator: node %s is not leader: %s", leader, errMsg)
			c.logger.Debug("not-leader response, retrying",
				"op", op, "shard", shard, "node", leader, "leader_hint", hint,
				"attempt", attempt+1, "error", errMsg)
			continue
		}
		return leader, nil
	}

	if notLeader {
		return lastHint, &notLeaderError{leader: lastHint}
	}
	if lastErr == nil {
		lastErr = errors.New("operation was not attempted")
	}
	return "", fmt.Errorf("coordinator: %s key %q (shard %q): %w", op, key, shard, lastErr)
}

// isNotLeader reports whether a node response means "ask the leader". A
// response is treated as a redirect when the error text says so, or when it
// carries a non-empty error together with a leader address different from the
// node that was called.
func isNotLeader(errMsg, leaderHint, calledAddr string) bool {
	lower := strings.ToLower(errMsg)
	if strings.Contains(lower, "not leader") ||
		strings.Contains(lower, "notleader") ||
		strings.Contains(lower, "not_leader") ||
		strings.Contains(lower, "not the leader") {
		return true
	}
	return errMsg != "" && leaderHint != "" && !sameAddr(leaderHint, calledAddr)
}

// sameAddr compares two node addresses, tolerating URL prefixes, a trailing
// slash and letter case.
func sameAddr(a, b string) bool {
	return normalizeAddr(a) == normalizeAddr(b)
}

func normalizeAddr(addr string) string {
	addr = strings.TrimSpace(strings.ToLower(addr))
	addr = strings.TrimPrefix(addr, "http://")
	addr = strings.TrimPrefix(addr, "https://")
	addr = strings.TrimSuffix(addr, "/")
	return addr
}

// sleepCtx sleeps for d unless ctx is done first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// isNotFoundMessage reports whether an upstream error string means the key
// does not exist.
func isNotFoundMessage(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "not found") ||
		strings.Contains(lower, "no such key") ||
		strings.Contains(lower, "does not exist")
}

// resultLabel maps a proxy error to a bounded metric label.
func resultLabel(err error) string {
	if err == nil {
		return "ok"
	}
	var nl *notLeaderError
	if errors.As(err, &nl) {
		return "not_leader"
	}
	if errors.Is(err, errNoShards) {
		return "no_shard"
	}
	if isNotFoundMessage(err.Error()) {
		return "not_found"
	}
	return "error"
}
