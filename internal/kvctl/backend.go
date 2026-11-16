package kvctl

import (
	"context"
	"errors"

	"distributed-db/internal/backup"
	"distributed-db/internal/v2client"
)

// openV2Backend connects the CLI to a v2 meta/KV cluster. Administrative
// commands (ranges, status, split, merge) pass no nodes and get a meta-only
// client.
func openV2Backend(_ context.Context, cfg Config) (Backend, error) {
	var (
		c   *v2client.Client
		err error
	)
	if len(cfg.Nodes) == 0 {
		c, err = v2client.NewMetaOnly(cfg.Meta)
	} else {
		c, err = v2client.New(cfg.Meta, cfg.Nodes)
	}
	if err != nil {
		return nil, err
	}
	return &v2Backend{client: c, cfg: cfg}, nil
}

type v2Backend struct {
	client *v2client.Client
	cfg    Config
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

func (b *v2Backend) ListRanges(ctx context.Context) ([]RangeInfo, error) {
	infos, err := b.client.ListRanges(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RangeInfo, 0, len(infos))
	for _, info := range infos {
		out = append(out, RangeInfo{
			ID:       info.ID,
			Start:    info.Start,
			End:      info.End,
			Replicas: info.Replicas,
			Leader:   info.Leader,
			Version:  info.Version,
		})
	}
	return out, nil
}

func (b *v2Backend) SplitRange(ctx context.Context, rangeID uint64, splitKey []byte) (uint64, error) {
	return b.client.SplitRange(ctx, rangeID, splitKey)
}

func (b *v2Backend) MergeRanges(ctx context.Context, leftID, rightID uint64) error {
	return b.client.MergeRanges(ctx, leftID, rightID)
}

func (b *v2Backend) MetaLeader(ctx context.Context) (string, uint64, error) {
	return b.client.MetaLeader(ctx)
}

func (b *v2Backend) Backup(ctx context.Context, opts BackupOptions) (BackupResult, error) {
	meta, err := backup.OpenMeta([]string{b.cfg.Meta})
	if err != nil {
		return BackupResult{}, err
	}
	defer func() { _ = meta.Close() }()

	manifest, err := backup.Backup(ctx, backup.BackupConfig{
		Client:     b.client,
		Meta:       meta,
		OutDir:     opts.OutDir,
		ChunkKeys:  opts.ChunkKeys,
		ChunkBytes: opts.ChunkBytes,
	})
	if err != nil {
		return BackupResult{}, err
	}
	return BackupResult{
		OutDir:     opts.OutDir,
		SnapshotTS: manifest.SnapshotTS,
		Keys:       manifest.TotalKeys,
		Bytes:      manifest.TotalBytes,
		Ranges:     len(manifest.Ranges),
	}, nil
}
