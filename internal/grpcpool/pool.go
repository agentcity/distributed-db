package grpcpool

import (
 "errors"
 "sync"
 "time"

 "google.golang.org/grpc"
)

// Pool represents a pool of gRPC connections to a given address.
type Pool struct {
 address  string
 options  []grpc.DialOption
 mu       sync.Mutex
 conns    []*grpc.ClientConn
 maxPool  int
 maxIdle  int
 connCounter int
}

// NewPool creates a new Pool for the given address and options.
func NewPool(address string, options []grpc.DialOption, maxPool int, maxIdle int) *Pool {
 return &Pool{
  address: address,
  options: options,
  conns:   make([]*grpc.ClientConn, 0),
  maxPool: maxPool,
  maxIdle: maxIdle,
  connCounter: 0,
 }
}

// Get returns a gRPC connection from the pool or creates a new one.
func (p *Pool) Get() (*grpc.ClientConn, error) {
 p.mu.Lock()
 defer p.mu.Unlock()

 if len(p.conns) > 0 {
  conn := p.conns[len(p.conns)-1]
  p.conns = p.conns[:len(p.conns)-1]
  return conn, nil
 }
 if p.connCounter < p.maxPool {
  p.connCounter++
        conn, err := grpc.Dial(p.address, p.options...)
        if err != nil {
            p.connCounter--
   return nil, err
  }

  return conn, nil

 }

 return nil, errors.New("maximum pool size reached")
}


// Put returns a gRPC connection to the pool.
func (p *Pool) Put(conn *grpc.ClientConn) {
 p.mu.Lock()
 defer p.mu.Unlock()

 if len(p.conns) < p.maxIdle {
  p.conns = append(p.conns, conn)
 } else {
        conn.Close()
 }
}

// Close closes all the connections in the pool.
func (p *Pool) Close() {
    p.mu.Lock()
    defer p.mu.Unlock()

 for _, conn := range p.conns {
        conn.Close()
 }
    p.conns = nil
    p.connCounter = 0
}

// Clean closes old connection from the pool.
func (p *Pool) Clean() {
    p.mu.Lock()
    defer p.mu.Unlock()

 if len(p.conns) > p.maxIdle {
        for i := 0; i < len(p.conns) - p.maxIdle; i++ {
            conn := p.conns[i]
            conn.Close()
        }
  p.conns = p.conns[len(p.conns)-p.maxIdle:]
 }
}