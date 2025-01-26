package node

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"distributed-db/internal/common"
	"distributed-db/internal/expirable"
	"distributed-db/internal/grpcpool"
	"distributed-db/node/grpc"
	"distributed-db/node/http"
	"distributed-db/node/metrics"
	"distributed-db/node/raft"
	"distributed-db/node/storage"
	pb "distributed-db/proto"

	"github.com/gorilla/mux"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

type Node struct {
	id            int
	address       string
	port          string
	shardCount    int
	logger        *log.Logger
	raft          *raft.Raft
	data          *storage.Storage
	gossipState   *expirable.LRUCache
	nodeInfo      common.NodeInfo
	mu            sync.RWMutex
	grpcPool      *grpcpool.PoolManager
	tlsEnabled    bool
	certFile      string
	keyFile       string
	authUser     string
	authPass      string
	raftLogPath   string
	dataPath      string
	aesKey string
	disableEncryption bool
	shardLeaders   map[int]common.NodeInfo
	metrics  *metrics.Metrics
}

func NewNode(id int, address string, shardCount int, port string, logger *log.Logger, tlsEnabled bool, certFile string, keyFile string, authUser string, authPass string, raftLogPath string, dataPath string, aesKey string, disableEncryption bool) *Node {
	n := &Node{
		id:            id,
		address:       address,
		port:          port,
		shardCount:    shardCount,
		logger:        logger,
		gossipState:   expirable.NewLRUCache(1000),
		nodeInfo: common.NodeInfo{
			Address: address,
		},
		mu:            sync.RWMutex{},
		tlsEnabled:    tlsEnabled,
		certFile:      certFile,
		keyFile:       keyFile,
		authUser:     authUser,
		authPass:      authPass,
		raftLogPath:   raftLogPath,
		dataPath:      dataPath,
		aesKey: aesKey,
		disableEncryption: disableEncryption,
		shardLeaders: make(map[int]common.NodeInfo),
		metrics:  metrics.NewMetrics(),
	}

	var opts []credentials.TransportCredentials
	if tlsEnabled {
		certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			logger.Fatalf("failed to load TLS certificate: %v", err)
		}

		tlsConfig := &tls.Config{
			Certificates:       []tls.Certificate{certificate},
			InsecureSkipVerify: true,
		}

		opts = append(opts, credentials.NewTLS(tlsConfig))
	} else {
		opts = append(opts, insecure.NewCredentials())
	}

    n.grpcPool = grpcpool.NewPoolManager(address, []grpc.DialOption{grpc.WithTransportCredentials(opts[0])}, 10, 3)
	n.data = storage.NewStorage(dataPath, aesKey, disableEncryption)
	n.raft = raft.NewRaft(id, address, logger, n.grpcPool, n.raftLogPath, n.gossipState, n.metrics)
    n.metrics.SetNodeId(id)
	return n
}
func (n *Node) Run() {
	n.logger.Printf("Node %d starting on address: %s, port: %s\n", n.id, n.address, n.port)

	// Start Raft
	go n.raft.Run()
	// Start gRPC server
	go n.startGrpcServer()
    // Start metrics server
    go n.startMetricsServer()
    //Start http server
	go n.startHttpServer()

	// Handle graceful shutdown
	n.handleShutdown()

	n.grpcPool.Monitor()
}

func (n *Node) handleShutdown() {
	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	n.logger.Printf("Node %d shutting down...\n", n.id)
	n.raft.Close()
	n.grpcPool.Close()
	n.logger.Printf("Node %d shutdown complete.\n", n.id)
	os.Exit(0)
}

func (n *Node) startGrpcServer() {
    grpcServer := grpc.NewGrpcServer(n.address, n.logger, n.tlsEnabled, n.certFile, n.keyFile)

    pb.RegisterNodeServiceServer(grpcServer.Server, n)
    pb.RegisterRaftServiceServer(grpcServer.Server, n.raft)

    err := grpcServer.Start()
    if err != nil {
        n.logger.Fatalf("failed to start grpc server: %v", err)
    }
}

