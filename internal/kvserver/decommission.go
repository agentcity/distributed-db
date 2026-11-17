package kvserver

// This file adds a caller-driven decommission orchestrator for one range
// replica. It deliberately builds only on the Raft status, leadership-transfer,
// promotion and removal APIs that already exist (Status, TransferLeadership,
// PromoteLearner and RemoveVoter) and never touches metaserver state directly:
// the final range-map/meta update is an injected DecommissionMetaUpdater.
//
// # Joint-consensus dependency
//
// The current Raft implementation changes one server at a time and has no
// joint consensus. Decommission is therefore only safe while the existing
// one-in-flight-change invariant holds. Callers must run at most one
// decommission per range at a time and must not run it concurrently with
// AddReplica/RemoveReplica for the same range. When joint consensus is wired
// later, this orchestrator should switch to a joint add/replace/remove
// transition, and the quorum checks below must be re-derived from the joint
// configuration (C_old and C_new) instead of the single voter set. Do not drive
// this from automatic placement before that work lands.
//
// # Learner targets
//
// Raft exposes RemoveVoter but no RemoveLearner. A learner target is therefore
// decommissioned in two steps: wait until it has applied the leader's commit
// index and is within the lag threshold, promote it to a voter, and only then
// remove it. The promotion is a conservative bridge; if the learner does not
// catch up, nothing is promoted and nothing is removed. A future RemoveLearner
// or joint-consensus implementation should replace this step with a direct
// learner removal.
//
// # Lifecycle
//
// Decommission is caller-driven and starts no goroutines. Every wait is a
// poll in the calling goroutine bounded by CatchUpTimeout/StepTimeout and by
// the caller's context. A retry is idempotent: an absent target is treated as
// already decommissioned, and the meta updater is called again so a crash
// between the Raft removal and the map update converges. The updater must
// itself be idempotent for the same reason.
//
// A concrete DecommissionControl can be built over the existing KVService
// RPCs: Status -> KVService.Status, TransferLeadership -> KVService.TransferLeader,
// PromoteLearner -> KVService.PromoteReplica and RemoveVoter ->
// KVService.RemoveVoter. That wiring is intentionally left outside this file
// so the orchestration stays testable without a network and so metaserver
// remains the sole owner of the range map.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"distributed-db/internal/raft"
)

const (
	// DefaultDecommissionCatchUpTimeout bounds how long a learner target may
	// take to apply the leader's commit index and reach the lag threshold.
	DefaultDecommissionCatchUpTimeout = 30 * time.Second
	// DefaultDecommissionStepTimeout bounds one control RPC and the
	// confirmation loops for leadership transfer, promotion and removal.
	DefaultDecommissionStepTimeout = 10 * time.Second
	// DefaultDecommissionPollInterval is the status polling period.
	DefaultDecommissionPollInterval = 50 * time.Millisecond
	// DefaultDecommissionMaxLagEntries is the maximum allowed last-index and
	// applied-index gap before a learner is promoted.
	DefaultDecommissionMaxLagEntries uint64 = 64

	// decommissionMaxReplicaSet bounds status expansion from leader hints so a
	// corrupt or malicious StatusResponse cannot make one call fan out without
	// limit.
	decommissionMaxReplicaSet = 1024
)

// Typed decommission errors. They are wrapped by DecommissionError, so callers
// match them with errors.Is and inspect the phase with errors.As.
var (
	ErrDecommissionInvalidRequest = errors.New("kvserver: invalid decommission request")
	ErrDecommissionLastVoter      = errors.New("kvserver: decommission would remove the last voter")
	ErrDecommissionQuorumLoss     = errors.New("kvserver: decommission would lose voter quorum")
	ErrDecommissionNoLeader       = errors.New("kvserver: decommission: no range leader available")
	ErrDecommissionSplitBrain     = errors.New("kvserver: decommission: multiple range leaders in one term")
	ErrDecommissionUnsafeConfig   = errors.New("kvserver: decommission: inconsistent raft configuration")
	ErrDecommissionTransfer       = errors.New("kvserver: decommission: leader transfer failed")
	ErrDecommissionNoCandidate    = errors.New("kvserver: decommission: no eligible leader transfer candidate")
	ErrDecommissionCatchUp        = errors.New("kvserver: decommission: learner did not catch up")
	ErrDecommissionPromotion      = errors.New("kvserver: decommission: learner promotion failed")
	ErrDecommissionRemoval        = errors.New("kvserver: decommission: voter removal failed")
	ErrDecommissionMeta           = errors.New("kvserver: decommission: range map/meta update failed")
	ErrDecommissionTimeout        = errors.New("kvserver: decommission: operation timed out")
	ErrDecommissionCanceled       = errors.New("kvserver: decommission: operation canceled")
)

// DecommissionPhase identifies the orchestration step that failed or
// completed. It is stable enough for metrics and operator messages.
type DecommissionPhase string

const (
	DecommissionPhasePreflight      DecommissionPhase = "preflight"
	DecommissionPhaseTransferLeader DecommissionPhase = "transfer_leader"
	DecommissionPhaseCatchUp        DecommissionPhase = "catch_up"
	DecommissionPhasePromote        DecommissionPhase = "promote"
	DecommissionPhaseRemoveVoter    DecommissionPhase = "remove_voter"
	DecommissionPhaseMeta           DecommissionPhase = "meta"
	DecommissionPhaseDone           DecommissionPhase = "done"
)

