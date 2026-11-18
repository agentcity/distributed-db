package v2client

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	pbv2 "distributed-db/internal/pb/v2"
)

const (
	// rangeHealthProbeTimeout bounds one KVService.Status call made by
	// ListRangesWithHealth.
	rangeHealthProbeTimeout = 400 * time.Millisecond
	// rangeHealthRangeTimeout bounds all probes for one range.
	rangeHealthRangeTimeout = 2 * time.Second
	// rangeHealthTotalTimeout bounds the whole health pass so a large or
	// partially unreachable cluster cannot stall the CLI for minutes. Ranges
	// not reached in time keep the metadata-only leader hint.
	rangeHealthTotalTimeout = 5 * time.Second
	// rangeHealthWorkers caps concurrent range probes.
	rangeHealthWorkers = 8
)

// RangeInfo is one meta range descriptor. Start is inclusive and End is
// exclusive; a nil boundary means the range is unbounded on that side.
//
// RangeInfo is intentionally a plain value type so administrative callers do
// not depend on the generated protobuf types.
//
// Leader and Version are the values carried by the wire descriptor. The health
// fields are filled only by ListRangesWithHealth (and are zero after a plain
// ListRanges): they come from best-effort KVService.Status probes, never from
// the metadata service, and never invent a leader. LeaderKnown is false when no
// leader hint is known; LiveVoters contains only replicas that answered Status
// as voters during the probe; Epoch is the descriptor version against which the
// probe was made; Stale reports that the wire leader hint disagreed with the
// leader observed live.
type RangeInfo struct {
	ID       uint64
	Start    []byte
	End      []byte
	Replicas []string
	Leader   string
	Version  uint64

	LeaderKnown  bool
	ReplicaCount int
	LiveVoters   []string
	Epoch        uint64
	Stale        bool
}

// metaAPI is the subset of pbv2.MetaServiceClient used by the client. Keeping
// it as an interface lets tests inject an in-memory meta service without
// changing the public API.
type metaAPI interface {
	AllocTS(ctx context.Context, in *pbv2.AllocTSRequest, opts ...grpc.CallOption) (*pbv2.AllocTSResponse, error)
	GetRange(ctx context.Context, in *pbv2.GetRangeRequest, opts ...grpc.CallOption) (*pbv2.RangeInfo, error)
	Leader(ctx context.Context, in *pbv2.LeaderRequest, opts ...grpc.CallOption) (*pbv2.LeaderResponse, error)
	ListRanges(ctx context.Context, in *pbv2.ListRangesRequest, opts ...grpc.CallOption) (*pbv2.ListRangesResponse, error)
	SetRanges(ctx context.Context, in *pbv2.SetRangesRequest, opts ...grpc.CallOption) (*pbv2.SetRangesResponse, error)
	SplitRange(ctx context.Context, in *pbv2.MetaSplitRangeRequest, opts ...grpc.CallOption) (*pbv2.MetaSplitRangeResponse, error)
	MergeRanges(ctx context.Context, in *pbv2.MetaMergeRangesRequest, opts ...grpc.CallOption) (*pbv2.MetaMergeRangesResponse, error)
}

// adminContext attaches the configured admin Bearer credential to an outgoing
// meta admin call. Read-only and data-plane calls deliberately bypass it so
// the token is never sent where it is not required.
func (c *Client) adminContext(ctx context.Context) context.Context {
	if c.adminToken == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+c.adminToken)
}

type metaEntry struct {
	cc *grpc.ClientConn
	mc metaAPI
}

// dialMeta opens one meta connection with the given dial options. Callers that
// need TLS pass the transport credentials built by the client constructor;
// there is no plaintext fallback here.
func dialMeta(addr string, opts ...grpc.DialOption) (*grpc.ClientConn, metaAPI, error) {
	cc, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, nil, err
	}
	return cc, pbv2.NewMetaServiceClient(cc), nil
}

// metaClientForAddr returns a pooled meta client for addr, dialing at most
// once per address for the lifetime of the Client.
func (c *Client) metaClientForAddr(addr string) (metaAPI, error) {
	c.metaMu.Lock()
	if c.metaClosed {
		c.metaMu.Unlock()
		return nil, errClientClosed
	}
	if e, ok := c.metaPool[addr]; ok && e != nil {
		mc := e.mc
		c.metaMu.Unlock()
		return mc, nil
	}
	dial := c.metaDial
	c.metaMu.Unlock()
	if dial == nil {
		return nil, errors.New("v2client: no meta dialer")
	}

	cc, mc, err := dial(addr)
	if err != nil {
		return nil, err
	}
	c.metaMu.Lock()
	if c.metaClosed {
		c.metaMu.Unlock()
		if cc != nil {
			_ = cc.Close()
		}
		return nil, errClientClosed
	}
	if e, ok := c.metaPool[addr]; ok && e != nil {
		// Another goroutine won the race.
		c.metaMu.Unlock()
		if cc != nil {
			_ = cc.Close()
		}
		return e.mc, nil
	}
	if c.metaPool == nil {
		c.metaPool = make(map[string]*metaEntry)
	}
	c.metaPool[addr] = &metaEntry{cc: cc, mc: mc}
	c.metaMu.Unlock()
	return mc, nil
}

