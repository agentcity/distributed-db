package raft

import (
 "context"
 "math/rand"
 "time"
 "distributed-db/node"
 pb "distributed-db/proto"
)

func (r *Raft) RequestVoteHandler(ctx context.Context, req *pb.RequestVoteRequest) (*pb.RequestVoteResponse, error) {
 r.node.mu.Lock()
 defer r.node.mu.Unlock()

 if int(req.Term) < r.node.Term(){
        return &pb.RequestVoteResponse{Term: int32(r.node.Term()), VoteGranted: false}, nil
    }

 if int(req.Term) > r.node.Term(){
  r.node.SetTerm(int(req.Term))
  r.node.SetVotedFor("")
        r.node.SetState(node.Follower)
    }
 return r.processRequestVote(req)
}
func (r *Raft) processRequestVote(req *pb.RequestVoteRequest) (*pb.RequestVoteResponse, error) {
    if r.node.VotedFor() == "" || r.node.VotedFor() == req.CandidateId {
        if r.isLogUpToDate(req.LastLogTerm, req.LastLogIndex) {
            r.node.SetVotedFor(req.CandidateId)
            r.electionTimer.Reset(time.Duration(rand.Intn(150) + 150) * time.Millisecond)
            return &pb.RequestVoteResponse{Term: int32(r.node.Term()), VoteGranted: true}, nil
        }
    }
    return &pb.RequestVoteResponse{Term: int32(r.node.Term()), VoteGranted: false}, nil
}
func (r *Raft) AppendEntriesHandler(ctx context.Context, req *pb.AppendEntriesRequest) (*pb.AppendEntriesResponse, error) {
 r.node.mu.Lock()
 defer r.node.mu.Unlock()
    if req.Term < int32(r.node.Term()) {
        return &pb.AppendEntriesResponse{Term: int32(r.node.Term()), Success: false}, nil
    }

    if req.Term > int32(r.node.Term()) {
        r.node.SetTerm(int(req.Term))
  r.node.SetVotedFor("")
        r.node.SetState(node.Follower)
    }
    r.node.lastLeaderHeartbeat = time.Now()
    r.electionTimer.Reset(time.Duration(rand.Intn(150) + 150) * time.Millisecond)

    return r.processAppendEntries(req)

}
func (r *Raft) processAppendEntries(req *pb.AppendEntriesRequest) (*pb.AppendEntriesResponse, error) {
    if !r.isLogConsistent(req.PrevLogIndex, req.PrevLogTerm) {
        conflictIndex := r.findConflictIndex(req.PrevLogIndex, req.PrevLogTerm)
  return &pb.AppendEntriesResponse{Term: int32(r.node.Term()), Success: false, ConflictIndex: int32(conflictIndex)}, nil
    }
    entries := r.filterNewEntries(req.Entries)
    r.node.raftLog = append(r.node.RaftLog()[:int(req.PrevLogIndex)+1], entries...)
    if req.LeaderCommit > int32(r.node.CommitIndex()) {
        commitIndex := int(req.LeaderCommit)
        if len(r.node.RaftLog()) < commitIndex{
            commitIndex = len(r.node.RaftLog())
        }
        r.updateCommitIndex(commitIndex)
    }

 return &pb.AppendEntriesResponse{Term: int32(r.node.Term()), Success: true, Entries: req.Entries}, nil
}