
package node

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
	"distributed-db/internal/expirable"
	"distributed-db/internal/grpcpool"
	"distributed-db/node/grpc"
	"distributed-db/node/http"
	"distributed-db/node/raft"
	"distributed-db/node/metrics"
	pb "distributed-db/proto"
    "github.com/cespare/xxhash/v2"
	"google.golang.org/grpc"
)
type LogLevel int
const (
	LevelError LogLevel = iota
	LevelWarning
    LevelInfo
	LevelDebug
)

type NodeState int

const (
	Follower NodeState = iota
	Candidate
	Leader
)

type LogEntry struct {
	Term  int
    Index int
	Key   string
}
type Node struct {
	id int
	address string
	port string
    raftPort string
    state NodeState
    term int
    votedFor string
	commitIndex int
	lastApplied int
	raftLog []LogEntry
    shardCount int
	nodes []*Node
	poolManager *grpcpool.PoolManager
	mu sync.RWMutex
    grpcServer *grpc.GrpcServer
	httpServer *http.HttpServer
	raft *raft.Raft
	gossipState *gossipState
	gossipHistory *gossipHistory
    data map[string]interface{}
	cache *expirable.LRU[string,interface{}]
    lastLeaderHeartbeat time.Time
    logger *log.Logger
    metrics *metrics.Metrics
    shardData map[string]int
    aesGCM cipher.AEAD
	aesKey string
	gossipInterval int
}

func NewNode(id int, address string, shardCount int, port string, logger *log.Logger, tlsEnabled bool, certFile string, keyFile string, authUser string, authPass string) *Node{
    n := &Node{
		id: id,
		address: address,
		shardCount: shardCount,
		port: port,
		state: Follower,
        term: 0,
		votedFor: "",
		commitIndex: 0,
		lastApplied: 0,
		raftLog: make([]LogEntry, 0),
		nodes: make([]*Node, 0),
		poolManager: grpcpool.NewPoolManager(),
		mu: sync.RWMutex{},
		data: make(map[string]interface{}),
		cache: expirable.NewLRU[string,interface{}](1000, time.Minute*5),
        lastLeaderHeartbeat: time.Now(),
        logger: logger,
		shardData: make(map[string]int),
		gossipInterval: 10,
    }
    n.gossipState = &gossipState{
		shards: make(map[int]shardInfo),
		nodes: make(map[string]nodeInfo),
		nodeVersion: 1,
		mu: sync.RWMutex{},
	}
    n.gossipHistory = &gossipHistory{
		lastSeen: make(map[string]time.Time),
		mu: sync.RWMutex{},
	}
    n.metrics = metrics.NewMetrics()
    n.metrics.RegisterMetrics()
	n.raft = raft.NewRaft(n)

	lis, err := net.Listen("tcp", address)
	if err != nil {
		n.Log(LevelError, "Failed to listen: %v", err)
	}

	n.grpcServer = grpc.NewGrpcServer(n, tlsEnabled, certFile, keyFile)
	go func() {
		err := n.grpcServer.Start(lis)
		if err != nil {
			n.Log(LevelError, "Failed to start gRPC server: %v", err)
		}
	}()
	n.httpServer = http.NewHttpServer(n, tlsEnabled, certFile, keyFile, authUser, authPass)
	go func() {
		err := n.httpServer.Start(port)
        if err != nil {
			n.Log(LevelError, "Failed to start HTTP server: %v", err)
        }
	}()

	n.initializeGossipState()
    n.Log(LevelInfo, "Node %v started on address: %v, HTTP port: %v", id, address, port)
	go n.checkGossipHistoryInterval()

    return n
}
func (n *Node) Start(nodes []*Node) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.nodes = nodes
	n.raft.Start(nodes)
}
func (n *Node) Shutdown() {
	n.mu.Lock()
	defer n.mu.Unlock()
    n.raft.Shutdown()
	n.grpcServer.Shutdown()
	n.httpServer.Shutdown()
	n.poolManager.Close()
    n.Log(LevelInfo, "Node shutdown")
}
func (n *Node) Address() string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.address
}
func (n *Node) Port() string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.port
}
func (n *Node) SetState(state NodeState) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.state = state
    n.metrics.SetNodeState(state.String())
}

