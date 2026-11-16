package metaserver

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"distributed-db/internal/meta"
	pbv2 "distributed-db/internal/pb/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// schedulerRPCTimeout bounds every per-replica KVService administration call.
const schedulerRPCTimeout = 30 * time.Second

// schedulerMu serializes split/merge orchestration in this process. The
// sequence "read range map snapshot -> fan out to replicas -> ApplyRanges" is
// not atomic on its own: without this lock two concurrent scheduling
// operations on the same meta leader could both derive their state (including
// the deterministic next range ID) from the same snapshot and the later apply
// would overwrite the earlier one. Raft still picks a single meta leader; this
// lock closes the same-process window only.
var schedulerMu sync.Mutex

// SplitRange implements pbv2.MetaServiceServer.
//
// The source range is split as follows:
//  1. the caller must be the meta leader, and the current range descriptor is
//     read from a fresh cluster snapshot;
//  2. every replica of the range is asked, concurrently, to split its local
//     Raft group at splitKey into a new group whose ID is
//     max(existing range IDs)+1 (chosen deterministically from the snapshot so
//     that all meta replicas agree without a local ID allocator);
//  3. only after every replica succeeds is the range map replaced through
//     ApplyRanges (one Raft entry). The left range keeps its ID and gains
//     End=splitKey; the right range gets the new ID. Both get
//     Version=old.Version+1 and the same replica set.
//
// A retry after a partially failed fan-out is safe at the KV layer: replicas
// that already created the new group answer success, so the meta update is
// simply retried. A retry after a fully applied split is recognized from the
// snapshot and returns the existing right range ID.
func (s *Server) SplitRange(ctx context.Context, req *pbv2.MetaSplitRangeRequest) (*pbv2.MetaSplitRangeResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	schedulerMu.Lock()
	defer schedulerMu.Unlock()

	if err := s.requireLeader(ctx); err != nil {
		return nil, err
	}
	if req == nil {
		return splitFailed("split: missing request"), nil
	}

	rangeID := req.GetRangeId()
	splitKey := req.GetSplitKey()

	rows := s.cluster.Snapshot()
	byID, maxID := indexRanges(rows)

	old, ok := byID[rangeID]
	if !ok {
		return splitFailed("split range %d: range not found", rangeID), nil
	}
	// Idempotency: the split has already been applied when the left range ends
	// exactly at splitKey and its right-hand sibling starts there.
	if newID, done := splitResult(rows, rangeID, splitKey); done {
		return &pbv2.MetaSplitRangeResponse{Success: true, NewRangeId: newID}, nil
	}
	if len(old.Replicas) == 0 {
		return splitFailed("split range %d: range has no replicas", rangeID), nil
	}
	if !strictlyInside(splitKey, old.Start, old.End) {
		return splitFailed("split range %d: split key %q is not strictly inside [%q, %q)",
			rangeID, splitKey, old.Start, old.End), nil
	}

	// Deterministic child ID: derived from the replicated range map, never
	// from the local AllocID counter (different meta replicas could disagree).
	newID := maxID + 1

	errs := callReplicas(ctx, old.Replicas, func(ctx context.Context, client pbv2.KVServiceClient) error {
		resp, err := client.SplitRange(ctx, &pbv2.SplitRangeRequest{
			RangeId:    old.ID,
			SplitKey:   splitKey,
			NewRangeId: newID,
			Epoch:      old.Version,
		})
		if err != nil {
			return err
		}
		return kvSplitError(resp)
	})
	if err := replicaFailures("split", old.Replicas, errs); err != nil {
		return splitFailed("split range %d at %q: %v", rangeID, splitKey, err), nil
	}

	next := make([]meta.Range, 0, len(rows)+1)
	for i := range rows {
		r := rows[i]
		if r.ID != rangeID {
			next = append(next, r)
			continue
		}
		left := r
		left.End = cloneBytes(splitKey)
		left.Version = old.Version + 1
		right := meta.Range{
			ID:       newID,
			Start:    cloneBytes(splitKey),
			End:      cloneBytes(r.End),
			Replicas: append([]string(nil), r.Replicas...),
			Version:  old.Version + 1,
		}
		next = append(next, left, right)
	}
	if err := s.ApplyRanges(ctx, next); err != nil {
		return splitFailed("split range %d at %q: apply range map: %v", rangeID, splitKey, err), nil
	}
	if s.log != nil {
		s.log.Info("range split applied",
			"range", rangeID, "new_range", newID, "split_key", fmt.Sprintf("%q", splitKey),
			"version", old.Version+1, "replicas", len(old.Replicas))
	}
	return &pbv2.MetaSplitRangeResponse{Success: true, NewRangeId: newID}, nil
}

