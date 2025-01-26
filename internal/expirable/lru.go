package expirable

import (
 "container/list"
 "sync"
 "time"
)

type entry[K comparable, V any] struct {
 key        K
 value      V
 accessTime time.Time
}

type LRU[K comparable, V any] struct {
 capacity  int
 cache     map[K]*list.Element
 evictList *list.List
 mu        sync.RWMutex
    expiry time.Duration
}

func NewLRU[K comparable, V any](capacity int, expiry time.Duration) *LRU[K, V] {
 return &LRU[K, V]{
  capacity:  capacity,
  cache:     make(map[K]*list.Element),
  evictList: list.New(),
        expiry: expiry,
 }
}

func (l *LRU[K, V]) Add(key K, value V) {
 l.mu.Lock()
 defer l.mu.Unlock()

 if elem, ok := l.cache[key]; ok {
  l.evictList.MoveToFront(elem)
  elem.Value.(*entry[K,V]).value = value
  elem.Value.(*entry[K,V]).accessTime = time.Now()
  return
 }

 ent := &entry[K, V]{
  key:        key,
  value:      value,
  accessTime: time.Now(),
 }

 elem := l.evictList.PushFront(ent)
 l.cache[key] = elem

 if l.evictList.Len() > l.capacity {
  l.removeOldest()
 }
}

func (l *LRU[K, V]) Get(key K) (V, bool) {
 l.mu.Lock()
 defer l.mu.Unlock()
 if elem, ok := l.cache[key]; ok {
  if time.Since(elem.Value.(*entry[K,V]).accessTime) > l.expiry {
   l.removeElement(elem)
   var zero V
   return zero, false
  }
  l.evictList.MoveToFront(elem)
  return elem.Value.(*entry[K,V]).value, true
 }
    var zero V
 return zero, false
}

func (l *LRU[K, V]) Invalidate(key K) {
    l.mu.Lock()
    defer l.mu.Unlock()
 if elem, ok := l.cache[key]; ok {
  l.removeElement(elem)
 }
}

func (l *LRU[K, V]) removeOldest() {
    if l.evictList.Len() == 0{
        return
    }
 elem := l.evictList.Back()
 l.removeElement(elem)
}

func (l *LRU[K, V]) removeElement(elem *list.Element) {
 l.evictList.Remove(elem)
 ent := elem.Value.(*entry[K,V])
 delete(l.cache, ent.key)
}