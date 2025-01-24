package grpc

import (
	"context"
	"errors"
	"distributed-db/internal/data"
	pb "distributed-db/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"time"
	"distributed-db/node"
)
type GRPCServer struct {
	pb.UnimplementedNodeServiceServer
	node *node.Node
}
// NewGRPCServer creates a new gRPC server
func NewGRPCServer(n *node.Node) *GRPCServer {
    return &GRPCServer{
        node: n,
    }
}
func (s *GRPCServer) PutData(ctx context.Context, req *pb.PutRequest) (*pb.PutResponse, error) {
	if !s.node.limiter.Allow() {
		s.node.log(node.LevelError, "Node %d: Rate limit exceeded for PUT request for key %s", s.node.id, req.Key)
		return &pb.PutResponse{Success: false}, nil
	}
	if len(req.Key) > 255 {
		s.node.log(node.LevelError, "Key is too long")
		return &pb.PutResponse{Success: false}, nil
	}
	err := s.node.hashTable.Put(req.Key)
	if errors.Is(err, data.ErrKeyExists){
		s.node.log(node.LevelDebug, "Node %d key %s already exists", s.node.id, req.Key)
		return &pb.PutResponse{Success: false}, nil
	}
	if err != nil {
		s.node.log(node.LevelError, "Node %d error putting key %s to local db: %v", s.node.id, req.Key, err)
		return &pb.PutResponse{Success: false}, nil
	}
	s.node.log(node.LevelDebug, "Node %d stored key %s locally", s.node.id, req.Key)
    s.node.appendLog(req.Key)
	s.node.replicateLog()
	return &pb.PutResponse{Success: true}, nil
}

func (s *GRPCServer) GetData(ctx context.Context, req *pb.GetRequest) (*pb.GetResponse, error) {
	if !s.node.limiter.Allow() {
		s.node.log(node.LevelError, "Node %d: Rate limit exceeded for GET request for key %s", s.node.id, req.Key)
		return &pb.GetResponse{Value: false}, nil
	}
	if len(req.Key) > 255 {
			s.node.log(node.LevelError, "Key is too long")
			return &pb.GetResponse{Value: false}, nil
		}
	value := s.node.hashTable.Get(req.Key)
	return &pb.GetResponse{Value: value}, nil
}

