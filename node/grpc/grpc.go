package grpc

import (
 "context"
 "crypto/tls"
 "crypto/x509"
 "fmt"
 "io/ioutil"
 "log"
 "net"
 "distributed-db/node"
    pb "distributed-db/proto"
 "google.golang.org/grpc"
 "google.golang.org/grpc/credentials"
)

type GrpcServer struct {
 node *node.Node
    server *grpc.Server
    tlsEnabled bool
    certFile string
    keyFile string
}

func NewGrpcServer(node *node.Node, tlsEnabled bool, certFile string, keyFile string) *GrpcServer {
    return &GrpcServer{
        node: node,
        server: grpc.NewServer(),
        tlsEnabled: tlsEnabled,
        certFile: certFile,
        keyFile: keyFile,
    }
}
func (s *GrpcServer) Start(lis net.Listener) error {
    pb.RegisterNodeServiceServer(s.server, s)
 if s.tlsEnabled {
  cert, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
  if err != nil {
   return fmt.Errorf("failed to load key pair: %w", err)
  }
  config := &tls.Config{Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true}
        creds := credentials.NewTLS(config)
        s.server = grpc.NewServer(grpc.Creds(creds))
  pb.RegisterNodeServiceServer(s.server, s)
    }

    if err := s.server.Serve(lis); err != nil {
  return fmt.Errorf("failed to serve: %w", err)
    }
 return nil
}

func (s *GrpcServer) GetClientConn(address string) (*grpc.ClientConn, error) {
 if s.tlsEnabled {
  certPool := x509.NewCertPool()
  cert, err := ioutil.ReadFile(s.certFile)
  if err != nil {
   return nil, fmt.Errorf("could not read certificate: %w", err)
  }
  if ok := certPool.AppendCertsFromPEM(cert); !ok {
   return nil, fmt.Errorf("failed to append certificate")
  }

  config := &tls.Config{
   RootCAs:            certPool,
   InsecureSkipVerify: true,
  }
        creds := credentials.NewTLS(config)
        conn, err := grpc.Dial(address, grpc.WithTransportCredentials(creds))
  if err != nil {
   return nil, fmt.Errorf("did not connect: %w", err)
  }
        return conn, nil
 }
    conn, err := grpc.Dial(address, grpc.WithInsecure())
    if err != nil {
  return nil, fmt.Errorf("did not connect: %w", err)
    }
 return conn, nil
}

func (s *GrpcServer) Shutdown(){
    s.server.Stop()
}

func (s *GrpcServer) RequestVote(ctx context.Context, req *pb.RequestVoteRequest) (*pb.RequestVoteResponse, error) {
 return s.node.Raft().RequestVoteHandler(ctx, req)
}
func (s *GrpcServer) AppendEntries(ctx context.Context, req *pb.AppendEntriesRequest) (*pb.AppendEntriesResponse, error) {
    return s.node.Raft().AppendEntriesHandler(ctx, req)
}
func (s *GrpcServer) Gossip(ctx context.Context, req *pb.GossipRequest) (*pb.GossipResponse, error) {
 return s.node.GossipHandler(ctx, req)
}
func (s *GrpcServer) PutData(ctx context.Context, req *pb.PutRequest) (*pb.PutResponse, error) {
 key := req.Key
    ok, err := s.node.PutData(key, key)
 if err != nil {
  return &pb.PutResponse{Success: ok}, fmt.Errorf("failed to put data: %w", err)
 }
    return &pb.PutResponse{Success: ok}, nil
}
func (s *GrpcServer) GetData(ctx context.Context, req *pb.GetRequest) (*pb.GetResponse, error) {
 key := req.Key
    ok, err := s.node.GetData(key)
 if err != nil {
        return &pb.GetResponse{Success: ok}, fmt.Errorf("failed to get data: %w", err)
    }
 return &pb.GetResponse{Success: ok}, nil
}
func (s *GrpcServer) DeleteData(ctx context.Context, req *pb.DeleteRequest) (*pb.DeleteResponse, error) {
    key := req.Key
 err := s.node.DeleteData(key)
    if err != nil {
        return &pb.DeleteResponse{}, fmt.Errorf("failed to delete data: %w", err)
    }
    return &pb.DeleteResponse{}, nil
}

func (s *GrpcServer) ShardHeartbeat(ctx context.Context, req *pb.ShardHeartbeatRequest) (*pb.ShardHeartbeatResponse, error) {
 return s.node.ShardHeartbeatHandler(ctx, req)
}