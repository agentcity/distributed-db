package node

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/crypto/bcrypt"

	"distributed-db/internal/model"
	"distributed-db/internal/raft"
)

// buildHTTPHandler wires the HTTP surface. Routing uses Go 1.22 method-aware
// ServeMux patterns, so unsupported methods automatically produce 405.
func (n *Node) buildHTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/kv/{key}", n.handlePut)
	mux.HandleFunc("GET /v1/kv/{key}", n.handleGet)
	mux.HandleFunc("DELETE /v1/kv/{key}", n.handleDelete)
	mux.HandleFunc("GET /health", n.handleHealth)
	mux.HandleFunc("GET /v1/status", n.handleStatus)
	mux.Handle("GET /metrics", promhttp.HandlerFor(n.metrics.registry, promhttp.HandlerOpts{}))

	var handler http.Handler = mux
	handler = n.authMiddleware(handler)
	handler = n.rateLimitMiddleware(handler)
	handler = n.metricsMiddleware(handler)
	return handler
}

func (n *Node) serveHTTP(ln net.Listener) {
	n.log.Info("http server listening", "addr", ln.Addr().String())
	var err error
	if n.cfg.TLSCert != "" {
		// The certificate/key pair was validated in New. ServeTLS loads it and
		// enables HTTP/2 over TLS for clients that support it.
		err = n.httpSrv.ServeTLS(ln, n.cfg.TLSCert, n.cfg.TLSKey)
	} else {
		err = n.httpSrv.Serve(ln)
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		n.log.Error("http server stopped unexpectedly", "error", err)
	}
}

// handlePut stores the raw request body under the path key. The write is
// replicated through Raft and only acknowledged after it is applied.
func (n *Node) handlePut(w http.ResponseWriter, r *http.Request) {
	n.metrics.putTotal.Inc()
	n.ops.Add(1)

	key := r.PathValue("key")
	if key == "" {
		n.metrics.errorsTotal.Inc()
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		n.metrics.errorsTotal.Inc()
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "value too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	index, err := n.propose(r.Context(), model.Entry{
		Type:  model.CommandPut,
		Key:   key,
		Value: body,
	})
	if err != nil {
		n.respondRaftError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, kvWriteResponse{Success: true, Index: index})
}

// handleGet performs a linearizable read: a Raft read barrier first, then the
// local KV lookup.
func (n *Node) handleGet(w http.ResponseWriter, r *http.Request) {
	n.metrics.getTotal.Inc()
	n.ops.Add(1)

	key := r.PathValue("key")
	if key == "" {
		n.metrics.errorsTotal.Inc()
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}
	if err := n.raft.ReadBarrier(r.Context()); err != nil {
		n.respondRaftError(w, err)
		return
	}
	value, ok := n.kv.Get(key)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(value)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(value)
}

// handleDelete proposes a delete. Deleting a missing key succeeds.
func (n *Node) handleDelete(w http.ResponseWriter, r *http.Request) {
	n.metrics.deleteTotal.Inc()
	n.ops.Add(1)

	key := r.PathValue("key")
	if key == "" {
		n.metrics.errorsTotal.Inc()
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}
	index, err := n.propose(r.Context(), model.Entry{
		Type: model.CommandDelete,
		Key:  key,
	})
	if err != nil {
		n.respondRaftError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, kvWriteResponse{Success: true, Index: index})
}

// handleHealth always reports 200 while the process is up.
func (n *Node) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{
		Status: "ok",
		ID:     n.cfg.ID,
		Shard:  n.cfg.Shard,
	})
}

// handleStatus returns the Raft status plus this node's shard.
func (n *Node) handleStatus(w http.ResponseWriter, _ *http.Request) {
	st := n.raft.Status()
	writeJSON(w, http.StatusOK, httpStatusResponse{
		ID:          st.ID,
		Shard:       n.cfg.Shard,
		Role:        string(st.Role),
		Term:        st.Term,
		Leader:      st.Leader,
		CommitIndex: st.CommitIndex,
		LastIndex:   st.LastIndex,
		LastApplied: st.LastApplied,
		KVKeys:      n.kv.Len(),
		Ops:         n.ops.Load(),
	})
}

// respondRaftError writes a normal HTTP error. A lost leadership maps to 503
// with the known leader advertised in both X-Leader and the JSON body, which is
// what the coordinator uses to retry against the right node.
func (n *Node) respondRaftError(w http.ResponseWriter, err error) {
	n.metrics.errorsTotal.Inc()
	leader := n.raft.Leader()
	w.Header().Set("X-Leader", leader)

	switch {
	case errors.Is(err, raft.ErrNotLeader):
		writeJSON(w, http.StatusServiceUnavailable, notLeaderResponse{Error: "not leader", Leader: leader})
	case errors.Is(err, raft.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		writeJSON(w, http.StatusServiceUnavailable, notLeaderResponse{Error: "timeout", Leader: leader})
	case errors.Is(err, context.Canceled):
		// The client went away; there is nobody left to read the body.
		return
	case errors.Is(err, raft.ErrStopped):
		writeJSON(w, http.StatusServiceUnavailable, notLeaderResponse{Error: "stopped", Leader: leader})
	default:
		n.log.Error("kv operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// authMiddleware enforces Basic Auth on /v1/kv/* when AuthUser is configured.
func (n *Node) authMiddleware(next http.Handler) http.Handler {
	if n.cfg.AuthUser == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/kv/") {
			next.ServeHTTP(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || !n.validCredentials(user, pass) {
			w.Header().Set("WWW-Authenticate", `Basic realm="db", charset="UTF-8"`)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// validCredentials compares credentials without leaking them. When AuthHash is
// set a bcrypt hash is used; otherwise the plaintext passphrase is compared in
// constant time. Production deployments should configure AuthHash.
func (n *Node) validCredentials(user, pass string) bool {
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(n.cfg.AuthUser)) == 1
	if n.cfg.AuthHash != "" {
		passOK := bcrypt.CompareHashAndPassword([]byte(n.cfg.AuthHash), []byte(pass)) == nil
		return userOK && passOK
	}
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(n.cfg.AuthPass)) == 1
	return userOK && passOK
}

// rateLimitMiddleware applies one global token bucket. Requests over the limit
// are rejected with 429.
func (n *Node) rateLimitMiddleware(next http.Handler) http.Handler {
	if n.limiter == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !n.limiter.Allow() {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// metricsMiddleware records the duration of every HTTP request.
func (n *Node) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		n.metrics.httpDuration.Observe(time.Since(start).Seconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.status = code
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type healthResponse struct {
	Status string `json:"status"`
	ID     string `json:"id"`
	Shard  string `json:"shard"`
}

type httpStatusResponse struct {
	ID          string `json:"id"`
	Shard       string `json:"shard"`
	Role        string `json:"role"`
	Term        uint64 `json:"term"`
	Leader      string `json:"leader"`
	CommitIndex uint64 `json:"commit_index"`
	LastIndex   uint64 `json:"last_index"`
	LastApplied uint64 `json:"last_applied"`
	KVKeys      int    `json:"kv_keys"`
	Ops         uint64 `json:"ops"`
}

type kvWriteResponse struct {
	Success bool   `json:"success"`
	Index   uint64 `json:"index,omitempty"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type notLeaderResponse struct {
	Error  string `json:"error"`
	Leader string `json:"leader"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
