package main

import (
 "flag"
 "fmt"
 "log"
 "os"
 "strconv"
 "strings"
 "time"

 "distributed-db/coordinator"
 "distributed-db/node"
)

func main() {
 var (
  nodeId           int
  address          string
  otherNodes       string
  shardId          int
  coordinatorAddr string
  logLevel      string
  tls           bool
  serverCert    string
  clientCert   string
  authUser       string
  authPass       string
 )

 flag.IntVar(&nodeId, "id", 1, "Node ID")
 flag.StringVar(&address, "address", ":8080", "Node address")
 flag.StringVar(&otherNodes, "otherNodes", "", "List of other nodes, comma-separated")
 flag.IntVar(&shardId, "shardId", 0, "Shard ID")
 flag.StringVar(&coordinatorAddr, "coordinatorAddress", ":8081", "Coordinator address")
 flag.StringVar(&logLevel, "logLevel", "INFO", "Log level (DEBUG, INFO, ERROR)")
 flag.BoolVar(&tls, "tls", false, "Enable TLS")
 flag.StringVar(&serverCert, "serverCert", "certs/server.crt", "Server certificate path")
 flag.StringVar(&clientCert, "clientCert", "certs/client.crt", "Client certificate path")
 flag.StringVar(&authUser, "authUser", "admin", "Auth user")
 flag.StringVar(&authPass, "authPass", "admin", "Auth pass")

    flag.Parse()

 logger := log.New(os.Stdout, "", log.LstdFlags)

 otherNodesList := []string{}
 if otherNodes != "" {
  otherNodesList = strings.Split(otherNodes, ",")
 }
 numNodes := len(otherNodesList) + 1
 if numNodes < 3 {
  fmt.Println("For proper raft work please specify at least 3 nodes in otherNodes parameter")
        os.Exit(1)
    }


 if nodeId == 0 {
  coord := coordinator.NewCoordinator(coordinatorAddr, logger, tls, serverCert, clientCert, authUser, authPass)
  coord.SetLogLevel(logLevel)
  logger.Printf("Coordinator started on: %s", coordinatorAddr)
  coord.StartServer()

  coord.StartLoadBalancing(5 * time.Second)
  c := make(chan os.Signal, 1)
  // We'll accept graceful shutdowns when quit via SIGINT (Ctrl+C) or SIGTERM
  // SIGKILL or SIGQUIT will not be caught.
  signal.Notify(c, os.Interrupt, syscall.SIGTERM)

  // Block until we receive our signal.
  <-c
  logger.Println("Shutting down coordinator")
  coord.StopServer()
  os.Exit(0)

 } else {
  n := node.NewNode(nodeId, address, numNodes, otherNodesList, shardId, coordinatorAddr, logger, tls, serverCert, clientCert, authUser, authPass)
  n.SetLogLevel(logLevel)
  logger.Printf("Node %d started on: %s, shard %d", nodeId, address, shardId)

  nodes := []*node.Node{}
  nodes = append(nodes, n)
  for _, other := range otherNodesList {
   id, err := strconv.Atoi(strings.Split(other, ":")[1])
   if err != nil {
    logger.Fatalf("Error parsing id from otherNodes: %v", err)
   }

   node := node.NewNode(id, other, numNodes, otherNodesList, shardId, coordinatorAddr, logger, tls, serverCert, clientCert, authUser, authPass)
   nodes = append(nodes, node)
  }
  n.StartServer(nodes)
  n.StartLoadReport(5 * time.Second)
  n.StartHeartbeat(nodes, 100 * time.Millisecond)
  n.startElectionTimer(nodes)
  n.startGossip(nodes, 100 * time.Millisecond)

  c := make(chan os.Signal, 1)
  // We'll accept graceful shutdowns when quit via SIGINT (Ctrl+C) or SIGTERM
  // SIGKILL or SIGQUIT will not be caught.
  signal.Notify(c, os.Interrupt, syscall.SIGTERM)

  // Block until we receive our signal.
  <-c
  logger.Printf("Shutting down node %d", nodeId)
  n.StopServer()
  os.Exit(0)
 }
}
