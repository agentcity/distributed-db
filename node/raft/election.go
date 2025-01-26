package raft

import (
 "context"
 "fmt"
 "sync"
 "time"
 "distributed-db/node"
 pb "distributed-db/proto"
)

func (r *Raft) startElection() {
 r.node.Log(node.LevelInfo, "Start election")
 r.node.IncrementTerm()
 r.node.SetState(node.Candidate)
 r.node.SetVotedFor(r.node.Address())
 votes := 1
    var wg sync.WaitGroup
 for _, peer := range r.node.Nodes() {
        if peer.Address() == r.node.Address() {
            continue
        }
        wg.Add(1)
  go func(peer *node.Node) {
            defer wg.Done()
   vote, err := r.sendRequestVote(peer)
            if err != nil {
    r.node.Log(node.LevelError, "Request vote error: %v", err)
    return
   }

   if vote {
                r.mu.Lock()
    votes++
    r.mu.Unlock()
   }
  }(peer)

 }
    wg.Wait()

    r.mu.Lock()
 defer r.mu.Unlock()
 if votes > len(r.node.Nodes())/2 {
        r.leader = r.node.Address()
  r.node.Log(node.LevelInfo, "Become a leader")
        r.node.SetState(node.Leader)
        r.sendHeartbeat()
 } else {
  r.node.Log(node.LevelInfo, "Election failed")
        r.node.SetState(node.Follower)
    }
}

func (r *Raft) sendRequestVote(peer *node.Node) (bool, error) {
 conn, err := r.node.GetConn(peer.Address())
    if err != nil {
  return false, fmt.Errorf("failed to get connection: %w", err)
 }
 defer r.node.PutConn(peer.Address(), conn)
    client := pb.NewNodeServiceClient(conn)
 ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    req := r.prepareRequestVoteRequest()
 resp, err := client.RequestVote(ctx, req)
 if err != nil {
        return false, fmt.Errorf("error on request vote: %v, error: %w", err)
    }
 return r.handleRequestVoteResponse(resp)
}
func (r *Raft) prepareRequestVoteRequest() *pb.RequestVoteRequest{
 req := &pb.RequestVoteRequest{
  Term: int32(r.node.Term()),
  CandidateId: r.node.Address(),
  LastLogIndex: int32(len(r.node.RaftLog())),
 }
    if len(r.node.RaftLog()) > 0{
        req.LastLogTerm = int32(r.node.RaftLog()[len(r.node.RaftLog()) -1].Term)
    }
    return req
}
func (r *Raft) handleRequestVoteResponse(resp *pb.RequestVoteResponse) (bool, error){
    if resp.Term > int32(r.node.Term()) {
        r.node.SetTerm(int(resp.Term))
        r.node.SetState(node.Follower)
        return false, fmt.Errorf("term is higher: %v", resp.Term)
    }
    return resp.VoteGranted, nil
}