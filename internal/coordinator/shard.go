package coordinator

import (
	"errors"
	"sort"

	"github.com/cespare/xxhash/v2"
)

// errNoShards is returned when the coordinator has no shard configured.
var errNoShards = errors.New("coordinator: no shards configured")

// sortedShardNames returns the shard names in deterministic (ascending) order.
// Shard selection indexes into this slice, so the order must never depend on
// map iteration order.
func sortedShardNames(shards map[string][]string) []string {
	names := make([]string, 0, len(shards))
	for name := range shards {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// shardForKey maps a key to a shard with xxhash and modulo over the sorted
// shard names.
func (c *Coordinator) shardForKey(key string) (string, error) {
	if len(c.shardNames) == 0 {
		return "", errNoShards
	}
	idx := xxhash.Sum64String(key) % uint64(len(c.shardNames))
	return c.shardNames[idx], nil
}

// metricShard returns the shard name for a key, or "" when no shard can be
// selected. It is only used for metric labels.
func (c *Coordinator) metricShard(key string) string {
	shard, err := c.shardForKey(key)
	if err != nil {
		return ""
	}
	return shard
}