// MergeRanges implements pbv2.MetaServiceServer.
//
// The left and right range descriptors are read from a fresh snapshot and must
// be adjacent with identical replica sets. Every replica of that set is asked,
// concurrently, to merge the right Raft group into the left one; only when all
// replicas succeed is the map replaced through ApplyRanges. The left range
// keeps its ID, absorbs right.End, gets Version=left.Version+1, and the right
// range is removed.
//
// Merging is idempotent only at the range-map level: if the right range is
// already absent the call succeeds without contacting replicas. Replicas that
// have not performed the merge yet are handled by the retry; replicas that
// already merged cannot be re-driven because KVService has no merge-completion
// query and reports the missing right range as an error (known limitation).
func (s *Server) MergeRanges(ctx context.Context, req *pbv2.MetaMergeRangesRequest) (*pbv2.MetaMergeRangesResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	schedulerMu.Lock()
	defer schedulerMu.Unlock()

	if err := s.requireLeader(ctx); err != nil {
		return nil, err
	}
	if req == nil {
		return mergeFailed("merge: missing request"), nil
	}

	leftID := req.GetLeftRangeId()
	rightID := req.GetRightRangeId()

	rows := s.cluster.Snapshot()
	byID, _ := indexRanges(rows)

	left, ok := byID[leftID]
	if !ok {
		return mergeFailed("merge: left range %d: range not found", leftID), nil
	}
	right, ok := byID[rightID]
	if !ok {
		// Idempotency: the right range has already been absorbed.
		return &pbv2.MetaMergeRangesResponse{Success: true}, nil
	}
	if leftID == rightID {
		return mergeFailed("merge: cannot merge range %d with itself", leftID), nil
	}
	if !adjacent(left, right) {
		return mergeFailed("merge: ranges %d [%q, %q) and %d [%q, %q) are not adjacent",
			leftID, left.Start, left.End, rightID, right.Start, right.End), nil
	}
	if !slices.Equal(left.Replicas, right.Replicas) {
		return mergeFailed("merge: ranges %d and %d have different replicas (%v vs %v)",
			leftID, rightID, left.Replicas, right.Replicas), nil
	}
	if len(left.Replicas) == 0 {
		return mergeFailed("merge: range %d has no replicas", leftID), nil
	}

	errs := callReplicas(ctx, left.Replicas, func(ctx context.Context, client pbv2.KVServiceClient) error {
		resp, err := client.MergeRanges(ctx, &pbv2.MergeRangesRequest{
			LeftRangeId:  left.ID,
			RightRangeId: right.ID,
			Epoch:        left.Version,
		})
		if err != nil {
			return err
		}
		return kvMergeError(resp)
	})
	if err := replicaFailures("merge", left.Replicas, errs); err != nil {
		return mergeFailed("merge ranges %d and %d: %v", leftID, rightID, err), nil
	}

	next := make([]meta.Range, 0, len(rows)-1)
	for i := range rows {
		r := rows[i]
		switch r.ID {
		case leftID:
			merged := r
			merged.End = cloneBytes(right.End)
			merged.Version = left.Version + 1
			next = append(next, merged)
		case rightID:
			// Dropped: absorbed into left.
		default:
			next = append(next, r)
		}
	}
	if err := s.ApplyRanges(ctx, next); err != nil {
		return mergeFailed("merge ranges %d and %d: apply range map: %v", leftID, rightID, err), nil
	}
	if s.log != nil {
		s.log.Info("range merge applied",
			"left_range", leftID, "right_range", rightID, "end", fmt.Sprintf("%q", right.End),
			"version", left.Version+1, "replicas", len(left.Replicas))
	}
	return &pbv2.MetaMergeRangesResponse{Success: true}, nil
}

// indexRanges maps range IDs to descriptors and returns the largest ID in the
// snapshot. Split children are named maxID+1 so that the choice is a pure
// function of replicated state.
func indexRanges(rows []meta.Range) (map[uint64]meta.Range, uint64) {
	byID := make(map[uint64]meta.Range, len(rows))
	var maxID uint64
	for i := range rows {
		byID[rows[i].ID] = rows[i]
		if rows[i].ID > maxID {
			maxID = rows[i].ID
		}
	}
	return byID, maxID
}