func (n *Node) State() NodeState {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.state
}

func (n *Node) IncrementTerm(){
	n.mu.Lock()
	defer n.mu.Unlock()
	n.term++
    n.metrics.SetTerm(n.term)
}
func (n *Node) SetTerm(term int){
	n.mu.Lock()
	defer n.mu.Unlock()
	n.term = term
    n.metrics.SetTerm(term)
}
func (n *Node) Term() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.term
}
func (n *Node) SetVotedFor(votedFor string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.votedFor = votedFor
}

func (n *Node) VotedFor() string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.votedFor
}
func (n *Node) SetCommitIndex(commitIndex int){
	n.mu.Lock()
	defer n.mu.Unlock()
	n.commitIndex = commitIndex
    n.metrics.SetCommitIndex(int64(commitIndex))
}
func (n *Node) CommitIndex() int {
    n.mu.RLock()
	defer n.mu.RUnlock()
    return n.commitIndex
}
func (n *Node) SetLastApplied(lastApplied int){
    n.mu.Lock()
    defer n.mu.Unlock()
	n.lastApplied = lastApplied
    n.metrics.SetLastApplied(int64(lastApplied))
}
func (n *Node) LastApplied() int {
    n.mu.RLock()
	defer n.mu.RUnlock()
    return n.lastApplied
}

func (n *Node) RaftLog() []LogEntry {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.raftLog
}
func (n *Node) Nodes() []*Node{
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.nodes
}
func (n *Node) Id() int{
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.id
}
func (n *Node) SetNextIndexValue(nodeId int, value int){
	n.gossipState.mu.Lock()
    defer n.gossipState.mu.Unlock()
    for address, node := range n.gossipState.nodes{
		if n.getNodeIdFromAddress(address) == nodeId{
			node.nextIndex[n.id] = value
			n.gossipState.nodes[address] = node
			return
		}
	}
}
func (n *Node) SetMatchIndexValue(nodeId int, value int){
	n.gossipState.mu.Lock()
    defer n.gossipState.mu.Unlock()
    for address, node := range n.gossipState.nodes{
		if n.getNodeIdFromAddress(address) == nodeId{
			node.matchIndex[n.id] = value
			n.gossipState.nodes[address] = node
			return
		}
	}
}
func (n *Node) getNextIndexValue(nodeId int) int{
	n.gossipState.mu.RLock()
	defer n.gossipState.mu.RUnlock()
	for address, node := range n.gossipState.nodes{
		if n.getNodeIdFromAddress(address) == nodeId{
			if val, ok := node.nextIndex[n.id]; ok{
				return val
			}
			return 1
		}
	}
	return 1
}
func (n *Node) getMatchIndexValue(nodeId int) int{
	n.gossipState.mu.RLock()
	defer n.gossipState.mu.RUnlock()
	for address, node := range n.gossipState.nodes{
		if n.getNodeIdFromAddress(address) == nodeId{
			if val, ok := node.matchIndex[n.id]; ok{
				return val
			}
			return 0
		}
	}
	return 0
}
func (n *Node) getShardForHash(key string) int{
    hash := xxhash.Sum64([]byte(key))
    shardId := int(hash % uint64(n.shardCount))
    return shardId
}
func (n *Node) GetConn(address string) (*grpc.ClientConn, error){
	p, _ := n.poolManager.GetConn(address)
	if p == nil{
		conn, err := n.grpcServer.GetClientConn(address)
		if err != nil {
			return nil, fmt.Errorf("failed to get connection: %w", err)
		}
		pool := grpcpool.NewPool(conn)
		n.poolManager.PutConn(address, pool)
		p, _ = n.poolManager.GetConn(address)
		if p == nil{
			return nil, fmt.Errorf("failed to get connection from pool")
		}
	}
	return p.conn, nil
}

