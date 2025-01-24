
package coordinator

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	"distributed-db/internal/auth"
	"distributed-db/internal/grpcpool"
	pb "distributed-db/proto"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
    "google.golang.org/grpc/reflection"
	"net/http/pprof"
	_ "distributed-db/docs" // Добавляем импорт для автодокументации
)
const (
	LevelDebug = "DEBUG"
	LevelInfo  = "INFO"
	LevelError = "ERROR"
)
type Coordinator struct {
	address             string
	logger              *log.Logger
	mu                  sync.RWMutex
    tls                  bool
    serverCert           string
    clientCert           string
    authUser            string
    authPass            string
    hashedAuthPass     string
    logLevel              string
	grpcServer *grpc.Server
	nodePools    map[int]*grpcpool.Pool
	loadMetrics   map[int]map[string]float64
	lastLoadUpdate time.Time
    ctx                   context.Context
    cancel                context.CancelFunc
}
func NewCoordinator(address string, logger *log.Logger, tls bool, serverCert string, clientCert string, authUser string, authPass string) *Coordinator {
    ctx, cancel := context.WithCancel(context.Background())
     hashedPass := auth.HashPassword(authPass)
	c := &Coordinator{
		address:             address,
		logger:              logger,
		mu:                  sync.RWMutex{},
        tls:                 tls,
        serverCert:          serverCert,
        clientCert:          clientCert,
        authUser:           authUser,
        authPass:           authPass,
        hashedAuthPass:     hashedPass,
		grpcServer: grpc.NewServer(),
		nodePools:    make(map[int]*grpcpool.Pool),
		loadMetrics:   make(map[int]map[string]float64),
		lastLoadUpdate: time.Now(),
        ctx: ctx,
        cancel: cancel,
	}
    if tls {
        creds, err := credentials.NewServerTLSFromFile(serverCert, "certs/server.key")
        if err != nil {
            logger.Fatalf("Failed to generate credentials %v", err)
        }
       c.grpcServer = grpc.NewServer(grpc.Creds(creds))
    }
	pb.RegisterCoordinatorServiceServer(c.grpcServer, c)
	return c
}
// StartServer start grpc server
func (c *Coordinator) StartServer() {
    lis, err := net.Listen("tcp", c.address)
	if err != nil {
		c.logger.Fatalf("failed to listen: %v", err)
	}
	go func() {
		if err := c.grpcServer.Serve(lis); err != nil {
			c.logger.Fatalf("failed to serve: %v", err)
		}
	}()
     http.HandleFunc("/debug/pprof/", pprof.Index)
	http.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	http.HandleFunc("/debug/pprof/profile", pprof.Profile)
	http.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	http.HandleFunc("/debug/pprof/trace", pprof.Trace)
     http.Handle("/metrics", promhttp.Handler())
    // Добавьте обработчик для Swagger UI
    http.HandleFunc("/swagger/", func(w http.ResponseWriter, r *http.Request) {
        http.StripPrefix("/swagger/", http.FileServer(http.Dir("./docs"))).ServeHTTP(w, r)
    })
    go func() {
        if err := http.ListenAndServe(c.address, c); err != nil {
            c.log(LevelError,"Failed to start HTTP server %v", err)
        }
    }()
}
// StopServer stop grpc server
func (c *Coordinator) StopServer() {
    c.grpcServer.GracefulStop()
    c.cancel()
    c.log(LevelInfo,"gRPC server stopped")
}

func (c *Coordinator) SetLogLevel(level string) {
	c.logLevel = level
}

func (c *Coordinator) log(level string, format string, v ...interface{}) {
	if c.shouldLog(level) {
		c.logger.Printf("[%s] Coordinator: %s", level, fmt.Sprintf(format, v...))
	}
}

