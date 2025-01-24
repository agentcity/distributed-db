package grpcpool

import (
 "context"
 "fmt"
 "log"
 "sync"
 "time"

 pb "distributed-db/proto"
 "google.golang.org/grpc"
 "google.golang.org/grpc/credentials"
)

type Pool struct {
 address    string
 maxSize    int
 mu         sync.Mutex
 conns      []*grpc.ClientConn
 logger     *log.Logger
 tls        bool
 clientCert string
}

func NewPool(address string, maxSize int, logger *log.Logger, tls bool, clientCert string) *Pool {
 return &Pool{
  address:    address,
  maxSize:    maxSize,
  conns:      make([]*grpc.ClientConn, 0),
  logger:     logger,
  tls:        tls,
  clientCert: clientCert,
 }
}

func (p *Pool) Get() (*grpc.ClientConn, error) {
 p.mu.Lock()
 defer p.mu.Unlock()
 if len(p.conns) > 0 {
  conn := p.conns[0]
  p.conns = p.conns[1:]
  return conn, nil
 }
 return p.createConn()
}

func (p *Pool) Release(conn *grpc.ClientConn) {
    if conn == nil {
  p.logger.Println("Trying to release nil connection")
        return
    }
 p.mu.Lock()
    defer p.mu.Unlock()

    if len(p.conns) >= p.maxSize {
        p.logger.Println("Pool is full. Closing connection")
         err := conn.Close()
        if err != nil {
            p.logger.Printf("Error closing gRPC connection %v", err)
        }
        return
    }
    p.conns = append(p.conns, conn)

}

func (p *Pool) createConn() (*grpc.ClientConn, error) {
 var opts []grpc.DialOption
 if p.tls {
  creds, err := credentials.NewClientTLSFromFile(p.clientCert, "")
  if err != nil {
   return nil, fmt.Errorf("failed to create client credentials: %w", err)
  }
  opts = append(opts, grpc.WithTransportCredentials(creds))
 } else {
  opts = append(opts, grpc.WithInsecure())
 }
 conn, err := grpc.Dial(p.address, opts...)
 if err != nil {
  return nil, fmt.Errorf("did not connect: %w", err)
 }
 p.logger.Printf("gRPC connection to %s was succesfully created", p.address)
 return conn, nil
}

func (p *Pool) Close() {
    p.mu.Lock()
    defer p.mu.Unlock()
    for _, conn := range p.conns {
        err := conn.Close()
        if err != nil {
            p.logger.Printf("Error closing gRPC connection %v", err)
        }
    }
 p.conns = nil
 p.logger.Println("gRPC pool was closed")
}
