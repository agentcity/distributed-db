package grpcpool

import (
 "net"
 "testing"
    "google.golang.org/grpc"
)

func TestPoolManager(t *testing.T){
    lis, err := net.Listen("tcp", "localhost:50052")
 if err != nil {
  t.Fatalf("failed to listen: %v", err)
 }
 s := grpc.NewServer()
    go func() {
        if err := s.Serve(lis); err != nil {
            t.Fatalf("failed to serve: %v", err)
        }
    }()
 defer s.Stop()

    conn, err := grpc.Dial("localhost:50052", grpc.WithInsecure())
    if err != nil {
  t.Fatalf("did not connect: %v", err)
    }
 defer conn.Close()


 poolManager := NewPoolManager()
 pool := NewPool(conn)

    poolManager.PutConn("localhost:50052", pool)

    p, _ := poolManager.GetConn("localhost:50052")
 if p == nil{
  t.Error("pool is nil")
 }
 p2, _ := poolManager.GetConn("localhost:50052")
 if p2 != nil{
  t.Error("pool must be nil")
 }

 poolManager.PutConn("localhost:50052", p)
 p3, _ := poolManager.GetConn("localhost:50052")
 if p3 == nil{
  t.Error("pool is nil")
 }
 poolManager.Remove("localhost:50052")
 p4, _ := poolManager.GetConn("localhost:50052")
 if p4 != nil{
  t.Error("pool must be nil")
 }
}