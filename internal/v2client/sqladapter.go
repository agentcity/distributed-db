package v2client

import (
	"context"
	"errors"

	"distributed-db/internal/sqlengine"
)

// SQLClient adapts the transactional v2 client to sqlengine.Client.
type SQLClient struct {
	c *Client
}

// NewSQLClient wraps c for the SQL engine.
func NewSQLClient(c *Client) *SQLClient { return &SQLClient{c: c} }

// Begin implements sqlengine.Client.
func (s *SQLClient) Begin(ctx context.Context) (sqlengine.Txn, error) {
	t, err := s.c.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &sqlTxn{t: t}, nil
}

type sqlTxn struct {
	t *Txn
}

func (s *sqlTxn) Get(ctx context.Context, key []byte) ([]byte, error) {
	v, err := s.t.Get(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return nil, sqlengine.ErrNotFound
	}
	return v, err
}

func (s *sqlTxn) Set(key, value []byte) { s.t.Set(key, value) }

func (s *sqlTxn) Delete(key []byte) { s.t.Delete(key) }

func (s *sqlTxn) Scan(ctx context.Context, start, end []byte, limit int) ([]sqlengine.KV, error) {
	rows, err := s.t.Scan(ctx, start, end, limit)
	if err != nil {
		return nil, err
	}
	out := make([]sqlengine.KV, 0, len(rows))
	for _, row := range rows {
		out = append(out, sqlengine.KV{Key: row.Key, Value: row.Value})
	}
	return out, nil
}

func (s *sqlTxn) Commit(ctx context.Context) error { return s.t.Commit(ctx) }

func (s *sqlTxn) Rollback(ctx context.Context) error { return s.t.Rollback(ctx) }

var _ sqlengine.Client = (*SQLClient)(nil)
var _ sqlengine.Txn = (*sqlTxn)(nil)
