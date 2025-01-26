package node

import "distributed-db/internal/common"

type ApplyData struct {
 Key   string
 Value []byte
}

type ApplyDeleteData struct {
 Key string
}


type NodeState struct {
 NodeInfo       common.NodeInfo
 CurrentTerm    int
 VotedFor       int
 CommitIndex    int
 LastApplied    int
 Log            []LogEntry
 ShardLeaders   map[int]common.NodeInfo
}

type LogEntry struct {
 Term    int
 Index   int
 Data    ApplyData
    DeleteData ApplyDeleteData
    IsDelete bool
}