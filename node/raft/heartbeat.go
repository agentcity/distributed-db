package raft

import (
 "context"
 "fmt"
 "time"
    "distributed-db/node"
 pb "distributed-db/proto"
)
func (r *Raft) sendHeartbeat() {
    r.node.mu.RLock()
 defer r.node.mu.RUnlock()

    for _, peer := range r.node.Nodes() {
        if peer.Address() == r.node.Address() {
            continue
        }
  go func(peer *node.Node) {
   err := r.sendAppendEntries(peer)
            if err != nil {
                r.node.Log(node.LevelError, "Append entries error: %v", err)
            }
  }(peer)
    }

}
func (r *Raft) sendAppendEntries(peer *node.Node) error {
    conn, err := r.node.GetConn(peer.Address())
    if err != nil {
        return fmt.Errorf("failed to get connection: %w", err)
    }
    defer r.node.PutConn(peer.Address(), conn)
    client := pb.NewNodeServiceClient(conn)
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    req := r.prepareAppendEntriesRequest(peer)

    resp, err := client.AppendEntries(ctx, req)
    if err != nil {
        return fmt.Errorf("append entries error: %v, error: %w", err)
    }
    return r.handleAppendEntriesResponse(peer, resp)
}
func (r *Raft) prepareAppendEntriesRequest(peer *node.Node) *pb.AppendEntriesRequest {
    prevLogIndex := 0
    prevLogTerm := 0
    if len(r.node.RaftLog()) > 0 {
        prevLogIndex = len(r.node.RaftLog()) - 1
        prevLogTerm = r.node.RaftLog()[len(r.node.RaftLog()) - 1].Term
    }
    req := &pb.AppendEntriesRequest{
        Term:         int32(r.node.Term()),
        LeaderId:     r.node.Address(),
        PrevLogIndex: int32(prevLogIndex),
        PrevLogTerm:  int32(prevLogTerm),
        LeaderCommit: int32(r.node.CommitIndex()),
    }

    if len(r.node.RaftLog()) > 0 && prevLogIndex >= 0 {
        entries := []*pb.LogEntry{}
        for i := prevLogIndex + 1; i < len(r.node.RaftLog()); i++ {
            entries = append(entries, &pb.LogEntry{
                Term:  int32(r.node.RaftLog()[i].Term),
                Index: int32(i),
                Key:   r.node.RaftLog()[i].Key,
            })
        }
  req.Entries = entries
    }

    return req
}
func (r *Raft) handleAppendEntriesResponse(peer *node.Node, resp *pb.AppendEntriesResponse) error {
    if resp.Term > int32(r.node.Term()) {
        r.node.SetTerm(int(resp.Term))
  r.node.SetState(node.Follower)
        return fmt.Errorf("term is higher: %v", resp.Term)
    }
    if resp.Success {
        if len(resp.Entries) > 0 {
   r.node.SetNextIndexValue(peer.Id(), int(resp.Entries[len(resp.Entries)-1].Index)+1)
   r.node.SetMatchIndexValue(peer.Id(), int(resp.Entries[len(resp.Entries)-1].Index))
  }
    }else{
  if resp.ConflictIndex > 0{
   r.node.SetNextIndexValue(peer.Id(), int(resp.ConflictIndex))
  }else{
   r.node.SetNextIndexValue(peer.Id(), 1)
  }
 }
    return nil
}