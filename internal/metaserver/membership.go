package metaserver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"distributed-db/internal/meta"
	pbv2 "distributed-db/internal/pb/v2"
)

const (
	// membershipRPCTimeout bounds one target/leader membership RPC.
	membershipRPCTimeout = 30 * time.Second
	// membershipCatchupTimeout bounds how long AddReplica waits for the new
	// learner to apply the leader's committed state before promoting it.
	membershipCatchupTimeout = 30 * time.Second
	// membershipPollInterval is the catch-up/leader-discovery poll period.
	membershipPollInterval = 50 * time.Millisecond
	// membershipLeaderRetryTimeout bounds how long a leader-only step waits
	// for an election. A single retry round of a few dozen milliseconds is not
	// enough for a loaded cluster to elect a new leader.
	membershipLeaderRetryTimeout = 5 * time.Second
)

// AddReplica adds target to the replica set of rangeID. It is the only
// supported way to grow a replica set: the target first creates a non-voting
// Raft group (reusing existing data if any), the range leader registers it as
// a learner, the scheduler waits until the learner has applied the leader's
// committed state, and only then promotes it to a voter. The meta range map is
// replaced only after every step succeeded; a failure at any point leaves the
// map unchanged, and a retry is safe because every step is idempotent.
//
// The caller must be the meta leader. Changes are serialized by schedulerMu and
// Raft enforces one server change at a time; there is no joint consensus.
//
// AddReplica is an in-process orchestration entry point, not a MetaService RPC,
// so the meta admin-token interceptor does not gate it. The remote KVService
// AddReplica RPC is served by kvserver and is outside this package; if these
// orchestration APIs are ever exposed over the network they need their own
// admin authentication.
func (s *Server) AddReplica(ctx context.Context, rangeID uint64, target string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	schedulerMu.Lock()
	defer schedulerMu.Unlock()

	if target == "" {
		return errors.New("add replica: empty target")
	}
	if err := s.requireLeader(ctx); err != nil {
		return err
	}

	rows := s.cluster.Snapshot()
	byID, _ := indexRanges(rows)
	r, ok := byID[rangeID]
	if !ok {
		return fmt.Errorf("add replica: range %d not found", rangeID)
	}
	if containsAddr(r.Replicas, target) {
		// Already placed. Do not touch the target: it may already be a voter
		// whose promotion was committed before a failed ApplyRanges, and
		// demoting it locally would diverge from the leader's voter set.
		return nil
	}
	if len(r.Replicas) == 0 {
		return fmt.Errorf("add replica: range %d has no voters to add a learner from", rangeID)
	}

	if _, err := s.addReplicaOnTarget(ctx, r, target); err != nil {
		return fmt.Errorf("add replica: range %d: create target group on %s: %w", rangeID, target, err)
	}

	leader, err := s.addLearnerOnLeader(ctx, r, target)
	if err != nil {
		return fmt.Errorf("add replica: range %d: add learner %s: %w", rangeID, target, err)
	}
	if err := s.waitLearnerCaughtUp(ctx, leader, target, r.ID); err != nil {
		return fmt.Errorf("add replica: range %d: learner %s catch-up: %w", rangeID, target, err)
	}
	if err := s.promoteOnLeader(ctx, r, target); err != nil {
		return fmt.Errorf("add replica: range %d: promote %s: %w", rangeID, target, err)
	}

	newVersion := versionBump(r.Version)
	newReplicas := append(append([]string(nil), r.Replicas...), target)
	// Publish the new version on every replica before the range map, so a
	// client that reads the new map is accepted by every node. This is the
	// same ordering the split path uses (replicas bump their epoch, then meta
	// publishes the map).
	if err := s.propagateRangeEpoch(ctx, newReplicas, r.ID, newVersion); err != nil {
		return fmt.Errorf("add replica: range %d: propagate version %d: %w", rangeID, newVersion, err)
	}

	next := make([]meta.Range, 0, len(rows))
	for i := range rows {
		if rows[i].ID != rangeID {
			next = append(next, rows[i])
			continue
		}
		updated := rows[i]
		updated.Replicas = newReplicas
		updated.Version = newVersion
		next = append(next, updated)
	}
	if err := s.ApplyRanges(ctx, next); err != nil {
		return fmt.Errorf("add replica: range %d: apply range map: %w", rangeID, err)
	}
	if s.log != nil {
		s.log.Info("replica added", "range", rangeID, "target", target,
			"replicas", len(newReplicas), "version", newVersion)
	}
	return nil
}