func (n *Node) PutConn(address string, conn *grpc.ClientConn) {
    pool, _ := n.poolManager.GetConn(address)
	if pool != nil {
		n.poolManager.PutConn(address, pool)
	}
}
func (n *Node) Log(LogLevel LogLevel, format string, v ...interface{}) {
    if LogLevel <= LevelDebug{
        n.logger.Printf(format, v...)
	}
}
func (n *Node) Raft() *raft.Raft {
	return n.raft
}
func (n *Node) GossipHandler(ctx context.Context, req *pb.GossipRequest) (*pb.GossipResponse, error) {
	return n.gossipHandler(ctx, req)
}
func (n *Node) ShardHeartbeatHandler(ctx context.Context, req *pb.ShardHeartbeatRequest) (*pb.ShardHeartbeatResponse, error) {
	return n.shardHeartbeatHandler(ctx, req)
}
func (n *Node) IsAlive() bool {
    n.mu.RLock()
	defer n.mu.RUnlock()
	return n.state != 0
}
func (n *Node) putDataLocal(key string, value interface{}) {
	n.mu.Lock()
    defer n.mu.Unlock()
    if n.aesKey != ""{
		encryptedData, err := n.encryptData(value)
		if err != nil {
			n.Log(LevelError, "Failed to encrypt data: %v", err)
			return
		}
		n.data[key] = encryptedData
	}else{
		n.data[key] = value
	}
	n.cache.Add(key, value)
}
func (n *Node) getDataLocal(key string, originalKey string) (bool, error) {
	n.mu.RLock()
	defer n.mu.RUnlock()
    if val, ok := n.cache.Get(key); ok {
        if n.aesKey != "" {
            data, ok := val.(string)
            if !ok{
				return false, fmt.Errorf("failed to convert data to string")
            }
            decryptedData, err := n.decryptData(data)
            if err != nil{
                return false, fmt.Errorf("failed to decrypt data: %w", err)
            }
            return decryptedData == originalKey, nil
        }
        return val == originalKey, nil
    }
    if val, ok := n.data[key]; ok {
		if n.aesKey != "" {
            data, ok := val.(string)
            if !ok{
				return false, fmt.Errorf("failed to convert data to string")
            }
			decryptedData, err := n.decryptData(data)
            if err != nil{
                return false, fmt.Errorf("failed to decrypt data: %w", err)
            }
            n.cache.Add(key, decryptedData)
            return decryptedData == originalKey, nil
        }
		n.cache.Add(key, val)
        return val == originalKey, nil
    }
    return false, nil
}
func (n *Node) deleteDataLocal(key string) {
	n.mu.Lock()
    defer n.mu.Unlock()
	delete(n.data, key)
    n.cache.Invalidate(key)
}
func (n *Node) PutData(key string, value interface{}) (bool, error) {
	if n.State() != Leader {
        leader, ok := n.getShardLeader(key)
        if !ok {
			n.Log(LevelWarning, "Leader not found for shard: %v", n.getShardForHash(key))
            return false, fmt.Errorf("leader not found for shard: %v", n.getShardForHash(key))
		}
		if leader == n.address {
            n.Raft().PutData(key)
            return true, nil
		}
        conn, err := n.GetConn(leader)
        if err != nil{
            return false, fmt.Errorf("failed to get connection: %w", err)
        }
        defer n.PutConn(leader, conn)
        client := pb.NewNodeServiceClient(conn)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
        _, err = client.PutData(ctx, &pb.PutRequest{Key: key})
		if err != nil {
            return false, fmt.Errorf("failed to put data to leader: %w", err)
		}
        return true, nil
	}
    n.Raft().PutData(key)
	return true, nil
}

