package v2client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"distributed-db/internal/sqlengine"

	pbv2 "distributed-db/internal/pb/v2"
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

// ScanRanges implements sqlengine.RangeScanner. A Txn.Scan is routed by its
// start key and the KV server rejects intervals that cross a range boundary,
// so this method asks meta for the range containing the cursor (clipped by
// end), scans exactly that slice at the transaction snapshot, and continues
// with the range that starts at the previous range's end. The limit is global
// across ranges and empty ranges are skipped.
func (s *sqlTxn) ScanRanges(ctx context.Context, start, end []byte, limit int) ([]sqlengine.KV, error) {
	out, err := s.scanRanges(ctx, start, end, limit, false)
	if err != nil && rangeRetryable(err) {
		// A split/merge raced the scan. Drop cached descriptors and rebuild
		// the boundaries from meta once.
		s.t.c.clearRangeCache()
		return s.scanRanges(ctx, start, end, limit, true)
	}
	return out, err
}

func (s *sqlTxn) scanRanges(ctx context.Context, start, end []byte, limit int, fresh bool) ([]sqlengine.KV, error) {
	if len(start) > 0 && len(end) > 0 && bytes.Compare(start, end) >= 0 {
		return nil, nil
	}
	out := make([]sqlengine.KV, 0)
	cursor := append([]byte(nil), start...)
	for {
		if limit > 0 && len(out) >= limit {
			break
		}
		ri, err := s.rangeAt(ctx, cursor, fresh)
		if err != nil {
			return nil, err
		}
		if !rangeContains(ri, cursor) {
			// The cached descriptor shadows the cursor after a split. Fall
			// back to a fresh lookup; in fresh mode this is a real error.
			if fresh {
				return nil, fmt.Errorf("v2client: no range contains key %q", cursor)
			}
			ri, err = s.rangeAt(ctx, cursor, true)
			if err != nil {
				return nil, err
			}
			if !rangeContains(ri, cursor) {
				return nil, fmt.Errorf("v2client: no range contains key %q", cursor)
			}
		}

		clipEnd := end
		lastRange := true
		if rangeEnd := ri.GetEnd(); len(rangeEnd) > 0 {
			if len(end) == 0 || bytes.Compare(rangeEnd, end) < 0 {
				clipEnd = rangeEnd
				lastRange = false
			}
		}
		if len(clipEnd) > 0 && bytes.Compare(clipEnd, cursor) <= 0 {
			if lastRange {
				break
			}
			// Degenerate/empty slice: jump to the next range boundary.
			cursor = append(cursor[:0], clipEnd...)
			continue
		}

		remaining := 0
		if limit > 0 {
			remaining = limit - len(out)
		}
		rows, err := s.t.Scan(ctx, cursor, clipEnd, remaining)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			out = append(out, sqlengine.KV{Key: row.Key, Value: row.Value})
		}
		if limit > 0 && len(out) >= limit {
			break
		}
		if lastRange {
			// The final range extends past end (or to +inf), so one scan
			// already covered the rest of the interval.
			break
		}
		cursor = append(cursor[:0], clipEnd...)
	}
	return out, nil
}

// rangeAt resolves the descriptor that owns cursor. In retry mode it bypasses
// the local descriptor cache so a just-completed split is visible.
func (s *sqlTxn) rangeAt(ctx context.Context, cursor []byte, fresh bool) (*pbv2.RangeInfo, error) {
	if fresh {
		return s.t.c.rangeForFresh(ctx, cursor)
	}
	return s.t.c.rangeFor(ctx, cursor)
}

// rangeRetryable reports whether an error can be fixed by refreshing range
// descriptors from meta.
func rangeRetryable(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "range mismatch") ||
		strings.Contains(msg, "not hosted") ||
		strings.Contains(msg, "stale epoch")
}

func (s *sqlTxn) Commit(ctx context.Context) error { return s.t.Commit(ctx) }

func (s *sqlTxn) Rollback(ctx context.Context) error { return s.t.Rollback(ctx) }

var _ sqlengine.Client = (*SQLClient)(nil)
var _ sqlengine.Txn = (*sqlTxn)(nil)
var _ sqlengine.RangeScanner = (*sqlTxn)(nil)