// RemoveReplica removes target from the replica set of rangeID. If target is
// the current leader, leadership is transferred to another voter first, then
// the target is removed from the Raft voter set on the leader and finally
// stopped locally. The meta range map is updated last; an unreachable target
// does not block the metadata update because its group is already excluded
// from the voter set and a restarted stale group cannot win votes.
//
// The caller must be the meta leader. Removing a range's last replica is
// rejected.
//
// Decommission gap: the target's data directory is left in place and, when the
// node is unreachable, its local Raft state is not updated. Such a node must not
// be restarted as a live replica without going through AddReplica again; its
// stale voter view can bump terms until it is re-added or its data directory is
// removed by an operator. There is no placement/capacity decommission policy.
//
// RemoveReplica is an in-process orchestration entry point, not a MetaService
// RPC, so the meta admin-token interceptor does not gate it. The remote
// KVService RemoveReplica RPC is served by kvserver and is outside this
// package.
func (s *Server) RemoveReplica(ctx context.Context, rangeID uint64, target string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	schedulerMu.Lock()
	defer schedulerMu.Unlock()

	if target == "" {
		return errors.New("remove replica: empty target")
	}
	if err := s.requireLeader(ctx); err != nil {
		return err
	}

	rows := s.cluster.Snapshot()
	byID, _ := indexRanges(rows)
	r, ok := byID[rangeID]
	if !ok {
		return fmt.Errorf("remove replica: range %d not found", rangeID)
	}
	if !containsAddr(r.Replicas, target) {
		// The map already reflects the removal. Best-effort stop the group so
		// a retry after a partial failure converges, and refresh the version.
		s.stopReplicaOnTarget(ctx, target, rangeID)
		_ = s.propagateRangeEpoch(ctx, r.Replicas, r.ID, r.Version)
		return nil
	}
	if len(r.Replicas) <= 1 {
		return fmt.Errorf("remove replica: range %d: cannot remove the last replica %s", rangeID, target)
	}

	if err := s.removeVoterWithTransfer(ctx, r, target); err != nil {
		return fmt.Errorf("remove replica: range %d: remove voter %s: %w", rangeID, target, err)
	}
	// The target is no longer a voter, so stopping it is not required for
	// safety. An unreachable node (for example a crashed machine) is logged
	// and the map update proceeds.
	s.stopReplicaOnTarget(ctx, target, rangeID)

	newVersion := versionBump(r.Version)
	newReplicas := make([]string, 0, len(r.Replicas)-1)
	for _, addr := range r.Replicas {
		if addr != target {
			newReplicas = append(newReplicas, addr)
		}
	}
	if err := s.propagateRangeEpoch(ctx, newReplicas, r.ID, newVersion); err != nil {
		return fmt.Errorf("remove replica: range %d: propagate version %d: %w", rangeID, newVersion, err)
	}

	next := make([]meta.Range, 0, len(rows))
	for i := range rows {
		if rows[i].ID != rangeID {
			next = append(next, rows[i])
			continue
		}
		updated := rows[i]
		updated.Replicas = newReplicas
		if updated.Leader == target {
			updated.Leader = ""
		}
		updated.Version = newVersion
		next = append(next, updated)
	}
	if err := s.ApplyRanges(ctx, next); err != nil {
		return fmt.Errorf("remove replica: range %d: apply range map: %w", rangeID, err)
	}
	if s.log != nil {
		s.log.Info("replica removed", "range", rangeID, "target", target,
			"replicas", len(newReplicas), "version", newVersion)
	}
	return nil
}

// addReplicaOnTarget asks the target node to create/restart the range group as
// a learner.
func (s *Server) addReplicaOnTarget(ctx context.Context, r meta.Range, target string) (*pbv2.AddReplicaResponse, error) {
	resp, err := s.callAddReplica(ctx, target, &pbv2.AddReplicaRequest{
		RangeId: r.ID,
		Target:  target,
		Peers:   append([]string(nil), r.Replicas...),
		Epoch:   r.Version,
		Start:   cloneBytes(r.Start),
		End:     cloneBytes(r.End),
	})
	if err != nil {
		return nil, err
	}
	if !resp.GetSuccess() {
		return resp, fmt.Errorf("%s", responseError(resp.GetError()))
	}
	return resp, nil
}