func (n *Node) startHttpServer() {
	r := mux.NewRouter()
	r.HandleFunc("/data/{key}", n.handleData).Methods("GET", "PUT", "DELETE")
	r.HandleFunc("/metrics", n.metrics.HandleMetrics).Methods("GET")

	server := http.Server{
		Addr:    ":" + n.port,
		Handler: r,
	}

	go func() {
        if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed){
		    n.logger.Fatalf("failed to start http server: %v", err)
		}
    }()
	n.logger.Printf("HTTP server listening on port %s\n", n.port)
}
func (n *Node) startMetricsServer() {
	server := http.Server{
		Addr:    ":9090",
		Handler: n.metrics.GetHandler(),
	}

	go func() {
        if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed){
		    n.logger.Fatalf("failed to start metrics server: %v", err)
		}
    }()

	n.logger.Printf("Metrics server listening on port 9090\n")
}
func (n *Node) handleData(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	key := vars["key"]
	if key == "" {
		http.Error(w, "Key is required", http.StatusBadRequest)
		return
	}
    if r.Method == http.MethodGet {
		n.handleGet(w, key)
		return
	}
	if r.Method == http.MethodPut {
		n.handlePut(w, r, key)
		return
	}
	if r.Method == http.MethodDelete{
        n.handleDelete(w, key)
        return
    }

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}
func (n *Node) handleGet(w http.ResponseWriter, key string) {
    shardId := n.getShardId(key)
    leader := n.getShardLeader(shardId)

	if leader.Address != n.address {
        if leader.Address == "" {
            http.Error(w, "Shard has no leader", http.StatusServiceUnavailable)
            return
        }
		n.logger.Printf("redirecting read request to leader: %s\n", leader.Address)
		http.Redirect(w, &http.Request{}, fmt.Sprintf("http://%s/data/%s", leader.Address[0:len(leader.Address)-1] + n.port, key), http.StatusTemporaryRedirect)
		return
	}


    value, ok := n.data.Get(key)

	if !ok {
		http.Error(w, "Key not found", http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(value)

}
func (n *Node) handlePut(w http.ResponseWriter, r *http.Request, key string) {
    shardId := n.getShardId(key)
    leader := n.getShardLeader(shardId)

    if leader.Address != n.address {
        if leader.Address == "" {
            http.Error(w, "Shard has no leader", http.StatusServiceUnavailable)
            return
        }
		n.logger.Printf("redirecting write request to leader: %s\n", leader.Address)
		http.Redirect(w, &http.Request{}, fmt.Sprintf("http://%s/data/%s", leader.Address[0:len(leader.Address)-1] + n.port, key), http.StatusTemporaryRedirect)

		return
	}
	value, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	err = n.raft.Propose(context.TODO(), key, value)
	if err != nil {
		http.Error(w, "Failed to propose change", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	w.Header().Set("Content-Type", "application/json")
    w.Write([]byte(`{"message":"success"}`))
}
func (n *Node) handleDelete(w http.ResponseWriter, key string) {
    shardId := n.getShardId(key)
    leader := n.getShardLeader(shardId)

    if leader.Address != n.address {
        if leader.Address == "" {
            http.Error(w, "Shard has no leader", http.StatusServiceUnavailable)
            return
        }
		n.logger.Printf("redirecting delete request to leader: %s\n", leader.Address)
		http.Redirect(w, &http.Request{}, fmt.Sprintf("http://%s/data/%s", leader.Address[0:len(leader.Address)-1] + n.port, key), http.StatusTemporaryRedirect)

		return
	}

	err := n.raft.ProposeDelete(context.TODO(), key)
	if err != nil {
		http.Error(w, "Failed to propose delete", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "application/json")
    w.Write([]byte(`{"message":"success"}`))

}
func (n *Node) Apply(key string, value []byte) error {
    n.logger.Printf("applying key: %s", key)
	n.data.Set(key, value)
    return nil
}
func (n *Node) ApplyDelete(key string) error{
    n.logger.Printf("applying delete for key: %s", key)
    n.data.Delete(key)
    return nil
}
func (n *Node) GetNodeInfo() common.NodeInfo{
	return n.nodeInfo
}
func (n *Node) GetId() int{
	return n.id
}
func (n *Node) getShardId(key string) int {
    hash := n.data.GetKeyHash(key)
	return int(hash % uint64(n.shardCount))
}
func (n *Node) getShardLeader(shardId int) common.NodeInfo {
    n.mu.RLock()
    defer n.mu.RUnlock()
    leader, ok := n.shardLeaders[shardId]
    if !ok{
        return common.NodeInfo{}
    }
    return leader
}
func (n *Node) setShardLeader(shardId int, node common.NodeInfo) {
    n.mu.Lock()
    defer n.mu.Unlock()
    n.shardLeaders[shardId] = node
	n.logger.Printf("Shard leader changed, shard: %d, leader address: %s\n", shardId, node.Address)
}
func (n *Node) SetShardLeaders(leaders map[int]common.NodeInfo){
    n.mu.Lock()
    defer n.mu.Unlock()

    n.shardLeaders = leaders
    n.logger.Printf("Shard leaders updated: %v\n", leaders)
}
func (n *Node) GetShardLeaders() map[int]common.NodeInfo{
    n.mu.RLock()
    defer n.mu.RUnlock()
    return n.shardLeaders
}
func (n *Node) PersistData() error{
	return n.data.PersistDataToDisk()
}

