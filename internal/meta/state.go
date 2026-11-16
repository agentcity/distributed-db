package meta

import "fmt"

// State is the complete replicated metadata state: the range map, the TSO
// high-water mark and the next range ID. It is what the meta Raft state
// machine snapshots.
type State struct {
	Ranges       []Range `json:"ranges"`
	LastTS       uint64  `json:"last_ts"`
	LastPhysical uint64  `json:"last_physical"`
	LastLogical  uint64  `json:"last_logical"`
	NextID       uint64  `json:"next_id"`
}

// Export returns a deep copy of the complete metadata state.
func (c *Cluster) Export() State {
	c.mu.RLock()
	ranges := make([]Range, len(c.ranges))
	for i, r := range c.ranges {
		ranges[i] = *cloneRange(r)
	}
	c.mu.RUnlock()

	c.tsMu.Lock()
	lastTS := c.lastTS
	lastPhysical := c.lastPhysical
	lastLogical := c.lastLogical
	c.tsMu.Unlock()

	return State{
		Ranges: ranges, LastTS: lastTS,
		LastPhysical: lastPhysical, LastLogical: lastLogical,
		NextID: c.nextID.Load(),
	}
}

// Restore replaces the complete metadata state. It is used when applying a
// snapshot or replaying a replicated command.
func (c *Cluster) Restore(s State) error {
	if len(s.Ranges) == 0 {
		return fmt.Errorf("meta: restore requires at least one range")
	}
	if err := c.SetRanges(s.Ranges); err != nil {
		return err
	}
	var maxID uint64
	for i := range s.Ranges {
		if s.Ranges[i].ID > maxID {
			maxID = s.Ranges[i].ID
		}
	}
	if s.NextID > maxID {
		c.nextID.Store(s.NextID)
	} else {
		c.nextID.Store(maxID)
	}

	c.tsMu.Lock()
	c.lastTS = s.LastTS
	c.lastPhysical = s.LastPhysical
	c.lastLogical = s.LastLogical
	if c.lastTS != 0 && c.lastPhysical == 0 {
		c.lastPhysical = c.lastTS >> logicalBits
		c.lastLogical = c.lastTS & logicalMask
	}
	c.tsMu.Unlock()
	return nil
}

// AdvanceTS moves the TSO high-water mark forward without consulting the
// local clock. Raft applies the leader's chosen timestamp on every replica;
// using time.Now() here would diverge the state machines.
func (c *Cluster) AdvanceTS(last uint64) {
	c.tsMu.Lock()
	defer c.tsMu.Unlock()
	if last <= c.lastTS {
		return
	}
	c.lastTS = last
	c.lastPhysical = last >> logicalBits
	c.lastLogical = last & logicalMask
}
