package kvctl

import (
	"context"
	"errors"

	"distributed-db/internal/v2client"
)

// openV2Backend connects the CLI to a v2 meta/KV cluster.
func openV2Backend(_ context.Context, cfg Config) (Backend, error) {
	c, err := v2client.New(cfg.Meta, cfg.Nodes)
	if err != nil {
		return nil, err
	}
	return &v2Backend{client: c}, nil
}

type v2Backend struct {
	client *v2client.Client
}

func (b *v2Backend) Put(ctx context.Context, key, value []byte) error {
	txn, err := b.client.Begin(ctx)
	if err != nil {
		return err
	}
	txn.Set(key, value)
	if err := txn.Commit(ctx); err != nil {
		_ = txn.Rollback(ctx)
		return err
	}
	return nil
}

func (b *v2Backend) Get(ctx context.Context, key []byte) ([]byte, error) {
	txn, err := b.client.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = txn.Rollback(ctx) }()
	value, err := txn.Get(ctx, key)
	if errors.Is(err, v2client.ErrNotFound) {
		return nil, ErrNotFound
	}
	return value, err
}

func (b *v2Backend) Delete(ctx context.Context, key []byte) error {
	txn, err := b.client.Begin(ctx)
	if err != nil {
		return err
	}
	txn.Delete(key)
	if err := txn.Commit(ctx); err != nil {
		_ = txn.Rollback(ctx)
		return err
	}
	return nil
}

func (b *v2Backend) Scan(ctx context.Context, start, end []byte, limit int) ([]KV, error) {
	txn, err := b.client.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = txn.Rollback(ctx) }()
	items, err := txn.Scan(ctx, start, end, limit)
	if err != nil {
		return nil, err
	}
	out := make([]KV, 0, len(items))
	for _, kv := range items {
		out = append(out, KV{Key: kv.Key, Value: kv.Value})
	}
	return out, nil
}

func (b *v2Backend) Txn(ctx context.Context, ops []Op) error {
	txn, err := b.client.Begin(ctx)
	if err != nil {
		return err
	}
	for _, op := range ops {
		if op.Delete {
			txn.Delete(op.Key)
		} else {
			txn.Set(op.Key, op.Value)
		}
	}
	if err := txn.Commit(ctx); err != nil {
		_ = txn.Rollback(ctx)
		return err
	}
	return nil
}

func (b *v2Backend) Ping(ctx context.Context) error {
	txn, err := b.client.Begin(ctx)
	if err != nil {
		return err
	}
	return txn.Rollback(ctx)
}

func (b *v2Backend) Close() error { return b.client.Close() }
