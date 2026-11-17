package kvctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"distributed-db/internal/grpctls"
	"distributed-db/internal/kvserver"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"
	"distributed-db/internal/v2client"
)

// DecommissionOptions configures one `kvctl decommission` run. Zero durations
// select the kvserver package defaults.
type DecommissionOptions struct {
	RangeID  uint64
	Target   string
	Replicas []string

	CatchUpTimeout time.Duration
	StepTimeout    time.Duration
	PollInterval   time.Duration
}

// DecommissionResult is the transport-neutral outcome printed by the CLI.
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

const (
	// decommissionRPCTimeout bounds one outbound KV admin RPC made by the
	// adapter. The orchestrator's own step timeout is the primary bound; this
	// is a backstop so an invoked RPC cannot hang a retry loop forever.
	decommissionRPCTimeout = 10 * time.Second
	// decommissionEpochTimeout bounds one UpdateRangeEpoch call during the
	// meta range-map publication step.
	decommissionEpochTimeout = 10 * time.Second
)

// Decommission runs the existing kvserver orchestrator against a live cluster.
// The orchestrator is unchanged: this method supplies a KVService-RPC
// DecommissionControl and a v2client-backed meta range-map updater.
func (b *v2Backend) Decommission(ctx context.Context, opts DecommissionOptions) (DecommissionResult, error) {
	if b == nil || b.client == nil {
		return DecommissionResult{}, errors.New("decommission: backend is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.RangeID == 0 {
		return DecommissionResult{}, errors.New("decommission: range ID must be greater than zero")
	}
	if strings.TrimSpace(opts.Target) == "" {
		return DecommissionResult{}, errors.New("decommission: target must not be empty")
	}
	if len(b.cfg.Nodes) == 0 {
		return DecommissionResult{}, errors.New("decommission: at least one KV node address is required")
	}

	dialer, err := newKVAdminDialer(b.cfg)
	if err != nil {
		return DecommissionResult{}, fmt.Errorf("decommission: transport: %w", err)
	}
	defer func() { _ = dialer.Close() }()

	control := &rpcDecommissionControl{dial: dialer.Dial, token: b.cfg.AdminToken}
	updater := &metaRangeUpdater{
		client:     b.client,
		dial:       dialer.Dial,
		adminToken: b.cfg.AdminToken,
	}
	d, err := kvserver.NewDecommissioner(control, updater, kvserver.DecommissionOptions{
		CatchUpTimeout: opts.CatchUpTimeout,
		StepTimeout:    opts.StepTimeout,
		PollInterval:   opts.PollInterval,
		// The CLI reports through its own output; keep library log noise out
		// of the operator's terminal and never log a credential.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		return DecommissionResult{}, err
	}

	res, err := d.Decommission(ctx, kvserver.DecommissionRequest{
		RangeID:  opts.RangeID,
		Target:   strings.TrimSpace(opts.Target),
		Replicas: append([]string(nil), opts.Replicas...),
	})
	if err != nil {
		return DecommissionResult{}, err
	}
	return DecommissionResult{
		RangeID:        res.RangeID,
		Target:         res.Target,
		Leader:         res.Leader,
		Voters:         append([]string(nil), res.Voters...),
		Learners:       append([]string(nil), res.Learners...),
		Transferred:    res.Transferred,
		Promoted:       res.Promoted,
		AlreadyRemoved: res.AlreadyRemoved,
		LagEntries:     res.LagEntries,
	}, nil
}

// kvAdminDialer opens pooled KV node connections using the CLI's transport
// security. The admin token is not a dial option: the adapter decorates only
// admin RPC contexts with kvserver.WithAdminToken, so read-only Status calls
// stay unauthenticated and a single connection serves both.
type kvAdminDialer struct {
	creds credentials.TransportCredentials

	mu    sync.Mutex
	conns map[string]*grpc.ClientConn
}

func newKVAdminDialer(cfg Config) (*kvAdminDialer, error) {
	creds := credentials.TransportCredentials(insecure.NewCredentials())
	tlsOpts, err := grpctls.NewClientOptions(cfg.TLSCA, cfg.TLSCert, cfg.TLSKey, cfg.TLSServerName)
	if err != nil {
		return nil, err
	}
	if tlsOpts != nil {
		creds, err = grpctls.ClientCredentials(*tlsOpts)
		if err != nil {
			return nil, err
		}
	}
	return &kvAdminDialer{creds: creds, conns: make(map[string]*grpc.ClientConn)}, nil
}

func (d *kvAdminDialer) Dial(addr string) (*grpc.ClientConn, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, errors.New("empty node address")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if cc, ok := d.conns[addr]; ok {
		return cc, nil
	}
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(d.creds))
	if err != nil {
		return nil, err
	}
	d.conns[addr] = cc
	return cc, nil
}

func (d *kvAdminDialer) Close() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	conns := d.conns
	d.conns = nil
	d.mu.Unlock()
	var errs []error
	for _, cc := range conns {
		if cc != nil {
			errs = append(errs, cc.Close())
		}
	}
	return errors.Join(errs...)
}