func (n *Node) GetData(key string) (bool, error) {
	if n.State() != Leader {
		leader, ok := n.getShardLeader(key)
        if !ok {
            return false, fmt.Errorf("leader not found for shard: %v", n.getShardForHash(key))
        }
        if leader == n.address {
            return n.Raft().GetData(key)
		}
		conn, err := n.GetConn(leader)
        if err != nil{
            return false, fmt.Errorf("failed to get connection: %w", err)
        }
        defer n.PutConn(leader, conn)
        client := pb.NewNodeServiceClient(conn)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
        resp, err := client.GetData(ctx, &pb.GetRequest{Key: key})
		if err != nil {
			return false, fmt.Errorf("failed to get data from leader: %w", err)
		}
		return resp.Success, nil
    }
	return n.Raft().GetData(key)
}

func (n *Node) DeleteData(key string) error {
	if n.State() != Leader {
		leader, ok := n.getShardLeader(key)
        if !ok {
            return fmt.Errorf("leader not found for shard: %v", n.getShardForHash(key))
        }
        if leader == n.address {
            return n.Raft().DeleteData(key)
		}
		conn, err := n.GetConn(leader)
        if err != nil{
            return fmt.Errorf("failed to get connection: %w", err)
        }
        defer n.PutConn(leader, conn)
        client := pb.NewNodeServiceClient(conn)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
        _, err = client.DeleteData(ctx, &pb.DeleteRequest{Key: key})
		if err != nil {
			return fmt.Errorf("failed to delete data from leader: %w", err)
		}
		return nil
	}
    return n.Raft().DeleteData(key)
}
func (n *Node) SetAesKey(key string){
	n.mu.Lock()
	defer n.mu.Unlock()
    if len(key) != 32{
		n.Log(LevelError, "AES key must be 32 bytes")
		return
	}
    n.aesKey = key
	block, err := aes.NewCipher([]byte(key))
	if err != nil{
		n.Log(LevelError, "Failed to create cipher: %v", err)
        return
    }
    aesGCM, err := cipher.NewGCM(block)
    if err != nil{
		n.Log(LevelError, "Failed to create GCM: %v", err)
        return
    }
    n.aesGCM = aesGCM
}
func (n *Node) encryptData(data interface{}) (string, error) {
	bytes, err := json.Marshal(data)
	if err != nil{
		return "", fmt.Errorf("failed to marshal data: %w", err)
	}
	nonce := make([]byte, n.aesGCM.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("failed to read nonce: %w", err)
	}
    ciphertext := n.aesGCM.Seal(nonce, nonce, bytes, nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}
func (n *Node) decryptData(data string) (string, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return "", fmt.Errorf("failed to decode data: %w", err)
	}
    nonceSize := n.aesGCM.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]

    plaintext, err := n.aesGCM.Open(nil, nonce, ciphertext, nil)
    if err != nil {
        return "", fmt.Errorf("failed to open gcm: %w", err)
    }
    return string(plaintext), nil
}
func (n *Node) SetGossipInterval(interval int){
    n.mu.Lock()
	defer n.mu.Unlock()
	n.gossipInterval = interval
}
func (n *Node) GetRandomElectionTimeout() int {
    return rand.Intn(150) + 150
}
func (n *Node) getNodeIdFromAddress(address string) int {
    for _, node := range n.nodes{
        if node.address == address{
            return node.id
        }
    }
    return 0
}
func (n *Node) StateString() string{
    n.mu.RLock()
    defer n.mu.RUnlock()
    return n.state.String()
}
func (s NodeState) String() string {
    switch s {
    case Follower:
        return "Follower"
    case Candidate:
        return "Candidate"
    case Leader:
        return "Leader"
    default:
        return "Unknown"
    }
}
func (n *Node) getShardLeader(key string) (string, bool){
    shardId := n.getShardForHash(key)
    n.gossipState.mu.RLock()
    defer n.gossipState.mu.RUnlock()
    shard, ok := n.gossipState.shards[shardId]
    if !ok {
        return "", false
    }
    if shard.leader == ""{
		return "", false
    }
    return shard.leader, true
}
