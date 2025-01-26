package raft

import (
 "context"
 "log"
 "net"
 "testing"
 "time"
 "distributed-db/node"
 pb "distributed-db/proto"
 "google.golang.org/grpc"
)
type mockNode struct {
    *node.Node
    raft *Raft
}

func (m *mockNode) Raft() *Raft {
    return m.raft
}
func (m *mockNode) GetConn(address string) (*grpc.ClientConn, error) {
 lis, err := net.Listen("tcp", address)
 if err != nil {
  log.Fatalf("failed to listen: %v", err)
  return nil, err
 }
 s := grpc.NewServer()
    pb.RegisterNodeServiceServer(s, m)

 go func() {
        if err := s.Serve(lis); err != nil {
   log.Fatalf("failed to serve: %v", err)
        }
 }()

    conn, err := grpc.Dial(address, grpc.WithInsecure())
 if err != nil {
  log.Fatalf("did not connect: %v", err)
  return nil, err
 }
 return conn, nil

}

func (m *mockNode) PutConn(address string, conn *grpc.ClientConn) {
    conn.Close()
}
func (m *mockNode) Log(level node.LogLevel, format string, v ...interface{}) {
    if level <= node.LevelDebug{
  log.Printf(format, v...)
 }
}
func (m *mockNode) ShardHeartbeatHandler(ctx context.Context, req *pb.ShardHeartbeatRequest) (*pb.ShardHeartbeatResponse, error) {
 return &pb.ShardHeartbeatResponse{}, nil
}
func TestRaftElection(t *testing.T) {
    mockNode1 := &mockNode{
  Node: node.NewNode(1, "localhost:50060", 1, "8081", log.Default(), false, "", "", "", ""),
 }
    mockNode2 := &mockNode{
  Node: node.NewNode(2, "localhost:50061", 1, "8082", log.Default(), false, "", "", "", ""),
    }
 mockNode3 := &mockNode{
  Node: node.NewNode(3, "localhost:50062", 1, "8083", log.Default(), false, "", "", "", ""),
 }
 mockNode1.raft = NewRaft(mockNode1.Node)
 mockNode2.raft = NewRaft(mockNode2.Node)
 mockNode3.raft = NewRaft(mockNode3.Node)
 nodes := []*node.Node{
        mockNode1.Node,
        mockNode2.Node,
        mockNode3.Node,
    }
    mockNode1.raft.Start(nodes)
    mockNode2.raft.Start(nodes)
    mockNode3.raft.Start(nodes)

 time.Sleep(time.Second * 10)

 if mockNode1.Node.State() != node.Leader &&  mockNode2.Node.State() != node.Leader && mockNode3.Node.State() != node.Leader{
        t.Error("No leader elected")
 }
 if mockNode1.Node.State() == node.Leader &&  mockNode2.Node.State() == node.Leader || mockNode1.Node.State() == node.Leader && mockNode3.Node.State() == node.Leader || mockNode2.Node.State() == node.Leader && mockNode3.Node.State() == node.Leader{
        t.Error("More than one leader elected")
    }
}
func TestRaftHeartbeat(t *testing.T){
 mockNode1 := &mockNode{
  Node: node.NewNode(1, "localhost:50063", 1, "8081", log.Default(), false, "", "", "", ""),
 }
    mockNode2 := &mockNode{
  Node: node.NewNode(2, "localhost:50064", 1, "8082", log.Default(), false, "", "", "", ""),
    }
    mockNode1.raft = NewRaft(mockNode1.Node)
 mockNode2.raft = NewRaft(mockNode2.Node)

 nodes := []*node.Node{
        mockNode1.Node,
        mockNode2.Node,
    }
    mockNode1.raft.Start(nodes)
    mockNode2.raft.Start(nodes)


 time.Sleep(time.Second * 10)


    if mockNode1.Node.State() == node.Leader{
  mockNode2.raft.CheckHeartbeat()
        if mockNode2.Node.State() == node.Candidate{
   t.Error("Heartbeat must not start election")
  }
    }
 if mockNode2.Node.State() == node.Leader{
  mockNode1.raft.CheckHeartbeat()
        if mockNode1.Node.State() == node.Candidate{
   t.Error("Heartbeat must not start election")
  }
    }
    if mockNode1.Node.State() != node.Leader && mockNode2.Node.State() != node.Leader{
        t.Error("No leader elected")
    }
}

func TestRaftLogReplication(t *testing.T) {
 mockNode1 := &mockNode{
  Node: node.NewNode(1, "localhost:50065", 1, "8081", log.Default(), false, "", "", "", ""),
 }
    mockNode2 := &mockNode{
  Node: node.NewNode(2, "localhost:50066", 1, "8082", log.Default(), false, "", "", "", ""),
    }
    mockNode1.raft = NewRaft(mockNode1.Node)
 mockNode2.raft = NewRaft(mockNode2.Node)
 nodes := []*node.Node{
        mockNode1.Node,
        mockNode2.Node,
    }
    mockNode1.raft.Start(nodes)
    mockNode2.raft.Start(nodes)

 time.Sleep(time.Second * 10)

 if mockNode1.Node.State() == node.Leader {
  mockNode1.raft.PutData("test")
 }
 if mockNode2.Node.State() == node.Leader {
  mockNode2.raft.PutData("test")
 }
 time.Sleep(time.Second * 2)
    if mockNode1.Node.State() == node.Leader {
  _, err := mockNode2.raft.GetData("test")
  if err != nil{
   t.Error("Data not found on follower")
  }
 }
    if mockNode2.Node.State() == node.Leader {
  _, err := mockNode1.raft.GetData("test")
  if err != nil{
   t.Error("Data not found on follower")
  }
    }
}