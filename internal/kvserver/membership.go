package kvserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"
)

// This file implements the dynamic replica-set operations exposed by
// KVService. Meta orchestration drives them in this order:
//
//	AddReplica (target) -> AddLearner (leader) -> catch up -> PromoteReplica
//	RemoveVoter (leader) -> RemoveReplica (target)
//
// Raft itself changes one server at a time and has no joint consensus. The KV
// layer adds idempotency for retries: adding an existing learner/voter and
// removing an absent voter are reported as success, so a partially completed
// orchestration can simply be retried.

// addRangeGroup creates, starts and registers one range group, or returns the
// existing group when the range is already hosted. Existing data and an
// existing Raft log are reused; the caller supplies the placement and must
// create the group as a learner.
func (s *Server) addRangeGroup(rc RangeConfig) (*rangeGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, errors.New("kvserver: stopped")
	}

	s.hostMu.RLock()
	existing, ok := s.groups[rc.ID]
	s.hostMu.RUnlock()
	if ok {
		if len(rc.Start) > 0 || len(rc.End) > 0 {
			if !bytes.Equal(existing.cfg.Start, rc.Start) || !bytes.Equal(existing.cfg.End, rc.End) {
				return nil, fmt.Errorf("kvserver: range %d already hosted with bounds [%q,%q)",
					rc.ID, existing.cfg.Start, existing.cfg.End)
			}
		}
		// A restart of a previously removed range keeps its data and log but
		// must adopt the version published by meta.
		if rc.Epoch > existing.cfg.Epoch {
			existing.cfg.Epoch = rc.Epoch
			s.rebuildRangeOrder()
		}
		return existing, nil
	}

	epoch := rc.Epoch
	if epoch == 0 {
		epoch = 1
	}
	normalized := RangeConfig{
		ID:           rc.ID,
		Start:        cloneBytes(rc.Start),
		End:          cloneBytes(rc.End),
		Peers:        cloneStrings(rc.Peers),
		Epoch:        epoch,
		Learner:      true,
		ForceLearner: true,
	}
	if normalized.ID == 0 {
		return nil, errors.New("kvserver: range ID must be non-zero")
	}

	g, err := newRangeGroup(s.cfg, s.engine, normalized, s.peerCreds)
	if err != nil {
		return nil, err
	}
	if err := g.start(context.Background()); err != nil {
		_ = g.stop()
		return nil, fmt.Errorf("kvserver: start range %d: %w", normalized.ID, err)
	}

	s.hostMu.Lock()
	s.groups[normalized.ID] = g
	s.hostMu.Unlock()
	s.rebuildRangeOrder()
	s.log.Info("range replica added", "range", normalized.ID, "epoch", epoch,
		"peers", normalized.Peers, "learner", true)
	return g, nil
}

// removeRangeGroup stops and unregisters one hosted range. The data directory
// and Raft log are left on disk, so a later AddReplica can restart the group
// and catch up. Removing an absent range is a no-op.
func (s *Server) removeRangeGroup(id uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return errors.New("kvserver: stopped")
	}

	s.hostMu.Lock()
	g := s.groups[id]
	delete(s.groups, id)
	s.hostMu.Unlock()
	if g == nil {
		return nil
	}
	// Always persist the reduced range map, even if stopping the group fails:
	// a leftover data directory is harmless, but keeping a stopped group in
	// the map would route requests to it after a restart.
	stopErr := g.stop()
	s.rebuildRangeOrder()
	s.log.Info("range replica removed", "range", id)
	return stopErr
}

// AddReplica is served by the target node. It creates the range as a learner
// (reusing any existing data and log) and returns the best-known leader hint;
// until the range leader registers the learner that hint may be empty.
func (k *kvService) AddReplica(_ context.Context, req *pbv2.AddReplicaRequest) (*pbv2.AddReplicaResponse, error) {
	if req == nil || req.GetRangeId() == 0 {
		return &pbv2.AddReplicaResponse{Error: "bad range id"}, nil
	}
	if req.GetTarget() != "" && req.GetTarget() != k.s.cfg.ID {
		return &pbv2.AddReplicaResponse{Error: "wrong target node"}, nil
	}
	peers := make([]string, 0, len(req.GetPeers()))
	for _, p := range req.GetPeers() {
		if p == "" || p == k.s.cfg.ID {
			continue
		}
		peers = append(peers, p)
	}
	g, err := k.s.addRangeGroup(RangeConfig{
		ID:      req.GetRangeId(),
		Start:   req.GetStart(),
		End:     req.GetEnd(),
		Peers:   peers,
		Epoch:   req.GetEpoch(),
		Learner: true,
	})
	if err != nil {
		return &pbv2.AddReplicaResponse{Error: opError(err)}, nil
	}
	return &pbv2.AddReplicaResponse{Success: true, Leader: g.node.Leader()}, nil
}