// DecommissionError is the typed error returned for every failed decommission.
// Err carries one or more sentinel errors from this file and/or the underlying
// Raft/transport error.
type DecommissionError struct {
	Phase   DecommissionPhase
	RangeID uint64
	Target  string
	Err     error
}

func (e *DecommissionError) Error() string {
	if e == nil {
		return "kvserver: decommission: <nil error>"
	}
	return fmt.Sprintf("kvserver: decommission range %d target %q phase %s: %v",
		e.RangeID, e.Target, e.Phase, e.Err)
}

func (e *DecommissionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// DecommissionReplica is the transport-neutral view of one replica's Raft
// state. LastIndex, CommitIndex and LastApplied are the target's local values;
// Voters and Learners come from the same Status call.
type DecommissionReplica struct {
	ID          string
	Role        raft.Role
	Leader      string
	Term        uint64
	CommitIndex uint64
	LastIndex   uint64
	LastApplied uint64
	Voters      []string
	Learners    []string
	Learner     bool
	ConfigEpoch uint64
}

// DecommissionControl is the control-plane seam for one range. Implementations
// must address the command to the given leader (or node), and must return the
// existing raft sentinels (raft.ErrNotLeader, raft.ErrNotVoter,
// raft.ErrAlreadyVoter, raft.ErrTargetNotCaughtUp, ...) unchanged so the
// orchestrator can treat retries as idempotent.
//
// Status must be safe to call for a replica that is no longer a voter: the
// orchestrator uses that view to detect an already-completed decommission.
type DecommissionControl interface {
	Status(ctx context.Context, rangeID uint64, node string) (DecommissionReplica, error)
	TransferLeadership(ctx context.Context, rangeID uint64, leader, target string) error
	PromoteLearner(ctx context.Context, rangeID uint64, leader, target string) error
	RemoveVoter(ctx context.Context, rangeID uint64, leader, target string) error
}

// DecommissionMetaUpdate is the final, post-Raft state handed to the meta
// updater. The updater owns the placement version bump and any propagation to
// the remaining replicas; this orchestrator never edits meta state.
type DecommissionMetaUpdate struct {
	RangeID  uint64
	Target   string
	Leader   string
	Voters   []string
	Learners []string
	Epoch    uint64
	// Removed reports that Target is absent from Voters and Learners.
	Removed bool
	// AlreadyRemoved reports that Target was already absent before this call.
	AlreadyRemoved bool
}

// DecommissionMetaUpdater persists the range map/meta after a successful Raft
// removal. It is called on the success path, including an idempotent retry
// where the target was already absent. Implementations must be idempotent and
// must honor ctx; this orchestrator calls the updater synchronously after the
// Raft membership change, so a failure leaves the caller with a typed error
// and a safe retry.
type DecommissionMetaUpdater interface {
	UpdateRangeMap(ctx context.Context, update DecommissionMetaUpdate) error
}

// DecommissionMetaFunc adapts a function to DecommissionMetaUpdater.
type DecommissionMetaFunc func(ctx context.Context, update DecommissionMetaUpdate) error

func (f DecommissionMetaFunc) UpdateRangeMap(ctx context.Context, update DecommissionMetaUpdate) error {
	if f == nil {
		return fmt.Errorf("%w: nil meta updater func", ErrDecommissionInvalidRequest)
	}
	return f(ctx, update)
}

// DecommissionOptions tunes the bounded waits. Zero values select the
// package defaults. MaxLagEntries bounds both leader.LastIndex-target.LastIndex
// and leader.CommitIndex-target.LastApplied; 0 selects
// DefaultDecommissionMaxLagEntries, so set a positive value for a stricter
// learner threshold.
type DecommissionOptions struct {
	CatchUpTimeout time.Duration
	StepTimeout    time.Duration
	PollInterval   time.Duration
	MaxLagEntries  uint64
	// Progress, when non-nil, is called synchronously on phase transitions and
	// once with Err set when the operation fails. It must not block.
	Progress func(DecommissionProgress)
	Logger   *slog.Logger
}

func (o DecommissionOptions) normalize() DecommissionOptions {
	if o.CatchUpTimeout <= 0 {
		o.CatchUpTimeout = DefaultDecommissionCatchUpTimeout
	}
	if o.StepTimeout <= 0 {
		o.StepTimeout = DefaultDecommissionStepTimeout
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultDecommissionPollInterval
	}
	if o.MaxLagEntries == 0 {
		o.MaxLagEntries = DefaultDecommissionMaxLagEntries
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// DecommissionRequest identifies one replica to decommission. Replicas is an
// optional snapshot of the range's known replica addresses; it is used for
// leader discovery and is expanded with leader hints and the leader's
// membership. At least Target must be provided. Epoch is passed through to the
// meta updater and may be used for fencing by the caller.
type DecommissionRequest struct {
	RangeID  uint64
	Target   string
	Replicas []string
	Epoch    uint64
}

// DecommissionResult describes the final observed state. Voters/Learners are
// the leader's post-operation sets and never include Target on success.
type DecommissionResult struct {
	RangeID        uint64
	Target         string
	Leader         string
	Voters         []string
	Learners       []string
	Transferred    bool
	Promoted       bool
	AlreadyRemoved bool
	LagEntries     uint64
}

// DecommissionProgress is delivered through DecommissionOptions.Progress.
type DecommissionProgress struct {
	Phase      DecommissionPhase
	RangeID    uint64
	Target     string
	Leader     string
	Voters     []string
	Learners   []string
	LagEntries uint64
	Message    string
	Err        error
}

// Decommissioner orchestrates one range-replica decommission at a time. It is
// safe for concurrent calls on different ranges; calls for the same range
// must be serialized by the caller (Raft itself rejects overlapping config
// changes, but the preflight view may be stale under concurrent calls).
type Decommissioner struct {
	control DecommissionControl
	meta    DecommissionMetaUpdater
	opts    DecommissionOptions
	log     *slog.Logger
}

// NewDecommissioner builds an orchestrator. Both control and meta are
// required: a nil meta updater would silently leave the range map stale after
// a successful Raft removal.
func NewDecommissioner(control DecommissionControl, meta DecommissionMetaUpdater, opts DecommissionOptions) (*Decommissioner, error) {
	if control == nil {
		return nil, fmt.Errorf("%w: control is required", ErrDecommissionInvalidRequest)
	}
	if meta == nil {
		return nil, fmt.Errorf("%w: meta updater is required", ErrDecommissionInvalidRequest)
	}
	opts = opts.normalize()
	return &Decommissioner{
		control: control,
		meta:    meta,
		opts:    opts,
		log:     opts.Logger,
	}, nil
}

// Decommission removes Target from range RangeID's Raft configuration and then
// asks the injected meta updater to publish the resulting replica set.
//
// Steps: preflight (leader, voter set, quorum, learner lag), transfer
// leadership away if Target leads, wait for a learner Target to catch up and
// promote it, remove the voter, verify the removal, then update meta. Every
// step is idempotent and no goroutine is started.
func (d *Decommissioner) Decommission(ctx context.Context, req DecommissionRequest) (res DecommissionResult, retErr error) {
	res = DecommissionResult{RangeID: req.RangeID, Target: req.Target}
	phase := DecommissionPhasePreflight

	defer func() {
		if retErr == nil {
			return
		}
		if d != nil && d.opts.Progress != nil {
			d.report(DecommissionProgress{
				Phase:    phase,
				RangeID:  req.RangeID,
				Target:   req.Target,
				Leader:   res.Leader,
				Voters:   cloneStrings(res.Voters),
				Learners: cloneStrings(res.Learners),
				Err:      retErr,
			})
		}
		if d != nil && d.log != nil {
			d.log.Warn("decommission failed", "range", req.RangeID, "target", req.Target,
				"phase", phase, "error", retErr)
		}
	}()

	if ctx == nil {
		ctx = context.Background()
	}
	if d == nil || d.control == nil || d.meta == nil || d.opts.PollInterval <= 0 {
		return res, decommissionPhaseError(req, phase, fmt.Errorf("%w: orchestrator is not configured", ErrDecommissionInvalidRequest))
	}
	if req.RangeID == 0 {
		return res, decommissionPhaseError(req, phase, fmt.Errorf("%w: range ID must be non-zero", ErrDecommissionInvalidRequest))
	}
	if req.Target == "" {
		return res, decommissionPhaseError(req, phase, fmt.Errorf("%w: target must not be empty", ErrDecommissionInvalidRequest))
	}
	if err := ctx.Err(); err != nil {
		return res, decommissionPhaseError(req, phase, decommissionContextError(ctx))
	}

	if d.log != nil {
		d.log.Debug("decommission starting", "range", req.RangeID, "target", req.Target)
	}

	view, err := d.preflight(ctx, req)
	if err != nil {
		return res, decommissionPhaseError(req, phase, err)
	}
	fillDecommissionResult(&res, view)
	if view.absent() {
		return d.finishAlreadyRemoved(ctx, req, view, res, phase)
	}
	d.reportProgress(phase, req, res, 0, "preflight passed")

	// Move leadership off the target before changing membership. The
	// RemoveVoter API rejects self-removal, so a leader target must hand off
	// first. A learner target cannot be leader, so this is voter-only.
	if view.leader == req.Target {
		phase = DecommissionPhaseTransferLeader
		if err := d.transferAway(ctx, req, view); err != nil {
			return res, decommissionPhaseError(req, phase, err)
		}
		res.Transferred = true
		view, err = d.preflight(ctx, req)
		if err != nil {
			return res, decommissionPhaseError(req, phase, err)
		}
		if view.absent() {
			return d.finishAlreadyRemoved(ctx, req, view, res, phase)
		}
		if view.leader == req.Target {
			return res, decommissionPhaseError(req, phase,
				fmt.Errorf("%w: leadership did not move away from %s", ErrDecommissionTransfer, req.Target))
		}
		fillDecommissionResult(&res, view)
		d.reportProgress(phase, req, res, 0, "leadership transferred")
	}

	// A learner must catch up before promotion. Promotion changes the voter
	// set and therefore quorum, so a lagging learner is never promoted.
	if view.targetLearner {
		phase = DecommissionPhaseCatchUp
		want := view.leaderStatus.CommitIndex
		d.reportProgress(phase, req, res, 0, fmt.Sprintf("waiting for learner to apply commit index %d", want))
		lag, err := d.waitCaughtUp(ctx, req, want)
		if err != nil {
			return res, decommissionPhaseError(req, phase, err)
		}
		res.LagEntries = lag
		view, err = d.inspect(ctx, req)
		if err != nil {
			return res, decommissionPhaseError(req, phase, err)
		}
		if view.absent() {
			return d.finishAlreadyRemoved(ctx, req, view, res, phase)
		}
		fillDecommissionResult(&res, view)
		d.reportProgress(phase, req, res, lag, "learner caught up")

		// A concurrent retry may already have promoted the target.
		if !view.targetVoter {
			phase = DecommissionPhasePromote
			if err := d.promoteLearner(ctx, req, view.leader); err != nil {
				return res, decommissionPhaseError(req, phase, err)
			}
			res.Promoted = true
			if err := d.waitForVoter(ctx, req); err != nil {
				return res, decommissionPhaseError(req, phase, err)
			}
			view, err = d.preflight(ctx, req)
			if err != nil {
				return res, decommissionPhaseError(req, phase, err)
			}
			if view.absent() {
				return d.finishAlreadyRemoved(ctx, req, view, res, phase)
			}
			fillDecommissionResult(&res, view)
			d.reportProgress(phase, req, res, lag, "learner promoted")
		}
	}

	// Revalidate the voter set and quorum after the leadership/promotion
	// steps. A target that is already gone is an idempotent success.
	phase = DecommissionPhaseRemoveVoter
	view, err = d.preflight(ctx, req)
	if err != nil {
		return res, decommissionPhaseError(req, phase, err)
	}
	if view.absent() {
		return d.finishAlreadyRemoved(ctx, req, view, res, phase)
	}
	if !view.targetVoter {
		return res, decommissionPhaseError(req, phase,
			fmt.Errorf("%w: target %s is still a learner after promotion", ErrDecommissionUnsafeConfig, req.Target))
	}
	// Promotion can theoretically elect the new voter. Do not call
	// RemoveVoter on a leader: transfer away again first.
	if view.leader == req.Target {
		phase = DecommissionPhaseTransferLeader
		if err := d.transferAway(ctx, req, view); err != nil {
			return res, decommissionPhaseError(req, phase, err)
		}
		res.Transferred = true
		view, err = d.preflight(ctx, req)
		if err != nil {
			return res, decommissionPhaseError(req, phase, err)
		}
		if view.absent() {
			return d.finishAlreadyRemoved(ctx, req, view, res, phase)
		}
		if view.leader == req.Target {
			return res, decommissionPhaseError(req, phase,
				fmt.Errorf("%w: leadership did not move away from %s", ErrDecommissionTransfer, req.Target))
		}
		fillDecommissionResult(&res, view)
		d.reportProgress(phase, req, res, 0, "leadership transferred before removal")
	}

	phase = DecommissionPhaseRemoveVoter
	if err := d.removeVoter(ctx, req, view.leader); err != nil {
		return res, decommissionPhaseError(req, phase, err)
	}

	view, err = d.inspectUntilLeader(ctx, req)
	if err != nil {
		return res, decommissionPhaseError(req, phase, err)
	}
	if view.targetVoter || view.targetLearner {
		return res, decommissionPhaseError(req, phase,
			fmt.Errorf("%w: %s is still present in the leader's membership", ErrDecommissionRemoval, req.Target))
	}
	fillDecommissionResult(&res, view)
	d.reportProgress(phase, req, res, res.LagEntries, "replica removed from raft")

	phase = DecommissionPhaseMeta
	if err := d.updateMeta(ctx, req, view, res); err != nil {
		return res, decommissionPhaseError(req, phase, err)
	}
	d.reportProgress(phase, req, res, res.LagEntries, "range map updated")

	phase = DecommissionPhaseDone
	d.reportProgress(phase, req, res, res.LagEntries, "decommission complete")
	if d.log != nil {
		d.log.Info("decommission complete", "range", req.RangeID, "target", req.Target,
			"leader", res.Leader, "transferred", res.Transferred, "promoted", res.Promoted,
			"already_removed", res.AlreadyRemoved, "voters", len(res.Voters))
	}
	return res, nil
}

// finishAlreadyRemoved handles the idempotent path: the target is no longer in
// the leader's voter/learner sets. Meta is updated again so a crash between the
// Raft removal and the map update converges on retry.
func (d *Decommissioner) finishAlreadyRemoved(ctx context.Context, req DecommissionRequest, view *decommissionView, res DecommissionResult, phase DecommissionPhase) (DecommissionResult, error) {
	res.AlreadyRemoved = true
	fillDecommissionResult(&res, view)
	d.reportProgress(phase, req, res, 0, "target already removed")

	if err := d.updateMeta(ctx, req, view, res); err != nil {
		return res, decommissionPhaseError(req, DecommissionPhaseMeta, err)
	}
	d.reportProgress(DecommissionPhaseMeta, req, res, 0, "range map updated")
	d.reportProgress(DecommissionPhaseDone, req, res, 0, "decommission already complete")
	return res, nil
}

// decommissionView is the leader's authoritative view plus per-replica statuses
// gathered during discovery.
type decommissionView struct {
	leader        string
	leaderStatus  DecommissionReplica
	statuses      map[string]DecommissionReplica
	voters        []string
	learners      []string
	targetVoter   bool
	targetLearner bool
}

func (v *decommissionView) absent() bool {
	return v != nil && !v.targetVoter && !v.targetLearner
}

// preflight discovers the leader, validates the target's membership, and
// enforces the last-voter/quorum safety checks. An absent target returns a
// view with absent()==true; Decommission then takes the idempotent path.
func (d *Decommissioner) preflight(ctx context.Context, req DecommissionRequest) (*decommissionView, error) {
	view, err := d.inspectUntilLeader(ctx, req)
	if err != nil {
		return nil, err
	}
	if view.absent() {
		return view, nil
	}

	// The set that will remain after the target is gone. For a learner this is
	// the current voter set (the promoted learner is removed again).
	post := withoutString(view.voters, req.Target)
	if len(view.voters) <= 1 && view.targetVoter {
		return nil, fmt.Errorf("%w: %s is the only voter of range %d", ErrDecommissionLastVoter, req.Target, req.RangeID)
	}
	if len(post) == 0 {
		return nil, fmt.Errorf("%w: no voter would remain after removing %s", ErrDecommissionLastVoter, req.Target)
	}

	// The membership change itself is a replicated entry committed under the
	// current voter set. A reachable current quorum is therefore a
	// precondition for either a removal or a learner promotion.
	currentRequired := decommissionQuorum(len(view.voters))
	currentHealthy := 0
	for _, node := range view.voters {
		if _, ok := view.statuses[node]; ok {
			currentHealthy++
		}
	}
	if currentHealthy < currentRequired {
		return nil, fmt.Errorf("%w: cannot change membership of range %d: %d of %d current voters are reachable (need %d)",
			ErrDecommissionQuorumLoss, req.RangeID, currentHealthy, len(view.voters), currentRequired)
	}

	// Removing a voter must leave a healthy quorum in the resulting voter set.
	// For a learner target, this checks the original voters that remain after
	// the temporary promotion and removal.
	required := decommissionQuorum(len(post))
	healthy := 0
	unreachable := make([]string, 0)
	for _, node := range post {
		if _, ok := view.statuses[node]; ok {
			healthy++
			continue
		}
		unreachable = append(unreachable, node)
	}
	if healthy < required {
		sort.Strings(unreachable)
		return nil, fmt.Errorf("%w: removing %s would leave %d healthy of %d voters %v (need %d)",
			ErrDecommissionQuorumLoss, req.Target, healthy, len(post), unreachable, required)
	}
	return view, nil
}

// inspectUntilLeader retries leader discovery for StepTimeout when no replica
// currently reports leadership. A removal or promotion can trigger an
// election, and a leaderless window is expected; retrying is what makes a
// closely following idempotent retry converge instead of failing on the
// election gap. Split-brain and configuration errors are returned immediately.
func (d *Decommissioner) inspectUntilLeader(ctx context.Context, req DecommissionRequest) (*decommissionView, error) {
	deadline := time.Now().Add(d.opts.StepTimeout)
	for {
		view, err := d.inspect(ctx, req)
		if err == nil {
			return view, nil
		}
		if !errors.Is(err, ErrDecommissionNoLeader) {
			return nil, err
		}
		if !time.Now().Before(deadline) {
			return nil, err
		}
		if sleepErr := sleepDecommissionPoll(ctx, d.opts.PollInterval, deadline); sleepErr != nil {
			return nil, sleepErr
		}
	}
}

// inspect queries every known replica, expands the set using leader hints and
// the leader's membership, and returns the highest-term leader's view. It
// fails closed on two leaders in the same term.
func (d *Decommissioner) inspect(ctx context.Context, req DecommissionRequest) (*decommissionView, error) {
	if err := ctx.Err(); err != nil {
		return nil, decommissionContextError(ctx)
	}

	pending := make(map[string]struct{}, len(req.Replicas)+2)
	addNode := func(node string) {
		if node == "" {
			return
		}
		if _, ok := pending[node]; !ok {
			pending[node] = struct{}{}
		}
	}
	addNode(req.Target)
	for _, node := range req.Replicas {
		addNode(node)
	}

	statuses := make(map[string]DecommissionReplica, len(pending))
	var lastErr error

	// Three rounds are enough to start from Target/Replicas, learn the leader
	// hint from any follower, and then query that leader.
	for round := 0; round < 3; round++ {
		nodes := make([]string, 0, len(pending))
		for node := range pending {
			if _, ok := statuses[node]; !ok {
				nodes = append(nodes, node)
			}
		}
		if len(nodes) == 0 {
			break
		}
		sort.Strings(nodes)
		expanded := false
		for _, node := range nodes {
			if err := ctx.Err(); err != nil {
				return nil, decommissionContextError(ctx)
			}
			st, err := d.status(ctx, req.RangeID, node)
			if err != nil {
				lastErr = fmt.Errorf("%s: %w", node, err)
				continue
			}
			statuses[node] = st
			expanded = true
			for _, peer := range append(append([]string(nil), st.Voters...), st.Learners...) {
				if _, ok := pending[peer]; !ok && peer != "" {
					pending[peer] = struct{}{}
					expanded = true
				}
			}
			if st.Leader != "" {
				if _, ok := pending[st.Leader]; !ok {
					pending[st.Leader] = struct{}{}
					expanded = true
				}
			}
			if len(statuses) > decommissionMaxReplicaSet {
				return nil, fmt.Errorf("%w: replica set exceeds %d nodes", ErrDecommissionInvalidRequest, decommissionMaxReplicaSet)
			}
		}
		if !expanded {
			break
		}
	}

	if len(statuses) == 0 {
		if lastErr != nil {
			return nil, fmt.Errorf("%w: no replica answered: %w", ErrDecommissionNoLeader, lastErr)
		}
		return nil, fmt.Errorf("%w: no replicas supplied for range %d", ErrDecommissionNoLeader, req.RangeID)
	}

	leaders := make([]DecommissionReplica, 0, 1)
	for _, st := range statuses {
		if st.Role == raft.Leader {
			leaders = append(leaders, st)
		}
	}
	if len(leaders) == 0 {
		if err := ctx.Err(); err != nil {
			return nil, decommissionContextError(ctx)
		}
		if lastErr != nil {
			return nil, fmt.Errorf("%w: %d replicas answered but none is leader (%v)", ErrDecommissionNoLeader, len(statuses), lastErr)
		}
		return nil, fmt.Errorf("%w: %d replicas answered but none is leader", ErrDecommissionNoLeader, len(statuses))
	}
	sort.Slice(leaders, func(i, j int) bool {
		if leaders[i].Term != leaders[j].Term {
			return leaders[i].Term > leaders[j].Term
		}
		return leaders[i].ID < leaders[j].ID
	})
	if len(leaders) > 1 && leaders[0].Term == leaders[1].Term {
		return nil, fmt.Errorf("%w: %s and %s both claim term %d",
			ErrDecommissionSplitBrain, leaders[0].ID, leaders[1].ID, leaders[0].Term)
	}

	leader := leaders[0]
	voters := sortedUniqueStrings(leader.Voters)
	learners := sortedUniqueStrings(leader.Learners)
	if len(voters) == 0 {
		return nil, fmt.Errorf("%w: leader %s reports an empty voter set", ErrDecommissionNoLeader, leader.ID)
	}
	if !containsString(voters, leader.ID) {
		return nil, fmt.Errorf("%w: leader %s is not in its own voter set", ErrDecommissionUnsafeConfig, leader.ID)
	}

	view := &decommissionView{
		leader:       leader.ID,
		leaderStatus: leader,
		statuses:     statuses,
		voters:       voters,
		learners:     learners,
		targetVoter:  containsString(voters, req.Target),
	}
	view.targetLearner = !view.targetVoter && containsString(learners, req.Target)
	return view, nil
}

// status wraps one control Status call with the step timeout.
func (d *Decommissioner) status(ctx context.Context, rangeID uint64, node string) (DecommissionReplica, error) {
	callCtx, cancel := context.WithTimeout(ctx, d.opts.StepTimeout)
	defer cancel()
	st, err := d.control.Status(callCtx, rangeID, node)
	if err != nil {
		return DecommissionReplica{}, err
	}
	if st.ID == "" {
		st.ID = node
	}
	return st, nil
}

// transferAway hands leadership from the target to a healthy remaining voter.
// It tries candidates in deterministic order and rechecks whether a concurrent
// change already moved leadership.
func (d *Decommissioner) transferAway(ctx context.Context, req DecommissionRequest, view *decommissionView) error {
	candidates := make([]string, 0, len(view.voters))
	for _, node := range withoutString(view.voters, req.Target) {
		if _, ok := view.statuses[node]; ok {
			candidates = append(candidates, node)
		}
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		return fmt.Errorf("%w: no healthy voter to take leadership from %s", ErrDecommissionNoCandidate, req.Target)
	}

	var lastErr error
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return decommissionContextError(ctx)
		}
		callCtx, cancel := context.WithTimeout(ctx, d.opts.StepTimeout)
		err := d.control.TransferLeadership(callCtx, req.RangeID, req.Target, candidate)
		cancel()
		if err == nil {
			if werr := d.waitForNewLeader(ctx, req); werr != nil {
				return werr
			}
			return nil
		}
		lastErr = errors.Join(lastErr, fmt.Errorf("%s: %w", candidate, err))

		// ErrNotLeader means leadership already moved; confirm instead of
		// trying another candidate.
		if errors.Is(err, raft.ErrNotLeader) {
			current, ierr := d.inspect(ctx, req)
			if ierr == nil && current.leader != req.Target {
				return nil
			}
		}
		if err := ctx.Err(); err != nil {
			return decommissionContextError(ctx)
		}
	}
	return fmt.Errorf("%w: target %s: %w", ErrDecommissionTransfer, req.Target, lastErr)
}

// waitForNewLeader polls until the range leader is no longer target.
func (d *Decommissioner) waitForNewLeader(ctx context.Context, req DecommissionRequest) error {
	deadline := time.Now().Add(d.opts.StepTimeout)
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return decommissionContextError(ctx)
		}
		view, err := d.inspect(ctx, req)
		if err == nil && view.leader != req.Target {
			return nil
		}
		if err != nil {
			lastErr = err
		}
		if !time.Now().Before(deadline) {
			return errors.Join(ErrDecommissionTransfer, ErrDecommissionTimeout,
				fmt.Errorf("leader is still %s", req.Target), lastErr)
		}
		if err := sleepDecommissionPoll(ctx, d.opts.PollInterval, deadline); err != nil {
			return err
		}
	}
}