// rpcDecommissionControl maps the transport-neutral DecommissionControl seam
// onto the existing KVService RPCs. Response-body errors are translated back to
// the raft sentinels the orchestrator uses for idempotency.
type rpcDecommissionControl struct {
	dial  func(string) (*grpc.ClientConn, error)
	token string
}

func (c *rpcDecommissionControl) client(node string) (pbv2.KVServiceClient, error) {
	if c == nil || c.dial == nil {
		return nil, errors.New("decommission: no KV dialer")
	}
	cc, err := c.dial(node)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", node, err)
	}
	return pbv2.NewKVServiceClient(cc), nil
}

func (c *rpcDecommissionControl) Status(ctx context.Context, rangeID uint64, node string) (kvserver.DecommissionReplica, error) {
	client, err := c.client(node)
	if err != nil {
		return kvserver.DecommissionReplica{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, decommissionRPCTimeout)
	defer cancel()
	resp, err := client.Status(callCtx, &pbv2.StatusRequest{RangeId: rangeID})
	if err != nil {
		return kvserver.DecommissionReplica{}, err
	}
	return decommissionReplicaFromStatus(node, resp), nil
}

// decommissionReplicaFromStatus converts the wire view. StatusResponse has no
// last_index field, so LastIndex is the best available conservative bound
// (max(commit_index, last_applied)). The applied-lag check (leader commit vs
// target last_applied) remains authoritative; the raw last-index lag is
// effectively disabled for RPC-driven decommission. This is a documented gap
// until the proto exposes last_index.
func decommissionReplicaFromStatus(node string, resp *pbv2.StatusResponse) kvserver.DecommissionReplica {
	lastIndex := resp.GetCommitIndex()
	if resp.GetLastApplied() > lastIndex {
		lastIndex = resp.GetLastApplied()
	}
	id := resp.GetId()
	if id == "" {
		id = node
	}
	return kvserver.DecommissionReplica{
		ID:          id,
		Role:        decommissionRole(resp.GetRole()),
		Leader:      resp.GetLeader(),
		Term:        resp.GetTerm(),
		CommitIndex: resp.GetCommitIndex(),
		LastIndex:   lastIndex,
		LastApplied: resp.GetLastApplied(),
		Voters:      append([]string(nil), resp.GetVoters()...),
		Learners:    append([]string(nil), resp.GetLearners()...),
		Learner:     resp.GetLearner(),
		ConfigEpoch: resp.GetEpoch(),
	}
}

func decommissionRole(role string) raft.Role {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case string(raft.Leader):
		return raft.Leader
	case string(raft.Candidate):
		return raft.Candidate
	default:
		return raft.Follower
	}
}

