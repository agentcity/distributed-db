package backup

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	pbv2 "distributed-db/internal/pb/v2"
)

// RangeLister reads the cluster range map. Backup only needs this side.
type RangeLister interface {
	ListRanges(ctx context.Context) ([]Range, error)
}

// RangeAdmin can also replace the range map. Restore uses it when the target
// cluster uses a different layout and EnsureRanges was requested.
type RangeAdmin interface {
	RangeLister
	SetRanges(ctx context.Context, ranges []Range) error
}

// MetaClient is a small MetaService client with leader discovery. The v2
// client does not expose ListRanges/SetRanges, so backup talks to the same
// MetaService directly without changing proto or v2client.
//
// Calls are serialized; ListRanges/SetRanges are not on a hot path.
type MetaClient struct {
	mu      sync.Mutex
	seeds   []string
	conns   map[string]*grpc.ClientConn
	clients map[string]pbv2.MetaServiceClient
	leader  string
	closed  bool

	// creds secures every meta connection. It is never nil: OpenMeta uses
	// plaintext credentials and OpenMetaWithOptions builds TLS/mTLS ones.
	creds credentials.TransportCredentials
	// adminToken is attached as a Bearer credential to SetRanges only. It is
	// never logged and never included in an error.
	adminToken string
}

// OpenMeta creates a plaintext client without an admin token over the given
// meta service addresses. At least one address is required. Addresses are
// tried in order until the leader answers.
//
// Use OpenMetaWithOptions for a TLS/mTLS cluster or a meta server that
// requires the admin token.
func OpenMeta(seeds []string) (*MetaClient, error) {
	return newMetaClient(seeds, insecure.NewCredentials(), "")
}

// Close closes every meta connection.
func (m *MetaClient) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	var errs []error
	for _, cc := range m.conns {
		if cc != nil {
			errs = append(errs, cc.Close())
		}
	}
	m.conns = nil
	m.clients = nil
	return errors.Join(errs...)
}

func (m *MetaClient) clientLocked(addr string) (pbv2.MetaServiceClient, error) {
	if m.closed {
		return nil, errors.New("backup: meta client is closed")
	}
	if c, ok := m.clients[addr]; ok {
		return c, nil
	}
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(m.creds))
	if err != nil {
		return nil, fmt.Errorf("backup: dial meta %s: %w", addr, err)
	}
	c := pbv2.NewMetaServiceClient(cc)
	m.conns[addr] = cc
	m.clients[addr] = c
	return c, nil
}

// candidateAddrsLocked asks every known address for the current meta leader
// and returns a deduplicated probe order with the reported leader first.
func (m *MetaClient) candidateAddrsLocked(ctx context.Context) []string {
	base := make([]string, 0, len(m.seeds)+1)
	if m.leader != "" {
		base = append(base, m.leader)
	}
	base = append(base, m.seeds...)

	var out []string
	seen := make(map[string]struct{}, len(base))
	add := func(addr string) {
		if addr == "" {
			return
		}
		if _, ok := seen[addr]; ok {
			return
		}
		seen[addr] = struct{}{}
		out = append(out, addr)
	}
	for _, addr := range base {
		c, err := m.clientLocked(addr)
		if err != nil {
			add(addr)
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		resp, err := c.Leader(probeCtx, &pbv2.LeaderRequest{})
		cancel()
		if err == nil {
			if leader := resp.GetLeader(); leader != "" {
				add(leader)
				m.leader = leader
			}
		}
		add(addr)
	}
	return out
}

func (m *MetaClient) forEachCandidate(ctx context.Context, op func(context.Context, pbv2.MetaServiceClient) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("backup: meta client is closed")
	}
	addrs := m.candidateAddrsLocked(ctx)
	var errs []error
	for _, addr := range addrs {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		c, err := m.clientLocked(addr)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = op(attemptCtx, c)
		cancel()
		if err == nil {
			m.leader = addr
			return nil
		}
		errs = append(errs, fmt.Errorf("meta %s: %w", addr, err))
	}
	if len(errs) == 0 {
		return errors.New("backup: no meta address answered")
	}
	return fmt.Errorf("backup: meta request failed: %w", errors.Join(errs...))
}

// ListRanges returns the current range map in ascending key order.
func (m *MetaClient) ListRanges(ctx context.Context) ([]Range, error) {
	var out []Range
	err := m.forEachCandidate(ctx, func(ctx context.Context, c pbv2.MetaServiceClient) error {
		resp, err := c.ListRanges(ctx, &pbv2.ListRangesRequest{})
		if err != nil {
			return err
		}
		ranges := make([]Range, 0, len(resp.GetRanges()))
		for _, r := range resp.GetRanges() {
			if r == nil {
				return errors.New("nil range in ListRanges response")
			}
			ranges = append(ranges, rangeFromProto(r))
		}
		out = ranges
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortRanges(out)
	if err := validateRangeList(out); err != nil {
		return nil, fmt.Errorf("%w: meta range map: %v", ErrRangeMismatch, err)
	}
	return out, nil
}

// SetRanges replaces the whole meta range map. The caller is responsible for
// making sure the target KV nodes host the same ranges; this only updates
// metadata.
func (m *MetaClient) SetRanges(ctx context.Context, ranges []Range) error {
	if err := validateRangeList(ranges); err != nil {
		return fmt.Errorf("%w: refusing to set invalid range map: %v", ErrRangeMismatch, err)
	}
	req := &pbv2.SetRangesRequest{Ranges: make([]*pbv2.RangeInfo, 0, len(ranges))}
	for _, r := range ranges {
		req.Ranges = append(req.Ranges, rangeToProto(r))
	}
	return m.forEachCandidate(ctx, func(callCtx context.Context, c pbv2.MetaServiceClient) error {
		resp, err := c.SetRanges(m.adminCallContext(callCtx), req)
		if err != nil {
			return err
		}
		if !resp.GetSuccess() {
			if msg := resp.GetError(); msg != "" {
				return errors.New(msg)
			}
			return errors.New("SetRanges was not successful")
		}
		return nil
	})
}

func rangeFromProto(r *pbv2.RangeInfo) Range {
	return Range{
		ID:       r.GetId(),
		Start:    cloneBytes(r.GetStart()),
		End:      cloneBytes(r.GetEnd()),
		Replicas: append([]string(nil), r.GetReplicas()...),
		Leader:   r.GetLeader(),
		Version:  r.GetVersion(),
	}
}

func rangeToProto(r Range) *pbv2.RangeInfo {
	return &pbv2.RangeInfo{
		Id: r.ID, Start: cloneBytes(r.Start), End: cloneBytes(r.End),
		Replicas: append([]string(nil), r.Replicas...),
		Leader:   r.Leader, Version: r.Version,
	}
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}