// splitResult reports whether rows already describe rangeID split at splitKey
// and returns the right range's ID. It only accepts a right-hand sibling with
// the same replicas, so unrelated adjacent ranges do not mask a real error.
func splitResult(rows []meta.Range, rangeID uint64, splitKey []byte) (uint64, bool) {
	if len(splitKey) == 0 {
		return 0, false
	}
	idx := -1
	for i := range rows {
		if rows[i].ID == rangeID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return 0, false
	}
	left := rows[idx]
	if left.End == nil || !bytes.Equal(left.End, splitKey) {
		return 0, false
	}
	if idx+1 >= len(rows) {
		return 0, false
	}
	right := rows[idx+1]
	if right.Start == nil || !bytes.Equal(right.Start, splitKey) {
		return 0, false
	}
	if !slices.Equal(left.Replicas, right.Replicas) {
		return 0, false
	}
	return right.ID, true
}

// strictlyInside reports whether key is inside [start, end), where nil bounds
// mean -inf/+inf. The lower bound is exclusive for split purposes: splitting at
// start would create an empty left range.
func strictlyInside(key, start, end []byte) bool {
	if len(key) == 0 {
		return false
	}
	if bytes.Compare(key, start) <= 0 {
		return false
	}
	if end != nil && bytes.Compare(key, end) >= 0 {
		return false
	}
	return true
}

// adjacent reports whether right picks up exactly where left stops. A nil
// bound means ±inf and is never a shared boundary between two ranges.
func adjacent(left, right meta.Range) bool {
	if left.End == nil || right.Start == nil {
		return false
	}
	return bytes.Equal(left.End, right.Start)
}

// callReplicas invokes call on every replica concurrently. Each replica gets
// its own short-lived connection (insecure credentials, as the rest of the v2
// data plane uses) and its own 10s timeout derived from ctx. It always waits
// for all calls, and the returned slice is indexed like replicas.
func callReplicas(ctx context.Context, replicas []string, call func(context.Context, pbv2.KVServiceClient) error) []error {
	errs := make([]error, len(replicas))
	var wg sync.WaitGroup
	wg.Add(len(replicas))
	for i := range replicas {
		go func(i int) {
			defer wg.Done()
			addr := replicas[i]
			cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				errs[i] = fmt.Errorf("dial %s: %w", addr, err)
				return
			}
			defer cc.Close()

			callCtx, cancel := context.WithTimeout(ctx, schedulerRPCTimeout)
			defer cancel()
			if err := call(callCtx, pbv2.NewKVServiceClient(cc)); err != nil {
				errs[i] = fmt.Errorf("%s: %w", addr, err)
				return
			}
			errs[i] = nil
		}(i)
	}
	wg.Wait()
	return errs
}

// replicaFailures combines per-replica errors into one descriptive error.
func replicaFailures(op string, replicas []string, errs []error) error {
	failures := make([]string, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("%s failed on %d/%d replicas: %s",
		op, len(failures), len(replicas), strings.Join(failures, "; "))
}

func kvSplitError(resp *pbv2.SplitRangeResponse) error {
	if resp == nil {
		return fmt.Errorf("empty split response")
	}
	if resp.GetSuccess() {
		return nil
	}
	msg := resp.GetError()
	if msg == "" {
		msg = "unspecified failure"
	}
	if leader := resp.GetLeader(); leader != "" {
		return fmt.Errorf("split rejected: %s (leader=%s)", msg, leader)
	}
	return fmt.Errorf("split rejected: %s", msg)
}

func kvMergeError(resp *pbv2.MergeRangesResponse) error {
	if resp == nil {
		return fmt.Errorf("empty merge response")
	}
	if resp.GetSuccess() {
		return nil
	}
	msg := resp.GetError()
	if msg == "" {
		msg = "unspecified failure"
	}
	if leader := resp.GetLeader(); leader != "" {
		return fmt.Errorf("merge rejected: %s (leader=%s)", msg, leader)
	}
	return fmt.Errorf("merge rejected: %s", msg)
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func splitFailed(format string, args ...any) *pbv2.MetaSplitRangeResponse {
	return &pbv2.MetaSplitRangeResponse{Error: fmt.Sprintf(format, args...)}
}

func mergeFailed(format string, args ...any) *pbv2.MetaMergeRangesResponse {
	return &pbv2.MetaMergeRangesResponse{Error: fmt.Sprintf(format, args...)}
}