// waitCaughtUp waits until the target has applied want (the leader's commit
// index observed at the start of the wait) and both its last-index lag and its
// applied lag are within MaxLagEntries. It polls the leader's current view so a
// leadership change during catch-up is handled.
func (d *Decommissioner) waitCaughtUp(ctx context.Context, req DecommissionRequest, want uint64) (uint64, error) {
	deadline := time.Now().Add(d.opts.CatchUpTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	var lastLag uint64
	var lastApplied uint64
	var lastAppliedLag uint64
	var observed bool
	var lastErr error

	for {
		if err := decommissionCtxOrDeadlineError(ctx); err != nil {
			return lastLag, err
		}
		view, err := d.inspect(ctx, req)
		if err == nil {
			if st, ok := view.statuses[req.Target]; ok {
				observed = true
				lastApplied = st.LastApplied
				lastLag = saturatingSub(view.leaderStatus.LastIndex, st.LastIndex)
				lastAppliedLag = saturatingSub(view.leaderStatus.CommitIndex, st.LastApplied)
				if st.LastApplied >= want &&
					lastLag <= d.opts.MaxLagEntries &&
					lastAppliedLag <= d.opts.MaxLagEntries {
					return lastLag, nil
				}
			}
		} else {
			lastErr = err
		}
		if !time.Now().Before(deadline) {
			if err := decommissionCtxOrDeadlineError(ctx); err != nil {
				return lastLag, err
			}
			if !observed {
				return lastLag, errors.Join(ErrDecommissionCatchUp, ErrDecommissionTimeout,
					fmt.Errorf("target %s status unavailable: %v", req.Target, lastErr))
			}
			return lastLag, errors.Join(ErrDecommissionCatchUp, ErrDecommissionTimeout,
				fmt.Errorf("target %s last_applied=%d (need >= %d), last-index lag=%d, applied lag=%d (threshold %d)",
					req.Target, lastApplied, want, lastLag, lastAppliedLag, d.opts.MaxLagEntries))
		}
		if err := sleepDecommissionPoll(ctx, d.opts.PollInterval, deadline); err != nil {
			return lastLag, err
		}
	}
}

// waitForVoter polls until the leader's membership contains the promoted
// target. PromoteLearner already waits for apply on the leader, so this is a
// short confirmation loop, not a second membership protocol.
func (d *Decommissioner) waitForVoter(ctx context.Context, req DecommissionRequest) error {
	deadline := time.Now().Add(d.opts.StepTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return decommissionContextError(ctx)
		}
		view, err := d.inspect(ctx, req)
		if err == nil && containsString(view.voters, req.Target) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return errors.Join(ErrDecommissionPromotion, ErrDecommissionTimeout,
				fmt.Errorf("%s did not appear in the leader's voter set", req.Target))
		}
		if err := sleepDecommissionPoll(ctx, d.opts.PollInterval, deadline); err != nil {
			return err
		}
	}
}

