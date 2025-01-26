package node

import (
    "context"
    "fmt"
    pb "distributed-db/proto"
    "time"
)

func (n *Node) addKeyToShardData(key string, shardId int){
    n.mu.Lock()
    defer n.mu.Unlock()
    n.shardData[key] = shardId
}
func (n *Node) deleteKeyFromShardData(key string){
    n.mu.Lock()
    defer n.mu.Unlock()
    delete(n.shardData, key)
}

func (n *Node) startShardLeaderElection() {
    n.mu.Lock()
    defer n.mu.Unlock()
    go func(){
        for _, shard := range n.gossipState.shards {
            go n.electShardLeader(shard.shardId)
        }
    }()
}

func (n *Node) electShardLeader(shardId int){
    n.Log(LevelDebug, "Starting shard election for shard: %v", shardId)
    for{
        n.mu.Lock()
        if n.state != Leader{
            n.mu.Unlock()
            time.Sleep(time.Second * 10)
            continue
        }
        n.mu.Unlock()
        n.gossipState.mu.Lock()
        shard, ok := n.gossipState.shards[shardId]
        if !ok{
            n.gossipState.mu.Unlock()
            n.Log(LevelWarning, "Shard not found for shard id: %v", shardId)
            time.Sleep(time.Second * 10)
            continue
        }

        if shard.leader == "" {
            shard.leader = n.address
            n.gossipState.shards[shardId] = shard
            n.gossipState.mu.Unlock()
            n.Log(LevelDebug, "Node: %v become a leader for shard %v", n.address, shardId)
            go n.sendShardHeartbeat(shardId)
            break
        }
        n.gossipState.mu.Unlock()
        time.Sleep(time.Second * 10)
    }
}
func (n *Node) sendShardHeartbeat(shardId int) {
    for {
        n.mu.RLock()
        if n.state != Leader {
            n.mu.RUnlock()
            break
        }
        n.mu.RUnlock()
        n.gossipState.mu.RLock()
        shard, ok := n.gossipState.shards[shardId]
        if !ok {
            n.gossipState.mu.RUnlock()
            n.Log(LevelWarning, "Shard not found for shard id: %v", shardId)
            break
        }
        n.gossipState.mu.RUnlock()
        for _, node := range n.nodes {
            if node.Address() == n.address {
                continue
            }
            go func(node *Node){
                conn, err := n.GetConn(node.Address())
                if err != nil {
                    n.Log(LevelError, "Failed to get connection: %v", err)
                    return
                }
                defer n.PutConn(node.Address(), conn)
                client := pb.NewNodeServiceClient(conn)
                ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
                defer cancel()
                _, err = client.ShardHeartbeat(ctx, &pb.ShardHeartbeatRequest{ShardId: int32(shardId), LeaderAddress: n.address})
                if err != nil {
                    n.Log(LevelError, "Failed to send shard heartbeat to %v: %v", node.Address(), err)
                }
            }(node)
        }
    time.Sleep(time.Second * 10)
    }
}
func (n *Node) shardHeartbeatHandler(ctx context.Context, req *pb.ShardHeartbeatRequest) (*pb.ShardHeartbeatResponse, error) {
    n.gossipState.mu.Lock()
    defer n.gossipState.mu.Unlock()
    shard, ok := n.gossipState.shards[int(req.ShardId)]
    if !ok{
        n.Log(LevelWarning, "Shard not found for shard id: %v", req.ShardId)
        return &pb.ShardHeartbeatResponse{Success: false}, fmt.Errorf("shard not found for shard id: %v", req.ShardId)
    }
    shard.leader = req.LeaderAddress
    n.gossipState.shards[int(req.ShardId)] = shard
    n.Log(LevelDebug, "Shard heartbeat from: %v, shard: %v", req.LeaderAddress, req.ShardId)
    return &pb.ShardHeartbeatResponse{Success: true}, nil
}