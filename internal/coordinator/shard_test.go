package coordinator

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/cespare/xxhash/v2"
)

func TestShardSelectionDeterministic(t *testing.T) {
	shards := map[string][]string{
		"shard-c": {"n5:7001", "n6:7001"},
		"shard-a": {"n1:7001", "n2:7001"},
		"shard-b": {"n3:7001", "n4:7001"},
	}
	c := newTestCoordinator(t, Config{Shards: shards}, nil)

	names := sortedShardNames(shards)
	if !sort.StringsAreSorted(names) {
		t.Fatalf("shard names not sorted: %v", names)
	}
	if len(names) != 3 || names[0] != "shard-a" || names[2] != "shard-c" {
		t.Fatalf("unexpected sorted shard names: %v", names)
	}

	keys := []string{"", "a", "user:42", "key/with/slash", strings.Repeat("x", 128), "ключ"}
	for _, key := range keys {
		want := names[xxhash.Sum64String(key)%uint64(len(names))]
		for i := 0; i < 5; i++ {
			got, err := c.shardForKey(key)
			if err != nil {
				t.Fatalf("shardForKey(%q): %v", key, err)
			}
			if got != want {
				t.Fatalf("shardForKey(%q) = %q, want %q", key, got, want)
			}
		}
	}

	// Every configured shard must be reachable for some key.
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		shard, err := c.shardForKey(fmt.Sprintf("key-%d", i))
		if err != nil {
			t.Fatalf("shardForKey: %v", err)
		}
		seen[shard] = true
	}
	if len(seen) != len(names) {
		t.Fatalf("keys only reached %d of %d shards", len(seen), len(names))
	}
}

func TestShardForKeyWithoutShards(t *testing.T) {
	c := &Coordinator{}
	if _, err := c.shardForKey("k"); !errors.Is(err, errNoShards) {
		t.Fatalf("shardForKey error = %v, want errNoShards", err)
	}
	if got := c.metricShard("k"); got != "" {
		t.Fatalf("metricShard = %q, want empty", got)
	}
}

func TestSortedShardNamesDoesNotMutateInput(t *testing.T) {
	shards := map[string][]string{"b": {"n2"}, "a": {"n1"}}
	_ = sortedShardNames(shards)
	if len(shards) != 2 {
		t.Fatalf("input map was mutated: %v", shards)
	}
}