func (s *GRPCServer) HandleAppendEntries(ctx context.Context, req *pb.AppendEntriesRequest) (*pb.AppendEntriesResponse, error) {
	s.node.mu.Lock()
	defer s.node.mu.Unlock()
    if !s.node.isAlive {
		s.node.log(node.LevelError, "Node %d is not alive. Can't handle append entries request", s.node.id)
		return &pb.AppendEntriesResponse{Success: false,}, nil
	}
	if req.Term < int32(s.node.term) {
		s.node.log(node.LevelDebug, "Node %d rejected append entries request, term: %d is less then my term: %d", s.node.id, req.Term, s.node.term)
		return &pb.AppendEntriesResponse{Success: false,}, nil
	}

	if req.Term > int32(s.node.term) {
		s.node.log(node.LevelDebug, "Node %d received append entries request for term: %d, my term is: %d, set my term to %d", s.node.id, req.Term, s.node.term, req.Term)
		s.node.term = int(req.Term)
		s.node.votedFor = ""
		s.node.state = node.Follower
	}
	s.node.lastLeaderHeartbeat = time.Now()
	if len(s.node.raftLog) < int(req.PrevLogIndex) {
		s.node.log(node.LevelDebug, "Node %d rejected append entries request, my log length is %d, prev log index is: %d", s.node.id, len(s.node.raftLog), req.PrevLogIndex)
		return &pb.AppendEntriesResponse{Success: false}, nil
	}
	if req.PrevLogIndex > 0 && s.node.raftLog[req.PrevLogIndex-1].Term != int(req.PrevLogTerm) {
		s.node.log(node.LevelDebug, "Node %d rejected append entries request, prev log term is: %d, my term on that index: %d", s.node.id, req.PrevLogTerm, s.node.raftLog[req.PrevLogIndex-1].Term)
		return &pb.AppendEntriesResponse{Success: false}, nil
	}

	entries := req.Entries
	for i, entry := range entries {
        index := int(req.PrevLogIndex) + 1 + i
		if len(s.node.raftLog) > index-1 {
			if s.node.raftLog[index-1].Term != int(entry.Term) {
				s.node.log(node.LevelDebug, "Node %d deleting log entries starting from %d, because term is not matching", s.node.id, index)
				s.node.raftLog = s.node.raftLog[:index-1]
				s.node.raftLog = append(s.node.raftLog, node.logEntry{
					Term:    int(entry.Term),
					Key:  entry.Key,
					Committed: entry.Committed,
				})
			}
		} else {
			s.node.log(node.LevelDebug, "Node %d appending log entry: %v to local log", s.node.id, entry)
			s.node.raftLog = append(s.node.raftLog, node.logEntry{
				Term:    int(entry.Term),
				Key:  entry.Key,
				Committed: entry.Committed,
			})
		}


    }
    if int(req.LeaderCommitIndex) > s.node.commitIndex {
        s.node.commitIndex = min(int(req.LeaderCommitIndex), len(s.node.raftLog))
		s.node.applyLogEntries()
		s.node.log(node.LevelDebug,"Node %d commit index was set to %d, applying entries", s.node.id, s.node.commitIndex)
    }

	return &pb.AppendEntriesResponse{Success: true}, nil
}
func (s *GRPCServer) HandleHeartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	s.node.mu.Lock()
	defer s.node.mu.Unlock()
	if !s.node.isAlive {
		s.node.log(node.LevelError, "Node %d is not alive. Can't handle heartbeat request", s.node.id)
		return &pb.HeartbeatResponse{}, nil
	}
    if req.Term > int32(s.node.term) {
		s.node.log(node.LevelDebug, "Node %d received heartbeat request for term: %d, my term is: %d, set my term to %d", s.node.id, req.Term, s.node.term, req.Term)
		s.node.term = int(req.Term)
		s.node.votedFor = ""
		s.node.state = node.Follower
	}
	s.node.lastLeaderHeartbeat = time.Now()
	return &pb.HeartbeatResponse{}, nil
}
func (s *GRPCServer) RequestVote(ctx context.Context, req *pb.VoteRequest) (*pb.VoteResponse, error) {
	s.node.mu.Lock()
	defer s.node.mu.Unlock()
	if !s.node.isAlive {
		s.node.log(node.LevelError,"Node %d is not alive. Can't handle vote request", s.node.id)
		return &pb.VoteResponse{Voted: false}, nil
	}
	if int(req.Term) > s.node.term {
		s.node.log(node.LevelDebug, "Node %d received vote request for term: %d, my term is: %d, voted for: %s", s.node.id, req.Term, s.node.term, s.node.votedFor)
		s.node.term = int(req.Term)
		s.node.votedFor = ""
		s.node.state = node.Follower
		return &pb.VoteResponse{Voted: true}, nil

	} else if int(req.Term) == s.node.term && (s.node.votedFor == "" || s.node.votedFor == s.node.address) && s.node.isLogUpToDate(int(req.Term), int(req.LastLogIndex)) {
		s.node.log(node.LevelDebug, "Node %d received vote request for term: %d, my term is: %d, voted for: %s", s.node.id, req.Term, s.node.term, s.node.votedFor)
		s.node.votedFor = s.node.address
		s.node.state = node.Follower
		return &pb.VoteResponse{Voted: true}, nil
	} else {
		s.node.log(node.LevelDebug, "Node %d rejected vote request for term: %d, my term is: %d, voted for: %s", s.node.id, req.Term, s.node.term, s.node.votedFor)
		return &pb.VoteResponse{Voted: false}, nil
	}
}

func (s *GRPCServer) SendHeartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	s.node.mu.Lock()
	defer s.node.mu.Unlock()
	if !s.node.isAlive {
		s.node.log(node.LevelError, "Node %d is not alive. Can't handle heartbeat request", s.node.id)
		return &pb.HeartbeatResponse{}, nil
	}
    if req.Term > int32(s.node.term) {
		s.node.log(node.LevelDebug, "Node %d received heartbeat request for term: %d, my term is: %d, set my term to %d", s.node.id, req.Term, s.node.term, req.Term)
		s.node.term = int(req.Term)
		s.node.votedFor = ""
		s.node.state = node.Follower
	}
	s.node.lastLeaderHeartbeat = time.Now()
	return &pb.HeartbeatResponse{}, nil
}
func (s *GRPCServer) sendPutRequest(key string, address string) bool {
	var opts []grpc.DialOption
	if s.node.tls {
		creds, err := credentials.NewClientTLSFromFile(s.node.clientCert, "")
		if err != nil {
			s.node.log(node.LevelError, "Failed to create client credentials: %v", err)
			return false
		}
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithInsecure())
	}
	conn, err := grpc.Dial(address, opts...)
	if err != nil {
		s.node.log(node.LevelError,"did not connect: %v", err)
		return false
	}
	defer func() {
		if err := conn.Close(); err != nil {
			s.node.log(node.LevelError,"Error closing gRPC connection %v", err)
		}
	}()
	client := pb.NewNodeServiceClient(conn)
	req := &pb.PutRequest{
		Key: key,
	}
	_, err = client.PutData(context.Background(), req)
	if err != nil {
		s.node.log(node.LevelError,"Error sending gRPC put request: %v", err)
		return false
	}
	return true
}