// RemoveReplica is served by the target node and stops its local Raft group
// after the leader has removed it from the voter set. It is idempotent.
func (k *kvService) RemoveReplica(_ context.Context, req *pbv2.RemoveReplicaRequest) (*pbv2.RemoveReplicaResponse, error) {
	if req == nil || req.GetRangeId() == 0 {
		return &pbv2.RemoveReplicaResponse{Error: "bad range id"}, nil
	}
	if req.GetTarget() != "" && req.GetTarget() != k.s.cfg.ID {
		return &pbv2.RemoveReplicaResponse{Error: "wrong target node"}, nil
	}
	if err := k.s.removeRangeGroup(req.GetRangeId()); err != nil {
		return &pbv2.RemoveReplicaResponse{Error: opError(err)}, nil
	}
	return &pbv2.RemoveReplicaResponse{Success: true}, nil
}

// AddLearner registers target as a non-voting member on this range's leader.
// Repeating the call for an existing learner or voter succeeds.
func (k *kvService) AddLearner(ctx context.Context, req *pbv2.AddLearnerRequest) (*pbv2.MembershipResponse, error) {
	g, resp := k.membershipTarget(req.GetRangeId(), req.GetEpoch())
	if resp != nil {
		return resp, nil
	}
	if req.GetTarget() == "" {
		return membershipFailure(g, errors.New("empty target")), nil
	}
	voters, learners := g.node.Membership()
	if containsString(voters, req.GetTarget()) || containsString(learners, req.GetTarget()) {
		return membershipSuccess(g), nil
	}
	if err := g.node.AddLearner(ctx, req.GetTarget()); err != nil {
		return membershipFailure(g, err), nil
	}
	return membershipSuccess(g), nil
}

// PromoteReplica promotes a caught-up learner to a voter on this range's
// leader. Repeating the call for an existing voter succeeds.
func (k *kvService) PromoteReplica(ctx context.Context, req *pbv2.PromoteReplicaRequest) (*pbv2.MembershipResponse, error) {
	g, resp := k.membershipTarget(req.GetRangeId(), req.GetEpoch())
	if resp != nil {
		return resp, nil
	}
	if req.GetTarget() == "" {
		return membershipFailure(g, errors.New("empty target")), nil
	}
	voters, learners := g.node.Membership()
	if containsString(voters, req.GetTarget()) {
		return membershipSuccess(g), nil
	}
	if !containsString(learners, req.GetTarget()) {
		return membershipFailure(g, fmt.Errorf("%w: %s", raft.ErrNotLearner, req.GetTarget())), nil
	}
	if err := g.node.PromoteLearner(ctx, req.GetTarget()); err != nil {
		return membershipFailure(g, err), nil
	}
	return membershipSuccess(g), nil
}

// RemoveVoter removes a voter on this range's leader. Repeating the call for
// an address that is no longer a voter succeeds.
func (k *kvService) RemoveVoter(ctx context.Context, req *pbv2.RemoveVoterRequest) (*pbv2.MembershipResponse, error) {
	g, resp := k.membershipTarget(req.GetRangeId(), req.GetEpoch())
	if resp != nil {
		return resp, nil
	}
	if req.GetTarget() == "" {
		return membershipFailure(g, errors.New("empty target")), nil
	}
	voters, _ := g.node.Membership()
	if !containsString(voters, req.GetTarget()) {
		return membershipSuccess(g), nil
	}
	if err := g.node.RemoveVoter(ctx, req.GetTarget()); err != nil {
		return membershipFailure(g, err), nil
	}
	return membershipSuccess(g), nil
}

