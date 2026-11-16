package meta

import (
	"fmt"
	"time"
)

// heartbeat is the latest report received from a node. It is kept as a copy so
// callers may reuse their slice after Heartbeat returns.
type heartbeat struct {
	ranges   []uint64
	lastSeen time.Time
}

// Heartbeat records that node is currently serving ranges. It validates that
// every range ID exists and replaces the node's previous report.
func (c *Cluster) Heartbeat(node string, ranges []uint64) error {
	if node == "" {
		return fmt.Errorf("meta: heartbeat node must not be empty")
	}

	c.mu.RLock()
	for _, id := range ranges {
		if c.rangeIndexLocked(id) < 0 {
			c.mu.RUnlock()
			return fmt.Errorf("meta: heartbeat references unknown range %d", id)
		}
	}
	c.mu.RUnlock()

	c.hbMu.Lock()
	c.heartbeats[node] = heartbeat{
		ranges:   cloneIDs(ranges),
		lastSeen: time.Now(),
	}
	c.hbMu.Unlock()

	return nil
}

func cloneIDs(in []uint64) []uint64 {
	if in == nil {
		return nil
	}
	out := make([]uint64, len(in))
	copy(out, in)
	return out
}
