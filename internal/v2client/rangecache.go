package v2client

import (
	"bytes"
	"context"
	"errors"
	"time"

	pbv2 "distributed-db/internal/pb/v2"
)

const (
	defaultRangeTTL      = time.Second
	defaultRangeCacheMax = 4096
)

// rangeEntry is one cached range descriptor. seq is the insertion/use order
// used for oldest-entry eviction.
type rangeEntry struct {
	info    *pbv2.RangeInfo
	expires time.Time
	gen     uint64
	seq     uint64
}

// cachedRange returns a fresh cached descriptor whose bounds contain key.
// Entries from an older meta generation or past their TTL are dropped.
func (c *Client) cachedRange(key []byte) *pbv2.RangeInfo {
	gen := c.metaGeneration()
	now := time.Now()
	c.rangeMu.Lock()
	defer c.rangeMu.Unlock()

	var best *rangeEntry
	for id, e := range c.ranges {
		if e == nil || e.info == nil || e.gen != gen || !now.Before(e.expires) {
			delete(c.ranges, id)
			continue
		}
		if !rangeContains(e.info, key) {
			continue
		}
		if best == nil || e.info.Version > best.info.Version ||
			(e.info.Version == best.info.Version && e.seq > best.seq) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	c.rangeSeq++
	best.seq = c.rangeSeq
	return cloneRangeInfo(best.info)
}

// cacheRange stores a descriptor. Overlapping entries from an older range
// version (for example the pre-split range) are dropped, which keeps a stale
// broad descriptor from shadowing a freshly learned child range.
func (c *Client) cacheRange(gen uint64, r *pbv2.RangeInfo) {
	if r == nil {
		return
	}
	now := time.Now()
	clone := cloneRangeInfo(r)
	c.rangeMu.Lock()
	defer c.rangeMu.Unlock()
	if c.ranges == nil {
		c.ranges = make(map[uint64]*rangeEntry)
	}
	if old := c.ranges[r.GetId()]; old != nil && old.gen == gen && old.info != nil &&
		old.info.GetVersion() > r.GetVersion() {
		// A newer descriptor for the same range is already cached.
		return
	}
	for id, e := range c.ranges {
		if id == r.GetId() {
			delete(c.ranges, id)
			continue
		}
		if e == nil || e.info == nil || e.gen != gen || !now.Before(e.expires) {
			delete(c.ranges, id)
			continue
		}
		if !rangeOverlaps(e.info, r) {
			continue
		}
		if e.info.GetVersion() > r.GetVersion() {
			// Do not let an out-of-order old response replace a newer view.
			return
		}
		delete(c.ranges, id)
	}
	c.rangeSeq++
	c.ranges[r.GetId()] = &rangeEntry{
		info:    clone,
		expires: now.Add(c.rangeTTLValue()),
		gen:     gen,
		seq:     c.rangeSeq,
	}
	c.evictRangesLocked()
}

func (c *Client) evictRangesLocked() {
	limit := c.rangeCap
	if limit <= 0 {
		limit = defaultRangeCacheMax
	}
	for len(c.ranges) > limit {
		var oldestID, oldestSeq uint64
		found := false
		for id, e := range c.ranges {
			if e == nil {
				delete(c.ranges, id)
				continue
			}
			if !found || e.seq < oldestSeq {
				oldestID, oldestSeq, found = id, e.seq, true
			}
		}
		if !found {
			return
		}
		delete(c.ranges, oldestID)
	}
}

// clearRangeCache drops every cached descriptor. It is called when the meta
// leader changes.
func (c *Client) clearRangeCache() {
	c.rangeMu.Lock()
	c.ranges = make(map[uint64]*rangeEntry)
	c.rangeMu.Unlock()
}

// invalidateRangeForKey drops the descriptor for rangeID and every cached
// descriptor whose bounds contain key. The latter is important after a split:
// a stale broad range can shadow both child ranges.
func (c *Client) invalidateRangeForKey(rangeID uint64, key []byte) {
	c.rangeMu.Lock()
	for id, e := range c.ranges {
		if id == rangeID || (e != nil && e.info != nil && rangeContains(e.info, key)) {
			delete(c.ranges, id)
		}
	}
	c.rangeMu.Unlock()
}

// rangeFor asks meta which range owns key, using the local cache when the key
// falls inside a fresh cached descriptor.
func (c *Client) rangeFor(ctx context.Context, key []byte) (*pbv2.RangeInfo, error) {
	if r := c.cachedRange(key); r != nil {
		return r, nil
	}
	return c.fetchRange(ctx, key, false)
}

// rangeForFresh bypasses the cache and re-resolves key through the current
// meta leader. It is used once after a stale-epoch/not-hosted response.
func (c *Client) rangeForFresh(ctx context.Context, key []byte) (*pbv2.RangeInfo, error) {
	return c.fetchRange(ctx, key, true)
}

func (c *Client) fetchRange(ctx context.Context, key []byte, fresh bool) (*pbv2.RangeInfo, error) {
	probeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	var mc metaAPI
	var gen uint64
	var err error
	if fresh {
		mc, gen, err = c.metaClientFresh(ctx)
	} else {
		mc, err = c.metaClient(ctx)
		if err == nil {
			gen = c.metaGeneration()
		}
	}
	if err != nil {
		return nil, err
	}
	r, err := mc.GetRange(probeCtx, &pbv2.GetRangeRequest{Key: key})
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, errors.New("v2client: meta returned a nil range")
	}
	c.cacheRange(gen, r)
	return cloneRangeInfo(r), nil
}

func (c *Client) rangeTTLValue() time.Duration {
	if c.rangeTTL <= 0 {
		return defaultRangeTTL
	}
	return c.rangeTTL
}

func cloneRangeInfo(r *pbv2.RangeInfo) *pbv2.RangeInfo {
	if r == nil {
		return nil
	}
	out := &pbv2.RangeInfo{
		Id:       r.GetId(),
		Start:    append([]byte(nil), r.GetStart()...),
		End:      append([]byte(nil), r.GetEnd()...),
		Replicas: append([]string(nil), r.GetReplicas()...),
		Leader:   r.GetLeader(),
		Version:  r.GetVersion(),
	}
	if r.GetStart() == nil {
		out.Start = nil
	}
	if r.GetEnd() == nil {
		out.End = nil
	}
	return out
}

func rangeContains(r *pbv2.RangeInfo, key []byte) bool {
	if r == nil {
		return false
	}
	if len(r.GetStart()) > 0 && bytes.Compare(key, r.GetStart()) < 0 {
		return false
	}
	if len(r.GetEnd()) > 0 && bytes.Compare(key, r.GetEnd()) >= 0 {
		return false
	}
	return true
}

func rangeOverlaps(a, b *pbv2.RangeInfo) bool {
	if a == nil || b == nil {
		return false
	}
	if len(a.GetEnd()) > 0 && len(b.GetStart()) > 0 && bytes.Compare(b.GetStart(), a.GetEnd()) >= 0 {
		return false
	}
	if len(b.GetEnd()) > 0 && len(a.GetStart()) > 0 && bytes.Compare(a.GetStart(), b.GetEnd()) >= 0 {
		return false
	}
	return true
}