// addLearnerOnLeader registers target as a learner, rediscovering the leader
// after a failover. It returns the leader that accepted the change.
func (s *Server) addLearnerOnLeader(ctx context.Context, r meta.Range, target string) (string, error) {
	deadline := membershipDeadline(ctx)
	var lastErr error
	for {
		leader, err := s.queryRangeLeader(ctx, r)
		if err != nil {
			lastErr = fmt.Errorf("find leader: %w", err)
		} else {
			resp, callErr := s.callAddLearner(ctx, leader, r.ID, target, r.Version)
			if callErr != nil {
				lastErr = fmt.Errorf("%s: %w", leader, callErr)
			} else if resp.GetSuccess() {
				return leader, nil
			} else {
				lastErr = fmt.Errorf("%s: %s%s", leader, responseError(resp.GetError()),
					leaderHintSuffix(resp.GetLeader()))
			}
		}
		if !time.Now().Before(deadline) {
			return "", lastErr
		}
		if !sleepContext(ctx, membershipPollInterval) {
			return "", ctx.Err()
		}
	}
}

// promoteOnLeader promotes target on the current leader, rediscovering the
// leader after a failover.
func (s *Server) promoteOnLeader(ctx context.Context, r meta.Range, target string) error {
	deadline := membershipDeadline(ctx)
	var lastErr error
	for {
		leader, err := s.queryRangeLeader(ctx, r)
		if err != nil {
			lastErr = fmt.Errorf("find leader: %w", err)
		} else {
			resp, callErr := s.callPromoteReplica(ctx, leader, r.ID, target, r.Version)
			if callErr != nil {
				lastErr = fmt.Errorf("%s: %w", leader, callErr)
			} else if resp.GetSuccess() {
				return nil
			} else {
				lastErr = fmt.Errorf("%s: %s%s", leader, responseError(resp.GetError()),
					leaderHintSuffix(resp.GetLeader()))
			}
		}
		if !time.Now().Before(deadline) {
			return lastErr
		}
		if !sleepContext(ctx, membershipPollInterval) {
			return ctx.Err()
		}
	}
}

// waitLearnerCaughtUp polls the target until it has applied the leader's
// commit index observed at the start of the wait.
func (s *Server) waitLearnerCaughtUp(ctx context.Context, leader, target string, rangeID uint64) error {
	deadline := time.Now().Add(membershipCatchupTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	leaderStatus, err := s.callRangeStatus(ctx, leader, rangeID)
	if err != nil {
		return fmt.Errorf("leader status: %w", err)
	}
	want := leaderStatus.GetCommitIndex()
	for {
		targetStatus, err := s.callRangeStatus(ctx, target, rangeID)
		if err == nil && targetStatus.GetLastApplied() >= want {
			return nil
		}
		if !time.Now().Before(deadline) {
			if err != nil {
				return fmt.Errorf("target status: %w", err)
			}
			return fmt.Errorf("last applied %d, want %d", targetStatus.GetLastApplied(), want)
		}
		if !sleepContext(ctx, membershipPollInterval) {
			return ctx.Err()
		}
	}
}

// removeVoterWithTransfer removes target from the voter set, transferring
// leadership away first when target currently leads the range.
func (s *Server) removeVoterWithTransfer(ctx context.Context, r meta.Range, target string) error {
	deadline := membershipDeadline(ctx)
	var lastErr error
	for {
		leader, err := s.queryRangeLeader(ctx, r)
		if err != nil {
			lastErr = fmt.Errorf("find leader: %w", err)
		} else if leader == target {
			if err := s.transferAwayFrom(ctx, r, target); err != nil {
				lastErr = fmt.Errorf("transfer leadership away from %s: %w", target, err)
			} else {
				lastErr = fmt.Errorf("leadership still on %s", target)
			}
		} else {
			resp, callErr := s.callRemoveVoter(ctx, leader, r.ID, target, r.Version)
			if callErr != nil {
				lastErr = fmt.Errorf("%s: %w", leader, callErr)
			} else if resp.GetSuccess() {
				return nil
			} else {
				lastErr = fmt.Errorf("%s: %s%s", leader, responseError(resp.GetError()),
					leaderHintSuffix(resp.GetLeader()))
			}
		}
		if !time.Now().Before(deadline) {
			return lastErr
		}
		if !sleepContext(ctx, membershipPollInterval) {
			return ctx.Err()
		}
	}
}

// membershipDeadline returns the earlier of the caller's deadline and the
// bounded leader-retry window.
func membershipDeadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(membershipLeaderRetryTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		return d
	}
	return deadline
}

// transferAwayFrom asks the current leader to hand leadership to another voter.
func (s *Server) transferAwayFrom(ctx context.Context, r meta.Range, leader string) error {
	var lastErr error
	for _, candidate := range r.Replicas {
		if candidate == leader {
			continue
		}
		resp, err := s.transferRangeLeader(ctx, leader, r, candidate)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.GetSuccess() {
			return nil
		}
		lastErr = fmt.Errorf("transfer to %s rejected: %s%s", candidate,
			responseError(resp.GetError()), leaderHintSuffix(resp.GetLeader()))
	}
	if lastErr == nil {
		lastErr = errors.New("no other voter to transfer leadership to")
	}
	return lastErr
}

