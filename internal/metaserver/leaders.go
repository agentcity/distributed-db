package metaserver

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"distributed-db/internal/meta"
)

const (
	// leaderRefreshInterval throttles the best-effort refresh performed by
	// ListRanges. Leader hints are advisory, so a stale value for a couple of
	// seconds is preferable to probing KV replicas on every read.
	leaderRefreshInterval = 2 * time.Second
	// leaderRefreshBudget bounds one whole ListRanges refresh, including all
	// ranges and replicas. An unreachable node must never stall the CLI.
	leaderRefreshBudget = 1500 * time.Millisecond
	// leaderProbeTimeout bounds one Status RPC. A live local replica answers
	// in well under this; a dead address fails within it.
	leaderProbeTimeout = 400 * time.Millisecond
	// leaderRefreshWorkers caps concurrent range probes.
	leaderRefreshWorkers = 8
)

// rememberLeaderHint records a leader learned from a live KV Status response or
// a leader redirect. epoch is the descriptor version observed before the probe;
// RefreshLeader rejects and clears a hint that belongs to an older version.
func (s *Server) rememberLeaderHint(id uint64, leader string, epoch uint64) {
	if leader == "" {
		_ = s.cluster.ClearLeader(id)
		return
	}
	// A version changed while the probe was in flight: the result describes
	// the old descriptor and is not safe to publish.
	_ = s.cluster.RefreshLeader(id, leader, epoch)
}

// RefreshRangeLeaders probes every range that has replicas and records the
// leader reported by KVService.Status. It is the explicit refresh path used by
// tests and by callers that want an up-to-date hint immediately. Unreachable
// replicas and ranges without a visible leader are left unknown: no leader is
// invented.
func (s *Server) RefreshRangeLeaders(ctx context.Context) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return s.refreshLeaders(ctx, s.cluster.Snapshot())
}

// refreshLeaders probes the supplied descriptors concurrently and records the
// observed leaders. It returns the number of hints updated.
func (s *Server) refreshLeaders(ctx context.Context, rows []meta.Range) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		updated int
		errs    []error
		sem     = make(chan struct{}, leaderRefreshWorkers)
	)
	for i := range rows {
		r := rows[i]
		if len(r.Replicas) == 0 {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(r meta.Range) {
			defer wg.Done()
			defer func() { <-sem }()

			leader, err := s.probeLiveLeader(ctx, r)
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			if leader == "" {
				return
			}
			if err := s.cluster.RefreshLeader(r.ID, leader, r.Version); err != nil {
				// The range descriptor changed while the probe was running.
				// The hint belongs to an older epoch; RefreshLeader already
				// cleared it. This is expected drift, not a refresh failure.
				return
			}
			mu.Lock()
			updated++
			mu.Unlock()
		}(r)
	}
	wg.Wait()
	return updated, errors.Join(errs...)
}

// maybeRefreshRangeLeaders performs a throttled refresh for ranges whose leader
// hint is missing or stale. ListRanges calls it after taking the meta read
// barrier, so the hint it publishes is at least as fresh as the map it returns.
func (s *Server) maybeRefreshRangeLeaders(ctx context.Context) {
	rows := s.cluster.Snapshot()
	var missing []meta.Range
	for i := range rows {
		r := rows[i]
		if len(r.Replicas) == 0 {
			continue
		}
		if r.Leader == "" || r.LeaderStale() {
			missing = append(missing, r)
		}
	}
	if len(missing) == 0 {
		return
	}

	s.leaderMu.Lock()
	if time.Since(s.lastLeaderRefresh) < leaderRefreshInterval {
		s.leaderMu.Unlock()
		return
	}
	s.lastLeaderRefresh = time.Now()
	s.leaderMu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, leaderRefreshBudget)
	defer cancel()
	_, _ = s.refreshLeaders(probeCtx, missing)
}

// probeLiveLeader asks the range replicas for their Status and returns the
// first node that reports role=leader. When no replica claims leadership, a
// follower's leader hint that names a current replica is used as a fallback.
// The descriptor's recorded hint is probed first. An error means no replica
// could be reached or none reported a leader; the caller must not fabricate
// one.
func (s *Server) probeLiveLeader(ctx context.Context, r meta.Range) (string, error) {
	if len(r.Replicas) == 0 {
		return "", errors.New("range has no replicas")
	}
	order := replicasStartingWith(r.Leader, r.Replicas)
	var (
		lastErr error
		hint    string
		seen    = make(map[string]struct{}, len(order))
	)
	for _, addr := range order {
		if addr == "" {
			continue
		}
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}

		probeCtx, cancel := context.WithTimeout(ctx, leaderProbeTimeout)
		resp, err := s.callRangeStatus(probeCtx, addr, r.ID)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		if strings.EqualFold(resp.GetRole(), "leader") {
			return addr, nil
		}
		if hint == "" && resp.GetLeader() != "" && slices.Contains(r.Replicas, resp.GetLeader()) {
			hint = resp.GetLeader()
		}
	}
	if hint != "" {
		return hint, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no replica reported a leader")
	}
	return "", lastErr
}
