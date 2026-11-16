package v2client

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pbv2 "distributed-db/internal/pb/v2"
)

// metaAPI is the subset of pbv2.MetaServiceClient used by the client. Keeping
// it as an interface lets tests inject an in-memory meta service without
// changing the public API.
type metaAPI interface {
	AllocTS(ctx context.Context, in *pbv2.AllocTSRequest, opts ...grpc.CallOption) (*pbv2.AllocTSResponse, error)
	GetRange(ctx context.Context, in *pbv2.GetRangeRequest, opts ...grpc.CallOption) (*pbv2.RangeInfo, error)
	Leader(ctx context.Context, in *pbv2.LeaderRequest, opts ...grpc.CallOption) (*pbv2.LeaderResponse, error)
}

type metaEntry struct {
	cc *grpc.ClientConn
	mc metaAPI
}

func dialMeta(addr string) (*grpc.ClientConn, metaAPI, error) {
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
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
