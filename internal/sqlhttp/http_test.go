package sqlhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"distributed-db/internal/sqlengine"
)

type nopClient struct{}

func (nopClient) Begin(context.Context) (sqlengine.Txn, error) { return nopTxn{}, nil }

type nopTxn struct{}

func (nopTxn) Get(context.Context, []byte) ([]byte, error)                       { return nil, sqlengine.ErrNotFound }
func (nopTxn) Set([]byte, []byte)                                                {}
func (nopTxn) Delete([]byte)                                                     {}
func (nopTxn) Scan(context.Context, []byte, []byte, int) ([]sqlengine.KV, error) { return nil, nil }
func (nopTxn) Commit(context.Context) error                                      { return nil }
func (nopTxn) Rollback(context.Context) error                                    { return nil }

func TestHandlerBasics(t *testing.T) {
	h := New(sqlengine.New(nopClient{}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("health: %v %v", err, resp)
	}
	_ = resp.Body.Close()

	resp, err = http.Post(srv.URL+"/v1/sql", "application/json", bytes.NewReader([]byte("{")))
	if err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid JSON status = %v, %v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()

	body, _ := json.Marshal(map[string]string{"sql": "SELECT * FROM missing"})
	resp, err = http.Post(srv.URL+"/v1/sql", "application/json", bytes.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing table status = %v, %v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()
}
