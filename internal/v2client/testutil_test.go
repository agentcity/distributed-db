package v2client

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pbv2 "distributed-db/internal/pb/v2"
)

// fakeMeta is an in-memory metaAPI with knobs for TSO, range, and leader
// behavior. It is safe for concurrent use.
type fakeMeta struct {
	mu sync.Mutex

	addr   string
	leader string
	next   uint64

	allocDelay time.Duration
	allocErr   error
	failAllocs int
	allocs     int
	allocSizes []int32

	getRangeFn    func(key []byte) (*pbv2.RangeInfo, error)
	getRangeCalls int

	leaderErr   error
	leaderTerm  uint64
	leaderCalls int

	listRangesFn  func() ([]*pbv2.RangeInfo, error)
	listRangesN   int
	setRangesFn   func(*pbv2.SetRangesRequest) (*pbv2.SetRangesResponse, error)
	setRangesReqs []*pbv2.SetRangesRequest
	splitRangeFn  func(*pbv2.MetaSplitRangeRequest) (*pbv2.MetaSplitRangeResponse, error)
	splitRequests []*pbv2.MetaSplitRangeRequest
	mergeRangesFn func(*pbv2.MetaMergeRangesRequest) (*pbv2.MetaMergeRangesResponse, error)
	mergeRequests []*pbv2.MetaMergeRangesRequest
}

var timeZero time.Time

func newFakeMeta(addr string) *fakeMeta {
	return &fakeMeta{addr: addr, leader: addr, next: 1}
}

func (f *fakeMeta) setLeader(leader string) {
	f.mu.Lock()
	f.leader = leader
	f.mu.Unlock()
}

func (f *fakeMeta) setGetRange(fn func([]byte) (*pbv2.RangeInfo, error)) {
	f.mu.Lock()
	f.getRangeFn = fn
	f.mu.Unlock()
}

func (f *fakeMeta) setNext(next uint64) {
	f.mu.Lock()
	f.next = next
	f.mu.Unlock()
}

func (f *fakeMeta) setLeaderErr(err error) {
	f.mu.Lock()
	f.leaderErr = err
	f.mu.Unlock()
}

func (f *fakeMeta) setLeaderInfo(leader string, term uint64) {
	f.mu.Lock()
	f.leader = leader
	f.leaderTerm = term
	f.mu.Unlock()
}

func (f *fakeMeta) setListRanges(fn func() ([]*pbv2.RangeInfo, error)) {
	f.mu.Lock()
	f.listRangesFn = fn
	f.mu.Unlock()
}

func (f *fakeMeta) setSetRanges(fn func(*pbv2.SetRangesRequest) (*pbv2.SetRangesResponse, error)) {
	f.mu.Lock()
	f.setRangesFn = fn
	f.mu.Unlock()
}

func (f *fakeMeta) setSplitRange(fn func(*pbv2.MetaSplitRangeRequest) (*pbv2.MetaSplitRangeResponse, error)) {
	f.mu.Lock()
	f.splitRangeFn = fn
	f.mu.Unlock()
}

func (f *fakeMeta) setMergeRanges(fn func(*pbv2.MetaMergeRangesRequest) (*pbv2.MetaMergeRangesResponse, error)) {
	f.mu.Lock()
	f.mergeRangesFn = fn
	f.mu.Unlock()
}

func (f *fakeMeta) adminStats() (listCalls int, splits []*pbv2.MetaSplitRangeRequest, merges []*pbv2.MetaMergeRangesRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listRangesN,
		append([]*pbv2.MetaSplitRangeRequest(nil), f.splitRequests...),
		append([]*pbv2.MetaMergeRangesRequest(nil), f.mergeRequests...)
}

func (f *fakeMeta) setAllocErr(err error) {
	f.mu.Lock()
	f.allocErr = err
	f.mu.Unlock()
}

func (f *fakeMeta) setFailAllocs(n int) {
	f.mu.Lock()
	f.failAllocs = n
	f.mu.Unlock()
}

func (f *fakeMeta) stats() (allocs int, sizes []int32, getRangeCalls, leaderCalls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.allocs, append([]int32(nil), f.allocSizes...), f.getRangeCalls, f.leaderCalls
}