// Membership reports the Raft voter/learner sets of one hosted range.
func (k *kvService) Membership(_ context.Context, req *pbv2.MembershipRequest) (*pbv2.MembershipResponse, error) {
	g, err := k.s.groupForID(req.GetRangeId())
	if err != nil {
		return &pbv2.MembershipResponse{Leader: k.s.defaultLeader(), Error: opError(err)}, nil
	}
	return membershipSuccess(g), nil
}

// UpdateRangeEpoch records a new range version published by meta on this
// replica. It is served by every replica (not only the leader) because it is
// local metadata, not a replicated Raft change. A version not newer than the
// cached one is accepted unchanged, so retries are idempotent.
func (k *kvService) UpdateRangeEpoch(_ context.Context, req *pbv2.UpdateRangeEpochRequest) (*pbv2.UpdateRangeEpochResponse, error) {
	if req == nil || req.GetRangeId() == 0 || req.GetEpoch() == 0 {
		return &pbv2.UpdateRangeEpochResponse{Error: "bad epoch request"}, nil
	}
	g, err := k.s.groupForID(req.GetRangeId())
	if err != nil {
		return &pbv2.UpdateRangeEpochResponse{Leader: k.s.defaultLeader(), Error: opError(err)}, nil
	}

	k.s.mu.Lock()
	if k.s.stopped {
		k.s.mu.Unlock()
		return &pbv2.UpdateRangeEpochResponse{Error: "stopped"}, nil
	}
	if req.GetEpoch() > g.cfg.Epoch {
		g.cfg.Epoch = req.GetEpoch()
		// Persist the epoch alongside the hosted range map so a restart does
		// not go back to the pre-membership version.
		k.s.rebuildRangeOrder()
	}
	epoch := g.cfg.Epoch
	k.s.mu.Unlock()
	return &pbv2.UpdateRangeEpochResponse{Success: true, Leader: g.node.Leader(), Epoch: epoch}, nil
}

// membershipTarget resolves a range and validates the optional epoch. A
// non-nil response means the caller must return it immediately.
func (k *kvService) membershipTarget(rangeID, epoch uint64) (*rangeGroup, *pbv2.MembershipResponse) {
	g, err := k.s.groupForID(rangeID)
	if err != nil {
		return nil, &pbv2.MembershipResponse{Leader: k.s.defaultLeader(), Error: opError(err)}
	}
	if err := k.s.acceptEpoch(g, epoch); err != nil {
		return nil, &pbv2.MembershipResponse{Leader: g.node.Leader(), Error: opError(err)}
	}
	return g, nil
}

func membershipSuccess(g *rangeGroup) *pbv2.MembershipResponse {
	voters, learners := g.node.Membership()
	return &pbv2.MembershipResponse{
		Success:  true,
		Leader:   g.node.Leader(),
		Voters:   voters,
		Learners: learners,
	}
}

func membershipFailure(g *rangeGroup, err error) *pbv2.MembershipResponse {
	leader := ""
	if g != nil {
		leader = g.node.Leader()
	}
	return &pbv2.MembershipResponse{Leader: leader, Error: membershipError(err)}
}

// membershipError renders a Raft membership failure. Unknown errors fall back
// to opError's generic categories.
func membershipError(err error) string {
	switch {
	case errors.Is(err, raft.ErrNotLeader):
		return "not leader"
	case errors.Is(err, raft.ErrTimeout):
		return "timeout"
	case errors.Is(err, raft.ErrStopped):
		return "stopped"
	case errors.Is(err, raft.ErrAlreadyVoter):
		return "already voter"
	case errors.Is(err, raft.ErrAlreadyLearner):
		return "already learner"
	case errors.Is(err, raft.ErrNotLearner):
		return "not learner"
	case errors.Is(err, raft.ErrNotVoter):
		return "not voter"
	case errors.Is(err, raft.ErrUnsafeConfigChange):
		return "unsafe membership change"
	case errors.Is(err, raft.ErrConfigChangeInProgress):
		return "membership change in progress"
	case errors.Is(err, raft.ErrUnknownTarget):
		return "unknown peer"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return opError(err)
	}
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
