package grpc

import (
 "context"
 "fmt"
 "log"
 "net"
 "testing"
 "time"
 "distributed-db/node"
 pb "distributed-db/proto"
 "google.golang.org/grpc"
 "google.golang.org/grpc/test/grpc_testing"
)

type mockNode struct {
    *node.Node
}

func (m *mockNode) Raft() *node.Raft {
 return nil
}
func (m *mockNode) GossipHandler(ctx context.Context, req *pb.GossipRequest) (*pb.GossipResponse, error) {
    return &pb.GossipResponse{}, nil
}
func (m *mockNode) PutData(key string, value interface{}) (bool, error) {
    return true, nil
}
func (m *mockNode) GetData(key string) (bool, error) {
    return true, nil
}
func (m *mockNode) DeleteData(key string) error {
    return nil
}
func (m *mockNode) ShardHeartbeatHandler(ctx context.Context, req *pb.ShardHeartbeatRequest) (*pb.ShardHeartbeatResponse, error) {
 return &pb.ShardHeartbeatResponse{}, nil
}
func (m *mockNode) Log(level node.LogLevel, format string, v ...interface{}) {
    if level <= node.LevelDebug{
  log.Printf(format, v...)
 }
}
func TestGrpcServer(t *testing.T) {
 lis, err := net.Listen("tcp", "localhost:50051")
 if err != nil {
  t.Fatalf("failed to listen: %v", err)
 }
 mockNode := &mockNode{
  Node: node.NewNode(1, "localhost:50051", 1, "8081", log.Default(), false, "", "", "", ""),
 }

 s := NewGrpcServer(mockNode, false, "", "")
 go func() {
  if err := s.Start(lis); err != nil {
   t.Fatalf("failed to serve: %v", err)
  }
 }()
    defer s.Shutdown()

 conn, err := s.GetClientConn("localhost:50051")

 if err != nil {
  t.Fatalf("did not connect: %v", err)
 }
 defer conn.Close()
 client := pb.NewNodeServiceClient(conn)
 ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
 defer cancel()

    _, err = client.Gossip(ctx, &pb.GossipRequest{})
 if err != nil {
  t.Fatalf("gossip error: %v", err)
 }

 _, err = client.PutData(ctx, &pb.PutRequest{Key: "test"})
 if err != nil {
  t.Fatalf("put error: %v", err)
 }

    _, err = client.GetData(ctx, &pb.GetRequest{Key: "test"})
 if err != nil {
        t.Fatalf("get error: %v", err)
 }

 _, err = client.DeleteData(ctx, &pb.DeleteRequest{Key: "test"})
 if err != nil {
  t.Fatalf("delete error: %v", err)
 }

    _, err = client.ShardHeartbeat(ctx, &pb.ShardHeartbeatRequest{})
    if err != nil {
  t.Fatalf("shard heartbeat error: %v", err)
    }

    testServer := grpc.NewServer()
 grpc_testing.RegisterTestServiceServer(testServer, &grpc_testing.UnimplementedTestServiceServer{})
    go func() {
  lis, err := net.Listen("tcp", "localhost:50052")
  if err != nil {
   log.Fatalf("failed to listen: %v", err)
  }
  testServer.Serve(lis)
    }()
    defer testServer.Stop()
    conn2, err := s.GetClientConn("localhost:50052")
    if err != nil{
  t.Fatalf("did not connect to test server: %v", err)
    }
    defer conn2.Close()

 fmt.Println("Test gRPC server finished")
}