func (c *Client) metaGeneration() uint64 {
	c.metaMu.Lock()
	gen := c.metaGen
	c.metaMu.Unlock()
	return gen
}

// metaClient returns a client connected to the current meta leader, using the
// cached leader hint while it is fresh.
func (c *Client) metaClient(ctx context.Context) (metaAPI, error) {
	c.metaMu.Lock()
	if c.metaClosed {
		c.metaMu.Unlock()
		return nil, errClientClosed
	}
	mc, addr := c.meta, c.metaAddr
	leader, until := c.metaLeaderAddr, c.metaLeaderUntil
	c.metaMu.Unlock()
	if mc != nil && leader != "" && leader == addr && time.Now().Before(until) {
		return mc, nil
	}
	mc, _, err := c.refreshMetaLeader(ctx, false)
	return mc, err
}

// metaClientFresh always asks the current connection where the meta leader is
// before returning it. It is used when the caller needs a post-failover view,
// such as when refilling a TSO batch or re-resolving a stale range.
func (c *Client) metaClientFresh(ctx context.Context) (metaAPI, uint64, error) {
	return c.refreshMetaLeader(ctx, true)
}

func (c *Client) refreshMetaLeader(ctx context.Context, force bool) (metaAPI, uint64, error) {
	c.metaMu.Lock()
	if c.metaClosed {
		gen := c.metaGen
		c.metaMu.Unlock()
		return nil, gen, errClientClosed
	}
	mc, addr := c.meta, c.metaAddr
	leader, until, gen := c.metaLeaderAddr, c.metaLeaderUntil, c.metaGen
	c.metaMu.Unlock()
	if mc == nil {
		return nil, gen, errClientClosed
	}
	if !force && leader != "" && leader == addr && time.Now().Before(until) {
		return mc, gen, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, gen, err
	}

	probeCtx, cancel := context.WithTimeout(ctx, time.Second)
	resp, err := mc.Leader(probeCtx, &pbv2.LeaderRequest{})
	cancel()
	if err != nil {
		// The current connection may be a follower or dead. Probe the
		// original seeds with pooled connections.
		if seeded, ok := c.tryMetaSeeds(ctx); ok {
			return seeded, c.metaGeneration(), nil
		}
		// Preserve the old behavior: let the caller try AllocTS/GetRange on
		// the cached connection and report its error.
		return mc, c.metaGeneration(), nil
	}
	leader = resp.GetLeader()
	if leader == "" || leader == addr {
		c.markMetaLeader(addr, mc)
		return mc, c.metaGeneration(), nil
	}
	next, err := c.metaClientForAddr(leader)
	if err != nil {
		return mc, c.metaGeneration(), nil
	}
	c.setMetaTarget(leader, next)
	return next, c.metaGeneration(), nil
}

