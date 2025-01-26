package grpc

import (
 "context"
 pb "distributed-db/proto"
 "google.golang.org/grpc"
)

type GrpcClient struct {
 client pb.NodeServiceClient
 conn   *grpc.ClientConn
}

func NewGrpcClient(conn *grpc.ClientConn) *GrpcClient {
 return &GrpcClient{
  client: pb.NewNodeServiceClient(conn),
  conn:   conn,
 }
}

func (c *GrpcClient) RequestVote(ctx context.Context, req *pb.RequestVoteRequest) (*pb.RequestVoteResponse, error) {
 return c.client.RequestVote(ctx, req)
}

func (c *GrpcClient) AppendEntries(ctx context.Context, req *pb.AppendEntriesRequest) (*pb.AppendEntriesResponse, error) {
 return c.client.AppendEntries(ctx, req)
}

func (c *GrpcClient) Gossip(ctx context.Context, req *pb.GossipRequest) (*pb.GossipResponse, error) {
 return c.client.Gossip(ctx, req)
}

func (c *GrpcClient) PutData(ctx context.Context, req *pb.PutRequest) (*pb.PutResponse, error) {
 return c.client.PutData(ctx, req)
}

func (c *GrpcClient) GetData(ctx context.Context, req *pb.GetRequest) (*pb.GetResponse, error) {
 return c.client.GetData(ctx, req)
}
func (c *GrpcClient) DeleteData(ctx context.Context, req *pb.DeleteRequest) (*pb.DeleteResponse, error) {
 return c.client.DeleteData(ctx, req)
}
func (c *GrpcClient) ShardHeartbeat(ctx context.Context, req *pb.ShardHeartbeatRequest) (*pb.ShardHeartbeatResponse, error) {
 return c.client.ShardHeartbeat(ctx, req)
}
func (c *GrpcClient) Close() error {
 return c.conn.Close()
}