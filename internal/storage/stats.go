package storage

// Stats returns a cheap point-in-time view of the engine.
//
// DiskSize and MemTableSize come from Pebble's metrics (DiskSpaceUsage and
// MemTable.Size, both cumulative/in-use byte counts). Reads counts successful
// and failed Get calls made through the Engine and Snapshot interfaces;
// Writes counts mutations applied through Set, Delete and committed batches.
// Iterators are not counted per key, so Reads/Writes track logical operations
// rather than internal Pebble reads/writes.
func (d *db) Stats() Stats {
	d.mu.RLock()
	defer d.mu.RUnlock()
	st := Stats{
		Reads:  d.reads.Load(),
		Writes: d.writes.Load(),
	}
	if d.closed {
		return st
	}
	m := d.pdb.Metrics()
	st.DiskSize = int64(m.DiskSpaceUsage())
	st.MemTableSize = int64(m.MemTable.Size)
	return st
}
