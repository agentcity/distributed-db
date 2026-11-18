package raft

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	// leaderTransferCatchupTimeout bounds how long TransferLeadership waits
	// for the target to replicate the leader's log before giving up. A caller
	// with an earlier context deadline shortens it further.
	leaderTransferCatchupTimeout = 2 * time.Second
	// leaderTransferPollInterval is how often the catch-up wait re-checks the
	// target's match index.
	leaderTransferPollInterval = 5 * time.Millisecond
)

// TransferLeadership moves leadership of this Raft group to target.
//
// The caller must currently be the leader and target must be one of its peers.
// The target must have replicated the leader's log: TransferLeadership waits,
// bounded by leaderTransferCatchupTimeout or the caller's deadline, until the
// target's match index reaches the leader's last index. Only then is a
// TimeoutNow RPC sent; on acceptance the leader steps down to follower so the
// target can win the election.
//
// Errors are explicit: ErrNotLeader, ErrUnknownTarget, ErrTargetNotCaughtUp,
// ErrLeaderTransferUnsupported, ErrStopped or a transport error. Except for a
// higher term observed during the attempt (which forces a step-down for Raft
// safety), a failed transfer leaves this node as leader.
func (n *Node) TransferLeadership(ctx context.Context, target string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if target == "" {
		return fmt.Errorf("%w: empty target", ErrUnknownTarget)
	}
	tr, ok := n.tr.(TimeoutNowTransport)
	if !ok {
		return ErrLeaderTransferUnsupported
	}

	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return ErrStopped
	}
	if n.role != Leader {
		n.mu.Unlock()
		return ErrNotLeader
	}
	if target == n.cfg.ID {
		n.mu.Unlock()
		return fmt.Errorf("%w: %s is the current leader", ErrUnknownTarget, target)
	}
	known := n.isVoterLocked(target)
	if !known {
		n.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrUnknownTarget, target)
	}
	term := n.term
	last := n.lastIndexLocked()
	caughtUp := n.matchIndex[target] >= last
	n.mu.Unlock()

	if !caughtUp {
		deadline := time.Now().Add(leaderTransferCatchupTimeout)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		ticker := time.NewTicker(leaderTransferPollInterval)
		defer ticker.Stop()
		for !caughtUp {
			select {
			case <-ctx.Done():
				return errors.Join(ErrTargetNotCaughtUp, ctx.Err())
			case <-n.stopCh:
				return ErrStopped
			case <-ticker.C:
			}

			n.mu.Lock()
			if n.stopped {
				n.mu.Unlock()
				return ErrStopped
			}
			if n.role != Leader || n.term != term {
				n.mu.Unlock()
				return fmt.Errorf("%w: leadership changed while waiting for %s", ErrNotLeader, target)
			}
			match := n.matchIndex[target]
			last = n.lastIndexLocked()
			caughtUp = match >= last
			n.mu.Unlock()

			if !caughtUp && !time.Now().Before(deadline) {
				return fmt.Errorf("%w: %s match index %d, leader last index %d",
					ErrTargetNotCaughtUp, target, match, last)
			}
		}
	}

	// Revalidate the state just before sending. Leadership can change while
	// the catch-up wait or an append is in flight.
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return ErrStopped
	}
	if n.role != Leader || n.term != term {
		n.mu.Unlock()
		return fmt.Errorf("%w: leadership changed before sending TimeoutNow", ErrNotLeader)
	}
	last = n.lastIndexLocked()
	match := n.matchIndex[target]
	n.mu.Unlock()
	if match < last {
		return fmt.Errorf("%w: %s match index %d, leader last index %d",
			ErrTargetNotCaughtUp, target, match, last)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	req := &TimeoutNowRequest{Term: term, LeaderID: n.cfg.ID}
	rpcCtx, cancel := n.rpcContext(n.voteTimeout())
	defer cancel()
	// A caller cancellation must abort the RPC too; n.rpcContext is tied to
	// the node lifetime, not to this caller.
	stopCancel := context.AfterFunc(ctx, cancel)
	defer stopCancel()
	resp, err := tr.TimeoutNow(rpcCtx, target, req)
	if err != nil {
		return fmt.Errorf("raft: TimeoutNow to %s: %w", target, err)
	}
	if resp == nil {
		return fmt.Errorf("raft: TimeoutNow to %s: empty response", target)
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopped {
		return ErrStopped
	}
	if resp.Term > n.term {
		// The target (or another peer) has moved to a newer term. This node
		// cannot remain leader; adopt the term for Raft safety and report a
		// failed transfer unless the target explicitly accepted.
		if err := n.adoptTermLocked(resp.Term, ""); err != nil {
			return fmt.Errorf("raft: adopt term from transfer target: %w", err)
		}
	}
	if resp.Success {
		if n.role == Leader {
			// Step down without claiming a leader: the target is expected to
			// win the election it just started.
			n.stepDownWithErrorLocked("", ErrNotLeader)
		}
		return nil
	}
	if resp.Term > term {
		return fmt.Errorf("%w: target term %d, leader term %d", ErrStaleTimeoutNow, resp.Term, term)
	}
	return fmt.Errorf("raft: leadership transfer to %s rejected (target term %d)", target, resp.Term)
}

// HandleTimeoutNow implements the TimeoutNow RPC receiver. A follower whose
// term matches the request and whose log is up to date starts an election
// immediately; every other state rejects the request without changing the
// term. In particular, a stale leader (req.Term < n.term) cannot trigger an
// election in a higher term, and a request from a higher, not-yet-replicated
// term is rejected rather than adopted.
func (n *Node) HandleTimeoutNow(ctx context.Context, req *TimeoutNowRequest) (*TimeoutNowResponse, error) {
	if req == nil {
		return nil, errors.New("raft: nil TimeoutNowRequest")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}

	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return nil, ErrStopped
	}
	if req.Term != n.term {
		term := n.term
		n.mu.Unlock()
		return &TimeoutNowResponse{Term: term, Success: false}, nil
	}
	if req.LeaderID == "" {
		term := n.term
		n.mu.Unlock()
		return &TimeoutNowResponse{Term: term, Success: false}, nil
	}
	if n.role != Follower {
		term := n.term
		n.mu.Unlock()
		return &TimeoutNowResponse{Term: term, Success: false}, nil
	}
	if !n.selfIsVoterLocked() {
		// Learners and removed nodes must not campaign.
		term := n.term
		n.mu.Unlock()
		return &TimeoutNowResponse{Term: term, Success: false}, nil
	}
	if n.leaderID != "" && n.leaderID != req.LeaderID {
		// The follower already acknowledges a different leader in this term.
		term := n.term
		n.mu.Unlock()
		return &TimeoutNowResponse{Term: term, Success: false}, nil
	}
	if !n.logUpToDateLocked() {
		term := n.term
		n.mu.Unlock()
		return &TimeoutNowResponse{Term: term, Success: false}, nil
	}
	acceptedTerm := n.term
	// Force the election loop to see an expired deadline as well, then start
	// the campaign directly so the target does not wait for its next tick.
	n.electionDeadline = time.Now()
	n.mu.Unlock()

	// The current leader asked for this election and is stepping down, so its
	// lease must not reject the campaign: start the real election directly.
	n.beginElection(true)
	return &TimeoutNowResponse{Term: acceptedTerm, Success: true}, nil
}

// logUpToDateLocked reports whether this node has a usable log and has not
// fallen behind its own commit index. The leader's bounded catch-up wait is
// the authoritative check that the target matches the leader's log; this
// guards the receiver against campaigning with an empty or uncommitted state.
// Caller must hold mu.
func (n *Node) logUpToDateLocked() bool {
	last := n.lastIndexLocked()
	return last > 0 && last >= n.commitIndex
}