// promoteLearner promotes a caught-up learner. Already being a voter is an
// idempotent success.
func (d *Decommissioner) promoteLearner(ctx context.Context, req DecommissionRequest, leader string) error {
	callCtx, cancel := context.WithTimeout(ctx, d.opts.StepTimeout)
	err := d.control.PromoteLearner(callCtx, req.RangeID, leader, req.Target)
	cancel()
	switch {
	case err == nil:
		return nil
	case errors.Is(err, raft.ErrAlreadyVoter):
		return nil
	case errors.Is(err, raft.ErrNotLearner):
		// A concurrent retry may have promoted the target after the status
		// snapshot. Confirm against the leader before failing.
		view, ierr := d.inspect(ctx, req)
		if ierr == nil && view.targetVoter {
			return nil
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(ErrDecommissionPromotion, decommissionContextError(ctx), err)
	}
	return fmt.Errorf("%w: %s on %s: %w", ErrDecommissionPromotion, req.Target, leader, err)
}

// removeVoter removes the target and treats an absent voter as idempotent
// success.
func (d *Decommissioner) removeVoter(ctx context.Context, req DecommissionRequest, leader string) error {
	callCtx, cancel := context.WithTimeout(ctx, d.opts.StepTimeout)
	err := d.control.RemoveVoter(callCtx, req.RangeID, leader, req.Target)
	cancel()
	switch {
	case err == nil:
		return nil
	case errors.Is(err, raft.ErrNotVoter):
		return nil
	case errors.Is(err, raft.ErrUnsafeConfigChange):
		return errors.Join(ErrDecommissionUnsafeConfig, fmt.Errorf("%s: %w", req.Target, err))
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(ErrDecommissionRemoval, decommissionContextError(ctx), err)
	}
	return fmt.Errorf("%w: %s on %s: %w", ErrDecommissionRemoval, req.Target, leader, err)
}

// updateMeta calls the injected range-map updater after the Raft change. It is
// intentionally not wrapped in StepTimeout: the updater may need to propagate
// the new epoch to every remaining replica and is responsible for its own
// progress, bounded by the caller's context.
func (d *Decommissioner) updateMeta(ctx context.Context, req DecommissionRequest, view *decommissionView, res DecommissionResult) error {
	if err := ctx.Err(); err != nil {
		return decommissionContextError(ctx)
	}
	update := DecommissionMetaUpdate{
		RangeID:        req.RangeID,
		Target:         req.Target,
		Leader:         view.leader,
		Voters:         cloneStrings(view.voters),
		Learners:       cloneStrings(view.learners),
		Epoch:          req.Epoch,
		Removed:        view.absent(),
		AlreadyRemoved: res.AlreadyRemoved,
	}
	if err := d.meta.UpdateRangeMap(ctx, update); err != nil {
		if ctxErr := decommissionContextError(ctx); ctxErr != nil {
			return errors.Join(ErrDecommissionMeta, ctxErr, err)
		}
		return fmt.Errorf("%w: %w", ErrDecommissionMeta, err)
	}
	return nil
}

// reportProgress delivers one phase transition to the optional callback.
func (d *Decommissioner) reportProgress(phase DecommissionPhase, req DecommissionRequest, res DecommissionResult, lag uint64, msg string) {
	if d == nil || d.opts.Progress == nil {
		return
	}
	d.report(DecommissionProgress{
		Phase:      phase,
		RangeID:    req.RangeID,
		Target:     req.Target,
		Leader:     res.Leader,
		Voters:     cloneStrings(res.Voters),
		Learners:   cloneStrings(res.Learners),
		LagEntries: lag,
		Message:    msg,
	})
}

func (d *Decommissioner) report(p DecommissionProgress) {
	if d == nil || d.opts.Progress == nil {
		return
	}
	d.opts.Progress(p)
}

func fillDecommissionResult(res *DecommissionResult, view *decommissionView) {
	if res == nil || view == nil {
		return
	}
	res.Leader = view.leader
	res.Voters = cloneStrings(view.voters)
	res.Learners = cloneStrings(view.learners)
}

// decommissionPhaseError wraps err once. It exists as a free function so a nil
// Decommissioner can still return a typed error.
func decommissionPhaseError(req DecommissionRequest, phase DecommissionPhase, err error) error {
	if err == nil {
		return nil
	}
	var de *DecommissionError
	if errors.As(err, &de) {
		return err
	}
	return &DecommissionError{Phase: phase, RangeID: req.RangeID, Target: req.Target, Err: err}
}

// decommissionContextError preserves both the package sentinel and the
// context sentinel in the error chain.
func decommissionContextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	switch ctx.Err() {
	case context.DeadlineExceeded:
		return errors.Join(ErrDecommissionTimeout, context.DeadlineExceeded)
	case context.Canceled:
		return errors.Join(ErrDecommissionCanceled, context.Canceled)
	default:
		return nil
	}
}

