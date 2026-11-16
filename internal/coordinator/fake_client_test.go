package coordinator

import (
	"context"
	"fmt"

	"distributed-db/internal/pb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeNodeClient is an in-memory pb.NodeServiceClient. Only the KV and Status
// methods are wired; Raft RPCs are not used by the coordinator.
type fakeNodeClient struct {
	get    func(ctx context.Context, req *pb.GetRequest) (*pb.GetResponse, error)
	put    func(ctx context.Context, req *pb.PutRequest) (*pb.PutResponse, error)
	del    func(ctx context.Context, req *pb.DeleteRequest) (*pb.DeleteResponse, error)
	status func(ctx context.Context, req *pb.StatusRequest) (*pb.StatusResponse, error)
}

var _ pb.NodeServiceClient = (*fakeNodeClient)(nil)

func (f *fakeNodeClient) RequestVote(context.Context, *pb.RequestVoteRequest, ...grpc.CallOption) (*pb.RequestVoteResponse, error) {
	return nil, status.Error(codes.Unimplemented, "RequestVote not implemented")
}

func (f *fakeNodeClient) AppendEntries(context.Context, *pb.AppendEntriesRequest, ...grpc.CallOption) (*pb.AppendEntriesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "AppendEntries not implemented")
}

func (f *fakeNodeClient) Get(ctx context.Context, req *pb.GetRequest, _ ...grpc.CallOption) (*pb.GetResponse, error) {
	if f.get == nil {
		return nil, status.Error(codes.Unimplemented, "Get not implemented")
	}
	return f.get(ctx, req)
}

func (f *fakeNodeClient) Put(ctx context.Context, req *pb.PutRequest, _ ...grpc.CallOption) (*pb.PutResponse, error) {
	if f.put == nil {
		return nil, status.Error(codes.Unimplemented, "Put not implemented")
	}
	return f.put(ctx, req)
}

func (f *fakeNodeClient) Delete(ctx context.Context, req *pb.DeleteRequest, _ ...grpc.CallOption) (*pb.DeleteResponse, error) {
	if f.del == nil {
		return nil, status.Error(codes.Unimplemented, "Delete not implemented")
	}
	return f.del(ctx, req)
}

func (f *fakeNodeClient) Status(ctx context.Context, req *pb.StatusRequest, _ ...grpc.CallOption) (*pb.StatusResponse, error) {
	if f.status == nil {
		return &pb.StatusResponse{}, nil
	}
	return f.status(ctx, req)
}

// fakeClients maps a node address to its fake client.
type fakeClients map[string]*fakeNodeClient

// newNodeClientFactory returns a client factory that hands out the fake for
// addr, failing for unknown addresses.
func (fc fakeClients) newNodeClientFactory() func(string) (pb.NodeServiceClient, error) {
	return func(addr string) (pb.NodeServiceClient, error) {
		client, ok := fc[addr]
		if !ok {
			return nil, fmt.Errorf("no fake client for %s", addr)
		}
		return client, nil
	}
}
