package v2client

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"distributed-db/internal/mvcc"
	pbv2 "distributed-db/internal/pb/v2"
)

// ErrBackupScanUnavailable reports that the KV node does not implement
// KVService.ScanForBackup. It is the signal the backup RemoteScanner uses to
// decide whether an explicitly permitted best-effort fallback is allowed. The
// default is to fail closed: a backup that promises a lock-aware scan must not
// silently downgrade to the plain Scan path.
var ErrBackupScanUnavailable = errors.New("v2client: server does not implement KVService.ScanForBackup")

// errEmptyBackupScanResponse guards against a malformed successful response.
var errEmptyBackupScanResponse = errors.New("v2client: empty ScanForBackup response")

// ScanForBackup performs the lock-aware snapshot scan added for logical
// backups. rangeID is the target range (0 lets the server resolve it from
// start, which is what backup.RemoteScanner uses); start/end are the exact
// range bounds, ts is the backup snapshot TSO and limit caps one page. start
// also routes leader discovery, so a non-zero rangeID requires start to fall
// inside that range.
//
// Routing and retries are the same as the other Client helpers: the cached or
// discovered leader is used, "not leader" hints trigger a redirect, and a
// stale epoch refreshes the range descriptor. A lock at or below ts is not a
// retryable transport/leader error; it is returned as *mvcc.SnapshotLockError
// (which unwraps to mvcc.ErrLocked) so the caller can abort or retry the whole
// backup at a fresh TSO. An old server that answers Unimplemented yields an
// error matching ErrBackupScanUnavailable.
//
// ScanForBackup does not change Txn.Scan or any other data-call behavior.
func (c *Client) ScanForBackup(ctx context.Context, rangeID uint64, start, end []byte, ts uint64, limit int) ([]KV, error) {
	if c == nil {
		return nil, errors.New("v2client: nil client")
	}

	var (
		out         *pbv2.ScanForBackupResponse
		unsupported error
		locked      *mvcc.SnapshotLockError
	)
	err := c.retry(ctx, start, func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
		// Each attempt either installs a complete result or clears the
		// previous attempt's state. A retry after "not leader" must not leak
		// rows or a lock from the rejected response.
		out, unsupported, locked = nil, nil, nil
		resp, err := cl.ScanForBackup(ctx, &pbv2.ScanForBackupRequest{
			RangeId: rangeID,
			Start:   start,
			End:     end,
			Ts:      ts,
			Limit:   int32(limit),
		})
		if err != nil {
			if status.Code(err) == codes.Unimplemented {
				// Let the retry helper finish immediately. Returning the
				// transport error here would retry an unimplemented method
				// across every cached leader and every backoff step.
				unsupported = err
				return "", "", nil
			}
			return "", "", err
		}
		out = resp
		if resp.GetLocked() {
			locked = &mvcc.SnapshotLockError{
				Key:        append([]byte(nil), resp.GetLockedKey()...),
				StartTS:    resp.GetLockTs(),
				SnapshotTS: ts,
			}
			// A lock is an authoritative result, not a leader/transport
			// failure. Return success from the RPC helper so retry does not
			// translate "locked" into its generic conflict error.
			return resp.GetLeader(), "", nil
		}
		return resp.GetLeader(), resp.GetError(), nil
	})
	if err != nil {
		return nil, err
	}
	if unsupported != nil {
		return nil, fmt.Errorf("%w: %v", ErrBackupScanUnavailable, unsupported)
	}
	if locked != nil {
		return nil, locked
	}
	if out == nil {
		return nil, errEmptyBackupScanResponse
	}
	rows := make([]KV, 0, len(out.GetKvs()))
	for _, kv := range out.GetKvs() {
		if kv == nil {
			return nil, errors.New("v2client: ScanForBackup returned a nil KV")
		}
		rows = append(rows, KV{Key: kv.GetKey(), Value: kv.GetValue()})
	}
	return rows, nil
}

// ScanBestEffort performs the historical lock-ignoring Scan at the caller's
// snapshot ts. It exists only for backup.RemoteScanner's explicit fallback
// when a server predates ScanForBackup; it must never be used as an implicit
// replacement for the lock-aware RPC.
//
// It has the same leader redirect/retry behavior as Txn.Scan and does not
// inspect the lock column on keys that have never been committed.
func (c *Client) ScanBestEffort(ctx context.Context, start, end []byte, ts uint64, limit int) ([]KV, error) {
	if c == nil {
		return nil, errors.New("v2client: nil client")
	}
	var (
		rows []KV
		msg  string
	)
	if err := c.retry(ctx, start, func(ctx context.Context, cl pbv2.KVServiceClient) (string, string, error) {
		msg = ""
		rows = rows[:0]
		resp, err := cl.Scan(ctx, &pbv2.ScanRequest{
			Start: start,
			End:   end,
			Ts:    ts,
			Limit: int32(limit),
		})
		if err != nil {
			return "", "", err
		}
		msg = resp.GetError()
		for _, kv := range resp.GetKvs() {
			if kv == nil {
				return "", "", errors.New("v2client: Scan returned a nil KV")
			}
			rows = append(rows, KV{Key: kv.GetKey(), Value: kv.GetValue()})
		}
		return resp.GetLeader(), resp.GetError(), nil
	}); err != nil {
		return nil, err
	}
	if msg != "" {
		return nil, fmt.Errorf("v2client: best-effort scan: %s", msg)
	}
	return rows, nil
}
