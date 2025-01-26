package main

import (
 "flag"
 "fmt"
 "log"
 "net/http"
 "os"
 "os/signal"
 "runtime"
 "strconv"
 "syscall"
 "distributed-db/node"
 "github.com/prometheus/client_golang/prometheus/promhttp"
 "golang.org/x/crypto/bcrypt"
)
var (
    id          = flag.Int("id", 0, "Node ID")
    address     = flag.String("address", "localhost:50051", "Node address")
    port        = flag.String("port", "8081", "HTTP port")
    shardCount  = flag.Int("shardCount", 1, "Number of shards")
    tlsEnabled  = flag.Bool("tls", false, "Enable TLS")
    certFile    = flag.String("cert", "", "Path to TLS certificate file")
    keyFile     = flag.String("key", "", "Path to TLS key file")
    authUser    = flag.String("authUser", "", "HTTP basic auth user")
    authPass    = flag.String("authPass", "", "HTTP basic auth password")
    aesKey      = flag.String("aesKey", "", "AES key for data encryption")
    gossipInterval = flag.Int("gossipInterval", 10, "Gossip interval in seconds")
)
func main() {
    flag.Parse()
    logger := log.New(os.Stdout, "", log.LstdFlags)
    var nodes []*node.Node

    if *id == 0 {
        logger.Fatalf("Node id must be greater than 0")
    }

    if *shardCount <= 0 {
        logger.Fatalf("Number of shards must be greater than 0")
    }

    if *tlsEnabled {
        if *certFile == "" || *keyFile == "" {
            logger.Fatalf("TLS enabled, but cert and key files are not provided")
        }
    }
    if *authUser != "" && *authPass == ""{
        logger.Fatalf("Auth user provided, but auth pass not provided")
    }
    if *authUser == "" && *authPass != ""{
        logger.Fatalf("Auth pass provided, but auth user not provided")
    }

    hashedPassword, err := bcrypt.GenerateFromPassword([]byte(*authPass), bcrypt.DefaultCost)
    if err != nil {
        logger.Fatalf("Failed to hash password: %v", err)
    }


    node := node.NewNode(*id, *address, *shardCount, *port, logger, *tlsEnabled, *certFile, *keyFile, *authUser, string(hashedPassword))

    if *aesKey != "" {
        node.SetAesKey(*aesKey)
    }

    node.SetGossipInterval(*gossipInterval)


    nodes = append(nodes, node)
    for i := 1; i <= 3; i++ {
        if i == *id {
            continue
        }
        newNodeAddress := fmt.Sprintf("localhost:5005%v", i)
        newNodePort := fmt.Sprintf("808%v", i)
        newNode := node.NewNode(i, newNodeAddress, *shardCount, newNodePort, logger, *tlsEnabled, *certFile, *keyFile, "", "")
        nodes = append(nodes, newNode)
    }
    node.Start(nodes)

    http.Handle("/metrics", promhttp.Handler())

    go func() {
        err := http.ListenAndServe(":9090", nil)
        if err != nil {
            logger.Fatalf("Failed to start metrics server: %v", err)
        }
    }()
    logger.Printf("Metrics server started on port 9090")

    signalChan := make(chan os.Signal, 1)
    signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)
    <-signalChan

    logger.Println("Shutdown signal received, shutting down...")
    node.Shutdown()
    for _, node := range nodes{
        node.Shutdown()
    }
    logger.Println("Shutdown complete")
    os.Exit(0)
}
func printMemUsage() {
    var m runtime.MemStats
    runtime.ReadMemStats(&m)
    fmt.Printf("Alloc = %v MiB", bToMb(m.Alloc))
    fmt.Printf("\tTotalAlloc = %v MiB", bToMb(m.TotalAlloc))
    fmt.Printf("\tSys = %v MiB", bToMb(m.Sys))
    fmt.Printf("\tNumGC = %v\n", m.NumGC)
}
func bToMb(b uint64) uint64 {
    return b / 1024 / 1024
}