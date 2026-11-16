package client

import (
	"container/list"
	"sync"
)

// leaderCache is a fixed-capacity LRU mapping a routing prefix to a leader
// address. Every operation touches the list under a mutex; that is cheap
// compared with an RPC and keeps the cache safe for concurrent Clients.
type leaderCache struct {
	mu       sync.Mutex
	capacity int
	order    *list.List // front = most recently used
	entries  map[string]*list.Element
}

type leaderCacheEntry struct {
	prefix string
	addr   string
}

func newLeaderCache(capacity int) *leaderCache {
	if capacity <= 0 {
		capacity = leaderCacheEntries
	}
	return &leaderCache{
		capacity: capacity,
		order:    list.New(),
		entries:  make(map[string]*list.Element, capacity),
	}
}

// get returns the cached leader for prefix and marks it as recently used.
func (c *leaderCache) get(prefix string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, ok := c.entries[prefix]
	if !ok {
		return "", false
	}
	c.order.MoveToFront(elem)
	return elem.Value.(*leaderCacheEntry).addr, true
}

// put inserts or updates a leader and evicts the least recently used entry
// when the cache is full. The cache never grows past its capacity.
func (c *leaderCache) put(prefix, addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.entries[prefix]; ok {
		entry := elem.Value.(*leaderCacheEntry)
		entry.addr = addr
		c.order.MoveToFront(elem)
		return
	}

	entry := &leaderCacheEntry{prefix: prefix, addr: addr}
	c.entries[prefix] = c.order.PushFront(entry)
	for c.order.Len() > c.capacity {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*leaderCacheEntry).prefix)
	}
}

// invalidate drops one routing prefix.
func (c *leaderCache) invalidate(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, ok := c.entries[prefix]
	if !ok {
		return
	}
	c.order.Remove(elem)
	delete(c.entries, prefix)
}

func (c *leaderCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// routePrefix maps a key to its cache key. The key itself is a prefix of
// itself, so keys shorter than leaderPrefixBytes are cached exactly; longer
// keys share an entry only when their first leaderPrefixBytes bytes match.
func routePrefix(key []byte) string {
	if len(key) > leaderPrefixBytes {
		key = key[:leaderPrefixBytes]
	}
	return string(key)
}
