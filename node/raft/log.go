package raft

import (
 pb "distributed-db/proto"
    "distributed-db/node"
)

func (r *Raft) isLogUpToDate(lastLogTerm int32, lastLogIndex int32) bool {
    if len(r.node.RaftLog()) == 0{
        return true
    }

    lastLog := r.node.RaftLog()[len(r.node.RaftLog()) - 1]

    if int32(lastLog.Term) > lastLogTerm{
        return false
    }
    if  int32(lastLog.Term) == lastLogTerm && int32(len(r.node.RaftLog())) > lastLogIndex{
        return false
    }
    return true
}
func (r *Raft) isLogConsistent(prevLogIndex int32, prevLogTerm int32) bool {
    if prevLogIndex == 0 && len(r.node.RaftLog()) == 0 {
  return true
 }
    if prevLogIndex > int32(len(r.node.RaftLog())-1){
        return false
    }
    if r.node.RaftLog()[prevLogIndex].Term != int(prevLogTerm){
        return false
    }
    return true
}
func (r *Raft) findConflictIndex(prevLogIndex int32, prevLogTerm int32) int {
 if prevLogIndex > int32(len(r.node.RaftLog())-1) {
  return len(r.node.RaftLog())
 }
    for i := int(prevLogIndex); i >= 0 ; i--{
        if r.node.RaftLog()[i].Term != int(prevLogTerm) {
            return i+1
        }
    }
 return 0
}
func (r *Raft) filterNewEntries(entries []*pb.LogEntry) []*node.LogEntry {
    newEntries := make([]*node.LogEntry, 0)
    for _, entry := range entries {
        newEntries = append(newEntries, &node.LogEntry{
            Term:  int(entry.Term),
            Index: int(entry.Index),
            Key:   entry.Key,
        })
    }
    return newEntries
}