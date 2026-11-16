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
	leaderCalls int
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
	leader, err := f.leader, f.leaderErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &pbv2.LeaderResponse{Leader: leader}, nil
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
