package http

import (
 "context"
 "crypto/tls"
 "encoding/json"
 "fmt"
 "io/ioutil"
 "log"
 "net/http"
 "time"
    "distributed-db/node"
 "github.com/gorilla/mux"
 "golang.org/x/crypto/bcrypt"
)

type HttpServer struct {
    node *node.Node
 server *http.Server
 tlsEnabled bool
    certFile string
    keyFile string
 authUser string
 authPass string
}

func NewHttpServer(node *node.Node, tlsEnabled bool, certFile string, keyFile string, authUser string, authPass string) *HttpServer{
 return &HttpServer{
        node: node,
  server: &http.Server{
   ReadTimeout:  5 * time.Second,
   WriteTimeout: 10 * time.Second,
  },
  tlsEnabled: tlsEnabled,
  certFile: certFile,
  keyFile: keyFile,
  authUser: authUser,
  authPass: authPass,
 }
}
func (s *HttpServer) Start(port string) error {
 r := mux.NewRouter()
 r.HandleFunc("/data/{key}", s.getDataHandler).Methods(http.MethodGet)
 r.HandleFunc("/data/{key}", s.putDataHandler).Methods(http.MethodPut)
 r.HandleFunc("/data/{key}", s.deleteDataHandler).Methods(http.MethodDelete)
    r.HandleFunc("/health", s.healthCheckHandler).Methods(http.MethodGet)
 r.Use(s.authMiddleware)
 s.server.Handler = r

 addr := fmt.Sprintf(":%s", port)
 s.node.Log(node.LevelInfo, "Starting HTTP server on: %v", addr)
 if s.tlsEnabled {
  cert, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
  if err != nil {
   return fmt.Errorf("failed to load key pair: %w", err)
  }
        config := &tls.Config{Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true}
  s.server.TLSConfig = config
  if err := s.server.ListenAndServeTLS(s.certFile, s.keyFile, addr); err != nil {
   return fmt.Errorf("failed to start https server: %w", err)
  }
 }else{
  if err := s.server.ListenAndServe(addr); err != nil {
   return fmt.Errorf("failed to start http server: %w", err)
  }
 }
 return nil
}

func (s *HttpServer) Shutdown() {
 ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
 defer cancel()

 if err := s.server.Shutdown(ctx); err != nil {
  s.node.Log(node.LevelError, "Failed to shutdown http server: %v", err)
 }
 s.node.Log(node.LevelInfo, "Http server shutdown")
}
func (s *HttpServer) authMiddleware(next http.Handler) http.Handler {
 return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
  if s.authUser == "" && s.authPass == ""{
   next.ServeHTTP(w, r)
   return
  }
  user, pass, ok := r.BasicAuth()
  if !ok || user != s.authUser {
     w.Header().Set("WWW-Authenticate", `Basic realm="restricted", charset="UTF-8"`)
     http.Error(w, "Unauthorized", http.StatusUnauthorized)
     return
    }
    err := bcrypt.CompareHashAndPassword([]byte(s.authPass), []byte(pass))

    if err != nil{
     w.Header().Set("WWW-Authenticate", `Basic realm="restricted", charset="UTF-8"`)
     http.Error(w, "Unauthorized", http.StatusUnauthorized)
     return
    }
    next.ServeHTTP(w, r)
   })
  }
  func (s *HttpServer) getDataHandler(w http.ResponseWriter, r *http.Request) {
   vars := mux.Vars(r)
   key, ok := vars["key"]
   if !ok {
    http.Error(w, "Key is missing", http.StatusBadRequest)
    return
   }

   ok, err := s.node.GetData(key)
   if err != nil {
    s.node.Log(node.LevelError, "Failed to get data: %v", err)
    http.Error(w, "Failed to get data", http.StatusInternalServerError)
    return
   }
   if !ok {
    http.Error(w, "Key not found", http.StatusNotFound)
    return
   }
   resp := map[string]interface{}{
    "key": key,
    "value": key,
   }
   w.Header().Set("Content-Type", "application/json")
   json.NewEncoder(w).Encode(resp)
  }
  func (s *HttpServer) putDataHandler(w http.ResponseWriter, r *http.Request) {
   vars := mux.Vars(r)
   key, ok := vars["key"]
   if !ok {
    http.Error(w, "Key is missing", http.StatusBadRequest)
    return
   }
   body, err := ioutil.ReadAll(r.Body)
   if err != nil {
    http.Error(w, "Failed to read request body", http.StatusBadRequest)
    return
   }
   var data map[string]interface{}
   if err := json.Unmarshal(body, &data); err != nil {
    http.Error(w, "Failed to unmarshal request body", http.StatusBadRequest)
    return
   }
   ok, err = s.node.PutData(key, data["value"])
   if err != nil {
    s.node.Log(node.LevelError, "Failed to put data: %v", err)
    http.Error(w, "Failed to put data", http.StatusInternalServerError)
    return
   }
      if !ok {
    http.Error(w, "Failed to put data", http.StatusInternalServerError)
    return
   }
      w.WriteHeader(http.StatusCreated)
      resp := map[string]string{
          "message": "data has been put",
      }
      w.Header().Set("Content-Type", "application/json")
      json.NewEncoder(w).Encode(resp)
  }
  func (s *HttpServer) deleteDataHandler(w http.ResponseWriter, r *http.Request) {
   vars := mux.Vars(r)
   key, ok := vars["key"]
   if !ok {
    http.Error(w, "Key is missing", http.StatusBadRequest)
    return
   }
      err := s.node.DeleteData(key)
   if err != nil {
    s.node.Log(node.LevelError, "Failed to delete data: %v", err)
    http.Error(w, "Failed to delete data", http.StatusInternalServerError)
    return
   }
      w.WriteHeader(http.StatusOK)
   resp := map[string]string{
          "message": "data has been deleted",
      }
      w.Header().Set("Content-Type", "application/json")
      json.NewEncoder(w).Encode(resp)
  }
  func (s *HttpServer) healthCheckHandler(w http.ResponseWriter, r *http.Request) {
      w.WriteHeader(http.StatusOK)
      resp := map[string]bool{
    "isAlive": s.node.IsAlive(),
      }
      w.Header().Set("Content-Type", "application/json")
      json.NewEncoder(w).Encode(resp)
  }