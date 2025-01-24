
package raft

import (
	"context"
	"fmt"
	"math/rand"
	"time"
	pb "distributed-db/proto"
	"distributed-db/node"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Raft структура для реализации Raft
type Raft struct {
	node *node.Node
}

// NewRaft создает новую структуру Raft
func NewRaft(n *node.Node) *Raft {
	return &Raft{
		node: n,
	}
}
// StartElectionTimer starts the election timer
func (r *Raft) StartElectionTimer(nodes []*node.Node) {
	for {
		r.node.mu.Lock()
		if r.node.state != node.Leader {
			timeout := time.Duration(150+rand.Intn(150)) * time.Millisecond
			r.node.electionTimer = time.NewTimer(timeout)
			r.node.mu.Unlock()
			<-r.node.electionTimer.C
			r.node.mu.Lock()
			if r.node.state != node.Leader && r.node.isAlive {
				r.startElection(nodes)
			}
			r.node.mu.Unlock()
		} else {
			r.node.mu.Unlock()
			time.Sleep(time.Millisecond * 100)
		}
	}
}
// StartHeartbeat starts sending heartbeats to other nodes
func (r *Raft) StartHeartbeat(nodes []*node.Node, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		r.node.mu.RLock()
		if r.node.state == node.Leader && r.node.isAlive {
			r.node.mu.RUnlock()
			r.sendHeartbeats(nodes)
		} else {
			r.node.mu.RUnlock()
		}

	}
}
// StartGossip starts the gossip protocol to find the least loaded shard
func (r *Raft) StartGossip(nodes []*node.Node, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
    for range ticker.C {
		if time.Since(r.node.lastLeastLoadedShardUpdate) > 5 * time.Second {
			r.findLeastLoadedShard(nodes)
			r.node.lastLeastLoadedShardUpdate = time.Now()
		}
	}
}
// startElection starts a new election
func (r *Raft) startElection(nodes []*node.Node) {
	r.node.log(node.LevelDebug, "Node %d starting election", r.node.id)
	r.node.state = node.Candidate
	r.node.term++
	r.node.votedFor = r.node.address
	votes := 1
	for _, n := range nodes {
		if n.id == r.node.id {
			continue
		}
		go func(n *node.Node) {
			voted := r.requestVote(n)
			if voted {
				r.node.mu.Lock()
				votes++
				r.node.mu.Unlock()
			}
		}(n)
	}
	time.Sleep(time.Millisecond * 100)
	r.node.mu.Lock()
	defer r.node.mu.Unlock()
	if votes > len(nodes)/2 && r.node.state == node.Candidate{
		r.node.log(node.LevelDebug, "Node %d became a leader with %d votes", r.node.id, votes)
		r.node.state = node.Leader
		r.node.nextIndex = make(map[int]int)
		r.node.matchIndex = make(map[int]int)
		for _, n := range nodes {
			if n.id != r.node.id {
				r.node.nextIndex[n.id] = len(r.node.raftLog) + 1
				r.node.matchIndex[n.id] = 0
			}
		}
	} else {
		r.node.log(node.LevelDebug, "Node %d lost election with %d votes, becoming follower", r.node.id, votes)
		r.node.state = node.Follower
	}
}
func (r *Raft) requestVote(n *node.Node) bool {
	var opts []grpc.DialOption
	if r.node.tls {
		creds, err := credentials.NewClientTLSFromFile(r.node.clientCert, "")
		if err != nil {
			r.node.log(node.LevelError, "Failed to create client credentials: %v", err)
			return false
		}
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithInsecure())
	}

	conn, err := grpc.Dial(n.address, opts...)
	if err != nil {
		r.node.log(node.LevelError, "did not connect: %v", err)
		return false
	}
	defer func() {
		if err := conn.Close(); err != nil {
			r.node.log(node.LevelError, "Error closing gRPC connection %v", err)
		}
	}()
	client := pb.NewNodeServiceClient(conn)
	req := &pb.VoteRequest{
		Term:         int32(r.node.term),
		LastLogIndex: int32(len(r.node.raftLog)),
		LastLogTerm:  int32(r.node.getLastLogTerm()),
	}
	resp, err := client.RequestVote(context.Background(), req)
	if err != nil {
		r.node.log(node.LevelError, "Error sending gRPC vote request %v", err)
		return false
	}
	return resp.Voted
}
// sendHeartbeats sends heartbeats to all other nodes
func (r *Raft) sendHeartbeats(nodes []*node.Node) {
	for _, n := range nodes {
		if n.id == r.node.id {
			continue
		}
		go func(n *node.Node) {
			r.sendAppendEntries(n)
		}(n)
	}
}
func (r *Raft) sendAppendEntries(n *node.Node) {
	r.node.mu.RLock()
	defer r.node.mu.RUnlock()

	var opts []grpc.DialOption
	if r.node.tls {
		creds, err := credentials.NewClientTLSFromFile(r.node.clientCert, "")
		if err != nil {
			r.node.log(node.LevelError, "Failed to create client credentials: %v", err)
			return
		}
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithInsecure())
	}

	conn, err := grpc.Dial(n.address, opts...)
	if err != nil {
		r.node.log(node.LevelError, "did not connect: %v", err)
		return
	}
	defer func() {
		if err := conn.Close(); err != nil {
			r.node.log(node.LevelError, "Error closing gRPC connection %v", err)
		}
	}()

	client := pb.NewNodeServiceClient(conn)

	prevLogIndex := r.node.nextIndex[n.id] - 1

	var prevLogTerm int
	if prevLogIndex > 0 {
		prevLogTerm = r.node.raftLog[prevLogIndex-1].Term
	}

	entries := r.node.raftLog[prevLogIndex:]
	pbEntries := make([]*pb.LogEntry, len(entries))

	for i, entry := range entries {
		pbEntries[i] = &pb.LogEntry{
			Term:    int32(entry.Term),
			Key:  entry.Key,
			Committed: entry.Committed,
		}
	}


	req := &pb.AppendEntriesRequest{
		Term:            int32(r.node.term),
		LeaderId:        int32(r.node.id),
		PrevLogIndex:  int32(prevLogIndex),
		PrevLogTerm:    int32(prevLogTerm),
		Entries:         pbEntries,
		LeaderCommitIndex: int32(r.node.commitIndex),
	}

	resp, err := client.HandleAppendEntries(context.Background(), req)
	if err != nil {
		r.node.log(node.LevelError,"Error sending gRPC append entries request: %v", err)
		return
	}
	if resp.Success {
		r.node.nextIndex[n.id] = len(r.node.raftLog) + 1
		r.node.matchIndex[n.id] = len(r.node.raftLog)
		r.node.applyLogEntries()
	} else {
		r.node.nextIndex[n.id] = r.node.nextIndex[n.id] - 1
		if r.node.nextIndex[n.id] < 1 {
			r.node.nextIndex[n.id] = 1
		}
	}
}
func (r *Raft) findLeastLoadedShard(nodes []*node.Node) {
	minLoad := int64(9223372036854775807)
	var leastLoadedShard *node.Node
	for _, n := range nodes {
		if n.id == r.node.id {
			continue
		}
		load := r.getNodeLoad(n)
		if load < minLoad {
			minLoad = load
			leastLoadedShard = n
		}
	}
	r.node.mu.Lock()
	defer r.node.mu.Unlock()
	r.node.leastLoadedShard = leastLoadedShard
}

func (r *Raft) getNodeLoad(n *node.Node) int64 {
	var opts []grpc.DialOption
	if r.node.tls {
		creds, err := credentials.NewClientTLSFromFile(r.node.clientCert, "")
		if err != nil {
			r.node.log(node.LevelError, "Failed to create client credentials: %v", err)
			return 0
		}
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithInsecure())
	}
	conn, err := grpc.Dial(n.address, opts...)
	if err != nil {
		r.node.log(node.LevelError, "did not connect: %v", err)
		return 0
	}
	defer func() {
		if err := conn.Close(); err != nil {
			r.node.log(node.LevelError, "Error closing gRPC connection %v", err)
		}
	}()

	client := pb.NewNodeServiceClient(conn)
	req := &pb.MetricsRequest{}
	resp, err := client.SendMetrics(context.Background(), req)
	if err != nil {
		r.node.log(node.LevelError, "Error getting node load: %v", err)
		return 0
	}
	return resp.Load
}
