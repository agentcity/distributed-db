package metaserver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"distributed-db/internal/meta"
	pbv2 "distributed-db/internal/pb/v2"
)

// Leader rebalancing timeouts. Status probes are advisory and short; a
// TransferLeader call has to wait for the target's bounded catch-up window, so
// it gets the same budget as the other scheduler mutations.
const (
	leaderRebalanceStatusTimeout   = 5 * time.Second
	leaderRebalanceTransferTimeout = 30 * time.Second
)

// RebalanceLeaders performs one leader-rebalancing pass and returns the number
// of ranges whose leadership was moved. The caller must be the meta leader.
//
// The range map is snapshotted once, then each range's current leader is
// discovered through KVService.Status (the descriptor's leader hint is tried
// first). Leaders are counted per node and, while one node leads at least two
// more ranges than a replica that could take one, the scheduler asks the
// donor's KVService.TransferLeader to move one range to the least-loaded
// replica. The pass is bounded: each range is attempted at most once and the
// loop stops as soon as no move improves the balance.
//
// Per-range failures (unreachable replica, stale leader, target not caught up)
// are skipped, do not change the local counts, and are returned joined with the
// successful move count. Descriptor leader hints are not rewritten here: they
// are advisory and may lag the actual Raft leader until the next metadata
// update.
func (s *Server) RebalanceLeaders(ctx context.Context) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	schedulerMu.Lock()
	defer schedulerMu.Unlock()

	if err := s.requireLeader(ctx); err != nil {
		return 0, err
	}

	rows := s.cluster.Snapshot()
	if len(rows) == 0 {
		return 0, nil
	}

	type rangeState struct {
		r      meta.Range
		leader string
	}
	states := make([]rangeState, 0, len(rows))
	counts := make(map[string]int)
	var errs []error

	// Every replica starts at zero so nodes that currently lead nothing are
	// valid transfer targets.
	for i := range rows {
		for _, addr := range rows[i].Replicas {
			if _, ok := counts[addr]; !ok {
				counts[addr] = 0
			}
		}
	}

	for _, r := range rows {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		leader, err := s.queryRangeLeader(ctx, r)
		if err != nil {
			errs = append(errs, fmt.Errorf("rebalance range %d: find leader: %w", r.ID, err))
			continue
		}
		counts[leader]++
		states = append(states, rangeState{r: r, leader: leader})
	}

	moved := 0
	attempted := make(map[uint64]bool, len(states))
	// One successful move per range is enough for a pass; the attempt cap also
	// covers repeated failures on the same donor.
	maxAttempts := 2*len(states) + 1
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}

		donor := mostLoadedLeader(counts)
		if donor == "" || counts[donor] < 2 {
			break
		}

		best := -1
		bestTarget := ""
		bestTargetCount := 0
		for i := range states {
			st := states[i]
			if st.leader != donor || attempted[st.r.ID] {
				continue
			}
			for _, addr := range st.r.Replicas {
				if addr == donor {
					continue
				}
				if c := counts[addr]; best < 0 || c < bestTargetCount {
					best = i
					bestTarget = addr
					bestTargetCount = c
				}
			}
		}
		if best < 0 {
			break
		}
		// Moving would not improve the balance: stop instead of oscillating.
		if bestTargetCount+1 >= counts[donor] {
			break
		}

		st := states[best]
		attempted[st.r.ID] = true
		resp, err := s.transferRangeLeader(ctx, st.leader, st.r, bestTarget)
		if err != nil {
			errs = append(errs, fmt.Errorf("rebalance range %d: leader %s -> %s: %w",
				st.r.ID, st.leader, bestTarget, err))
			continue
		}
		if resp == nil {
			errs = append(errs, fmt.Errorf("rebalance range %d: leader %s -> %s: empty response",
				st.r.ID, st.leader, bestTarget))
			continue
		}
		if !resp.GetSuccess() {
			msg := resp.GetError()
			if msg == "" {
				msg = "unspecified failure"
			}
			errs = append(errs, fmt.Errorf("rebalance range %d: leader %s -> %s: %s%s",
				st.r.ID, st.leader, bestTarget, msg, leaderHintSuffix(resp.GetLeader())))
			continue
		}

		counts[st.leader]--
		counts[bestTarget]++
		states[best].leader = bestTarget
		moved++
		if s.log != nil {
			s.log.Info("leader rebalanced", "range", st.r.ID,
				"from", st.leader, "to", bestTarget, "epoch", st.r.Version)
		}
	}

	return moved, errors.Join(errs...)
}

