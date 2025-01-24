package metrics

import (
 "context"
 "time"
 pb "distributed-db/proto"
 "distributed-db/node"
 "google.golang.org/grpc"
 "google.golang.org/grpc/credentials"
)
type Metrics struct {
 node *node.Node
}
// NewMetrics creates a new Metrics structure
func NewMetrics(n *node.Node) *Metrics {
 return &Metrics{
  node: n,
 }
}
// StartLoadReport starts the goroutine to send load report
func (m *Metrics) StartLoadReport(interval time.Duration) {
 ticker := time.NewTicker(interval)
 defer ticker.Stop()
 for range ticker.C {
  m.sendLoadReport()
 }
}
func (m *Metrics) sendLoadReport() {
 if m.node.coordinatorAddress == "" {
  return
 }
 m.node.mu.RLock()
 if !m.node.isAlive {
  m.node.mu.RUnlock()
  return
 }
 m.node.mu.RUnlock()

 var opts []grpc.DialOption
 if m.node.tls {
  creds, err := credentials.NewClientTLSFromFile(m.node.clientCert, "")
  if err != nil {
   m.node.log(node.LevelError, "Failed to create client credentials: %v", err)
   return
  }
  opts = append(opts, grpc.WithTransportCredentials(creds))
 } else {
  opts = append(opts, grpc.WithInsecure())
 }
 conn, err := grpc.Dial("localhost"+m.node.coordinatorAddress, opts...)
 if err != nil {
  m.node.log(node.LevelError, "did not connect to coordinator: %v", err)
  return
 }
 defer func() {
  if err := conn.Close(); err != nil {
   m.node.log(node.LevelError, "Error closing gRPC connection %v", err)
  }
 }()
 client := pb.NewCoordinatorServiceClient(conn)
 req := &pb.LoadReport{
  NodeId: int32(m.node.id),
  ShardId: int32(m.node.shardId),
  Load:   m.getNodeLoad(),
 }
 _, err = client.HandleLoadReport(context.Background(), req)
 if err != nil {
  m.node.log(node.LevelError, "Error sending load report: %v", err)
 }

}

func (m *Metrics) getNodeLoad() int64 {
 m.node.mu.RLock()
 defer m.node.mu.RUnlock()
 return int64(m.node.messageQueue.Len())
}
func (m *Metrics) SendMetrics(ctx context.Context, req *pb.MetricsRequest) (*pb.MetricsResponse, error){
 m.node.mu.RLock()
 defer m.node.mu.RUnlock()
    if !m.node.isAlive {
  m.node.log(node.LevelError,"Node %d is not alive. Can't send metrics", m.node.id)
  return &pb.MetricsResponse{Load: 0}, nil
 }
 return &pb.MetricsResponse{Load: int64(m.node.messageQueue.Len())}, nil
}