func (c *rpcDecommissionControl) TransferLeadership(ctx context.Context, rangeID uint64, leader, target string) error {
	client, err := c.client(leader)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, decommissionRPCTimeout)
	defer cancel()
	resp, err := client.TransferLeader(kvserver.WithAdminToken(callCtx, c.token), &pbv2.TransferLeaderRequest{
		RangeId: rangeID,
		Target:  target,
	})
	if err != nil {
		return err
	}
	if resp.GetSuccess() {
		return nil
	}
	return decommissionRPCRejection("transfer leadership", resp.GetError(), resp.GetLeader())
}

func (c *rpcDecommissionControl) PromoteLearner(ctx context.Context, rangeID uint64, leader, target string) error {
	client, err := c.client(leader)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, decommissionRPCTimeout)
	defer cancel()
	resp, err := client.PromoteReplica(kvserver.WithAdminToken(callCtx, c.token), &pbv2.PromoteReplicaRequest{
		RangeId: rangeID,
		Target:  target,
	})
	if err != nil {
		return err
	}
	if resp.GetSuccess() {
		return nil
	}
	return decommissionRPCRejection("promote replica", resp.GetError(), resp.GetLeader())
}

func (c *rpcDecommissionControl) RemoveVoter(ctx context.Context, rangeID uint64, leader, target string) error {
	client, err := c.client(leader)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, decommissionRPCTimeout)
	defer cancel()
	resp, err := client.RemoveVoter(kvserver.WithAdminToken(callCtx, c.token), &pbv2.RemoveVoterRequest{
		RangeId: rangeID,
		Target:  target,
	})
	if err != nil {
		return err
	}
	if resp.GetSuccess() {
		return nil
	}
	return decommissionRPCRejection("remove voter", resp.GetError(), resp.GetLeader())
}

// decommissionRPCRejection converts a KVService response-body error to an
// error wrapping the matching raft sentinel. The token is never part of the
// message.
func decommissionRPCRejection(op, msg, leader string) error {
	if strings.TrimSpace(msg) == "" {
		msg = "unspecified failure"
	}
	if leader != "" {
		msg = fmt.Sprintf("%s (leader=%s)", msg, leader)
	}
	if sentinel := decommissionRPCErrorSentinel(msg); sentinel != nil {
		return fmt.Errorf("%s rejected: %s: %w", op, msg, sentinel)
	}
	return fmt.Errorf("%s rejected: %s", op, msg)
}

func decommissionRPCErrorSentinel(msg string) error {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "not leader"):
		return raft.ErrNotLeader
	case strings.Contains(lower, "unknown transfer target"):
		return raft.ErrUnknownTarget
	case strings.Contains(lower, "not caught up"):
		return raft.ErrTargetNotCaughtUp
	case strings.Contains(lower, "not a voter"), strings.Contains(lower, "not voter"):
		return raft.ErrNotVoter
	case strings.Contains(lower, "not a learner"), strings.Contains(lower, "not learner"):
		return raft.ErrNotLearner
	case strings.Contains(lower, "already a voter"), strings.Contains(lower, "already voter"):
		return raft.ErrAlreadyVoter
	case strings.Contains(lower, "cannot remove the last voter"):
		return raft.ErrUnsafeConfigChange
	default:
		return nil
	}
}

// metaRangeUpdater publishes the post-removal voter set through MetaService
// SetRanges. It reads the current map first so a SetRanges retry never clobbers
// an unrelated range, and it propagates the new version to the remaining
// replicas before publishing the map (the same ordering the meta scheduler
// uses for split/merge).
type metaRangeUpdater struct {
	client     *v2client.Client
	dial       func(string) (*grpc.ClientConn, error)
	adminToken string
}