// mostLoadedLeader returns the node with the most leaderships, breaking ties by
// address so a pass is deterministic. Nodes that lead nothing are not donors.
func mostLoadedLeader(counts map[string]int) string {
	donor := ""
	for node, count := range counts {
		if count < 2 {
			continue
		}
		if donor == "" || count > counts[donor] || (count == counts[donor] && node < donor) {
			donor = node
		}
	}
	return donor
}

func leaderHintSuffix(leader string) string {
	if leader == "" {
		return ""
	}
	return " (leader=" + leader + ")"
}

// queryRangeLeader discovers the current leader of r via KVService.Status. The
// descriptor's leader hint is probed first, then every other replica. A replica
// that reports role=leader wins; otherwise the first leader hint naming a
// replica is used, and the transfer RPC will fail cleanly if that hint is
// stale.
func (s *Server) queryRangeLeader(ctx context.Context, r meta.Range) (string, error) {
	if len(r.Replicas) == 0 {
		return "", errors.New("range has no replicas")
	}
	order := replicasStartingWith(r.Leader, r.Replicas)
	var hint string
	var lastErr error
	for _, addr := range order {
		resp, err := s.callRangeStatus(ctx, addr, r.ID)
		if err != nil {
			lastErr = err
			continue
		}
		if strings.EqualFold(resp.GetRole(), "leader") {
			return addr, nil
		}
		if len(hint) == 0 && resp.GetLeader() != "" && slices.Contains(r.Replicas, resp.GetLeader()) {
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

// callRangeStatus performs one Status RPC against one replica.
func (s *Server) callRangeStatus(ctx context.Context, addr string, rangeID uint64) (*pbv2.StatusResponse, error) {
	cc, err := s.dial(addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	defer cc.Close()

	callCtx, cancel := context.WithTimeout(ctx, leaderRebalanceStatusTimeout)
	defer cancel()
	resp, err := pbv2.NewKVServiceClient(cc).Status(callCtx, &pbv2.StatusRequest{RangeId: rangeID})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", addr, err)
	}
	return resp, nil
}

// transferRangeLeader asks the current leader of r to hand leadership to
// target. epoch is the range version; zero disables the KV-side epoch check.
func (s *Server) transferRangeLeader(ctx context.Context, leader string, r meta.Range, target string) (*pbv2.TransferLeaderResponse, error) {
	cc, err := s.dial(leader)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", leader, err)
	}
	defer cc.Close()

	callCtx, cancel := context.WithTimeout(ctx, leaderRebalanceTransferTimeout)
	defer cancel()
	resp, err := pbv2.NewKVServiceClient(cc).TransferLeader(callCtx, &pbv2.TransferLeaderRequest{
		RangeId: r.ID,
		Target:  target,
		Epoch:   r.Version,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", leader, err)
	}
	return resp, nil
}

// StartLeaderRebalance starts a background loop that runs one RebalanceLeaders
// pass every interval. It is leader-only: on a replicated meta group followers
// skip their turn and an election moves the work to the new leader. The loop
// stops when ctx is canceled or Server.Stop is called; only one rebalance loop
// may run per Server. It is independent of StartLifecycle so enabling leader
// rebalancing does not change split/merge behavior.
func (s *Server) StartLeaderRebalance(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("metaserver: leader rebalance interval must be positive")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return errors.New("metaserver: stopped")
	}
	if s.rebalance != nil {
		s.mu.Unlock()
		return errors.New("metaserver: leader rebalance already running")
	}
	runCtx, cancel := context.WithCancel(ctx)
	loop := &rebalanceLoop{cancel: cancel, done: make(chan struct{})}
	s.rebalance = loop
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			if s.rebalance == loop {
				s.rebalance = nil
			}
			s.mu.Unlock()
			close(loop.done)
		}()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if err := s.requireLeader(runCtx); err != nil {
					continue
				}
				moved, err := s.RebalanceLeaders(runCtx)
				if s.log == nil {
					continue
				}
				if err != nil {
					s.log.Warn("leader rebalance pass finished with errors", "moved", moved, "error", err)
				} else if moved > 0 {
					s.log.Info("leader rebalance pass", "moved", moved)
				}
			}
		}
	}()
	return nil
}
