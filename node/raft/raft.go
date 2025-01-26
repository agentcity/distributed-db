package raft

import (
 "sync"
 "time"
 "distributed-db/node"
)

type Raft struct {
 node *node.Node
    mu sync.RWMutex
    leader string
 electionTimer *time.Timer
 heartbeatTicker *time.Ticker
}

func NewRaft(node *node.Node) *Raft {
    r := &Raft{
  node: node,
  leader: "",
        mu: sync.RWMutex{},
  electionTimer: time.NewTimer(time.Duration(node.GetRandomElectionTimeout())),
  heartbeatTicker: time.NewTicker(100 * time.Millisecond),
    }
 return r
}
func (r *Raft) Start(nodes []*node.Node) {
    r.node.StartGossip()
 r.node.Log(node.LevelInfo, "Starting Raft")
 r.node.SetState(node.Follower)
    r.electionTimer.Reset(time.Duration(r.node.GetRandomElectionTimeout()))
    go r.runElectionTimer()
    go r.runHeartbeatTicker()
    r.node.startShardLeaderElection()
}
func (r *Raft) Shutdown() {
 r.mu.Lock()
 defer r.mu.Unlock()
    r.heartbeatTicker.Stop()
 r.electionTimer.Stop()
    r.node.Log(node.LevelInfo, "Raft shutdown")
}

func (r *Raft) runElectionTimer() {
    for {
        <-r.electionTimer.C
        r.mu.Lock()
  if r.node.State() != node.Leader{
   r.startElection()
  }
        r.electionTimer.Reset(time.Duration(r.node.GetRandomElectionTimeout()))
        r.mu.Unlock()
 }
}

func (r *Raft) runHeartbeatTicker() {
    for range r.heartbeatTicker.C {
        r.mu.Lock()
        if r.node.State() == node.Leader {
            r.sendHeartbeat()
        }
        r.mu.Unlock()
    }
}
func (r *Raft) CheckHeartbeat(){
    r.mu.Lock()
    defer r.mu.Unlock()
    if r.node.State() == node.Follower {
        if time.Since(r.node.lastLeaderHeartbeat) > time.Second*30 {
   r.node.Log(node.LevelDebug, "Heartbeat timeout")
   r.startElection()
  }
    }
}
func (r *Raft) PutData(key string) {
    r.mu.Lock()
    defer r.mu.Unlock()
 r.node.raftLog = append(r.node.RaftLog(), node.LogEntry{
        Term: r.node.Term(),
        Index: len(r.node.RaftLog()),
        Key: key,
    })
 r.updateCommitIndex(len(r.node.RaftLog()))
    r.node.Log(node.LevelDebug, "PutData to raft log: %v, index: %v", key, len(r.node.RaftLog()) - 1)
}
func (r *Raft) GetData(key string) (bool, error) {
 r.mu.RLock()
 defer r.mu.RUnlock()

 for _, entry := range r.node.RaftLog(){
        if entry.Key == key{
   return r.node.getDataLocal(key, key)
  }
 }
    return false, nil

}
func (r *Raft) DeleteData(key string) error{
 r.mu.Lock()
 defer r.mu.Unlock()
 for _, entry := range r.node.RaftLog(){
        if entry.Key == key{
   r.node.deleteDataLocal(key)
            return nil
  }
 }
 return nil
}
func (r *Raft) updateCommitIndex(commitIndex int) {
    r.node.SetCommitIndex(commitIndex)
    for i := r.node.LastApplied()+1; i <= r.node.CommitIndex(); i++{
        r.applyLog(r.node.RaftLog()[i-1])
    }
}

func (r *Raft) applyLog(entry node.LogEntry) {
    r.node.putDataLocal(entry.Key, entry.Key)
 r.node.SetLastApplied(entry.Index + 1)
    r.node.addKeyToShardData(entry.Key, r.node.getShardForHash(entry.Key))
    r.node.Log(node.LevelDebug, "Apply log: %v", entry.Key)
}