// tryMetaSeeds re-discovers the leader from the configured seed addresses and
// the last known meta address, reusing pooled connections.
func (c *Client) tryMetaSeeds(ctx context.Context) (metaAPI, bool) {
	c.metaMu.Lock()
	if c.metaClosed {
		c.metaMu.Unlock()
		return nil, false
	}
	seeds := make([]string, 0, len(c.metaSeeds)+1)
	seeds = append(seeds, c.metaSeeds...)
	seeds = append(seeds, c.metaAddr)
	c.metaMu.Unlock()

	seen := make(map[string]struct{}, len(seeds))
	for _, addr := range seeds {
		if addr == "" {
			continue
		}
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		mc, err := c.metaClientForAddr(addr)
		if err != nil {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		resp, err := mc.Leader(probeCtx, &pbv2.LeaderRequest{})
		cancel()
		if err != nil {
			continue
		}
		target, targetMC := addr, mc
		if leader := resp.GetLeader(); leader != "" && leader != addr {
			lmc, err := c.metaClientForAddr(leader)
			if err != nil {
				continue
			}
			target, targetMC = leader, lmc
		}
		c.setMetaTarget(target, targetMC)
		return targetMC, true
	}
	return nil, false
}

// setMetaTarget points the client at addr and bumps the meta generation when
// the target changes. A generation bump invalidates the TSO batch and every
// cached range descriptor.
func (c *Client) setMetaTarget(addr string, mc metaAPI) {
	changed := false
	c.metaMu.Lock()
	if !c.metaClosed {
		if c.metaAddr != addr || c.meta == nil {
			changed = true
			c.metaGen++
		}
		c.meta = mc
		c.metaAddr = addr
		c.metaLeaderAddr = addr
		c.metaLeaderUntil = time.Now().Add(leaderTTL)
	}
	c.metaMu.Unlock()
	if changed {
		c.clearRangeCache()
	}
}

// markMetaLeader records addr as the leader without changing the generation.
func (c *Client) markMetaLeader(addr string, mc metaAPI) {
	c.metaMu.Lock()
	if !c.metaClosed {
		c.meta = mc
		c.metaAddr = addr
		c.metaLeaderAddr = addr
		c.metaLeaderUntil = time.Now().Add(leaderTTL)
	}
	c.metaMu.Unlock()
}

// invalidateMetaLeader drops the cached leader hint and all derived state.
func (c *Client) invalidateMetaLeader() {
	c.metaMu.Lock()
	if !c.metaClosed {
		c.metaLeaderAddr = ""
		c.metaLeaderUntil = time.Time{}
		c.metaGen++
	}
	c.metaMu.Unlock()
	c.clearRangeCache()
}

// metaServerError reports a request that reached the meta service but was
// rejected with success=false. It is not retried on another connection.
type metaServerError struct{ msg string }

func (e *metaServerError) Error() string { return e.msg }

func newMetaServerError(fallback, msg string) error {
	if msg != "" {
		return &metaServerError{msg: msg}
	}
	return &metaServerError{msg: fallback}
}

// metaCall runs op against the current meta leader. If the call fails with a
// transport error (for example a stale leader hint after failover), it asks
// for a fresh leader view and retries once. Server-side rejections are
// returned unchanged. Every operation routed through metaCall is idempotent.
func (c *Client) metaCall(ctx context.Context, op func(metaAPI) error) error {
	mc, err := c.metaClient(ctx)
	if err == nil {
		err = op(mc)
	}
	if err == nil {
		return nil
	}
	var serverErr *metaServerError
	if errors.As(err, &serverErr) || ctx.Err() != nil {
		return err
	}
	fresh, _, freshErr := c.metaClientFresh(ctx)
	if freshErr != nil {
		return err
	}
	return op(fresh)
}

// ListRanges returns the current meta range map in the order reported by the
// meta service. Callers that need a stable order should sort by ID.
func (c *Client) ListRanges(ctx context.Context) ([]RangeInfo, error) {
	var out []RangeInfo
	err := c.metaCall(ctx, func(mc metaAPI) error {
		resp, err := mc.ListRanges(ctx, &pbv2.ListRangesRequest{})
		if err != nil {
			return err
		}
		infos := make([]RangeInfo, 0, len(resp.GetRanges()))
		for _, r := range resp.GetRanges() {
			if r == nil {
				return &metaServerError{msg: "v2client: meta returned a nil range descriptor"}
			}
			infos = append(infos, RangeInfo{
				ID:       r.GetId(),
				Start:    append([]byte(nil), r.GetStart()...),
				End:      append([]byte(nil), r.GetEnd()...),
				Replicas: append([]string(nil), r.GetReplicas()...),
				Leader:   r.GetLeader(),
				Version:  r.GetVersion(),
			})
		}
		out = infos
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListRangesWithHealth returns the meta range map enriched with best-effort
// liveness information. Every replica is probed with KVService.Status, using
// the same read-only path the data plane uses for leader discovery. The probe
// is bounded per replica, per range and for the whole pass, so an unreachable
// node degrades to an empty LiveVoters list (and the metadata leader hint)
// instead of failing the command. A leader is reported
// only when a replica claims leadership or a follower names a current replica;
// an unknown leader stays empty rather than being guessed.
//
// ListRanges remains the pure wire mapping, so existing callers and tests are
// unaffected.
func (c *Client) ListRangesWithHealth(ctx context.Context) ([]RangeInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ranges, err := c.ListRanges(ctx)
	if err != nil {
		return nil, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, rangeHealthTotalTimeout)
	defer cancel()
	c.attachRangeHealth(probeCtx, ranges)
	return ranges, nil
}

// attachRangeHealth fills the health fields of ranges in place, probing up to
// rangeHealthWorkers ranges concurrently.
func (c *Client) attachRangeHealth(ctx context.Context, ranges []RangeInfo) {
	if len(ranges) == 0 {
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, rangeHealthWorkers)
	for i := range ranges {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			ranges[i] = c.probeRangeHealth(ctx, ranges[i])
		}(i)
	}
	wg.Wait()
}

// probeRangeHealth probes one range's replicas. It never mutates the cluster or
// the map; it only observes KVService.Status.
func (c *Client) probeRangeHealth(ctx context.Context, info RangeInfo) RangeInfo {
	info.ReplicaCount = len(info.Replicas)
	info.Epoch = info.Version
	originalLeader := info.Leader
	live := make([]string, 0, len(info.Replicas))
	observed := ""
	hint := ""

	probeCtx, cancel := context.WithTimeout(ctx, rangeHealthRangeTimeout)
	defer cancel()

	seen := make(map[string]struct{}, len(info.Replicas)+1)
	candidates := make([]string, 0, len(info.Replicas)+1)
	if info.Leader != "" {
		candidates = append(candidates, info.Leader)
	}
	candidates = append(candidates, info.Replicas...)
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
			continue
		}
		callCtx, callCancel := context.WithTimeout(probeCtx, rangeHealthProbeTimeout)
		resp, err := cl.Status(callCtx, &pbv2.StatusRequest{RangeId: info.ID})
		callCancel()
		if err != nil {
			continue
		}
		if strings.EqualFold(resp.GetRole(), "leader") {
			observed = addr
		} else if hint == "" && resp.GetLeader() != "" && containsString(info.Replicas, resp.GetLeader()) {
			hint = resp.GetLeader()
		}
		if !resp.GetLearner() && (len(resp.GetVoters()) == 0 || containsString(resp.GetVoters(), addr)) {
			live = append(live, addr)
		}
	}
	if observed == "" {
		observed = hint
	}
	if observed != "" {
		info.Leader = observed
	}
	info.LeaderKnown = info.Leader != ""
	info.LiveVoters = sortedUniqueStrings(live)
	info.Stale = originalLeader != "" && observed != "" && originalLeader != observed
	return info
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func sortedUniqueStrings(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, item := range in {
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}

// SetRanges replaces the whole meta range map. It is an admin RPC and carries
// the configured admin token; the caller is responsible for applying a layout
// that is contiguous, sorted and matches the data placement.
//
// The operation is idempotent (it replaces the map wholesale), so a transport
// failure is retried once against a freshly discovered meta leader.
func (c *Client) SetRanges(ctx context.Context, ranges []RangeInfo) error {
	req := &pbv2.SetRangesRequest{Ranges: make([]*pbv2.RangeInfo, 0, len(ranges))}
	for _, r := range ranges {
		req.Ranges = append(req.Ranges, &pbv2.RangeInfo{
			Id: r.ID, Start: append([]byte(nil), r.Start...), End: append([]byte(nil), r.End...),
			Replicas: append([]string(nil), r.Replicas...),
			Leader:   r.Leader, Version: r.Version,
		})
	}
	return c.metaCall(ctx, func(mc metaAPI) error {
		resp, err := mc.SetRanges(c.adminContext(ctx), req)
		if err != nil {
			return err
		}
		if !resp.GetSuccess() {
			return newMetaServerError("v2client: set ranges failed", resp.GetError())
		}
		return nil
	})
}

// SplitRange asks the meta service to split rangeID at splitKey and returns
// the ID of the new right-hand range. The meta service allocates the new ID.
func (c *Client) SplitRange(ctx context.Context, rangeID uint64, splitKey []byte) (uint64, error) {
	var newRangeID uint64
	err := c.metaCall(ctx, func(mc metaAPI) error {
		resp, err := mc.SplitRange(c.adminContext(ctx), &pbv2.MetaSplitRangeRequest{
			RangeId:  rangeID,
			SplitKey: splitKey,
		})
		if err != nil {
			return err
		}
		if !resp.GetSuccess() {
			return newMetaServerError(
				fmt.Sprintf("v2client: split range %d failed", rangeID),
				resp.GetError())
		}
		newRangeID = resp.GetNewRangeId()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return newRangeID, nil
}

// MergeRanges asks the meta service to merge the adjacent ranges leftID and
// rightID into leftID.
func (c *Client) MergeRanges(ctx context.Context, leftID, rightID uint64) error {
	return c.metaCall(ctx, func(mc metaAPI) error {
		resp, err := mc.MergeRanges(c.adminContext(ctx), &pbv2.MetaMergeRangesRequest{
			LeftRangeId:  leftID,
			RightRangeId: rightID,
		})
		if err != nil {
			return err
		}
		if !resp.GetSuccess() {
			return newMetaServerError(
				fmt.Sprintf("v2client: merge ranges %d and %d failed", leftID, rightID),
				resp.GetError())
		}
		return nil
	})
}

// MetaLeader returns the current meta leader address and term as reported by
// the meta service. A standalone (non-replicated) meta service may report an
// empty leader.
func (c *Client) MetaLeader(ctx context.Context) (string, uint64, error) {
	var leader string
	var term uint64
	err := c.metaCall(ctx, func(mc metaAPI) error {
		resp, err := mc.Leader(ctx, &pbv2.LeaderRequest{})
		if err != nil {
			return err
		}
		leader = resp.GetLeader()
		term = resp.GetTerm()
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	return leader, term, nil
}
