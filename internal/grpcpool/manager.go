package grpcpool

import (
 "sync"
 "time"

 "google.golang.org/grpc"
)

// PoolManager manages a pool of gRPC connections to a given address.
type PoolManager struct {
 address string
 options []grpc.DialOption
 mu      sync.RWMutex
 pools   map[string]*Pool // Key is node address.
 maxPool int
    maxIdle int
}

// NewPoolManager creates a new PoolManager for the given address and options.
func NewPoolManager(address string, options []grpc.DialOption, maxPool int, maxIdle int) *PoolManager {
 return &PoolManager{
  address: address,
  options: options,
  pools:   make(map[string]*Pool),
  maxPool: maxPool,
        maxIdle: maxIdle,
 }
}

// GetConn returns a gRPC connection from the pool or creates a new one.
func (pm *PoolManager) GetConn(nodeAddress string) (*grpc.ClientConn, error) {
 pm.mu.Lock()
 defer pm.mu.Unlock()

 if pool, ok := pm.pools[nodeAddress]; ok {
  conn, err := pool.Get()
  if err != nil {
   return nil, err
  }
  return conn, nil
 }

 pool := NewPool(pm.address, pm.options, pm.maxPool, pm.maxIdle)
 conn, err := pool.Get()
    if err != nil {
  return nil, err
 }
 pm.pools[nodeAddress] = pool

 return conn, nil
}

// PutConn returns a gRPC connection to the pool.
func (pm *PoolManager) PutConn(nodeAddress string, conn *grpc.ClientConn) {
 pm.mu.RLock()
 defer pm.mu.RUnlock()
 if pool, ok := pm.pools[nodeAddress]; ok {
        pool.Put(conn)
 }
}

// Close closes all the pools and their connections
func (pm *PoolManager) Close() {
 pm.mu.Lock()
 defer pm.mu.Unlock()

 for _, pool := range pm.pools {
        pool.Close()
 }
}
// Clean removes old connections from pool
func (pm *PoolManager) Clean() {
 pm.mu.Lock()
 defer pm.mu.Unlock()

 for _, pool := range pm.pools {
        pool.Clean()
 }
}

// Monitor periodically clean the pool
func (pm *PoolManager) Monitor() {
    ticker := time.NewTicker(1 * time.Second)
    defer ticker.Stop()

    for range ticker.C {
       pm.Clean()
    }
}