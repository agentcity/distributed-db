package main

import (
 "flag"
 "fmt"
 "log"
 "os"
 "os/signal"
 "syscall"

 "distributed-db/node"
)

func main() {
 id := flag.Int("id", 1, "Node ID")
 address := flag.String("address", ":8080", "Address to bind to")
 otherNodes := flag.String("other-nodes", "", "Comma-separated list of other node addresses")
 shardId := flag.Int("shard-id", 1, "Shard ID")
 coordinatorAddress := flag.String("coordinator-address", "", "Coordinator address")
 tls := flag.Bool("tls", false, "Use TLS")
 serverCert := flag.String("server-cert", "certs/server.crt", "Path to server certificate")
 clientCert := flag.String("client-cert", "certs/client.crt", "Path to client certificate")
 authUser := flag.String("auth-user", "user", "Basic auth user")
 authPass := flag.String("auth-pass", "pass", "Basic auth password")
 encryptionKey := flag.String("encryption-key", "supersecretkey", "encryption key for local db")

 flag.Parse()

 logger := log.New(os.Stdout, fmt.Sprintf("Node %d: ", *id), log.LstdFlags|log.Lshortfile)

 otherNodesList := make([]string, 0)

 if *otherNodes != "" {
  for _, n := range  split(*otherNodes, ",") {
   otherNodesList = append(otherNodesList, n)
  }
 }
 node := node.NewNode(*id, *address, len(otherNodesList)+1, otherNodesList, *shardId, *coordinatorAddress, logger, *tls, *serverCert, *clientCert, *authUser, *authPass, *encryptionKey)
 nodes := []*node.Node{node}
    for _, addr := range otherNodesList {
  n := node.NewNode(len(nodes)+1, addr, len(otherNodesList)+1, otherNodesList, *shardId, *coordinatorAddress, logger, *tls, *serverCert, *clientCert, *authUser, *authPass, *encryptionKey)
  nodes = append(nodes, n)
 }

 node.StartServer(nodes)
    for _, n := range nodes {
  if n.id != *id {
   n.StartServer(nodes)
  }
 }
 signalChan := make(chan os.Signal, 1)
 signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

 <-signalChan
 node.cancel()
 node.log(node.LevelInfo,"Gracefully shutting down application")
 os.Exit(0)

}
func split(s string, sep string) []string {
 res := make([]string,0)
 current := ""
 for _, char := range s {
  if string(char) == sep {
   res = append(res, current)
   current = ""
  } else {
   current += string(char)
  }
 }
 res = append(res, current)
 return res
}
