package node

import (
    "fmt"
    "time"
    pb "distributed-db/proto"
)
type nodeInfo struct {
    address string
    nextIndex map[int]int
    matchIndex map[int]int
}
type shardInfo struct {
    shardId int
    leader string
}
type gossipState struct {
    shards    map[int]shardInfo
    nodes map[string]nodeInfo
    nodeVersion int
    mu        sync.RWMutex
}
type gossipHistory struct {
    lastSeen map[string]time.Time
    mu sync.RWMutex
}
func (n *Node) initializeGossipState() {
    n.gossipState.mu.Lock()
    defer n.gossipState.mu.Unlock()
    for i := 0; i < n.shardCount; i++{
        n.gossipState.shards[i] = shardInfo{
            shardId: i,
            leader: "",
        }
    }
    n.gossipState.nodes[n.address] = nodeInfo{
        address: n.address,
        nextIndex: make(map[int]int),
        matchIndex: make(map[int]int),
    }
}
func (n *Node) checkGossipHistoryInterval(){
    for{
        n.gossipHistory.mu.Lock()
        for address, lastSeen := range n.gossipHistory.lastSeen{
            if time.Since(lastSeen) > time.Minute * 1 {
                n.Log(LevelDebug, "Node %v inactive for more than 1 minute", address)
                n.removeNode(address)
                delete(n.gossipHistory.lastSeen, address)
            }
        }
        n.gossipHistory.mu.Unlock()
        time.Sleep(time.Second * time.Duration(n.gossipInterval))
    }
}
func (n *Node) startGossip() {
    go func(){
        for{
            n.mu.RLock()
            nodes := n.nodes
            n.mu.RUnlock()
            for _, node := range nodes {
                if node.Address() == n.address {
                    continue
                }
                go n.sendGossip(node)
            }
            time.Sleep(time.Second * time.Duration(n.gossipInterval))
        }
    }()
}

func (n *Node) sendGossip(peer *Node) {
    conn, err := n.GetConn(peer.Address())
    if err != nil {
        n.Log(LevelError, "Failed to get connection: %v", err)
        return
    }
    defer n.PutConn(peer.Address(), conn)
    client := pb.NewNodeServiceClient(conn)
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    req := n.prepareGossipRequest()
    resp, err := client.Gossip(ctx, req)
    if err != nil {
        n.Log(LevelError, "Error on send gossip: %v, error: %v", peer.address, err)
        return
    }
    n.handleGossipResponse(resp, peer)
}
func (n *Node) prepareGossipRequest() *pb.GossipRequest {
    n.gossipState.mu.RLock()
    defer n.gossipState.mu.RUnlock()
    shards := make(map[int32]*pb.ShardInfo)

    for id, shard := range n.gossipState.shards{
        shards[int32(id)] = &pb.ShardInfo{
            ShardId: int32(shard.shardId),
            Leader: shard.leader,
        }
    }
    nodes := make(map[string]*pb.NodeInfo)

    for address, node := range n.gossipState.nodes{
        nodes[address] = &pb.NodeInfo{
         Address: address,
        }
    }
    req := &pb.GossipRequest{
        Shards:      shards,
        Nodes: nodes,
        NodeVersion: int32(n.gossipState.nodeVersion),
    }
    return req
}
func (n *Node) gossipHandler(ctx context.Context, req *pb.GossipRequest) (*pb.GossipResponse, error) {
    n.gossipState.mu.Lock()
    defer n.gossipState.mu.Unlock()
    if int(req.NodeVersion) <= n.gossipState.nodeVersion{
        return &pb.GossipResponse{NodeVersion: int32(n.gossipState.nodeVersion)}, nil
    }
    n.gossipState.nodeVersion = int(req.NodeVersion)
    for id, shard := range req.Shards {
        shardInfo, ok := n.gossipState.shards[int(id)]
        if !ok {
            n.gossipState.shards[int(id)] = shardInfo{
                shardId: int(shard.ShardId),
                leader: shard.Leader,
            }
            n.Log(LevelDebug, "Added new shard id: %v", id)
            continue
        }
        if shard.Leader != ""{
            shardInfo.leader = shard.Leader
            n.gossipState.shards[int(id)] = shardInfo
        }
    }

    for address, node := range req.Nodes{
        if _, ok := n.gossipState.nodes[address]; !ok {
            n.Log(LevelDebug, "Added new node: %v", address)
            n.gossipState.nodes[address] = nodeInfo{
                address: address,
                nextIndex: make(map[int]int),
                matchIndex: make(map[int]int),
            }
        }
    }
    n.metrics.SetNodesCount(int64(len(n.gossipState.nodes)))
    n.metrics.SetShardsCount(int64(len(n.gossipState.shards)))
    return &pb.GossipResponse{NodeVersion: int32(n.gossipState.nodeVersion)}, nil
}
func (n *Node) handleGossipResponse(resp *pb.GossipResponse, peer *Node) {
    n.gossipHistory.mu.Lock()
    defer n.gossipHistory.mu.Unlock()
    n.gossipHistory.lastSeen[peer.address] = time.Now()
}
func (n *Node) removeNode(address string) {
    n.gossipState.mu.Lock()
    defer n.gossipState.mu.Unlock()
    for id, node := range n.gossipState.nodes{
        if node.address == address {
            n.Log(LevelDebug, "Removed node %v from gossip", address)
            delete(n.gossipState.nodes, id)
        }
    }
    n.metrics.SetNodesCount(int64(len(n.gossipState.nodes)))
}