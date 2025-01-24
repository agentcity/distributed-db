package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"distributed-db/internal/auth"
	"distributed-db/node"
)

// HTTPServer handles HTTP requests for the Node
type HTTPServer struct {
	node *node.Node
	mux *http.ServeMux
}

// NewHTTPServer creates a new HTTP server
func NewHTTPServer(n *node.Node, mux *http.ServeMux) *HTTPServer {
    return &HTTPServer{
        node: n,
		mux: mux,
    }
}

func (s *HTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/put":
		s.handlePutRequest(w, r)
	case "/get":
		s.handleGetRequest(w, r)
	case fmt.Sprintf("/delete/%s", r.PathValue("key")):
		s.handleDeleteRequest(w, r)
	case fmt.Sprintf("/vote/%s", r.PathValue("term")):
		s.handleVoteRequest(w, r)
	case "/health":
		s.handleHealthCheck(w, r)
	default:
		http.Error(w, "Not found", http.StatusNotFound)
	}
}
// handleHealthCheck returns node state
// @Summary Healthcheck endpoint
// @Description Get the health status of the node
// @Tags health
// @Produce plain
// @Success 200 {string} string "OK"
// @Router /health [get]
func (s *HTTPServer) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	fmt.Fprint(w, "OK, node state:", s.node.state)
}
// handlePutRequest handles put request
// @Summary Create data on the node.
// @Description Creates data on the node
// @Tags data
// @Accept json
// @Produce plain
// @Param key query string true "Key of the data to create"
// @Security basicAuth
// @Success 200 {string} string "OK"
// @Failure 400 {string} string "Bad Request"
// @Failure 401 {string} string "Unauthorized"
// @Failure 405 {string} string "Method Not Allowed"
// @Router /put [put]
func (s *HTTPServer) handlePutRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, pass, ok := r.BasicAuth()
	if !ok || user != s.node.authUser || !auth.CheckPassword(s.node.hashedAuthPass, pass) {
		w.Header().Set("WWW-Authenticate", `Basic realm="restricted", charset="UTF-8"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		s.node.log(node.LevelError, "Empty key for PUT request")
		http.Error(w, "Empty key", http.StatusBadRequest)
		return
	}
	s.node.log(node.LevelDebug, "Node %d received PUT request for key %s", s.node.id, key)
	if ok := s.node.Put(key); ok {
		fmt.Fprintf(w, "Data stored succesfully for key %s in node %d \n", key, s.node.id)
		s.node.putCounter.Inc()
	} else {
		s.node.errorCounter.Inc()
		http.Error(w, "Failed to store data", http.StatusBadRequest)
	}
}

// handleGetRequest handles get request
// @Summary Get data from the node.
// @Description Get data from a node by key
// @Tags data
// @Accept json
// @Produce plain
// @Param key query string true "Key of the data to get"
// @Security basicAuth
// @Success 200 {string} string "OK"
// @Failure 400 {string} string "Bad Request"
// @Failure 401 {string} string "Unauthorized"
// @Failure 405 {string} string "Method Not Allowed"
// @Router /get [get]
func (s *HTTPServer) handleGetRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, pass, ok := r.BasicAuth()
	if !ok || user != s.node.authUser || !auth.CheckPassword(s.node.hashedAuthPass, pass) {
		w.Header().Set("WWW-Authenticate", `Basic realm="restricted", charset="UTF-8"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		s.node.log(node.LevelError, "Empty key for GET request")
		http.Error(w, "Empty key", http.StatusBadRequest)
		return
	}
	s.node.log(node.LevelDebug, "Node %d received GET request for key %s", s.node.id, key)
	if value := s.node.Get(key); value {
		fmt.Fprintf(w, "Data found for key %s in node %d \n", key, s.node.id)
		s.node.getCounter.Inc()
	} else {
		s.node.errorCounter.Inc()
		http.Error(w, "Data not found", http.StatusNotFound)
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
func (s *HTTPServer) handleDeleteRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, pass, ok := r.BasicAuth()
	if !ok || user != s.node.authUser || !auth.CheckPassword(s.node.hashedAuthPass, pass) {
		w.Header().Set("WWW-Authenticate", `Basic realm="restricted", charset="UTF-8"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	key := r.PathValue("key")
	if key == "" {
		s.node.log(node.LevelError, "Empty key for DELETE request")
		http.Error(w, "Empty key", http.StatusBadRequest)
		return
	}
	if len(key) > 255 {
		s.node.log(node.LevelError, "Key is too long")
		http.Error(w, "Key is too long", http.StatusBadRequest)
		return
	}
	s.node.log(node.LevelDebug, "Node %d received DELETE request for key %s", s.node.id, key)
	s.node.appendLog(key)
	s.node.replicateLog()
	for _, entry := range s.node.raftLog {
		if entry.Key == key && entry.Committed {
			s.node.hashTable.Delete(key)
			break
		}
	}

	fmt.Fprintf(w, "Data deleted succesfully for key %s in node %d \n", key, s.node.id)
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
func (s *HTTPServer) handleVoteRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, pass, ok := r.BasicAuth()
	if !ok || user != s.node.authUser || !auth.CheckPassword(s.node.hashedAuthPass, pass) {
		w.Header().Set("WWW-Authenticate", `Basic realm="restricted", charset="UTF-8"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	termStr := r.PathValue("term")
	term, err := strconv.Atoi(termStr)
	if err != nil {
		s.node.log(node.LevelError, "Invalid term: %v", err)
		http.Error(w, "Invalid term", http.StatusBadRequest)
		return
	}
	if !s.node.isAlive {
		s.node.log(node.LevelError,"Node %d is not alive. Can't handle vote request", s.node.id)
		response := map[string]bool{"voted": false}
		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(response)
		if err != nil {
			s.node.log(node.LevelError,"Error encoding response %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return
	}

	if term > s.node.term {
		s.node.mu.Lock()
		defer s.node.mu.Unlock()
		s.node.log(node.LevelDebug, "Node %d received vote request for term: %d, my term is: %d, voted for: %s", s.node.id, term, s.node.term, s.node.votedFor)
		s.node.term = term
		s.node.votedFor = r.RemoteAddr
		s.node.state = node.Follower
		response := map[string]bool{"voted": true}
		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(response)
		if err != nil {
			s.node.log(node.LevelError,"Error encoding response %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}

	} else if term == s.node.term && (s.node.votedFor == "" || s.node.votedFor == r.RemoteAddr) && s.node.isLogUpToDate(term, len(s.node.raftLog)){
		s.node.mu.Lock()
		defer s.node.mu.Unlock()
		s.node.log(node.LevelDebug, "Node %d received vote request for term: %d, my term is: %d, voted for: %s", s.node.id, term, s.node.term, s.node.votedFor)
		s.node.votedFor = r.RemoteAddr
		s.node.state = node.Follower
		response := map[string]bool{"voted": true}
		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(response)
		if err != nil {
			s.node.log(node.LevelError,"Error encoding response %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	} else {
		s.node.log(node.LevelDebug, "Node %d rejected vote request for term: %d, my term is: %d, voted for: %s", s.node.id, term, s.node.term, s.node.votedFor)
		response := map[string]bool{"voted": false}
		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(response)
		if err != nil {
			s.node.log(node.LevelError,"Error encoding response %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}
