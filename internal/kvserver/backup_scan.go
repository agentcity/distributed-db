package kvserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc"

	"distributed-db/internal/mvcc"
	pbv2 "distributed-db/internal/pb/v2"
)

// backupScanService adds the lock-aware ScanForBackup RPC on top of the
// existing KVService implementation. It embeds *kvService so every existing
// handler stays exactly as it is; only the new method is overridden instead of
// inheriting the generated UnimplementedKVServiceServer stub.
//
// This type is deliberately separate from kvService: the legacy registration
// in server.go still registers the base kvService, which therefore keeps the
// generated Unimplemented stub for ScanForBackup. Wiring the helper below is
// intentionally left to the parent change so this slice does not edit
// server.go:
//
//	pbv2.RegisterKVServiceServer(s.grpcSrv, &kvService{s: s})
//	// becomes
//	RegisterKVServiceWithBackupScan(s.grpcSrv, s)
//
// Wire compatibility: adding an RPC to the existing service is additive. An
// older node answers Unimplemented for the new method, which the strict backup
// scanner treats as a hard error; an older client simply never calls it. The
// new response fields are ignored by old clients, and the RPC reuses the
// existing in-band error convention for every non-lock failure.
type backupScanService struct {
	*kvService
}

// NewKVServiceServerWithBackupScan returns a KVServiceServer that implements
// ScanForBackup in addition to the historical KVService methods.
func NewKVServiceServerWithBackupScan(s *Server) pbv2.KVServiceServer {
	return &backupScanService{kvService: &kvService{s: s}}
}

// RegisterKVServiceWithBackupScan registers the complete KVService, including
// the lock-aware ScanForBackup RPC, on registrar. The parent can replace the
// existing direct pbv2.RegisterKVServiceServer call with this helper without
// changing any handler in service.go.
func RegisterKVServiceWithBackupScan(registrar grpc.ServiceRegistrar, s *Server) {
	pbv2.RegisterKVServiceServer(registrar, NewKVServiceServerWithBackupScan(s))
}

// ScanForBackup implements pbv2.KVServiceServer.
//
// The request is routed by range_id when supplied, otherwise by start so the
// remote backup adapter does not have to pin a possibly stale descriptor. The
// handler requires leadership and a Raft read barrier, exactly like Scan, so
// the scan observes all state applied on the leader. It then calls
// mvcc.Store.ScanForBackup, which checks the lock column before the write
// column in one engine snapshot.
//
// A lock at or below ts is reported in-band with locked=true and no values. A
// lock above ts is invisible to this snapshot and is ignored by the store, as
// is a lock outside [start, end). Every other failure uses the same opError
// categories as the existing RPCs so leader redirects keep working unchanged.
func (k *backupScanService) ScanForBackup(ctx context.Context, req *pbv2.ScanForBackupRequest) (*pbv2.ScanForBackupResponse, error) {
	if req == nil {
		return &pbv2.ScanForBackupResponse{
			Leader: k.s.defaultLeader(),
			Error:  "missing request",
		}, nil
	}

	var (
		g   *rangeGroup
		err error
	)
	if req.GetRangeId() != 0 {
		g, err = k.s.groupForID(req.GetRangeId())
	} else {
		g, err = k.s.groupForKey(req.GetStart())
	}
	if err != nil {
		return &pbv2.ScanForBackupResponse{
			Leader: k.s.defaultLeader(),
			Error:  opError(err),
		}, nil
	}

	leader := g.node.Leader()
	if err := k.s.checkEpoch(ctx, g); err != nil {
		return &pbv2.ScanForBackupResponse{Leader: leader, Error: opError(err)}, nil
	}
	rangeStart, rangeEnd, _ := k.s.rangeView(g)
	if !backupRangeContains(rangeStart, rangeEnd, req.GetStart(), req.GetEnd()) {
		err := fmt.Errorf("%w: backup scan [%q,%q) crosses range %d [%q,%q)",
			ErrRangeMismatch, req.GetStart(), req.GetEnd(), g.cfg.ID, rangeStart, rangeEnd)
		return &pbv2.ScanForBackupResponse{Leader: leader, Error: opError(err)}, nil
	}
	if err := g.node.ReadBarrier(ctx); err != nil {
		return &pbv2.ScanForBackupResponse{Leader: leader, Error: opError(err)}, nil
	}

	rows, err := g.store.ScanForBackup(req.GetStart(), req.GetEnd(), req.GetTs(), int(req.GetLimit()))
	if err != nil {
		var lockErr *mvcc.SnapshotLockError
		if errors.As(err, &lockErr) {
			// kvs stays empty on purpose: a partial snapshot must not be
			// mistaken for a complete, lock-free one.
			return &pbv2.ScanForBackupResponse{
				Leader:    leader,
				Error:     opError(err),
				Locked:    true,
				LockedKey: cloneBytes(lockErr.Key),
				LockTs:    lockErr.StartTS,
			}, nil
		}
		return &pbv2.ScanForBackupResponse{Leader: leader, Error: opError(err)}, nil
	}

	out := make([]*pbv2.KV, 0, len(rows))
	for _, row := range rows {
		out = append(out, &pbv2.KV{Key: row.Key, Value: row.Value})
	}
	return &pbv2.ScanForBackupResponse{Kvs: out, Leader: leader}, nil
}

// backupRangeContains mirrors rangeGroup.containsRange for a range descriptor
// snapshot taken through Server.rangeView. The latter avoids racing with a
// concurrent split/merge that rewrites g.cfg under s.mu.
func backupRangeContains(rangeStart, rangeEnd, start, end []byte) bool {
	if rangeStart != nil {
		if start == nil || bytes.Compare(start, rangeStart) < 0 {
			return false
		}
	}
	if rangeEnd != nil {
		if end == nil || bytes.Compare(end, rangeEnd) > 0 {
			return false
		}
	}
	return true
}
