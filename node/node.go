
package node

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"distributed-db/internal/auth"
	"distributed-db/internal/data"
	"distributed-db/internal/grpcpool"
	"distributed-db/node/grpc"
	"distributed-db/node/http"
	"distributed-db/node/raft"
	"distributed-db/node/metrics"
	pb "distributed-db/proto"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"golang.org/x/time/rate"
	"net/http/pprof"
	_ "distributed-db/docs" // Добавляем импорт для автодокументации
)

// Node states
const (
	Follower  = "Follower"
	Candidate = "Candidate"
	Leader    = "Leader"
)

// Log levels
const (
	LevelDebug = "DEBUG"
	LevelInfo  = "INFO"
	LevelError = "ERROR"
)

// logEntry структура для хранения записей лога
type logEntry struct {
	Term    int    `json:"term"`
	Key     string `json:"key"`
	Committed bool `json:"committed"`
}
// messageQueue структура для хранения сообщений для репликации
type messageQueue struct {
	queue []logEntry
	mu    sync.Mutex
}
// Push добавляет элемент в очередь
func (mq *messageQueue) Push(entry logEntry) {
	mq.mu.Lock()
	defer mq.mu.Unlock()
	mq.queue = append(mq.queue, entry)
}
// PopAll достает все элементы из очереди
func (mq *messageQueue) PopAll() []logEntry {
	mq.mu.Lock()
	defer mq.mu.Unlock()
	if len(mq.queue) == 0 {
		return nil
	}
	popped := mq.queue
	mq.queue = nil
	return popped
}
// Len возвращает длинну очереди
func (mq *messageQueue) Len() int {
	mq.mu.Lock()
	defer mq.mu.Unlock()
	return len(mq.queue)
}
// Node структура для представления узла распределенной базы данных
type Node struct {
	id                    int
	address               string
	numNodes              int
	otherNodes            []string
	shardId               int
	coordinatorAddress    string
	tls                  bool
	serverCert           string
	clientCert           string
	authUser            string
	authPass            string
	hashedAuthPass     string
	state                 string
	term                  int
	votedFor              string
	isAlive               bool
	lastLeaderHeartbeat   time.Time
	electionTimer         *time.Timer
	logLevel              string
	hashTable             *data.HashTable
	logger                *log.Logger
	mu                    sync.RWMutex
	putCounter            prometheus.Counter
	getCounter            prometheus.Counter
	errorCounter          prometheus.Counter
	ctx                   context.Context
	cancel                context.CancelFunc
	grpcServer *grpc.Server
	coordinatorPool *grpcpool.Pool
	messageQueue *messageQueue
	raftLog []logEntry
	commitIndex        int
	lastApplied        int
	nextIndex      map[int]int
	matchIndex      map[int]int
	leastLoadedShard   *Node
	lastLeastLoadedShardUpdate time.Time
	limiter *rate.Limiter
	httpServer *http.Server
	raft *raft.Raft
	metrics *metrics.Metrics
}

