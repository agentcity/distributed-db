package expirable

import (
 "testing"
 "time"
)

func TestLRU(t *testing.T) {
 lru := NewLRU[int, string](3, 1*time.Hour)

 lru.Add(1, "one")
 lru.Add(2, "two")
 lru.Add(3, "three")

 val, ok := lru.Get(1)
 if !ok || val != "one" {
  t.Errorf("Expected 'one', got '%s'", val)
 }

 lru.Add(4, "four") // Evict 2
 _, ok = lru.Get(2)
 if ok {
  t.Error("Expected '2' to be evicted")
 }
    val, ok = lru.Get(4)
    if !ok || val != "four"{
  t.Errorf("Expected 'four', got '%s'", val)
    }

    lru.Invalidate(4)
    _, ok = lru.Get(4)
    if ok {
  t.Error("Expected '4' to be invalidated")
    }
}

func TestLRU_Expiry(t *testing.T){
 lru := NewLRU[int, string](3, 1*time.Millisecond)
 lru.Add(1, "one")

 time.Sleep(time.Millisecond*2)

 _, ok := lru.Get(1)
    if ok{
  t.Error("Expected '1' to be expired")
 }
}