func (f *fakeMeta) AllocTS(_ context.Context, req *pbv2.AllocTSRequest, _ ...grpc.CallOption) (*pbv2.AllocTSResponse, error) {
	f.mu.Lock()
	delay := f.allocDelay
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.allocs++
	f.allocSizes = append(f.allocSizes, req.GetCount())
	if f.allocErr != nil {
		return nil, f.allocErr
	}
	if f.failAllocs > 0 {
		f.failAllocs--
		return nil, status.Error(codes.Unavailable, "fake meta is failing")
	}
	n := int(req.GetCount())
	if n <= 0 {
		n = 1
	}
	start := f.next
	if start == 0 {
		start = 1
	}
	f.next = start + uint64(n)
	return &pbv2.AllocTSResponse{Start: start, Count: int32(n)}, nil
}

func (f *fakeMeta) GetRange(_ context.Context, req *pbv2.GetRangeRequest, _ ...grpc.CallOption) (*pbv2.RangeInfo, error) {
	f.mu.Lock()
	f.getRangeCalls++
	fn := f.getRangeFn
	f.mu.Unlock()
	if fn != nil {
		return fn(req.GetKey())
	}
	return &pbv2.RangeInfo{Id: 1, Version: 1}, nil
}

func (f *fakeMeta) Leader(context.Context, *pbv2.LeaderRequest, ...grpc.CallOption) (*pbv2.LeaderResponse, error) {
	f.mu.Lock()
	f.leaderCalls++
	leader, term, err := f.leader, f.leaderTerm, f.leaderErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &pbv2.LeaderResponse{Leader: leader, Term: term}, nil
}

func (f *fakeMeta) ListRanges(context.Context, *pbv2.ListRangesRequest, ...grpc.CallOption) (*pbv2.ListRangesResponse, error) {
	f.mu.Lock()
	f.listRangesN++
	fn := f.listRangesFn
	f.mu.Unlock()
	if fn != nil {
		ranges, err := fn()
		if err != nil {
			return nil, err
		}
		return &pbv2.ListRangesResponse{Ranges: ranges}, nil
	}
	return &pbv2.ListRangesResponse{}, nil
}

func (f *fakeMeta) SetRanges(_ context.Context, req *pbv2.SetRangesRequest, _ ...grpc.CallOption) (*pbv2.SetRangesResponse, error) {
	f.mu.Lock()
	f.setRangesReqs = append(f.setRangesReqs, req)
	fn := f.setRangesFn
	f.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return &pbv2.SetRangesResponse{Success: true}, nil
}

func (f *fakeMeta) SplitRange(_ context.Context, req *pbv2.MetaSplitRangeRequest, _ ...grpc.CallOption) (*pbv2.MetaSplitRangeResponse, error) {
	f.mu.Lock()
	f.splitRequests = append(f.splitRequests, req)
	fn := f.splitRangeFn
	f.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return &pbv2.MetaSplitRangeResponse{Success: true, NewRangeId: req.GetRangeId() + 1}, nil
}

func (f *fakeMeta) MergeRanges(_ context.Context, req *pbv2.MetaMergeRangesRequest, _ ...grpc.CallOption) (*pbv2.MetaMergeRangesResponse, error) {
	f.mu.Lock()
	f.mergeRequests = append(f.mergeRequests, req)
	fn := f.mergeRangesFn
	f.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return &pbv2.MetaMergeRangesResponse{Success: true}, nil
}

// newTestClient builds a Client wired to an in-memory meta service. The KV
// node set is synthetic; tests that exercise retry install fake KV clients in
// c.clients directly.
func newTestClient(meta metaAPI) *Client {
	const addr = "meta1"
	c := &Client{
		meta:            meta,
		metaAddr:        addr,
		metaSeeds:       []string{addr},
		metaPool:        map[string]*metaEntry{addr: {mc: meta}},
		metaDial:        func(string) (*grpc.ClientConn, metaAPI, error) { return nil, meta, nil },
		addrs:           []string{"kv1"},
		clients:         make(map[string]pbv2.KVServiceClient),
		conns:           make(map[string]*grpc.ClientConn),
		leaders:         make(map[uint64]leaderEntry),
		ranges:          make(map[uint64]*rangeEntry),
		tsBatchSize:     8,
		tsBatchTTL:      time.Second,
		tsIdleCheck:     time.Hour,
		tsValidateEvery: 1 << 62,
		rangeTTL:        time.Second,
		rangeCap:        128,
	}
	c.metaLeaderAddr = addr
	c.metaLeaderUntil = time.Now().Add(time.Hour)
	return c
}