// NewNode создает новый узел
func NewNode(id int, address string, numNodes int, otherNodes []string, shardId int, coordinatorAddress string, logger *log.Logger, tls bool, serverCert string, clientCert string, authUser string, authPass string) *Node {
	ctx, cancel := context.WithCancel(context.Background())
	ht := data.NewHashTable()
	filename := fmt.Sprintf("node_%d_data.json", id)
	if err := ht.LoadData(filename); err != nil {
		logger.Printf("Error loading data: %v", err)
	}

	hashedPass := auth.HashPassword(authPass)
    n := &Node{
        id:                    id,
        address:               address,
        numNodes:              numNodes,
        otherNodes:            otherNodes,
        shardId:               shardId,
        coordinatorAddress:    coordinatorAddress,
        tls:                  tls,
        serverCert:           serverCert,
        clientCert:           clientCert,
        authUser:            authUser,
        authPass:            authPass,
        hashedAuthPass:     hashedPass,
        state:                 Follower,
        term:                  0,
        votedFor:              "",
        isAlive:               true,
        lastLeaderHeartbeat:   time.Now(),
        hashTable:             ht,
        logger:                logger,
        mu:                    sync.RWMutex{},
        putCounter:            promauto.NewCounter(prometheus.CounterOpts{Name: "db_put_counter", Help: "Number of put requests"}),
        getCounter:            promauto.NewCounter(prometheus.CounterOpts{Name: "db_get_counter", Help: "Number of get requests"}),
        errorCounter:          promauto.NewCounter(prometheus.CounterOpts{Name: "db_error_counter", Help:"Number of errors"}),
        ctx:                   ctx,
        cancel:                cancel,
		limiter: rate.NewLimiter(rate.Every(time.Second), 100), // Allow 100 requests per second
		grpcServer: grpc.NewServer(),
		coordinatorPool: grpcpool.NewPool("localhost"+coordinatorAddress, 10, logger, tls, clientCert),
		messageQueue: &messageQueue{queue: make([]logEntry,0)},
		raftLog:          make([]logEntry, 0),
		nextIndex:        make(map[int]int),
		matchIndex:       make(map[int]int),
		lastLeastLoadedShardUpdate: time.Now(),
	}
	n.raft = raft.NewRaft(n)
	n.metrics = metrics.NewMetrics(n)
	if tls {
		creds, err := credentials.NewServerTLSFromFile(serverCert, "certs/server.key")
		if err != nil {
			logger.Fatalf("Failed to generate credentials %v", err)
		}
		n.grpcServer = grpc.NewServer(grpc.Creds(creds))
	}
	pb.RegisterNodeServiceServer(n.grpcServer, grpc.NewGRPCServer(n))
	return n
}

// StartServer start grpc and http servers
func (n *Node) StartServer(nodes []*Node) {
	lis, err := net.Listen("tcp", n.address)
	if err != nil {
		n.logger.Fatalf("failed to listen: %v", err)
	}
	go func() {
		if err := n.grpcServer.Serve(lis); err != nil {
			n.logger.Fatalf("failed to serve: %v", err)
		}
	}()
    mux := http.NewServeMux()
    mux.HandleFunc("/debug/pprof/", pprof.Index)
    mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
    mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
    mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
    mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle("/metrics", promhttp.Handler())
	// Добавьте обработчик для Swagger UI
	mux.HandleFunc("/swagger/", func(w http.ResponseWriter, r *http.Request) {
		http.StripPrefix("/swagger/", http.FileServer(http.Dir("./docs"))).ServeHTTP(w, r)
	})
	n.httpServer = &http.Server{
        Addr: n.address,
        Handler: http.NewHTTPServer(n, mux),
    }
	go func() {
		if err := n.httpServer.ListenAndServe(); err != nil {
			n.log(LevelError,"Failed to start HTTP server %v", err)
		}
	}()
	n.startRaft(nodes)
	n.startMetrics()
}
// SetLogLevel sets log level for node
func (n *Node) SetLogLevel(level string) {
	n.logLevel = level
}

// log logs a message with the specified level
func (n *Node) log(level string, format string, v ...interface{}) {
	if n.shouldLog(level) {
		n.logger.Printf("[%s] %s: %s", level, fmt.Sprintf("Node %d", n.id), fmt.Sprintf(format, v...))
	}
}

// shouldLog checks if a message should be logged based on the current level
func (n *Node) shouldLog(level string) bool {
	switch n.logLevel {
	case LevelDebug:
		return true
	case LevelInfo:
		return level != LevelDebug
	case LevelError:
		return level == LevelError
	default:
		return false
	}
}

func (n *Node) startRaft(nodes []*Node) {
	go n.raft.StartHeartbeat(nodes, time.Second)
	go n.raft.StartGossip(nodes, time.Second)
	go n.raft.StartElectionTimer(nodes)
}
func (n *Node) startMetrics(){
	go n.metrics.StartLoadReport(time.Second)
}

func (n *Node) isNodeAlive() bool {
	return n.isAlive
}