func (c *Coordinator) shouldLog(level string) bool {
	switch c.logLevel {
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

func (c *Coordinator) ReportLoad(ctx context.Context, req *pb.LoadReportRequest) (*pb.LoadReportResponse, error) {
    metrics := req.Metrics
    for id, metric := range metrics {
		intId, err := strconv.Atoi(id)
		if err != nil {
            c.log(LevelError, "Error parsing node id from request %v", err)
			continue
        }
        c.mu.Lock()
        c.loadMetrics[intId] = metric
		c.mu.Unlock()
    }
	return &pb.LoadReportResponse{}, nil
}
func (c *Coordinator) GetLeastLoadedShard(ctx context.Context, req *pb.LeastLoadedShardRequest) (*pb.LeastLoadedShardResponse, error) {
    c.mu.RLock()
    defer c.mu.RUnlock()

    var leastLoadedShard int
    minLoad := float64(1000000000)
     for shard, metrics := range c.loadMetrics {
        for _, load := range metrics {
            if load < minLoad {
                minLoad = load
                leastLoadedShard = shard
            }
        }
	}
    if leastLoadedShard == 0 {
		c.log(LevelError, "No load metrics available")
        return &pb.LeastLoadedShardResponse{ShardId: 0}, nil
    }
	return &pb.LeastLoadedShardResponse{ShardId: int32(leastLoadedShard)}, nil
}

func (c *Coordinator) StartLoadBalancing(interval time.Duration) {
    for {
        select {
        case <-time.After(interval):
            c.log(LevelDebug, "Current load metrics: %v", c.loadMetrics)
        case <-c.ctx.Done():
            c.log(LevelInfo, "Load balancer stopped")
            return
        }
    }
}

// handleHealthCheck handles health check request
// @Summary Health check for the coordinator.
// @Description Checks if the coordinator is healthy
// @Tags health
// @Produce plain
// @Success 200 {string} string "OK"
// @Failure 405 {string} string "Method Not Allowed"
// @Router /health [get]
func (c *Coordinator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    if r.URL.Path == "/health" {
       if r.Method != http.MethodGet {
           http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
           return
       }
        fmt.Fprintf(w, "Coordinator is healthy \n")
    } else if strings.HasPrefix(r.URL.Path, "/delete/") {
        c.handleDeleteRequest(w,r)
    } else if strings.HasPrefix(r.URL.Path, "/vote/") {
		c.handleVoteRequest(w,r)
	}else {
        http.Error(w, "Not Found", http.StatusNotFound)
    }

}
// handleDeleteRequest handles delete request
// @Summary Delete data from the node.
// @Description Deletes data from a node
// @Tags data
// @Accept json
// @Produce plain
// @Param key path string true "Key of the data to delete"
// @Security basicAuth
// @Success 200 {string} string "OK"
// @Failure 400 {string} string "Bad Request"
// @Failure 401 {string} string "Unauthorized"
// @Failure 405 {string} string "Method Not Allowed"
// @Router /delete/{key} [delete]
func (c *Coordinator) handleDeleteRequest(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodDelete {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }
     user, pass, ok := r.BasicAuth()
    if !ok || user != c.authUser || !auth.CheckPassword(c.hashedAuthPass, pass) {
        w.Header().Set("WWW-Authenticate", `Basic realm="restricted", charset="UTF-8"`)
        http.Error(w, "Unauthorized", http.StatusUnauthorized)
        return
    }
   key := r.PathValue("key")
   if key == "" {
        c.log(LevelError, "Empty key for DELETE request")
        http.Error(w, "Empty key", http.StatusBadRequest)
        return
    }

    for _, pool := range c.nodePools {
        conn, err := pool.Get()
        if err != nil {
            c.log(LevelError, "did not connect: %v", err)
           continue
        }
        defer func() {
            pool.Release(conn)
        }()
        client := pb.NewNodeServiceClient(conn)

       req := &pb.DeleteRequest{
            Key: key,
        }

		_, err = client.DeleteData(c.ctx, req)
        if err != nil {
            c.log(LevelError, "Error sending delete request to node: %v", err)
            continue
        }
	}
   fmt.Fprintf(w, "Data deleted succesfully for key %s \n", key)
}
// handleVoteRequest handles vote request
// @Summary Request vote for term.
// @Description Request vote for term
// @Tags raft
// @Accept json
// @Produce json
// @Param term path integer true "Term for election"
// @Security basicAuth
// @Success 200 {object} map[string]bool "OK"
// @Failure 400 {string} string "Bad Request"
// @Failure 401 {string} string "Unauthorized"
// @Failure 405 {string} string "Method Not Allowed"
// @Router /vote/{term} [get]
func (c *Coordinator) handleVoteRequest(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }
     user, pass, ok := r.BasicAuth()
    if !ok || user != c.authUser || !auth.CheckPassword(c.hashedAuthPass, pass) {
        w.Header().Set("WWW-Authenticate", `Basic realm="restricted", charset="UTF-8"`)
        http.Error(w, "Unauthorized", http.StatusUnauthorized)
        return
    }
    http.Error(w, "Not implemented yet", http.StatusNotImplemented)
}