// stopReplicaOnTarget asks the target to stop its local group. Failures are
// logged, not returned: after RemoveVoter the target no longer contributes to
// quorum, and a restarted stale group is rejected as a non-voter candidate.
func (s *Server) stopReplicaOnTarget(ctx context.Context, target string, rangeID uint64) {
	resp, err := s.callRemoveReplica(ctx, target, rangeID, target)
	if err != nil {
		if s.log != nil {
			s.log.Warn("stop removed replica failed", "range", rangeID, "target", target, "error", err)
		}
		return
	}
	if !resp.GetSuccess() && s.log != nil {
		s.log.Warn("stop removed replica rejected", "range", rangeID, "target", target,
			"error", resp.GetError())
	}
}

// propagateRangeEpoch records a new range version on every replica. It is
// called before ApplyRanges so the published version is accepted by the whole
// replica set; a failure leaves the meta map unchanged and the orchestration
// can be retried.
func (s *Server) propagateRangeEpoch(ctx context.Context, replicas []string, rangeID, epoch uint64) error {
	errs := s.callReplicas(ctx, replicas, func(ctx context.Context, client pbv2.KVServiceClient) error {
		resp, err := client.UpdateRangeEpoch(ctx, &pbv2.UpdateRangeEpochRequest{
			RangeId: rangeID, Epoch: epoch,
		})
		if err != nil {
			return err
		}
		if !resp.GetSuccess() {
			return fmt.Errorf("%s", responseError(resp.GetError()))
		}
		return nil
	})
	return replicaFailures("update range epoch", replicas, errs)
}

func (s *Server) callAddReplica(ctx context.Context, addr string, req *pbv2.AddReplicaRequest) (*pbv2.AddReplicaResponse, error) {
	cc, err := s.dial(addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	defer cc.Close()
	callCtx, cancel := context.WithTimeout(ctx, membershipRPCTimeout)
	defer cancel()
	return pbv2.NewKVServiceClient(cc).AddReplica(callCtx, req)
}

func (s *Server) callRemoveReplica(ctx context.Context, addr string, rangeID uint64, target string) (*pbv2.RemoveReplicaResponse, error) {
	cc, err := s.dial(addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	defer cc.Close()
	callCtx, cancel := context.WithTimeout(ctx, membershipRPCTimeout)
	defer cancel()
	return pbv2.NewKVServiceClient(cc).RemoveReplica(callCtx, &pbv2.RemoveReplicaRequest{
		RangeId: rangeID, Target: target,
	})
}

func (s *Server) callAddLearner(ctx context.Context, addr string, rangeID uint64, target string, epoch uint64) (*pbv2.MembershipResponse, error) {
	cc, err := s.dial(addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	defer cc.Close()
	callCtx, cancel := context.WithTimeout(ctx, membershipRPCTimeout)
	defer cancel()
	return pbv2.NewKVServiceClient(cc).AddLearner(callCtx, &pbv2.AddLearnerRequest{
		RangeId: rangeID, Target: target, Epoch: epoch,
	})
}

func (s *Server) callPromoteReplica(ctx context.Context, addr string, rangeID uint64, target string, epoch uint64) (*pbv2.MembershipResponse, error) {
	cc, err := s.dial(addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	defer cc.Close()
	callCtx, cancel := context.WithTimeout(ctx, membershipRPCTimeout)
	defer cancel()
	return pbv2.NewKVServiceClient(cc).PromoteReplica(callCtx, &pbv2.PromoteReplicaRequest{
		RangeId: rangeID, Target: target, Epoch: epoch,
	})
}

func (s *Server) callRemoveVoter(ctx context.Context, addr string, rangeID uint64, target string, epoch uint64) (*pbv2.MembershipResponse, error) {
	cc, err := s.dial(addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	defer cc.Close()
	callCtx, cancel := context.WithTimeout(ctx, membershipRPCTimeout)
	defer cancel()
	return pbv2.NewKVServiceClient(cc).RemoveVoter(callCtx, &pbv2.RemoveVoterRequest{
		RangeId: rangeID, Target: target, Epoch: epoch,
	})
}

func containsAddr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func versionBump(v uint64) uint64 {
	if v == 0 {
		return 1
	}
	return v + 1
}

func responseError(msg string) string {
	if msg == "" {
		return "unspecified failure"
	}
	return msg
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
