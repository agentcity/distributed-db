package coordinator

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"distributed-db/internal/pb"
)

// maxRequestBody mirrors the node's limit on a PUT value.
const maxRequestBody = 8 << 20

// maxValueBytes mirrors the node's HTTP body cap so the coordinator rejects
// oversized values before buffering them.
const maxValueBytes = 8 << 20

// buildHTTPHandler wires the public HTTP surface. The rate limiter wraps the
// whole mux; Basic Auth protects only /v1/kv/*.
func (c *Coordinator) buildHTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("PUT /v1/kv/{key}", c.withAuth(http.HandlerFunc(c.handlePut)))
	mux.Handle("GET /v1/kv/{key}", c.withAuth(http.HandlerFunc(c.handleGet)))
	mux.Handle("DELETE /v1/kv/{key}", c.withAuth(http.HandlerFunc(c.handleDelete)))
	mux.HandleFunc("GET /health", c.handleHealth)
	mux.HandleFunc("GET /v1/status", c.handleStatus)
	mux.Handle("GET /metrics", c.metricsHandler())
	return c.withRateLimit(mux)
}

// withRateLimit rejects requests over the configured global rate.
func (c *Coordinator) withRateLimit(next http.Handler) http.Handler {
	if c.limiter == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !c.limiter.Allow() {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withAuth enforces HTTP Basic Auth with constant-time comparisons. When
// AuthUser is empty authentication is disabled.
func (c *Coordinator) withAuth(next http.Handler) http.Handler {
	if c.cfg.AuthUser == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		// Compare both fields unconditionally so the response time does not
		// reveal which one was wrong.
		userOK := secureEqual(user, c.cfg.AuthUser)
		passOK := secureEqual(pass, c.cfg.AuthPass)
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="db", charset="UTF-8"`)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// secureEqual compares two strings in constant time, including for different
// lengths, by comparing their SHA-256 digests.
func secureEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}

func (c *Coordinator) handlePut(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "value too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	start := time.Now()
	shard := c.metricShard(key)
	var resp *pb.PutResponse
	_, err = c.proxy(r.Context(), "put", key, func(ctx context.Context, client pb.NodeServiceClient, _ string) (string, string, error) {
		r, callErr := client.Put(ctx, &pb.PutRequest{Key: key, Value: body})
		if callErr != nil {
			return "", "", callErr
		}
		resp = r
		return r.GetLeader(), r.GetError(), nil
	})
	if err != nil {
		c.observeRequest("put", shard, resultLabel(err), start)
		writeUpstreamError(w, err)
		return
	}
	if !resp.GetSuccess() {
		msg := resp.GetError()
		if msg == "" {
			msg = "put was not applied"
		}
		c.observeRequest("put", shard, resultLabel(errors.New(msg)), start)
		writeOperationFailure(w, msg)
		return
	}
	c.observeRequest("put", shard, "ok", start)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "index": resp.GetIndex()})
}

func (c *Coordinator) handleGet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}

	start := time.Now()
	shard := c.metricShard(key)
	var resp *pb.GetResponse
	_, err := c.proxy(r.Context(), "get", key, func(ctx context.Context, client pb.NodeServiceClient, _ string) (string, string, error) {
		r, callErr := client.Get(ctx, &pb.GetRequest{Key: key})
		if callErr != nil {
			return "", "", callErr
		}
		resp = r
		return r.GetLeader(), r.GetError(), nil
	})
	if err != nil {
		c.observeRequest("get", shard, resultLabel(err), start)
		writeUpstreamError(w, err)
		return
	}
	if msg := resp.GetError(); msg != "" {
		c.observeRequest("get", shard, resultLabel(errors.New(msg)), start)
		writeOperationFailure(w, msg)
		return
	}
	if !resp.GetFound() {
		c.observeRequest("get", shard, "not_found", start)
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	c.observeRequest("get", shard, "ok", start)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp.GetValue())
}

func (c *Coordinator) handleDelete(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}

	start := time.Now()
	shard := c.metricShard(key)
	var resp *pb.DeleteResponse
	_, err := c.proxy(r.Context(), "delete", key, func(ctx context.Context, client pb.NodeServiceClient, _ string) (string, string, error) {
		r, callErr := client.Delete(ctx, &pb.DeleteRequest{Key: key})
		if callErr != nil {
			return "", "", callErr
		}
		resp = r
		return r.GetLeader(), r.GetError(), nil
	})
	if err != nil {
		c.observeRequest("delete", shard, resultLabel(err), start)
		writeUpstreamError(w, err)
		return
	}
	if !resp.GetSuccess() {
		msg := resp.GetError()
		if msg == "" {
			msg = "delete was not applied"
		}
		c.observeRequest("delete", shard, resultLabel(errors.New(msg)), start)
		writeOperationFailure(w, msg)
		return
	}
	c.observeRequest("delete", shard, "ok", start)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "index": resp.GetIndex()})
}

func (c *Coordinator) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (c *Coordinator) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, c.statusSnapshot())
}

// writeUpstreamError maps a proxy failure to an HTTP status:
// not-leader after retries -> 503 with X-Leader, other upstream failures -> 502.
func writeUpstreamError(w http.ResponseWriter, err error) {
	var notLeader *notLeaderError
	if errors.As(err, &notLeader) {
		if notLeader.Leader() != "" {
			w.Header().Set("X-Leader", notLeader.Leader())
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error":  "not leader",
			"leader": notLeader.Leader(),
		})
		return
	}
	if errors.Is(err, errNoShards) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
}

// writeOperationFailure maps an unsuccessful (but transport-successful)
// response: "not found" becomes 404, anything else 502.
func writeOperationFailure(w http.ResponseWriter, msg string) {
	if isNotFoundMessage(msg) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": msg})
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