func (u *metaRangeUpdater) UpdateRangeMap(ctx context.Context, update kvserver.DecommissionMetaUpdate) error {
	if u == nil || u.client == nil {
		return errors.New("decommission: meta updater is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if update.RangeID == 0 {
		return errors.New("decommission: meta update has no range ID")
	}
	if !update.Removed {
		return fmt.Errorf("decommission: refusing to publish range %d: target %q is still a member",
			update.RangeID, update.Target)
	}
	want := sortedUniqueNonEmpty(update.Voters)
	if len(want) == 0 {
		return fmt.Errorf("decommission: refusing to publish range %d with no voters", update.RangeID)
	}
	if containsString(want, update.Target) {
		return fmt.Errorf("decommission: refusing to publish range %d: target %q is still a voter",
			update.RangeID, update.Target)
	}

	rows, err := u.client.ListRanges(ctx)
	if err != nil {
		return fmt.Errorf("read range map: %w", err)
	}
	idx := -1
	for i := range rows {
		if rows[i].ID == update.RangeID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("range %d not found in the meta range map", update.RangeID)
	}
	current := rows[idx]

	if !containsString(current.Replicas, update.Target) {
		// The map already excludes the target. That is the crash-recovery
		// path; it is only a success if the remaining set is already exactly
		// what Raft reports. Anything else is a concurrent placement change
		// that this orchestrator must not overwrite.
		if sameReplicaSet(current.Replicas, want) {
			return nil
		}
		return fmt.Errorf("range %d map replicas %v changed concurrently; want %v",
			update.RangeID, current.Replicas, want)
	}

	newVersion := nextRangeVersion(current.Version)
	if err := u.propagateEpoch(ctx, want, update.RangeID, newVersion); err != nil {
		return fmt.Errorf("propagate range version %d: %w", newVersion, err)
	}

	next := make([]v2client.RangeInfo, 0, len(rows))
	next = append(next, rows...)
	updated := current
	updated.Replicas = want
	updated.Version = newVersion
	if updated.Leader == update.Target || !containsString(want, updated.Leader) {
		updated.Leader = ""
	}
	next[idx] = updated
	if err := u.client.SetRanges(ctx, next); err != nil {
		return fmt.Errorf("set range map: %w", err)
	}

	after, err := u.client.ListRanges(ctx)
	if err != nil {
		return fmt.Errorf("verify range map: %w", err)
	}
	for i := range after {
		if after[i].ID != update.RangeID {
			continue
		}
		if containsString(after[i].Replicas, update.Target) || !sameReplicaSet(after[i].Replicas, want) {
			return fmt.Errorf("published range %d replicas %v, want %v",
				update.RangeID, after[i].Replicas, want)
		}
		if after[i].Version != newVersion {
			return fmt.Errorf("published range %d version %d, want %d",
				update.RangeID, after[i].Version, newVersion)
		}
		return nil
	}
	return fmt.Errorf("range %d disappeared from the meta range map after SetRanges", update.RangeID)
}

func (u *metaRangeUpdater) propagateEpoch(ctx context.Context, replicas []string, rangeID, epoch uint64) error {
	for _, addr := range replicas {
		if err := u.propagateEpochOne(ctx, addr, rangeID, epoch); err != nil {
			return fmt.Errorf("%s: %w", addr, err)
		}
	}
	return nil
}

func (u *metaRangeUpdater) propagateEpochOne(ctx context.Context, addr string, rangeID, epoch uint64) error {
	if u.dial == nil {
		return errors.New("no KV dialer")
	}
	cc, err := u.dial(addr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, decommissionEpochTimeout)
	defer cancel()
	resp, err := pbv2.NewKVServiceClient(cc).UpdateRangeEpoch(
		kvserver.WithAdminToken(callCtx, u.adminToken),
		&pbv2.UpdateRangeEpochRequest{RangeId: rangeID, Epoch: epoch})
	if err != nil {
		return err
	}
	if !resp.GetSuccess() {
		msg := resp.GetError()
		if strings.TrimSpace(msg) == "" {
			msg = "unspecified failure"
		}
		return errors.New(msg)
	}
	return nil
}

func nextRangeVersion(version uint64) uint64 {
	if version == 0 {
		return 1
	}
	return version + 1
}

func sortedUniqueNonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
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

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