// decommissionCtxOrDeadlineError closes the small race where a context's
// deadline has passed but ctx.Err() has not yet observed it: a timer-based
// wait can wake at the deadline before the context goroutine marks itself
// done. Callers that use the caller's deadline as their own wait bound use
// this to report context.DeadlineExceeded deterministically.
func decommissionCtxOrDeadlineError(ctx context.Context) error {
	if err := decommissionContextError(ctx); err != nil {
		return err
	}
	if ctx == nil {
		return nil
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return errors.Join(ErrDecommissionTimeout, context.DeadlineExceeded)
	}
	return nil
}

// sleepDecommissionPoll waits for one poll interval, the deadline, or context
// cancellation. It never starts a goroutine.
func sleepDecommissionPoll(ctx context.Context, interval time.Duration, deadline time.Time) error {
	wait := interval
	if remaining := time.Until(deadline); remaining < wait {
		wait = remaining
	}
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return decommissionContextError(ctx)
	case <-timer.C:
		return nil
	}
}

func decommissionQuorum(voters int) int {
	if voters <= 0 {
		return 1
	}
	return voters/2 + 1
}

func saturatingSub(a, b uint64) uint64 {
	if a <= b {
		return 0
	}
	return a - b
}

func withoutString(in []string, drop string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v != drop {
			out = append(out, v)
		}
	}
	return out
}

func sortedUniqueStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v != "" {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	if len(out) < 2 {
		return out
	}
	dedup := out[:1]
	for _, v := range out[1:] {
		if v != dedup[len(dedup)-1] {
			dedup = append(dedup, v)
		}
	}
	return dedup
}
