package grpcpool

import (
 "sync"
 "google.golang.org/grpc"
)

type Pool struct {
 conn *grpc.ClientConn
 mu sync.Mutex
 inUse bool
}
type PoolManager struct {
 pools map[string]*Pool
 mu sync.RWMutex
}

func NewPoolManager() *PoolManager{
 return &PoolManager{
  pools: make(map[string]*Pool),
  mu: sync.RWMutex{},
 }
}
func NewPool(conn *grpc.ClientConn) *Pool {
    return &Pool{
  conn: conn,
  mu: sync.Mutex{},
  inUse: false,
    }
}
func (p *PoolManager) GetConn(address string) (*Pool, error) {
 p.mu.Lock()
 defer p.mu.Unlock()
    if pool, ok := p.pools[address]; ok{
        if !pool.inUse {
   pool.mu.Lock()
   pool.inUse = true
   pool.mu.Unlock()
            return pool, nil
        }else{
            return nil, nil
        }
    }
    return nil, nil
}
func (p *PoolManager) PutConn(address string, pool *Pool) {
 p.mu.Lock()
 defer p.mu.Unlock()
 if _, ok := p.pools[address]; ok {
  pool.mu.Lock()
  pool.inUse = false
  pool.mu.Unlock()
 }else{
  p.pools[address] = pool
  pool.mu.Lock()
  pool.inUse = false
  pool.mu.Unlock()
 }
}
func (p *PoolManager) Remove(address string) {
 p.mu.Lock()
 defer p.mu.Unlock()
    if pool, ok := p.pools[address]; ok{
        pool.conn.Close()
        delete(p.pools, address)
    }
}
func (p *PoolManager) Close() {
 p.mu.Lock()
 defer p.mu.Unlock()
 for _, pool := range p.pools {
  pool.conn.Close()
 }
 p.pools = make(map[string]*Pool)
}