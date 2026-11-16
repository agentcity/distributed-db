package coordinator

import (
	"context"
	"errors"
	"strings"
	"time"

	"distributed-db/internal/pb"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Get proxies a read to the leader of the key's shard.
func (c *Coordinator) Get(ctx context.Context, req *pb.GetRequest) (*pb.GetResponse, error) {
	key := req.GetKey()
	if key == "" {
		return nil, status.Error(codes.InvalidArgument, "key is required")
	}

	start := time.Now()
	shard := c.metricShard(key)
	var resp *pb.GetResponse
	leader, err := c.proxy(ctx, "get", key, func(ctx context.Context, client pb.NodeServiceClient, _ string) (string, string, error) {
		r, callErr := client.Get(ctx, &pb.GetRequest{Key: key})
		if callErr != nil {
			return "", "", callErr
		}
		resp = r
		return r.GetLeader(), r.GetError(), nil
	})
	if err != nil {
		c.observeRequest("get", shard, resultLabel(err), start)
		var notLeader *notLeaderError
		if errors.As(err, &notLeader) {
			return &pb.GetResponse{Leader: notLeader.Leader(), Error: "not leader"}, nil
		}
		return nil, status.Error(codes.Unavailable, err.Error())
	}

	result := "ok"
	switch {
	case resp.GetError() != "":
		result = resultLabel(errors.New(resp.GetError()))
	case !resp.GetFound():
		result = "not_found"
	}
	c.observeRequest("get", shard, result, start)
	if resp.GetLeader() == "" && leader != "" {
		resp.Leader = leader
	}
	return resp, nil
}

// Put proxies a write to the leader of the key's shard.
func (c *Coordinator) Put(ctx context.Context, req *pb.PutRequest) (*pb.PutResponse, error) {
	key := req.GetKey()
	if key == "" {
		return nil, status.Error(codes.InvalidArgument, "key is required")
	}

	start := time.Now()
	shard := c.metricShard(key)
	var resp *pb.PutResponse
	leader, err := c.proxy(ctx, "put", key, func(ctx context.Context, client pb.NodeServiceClient, _ string) (string, string, error) {
		r, callErr := client.Put(ctx, &pb.PutRequest{Key: key, Value: req.GetValue()})
		if callErr != nil {
			return "", "", callErr
		}
		resp = r
		return r.GetLeader(), r.GetError(), nil
	})
	if err != nil {
		c.observeRequest("put", shard, resultLabel(err), start)
		var notLeader *notLeaderError
		if errors.As(err, &notLeader) {
			return &pb.PutResponse{Leader: notLeader.Leader(), Error: "not leader"}, nil
		}
		return nil, status.Error(codes.Unavailable, err.Error())
	}

	result := "ok"
	if resp.GetError() != "" {
		result = resultLabel(errors.New(resp.GetError()))
	} else if !resp.GetSuccess() {
		result = "error"
	}
	c.observeRequest("put", shard, result, start)
	if resp.GetLeader() == "" && leader != "" {
		resp.Leader = leader
	}
	return resp, nil
}

// Delete proxies a delete to the leader of the key's shard.
func (c *Coordinator) Delete(ctx context.Context, req *pb.DeleteRequest) (*pb.DeleteResponse, error) {
	key := req.GetKey()
	if key == "" {
		return nil, status.Error(codes.InvalidArgument, "key is required")
	}

	start := time.Now()
	shard := c.metricShard(key)
	var resp *pb.DeleteResponse
	leader, err := c.proxy(ctx, "delete", key, func(ctx context.Context, client pb.NodeServiceClient, _ string) (string, string, error) {
		r, callErr := client.Delete(ctx, &pb.DeleteRequest{Key: key})
		if callErr != nil {
			return "", "", callErr
		}
		resp = r
		return r.GetLeader(), r.GetError(), nil
	})
	if err != nil {
		c.observeRequest("delete", shard, resultLabel(err), start)
		var notLeader *notLeaderError
		if errors.As(err, &notLeader) {
			return &pb.DeleteResponse{Leader: notLeader.Leader(), Error: "not leader"}, nil
		}
		return nil, status.Error(codes.Unavailable, err.Error())
	}

	result := "ok"
	if resp.GetError() != "" {
		result = resultLabel(errors.New(resp.GetError()))
	} else if !resp.GetSuccess() {
		result = "error"
	}
	c.observeRequest("delete", shard, result, start)
	if resp.GetLeader() == "" && leader != "" {
		resp.Leader = leader
	}
	return resp, nil
}

// Status returns the coordinator identity. The protobuf StatusResponse has no
// fields for the shard map, leader cache or load samples, so the full status
// is served by HTTP GET /v1/status; here Shard carries the comma-separated
// shard names.
func (c *Coordinator) Status(_ context.Context, _ *pb.StatusRequest) (*pb.StatusResponse, error) {
	return &pb.StatusResponse{
		Id:    "coordinator",
		Role:  "coordinator",
		Shard: strings.Join(c.shardNames, ","),
	}, nil
}

// ReportLoad stores a node load sample keyed by node and shard.
func (c *Coordinator) ReportLoad(_ context.Context, req *pb.LoadReportRequest) (*pb.LoadReportResponse, error) {
	c.storeLoad(req.GetNodeId(), req.GetShard(), req.GetLoad(), req.GetOps())
	c.logger.Debug("load report",
		"node", req.GetNodeId(), "shard", req.GetShard(),
		"load", req.GetLoad(), "ops", req.GetOps())
	return &pb.LoadReportResponse{Success: true}, nil
}
