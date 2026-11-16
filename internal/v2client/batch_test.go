package v2client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	pbv2 "distributed-db/internal/pb/v2"
)

// batchTestKV is a scripted single-range KV node for CommitBatch tests. It
// stores committed values in memory so visibility can be checked through the
// normal Txn.Get path.
type batchTestKV struct {
	pbv2.KVServiceClient // unimplemented methods panic if used

	mu   sync.Mutex
	vals map[string]string

	commitSingleFn func(context.Context, *pbv2.CommitSingleRequest) (*pbv2.CommitSingleResponse, error)

	commitCalls int
	checkCalls  int
	getCalls    int
}

func newBatchTestKV() *batchTestKV {
	return &batchTestKV{vals: make(map[string]string)}
}

func (f *batchTestKV) setCommitSingleFn(fn func(context.Context, *pbv2.CommitSingleRequest) (*pbv2.CommitSingleResponse, error)) {
	f.mu.Lock()
	f.commitSingleFn = fn
	f.mu.Unlock()
}

func (f *batchTestKV) applyCommit(req *pbv2.CommitSingleRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range req.GetMutations() {
		key := string(m.GetKey())
		if m.GetDelete() {
			delete(f.vals, key)
			continue
		}
		f.vals[key] = string(m.GetValue())
	}
}

func (f *batchTestKV) value(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vals[key]
	return v, ok
}

func (f *batchTestKV) commitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commitCalls
}

func (f *batchTestKV) Status(context.Context, *pbv2.StatusRequest, ...grpc.CallOption) (*pbv2.StatusResponse, error) {
	return &pbv2.StatusResponse{Leader: "kv1", Role: "leader"}, nil
}

func (f *batchTestKV) CommitSingle(ctx context.Context, req *pbv2.CommitSingleRequest, _ ...grpc.CallOption) (*pbv2.CommitSingleResponse, error) {
	f.mu.Lock()
	f.commitCalls++
	fn := f.commitSingleFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	f.applyCommit(req)
	return &pbv2.CommitSingleResponse{Success: true, Leader: "kv1"}, nil
}

func (f *batchTestKV) CheckTxnStatus(context.Context, *pbv2.CheckTxnStatusRequest, ...grpc.CallOption) (*pbv2.CheckTxnStatusResponse, error) {
	f.mu.Lock()
	f.checkCalls++
	f.mu.Unlock()
	return &pbv2.CheckTxnStatusResponse{Leader: "kv1"}, nil
}

func (f *batchTestKV) Get(_ context.Context, req *pbv2.GetRequest, _ ...grpc.CallOption) (*pbv2.GetResponse, error) {
	f.mu.Lock()
	f.getCalls++
	v, ok := f.vals[string(req.GetKey())]
	f.mu.Unlock()
	if !ok {
		return &pbv2.GetResponse{Found: false, Leader: "kv1"}, nil
	}
	return &pbv2.GetResponse{Found: true, Value: []byte(v), Leader: "kv1"}, nil
}

// newBatchTestClient wires one fake KV node and a meta service that maps every
// key to range 1.
func newBatchTestClient(t *testing.T) (*Client, *batchTestKV) {
	t.Helper()
	meta := newFakeMeta("meta1")
	meta.setGetRange(func([]byte) (*pbv2.RangeInfo, error) {
		return &pbv2.RangeInfo{
			Id: 1, Version: 1, Replicas: []string{"kv1"}, Leader: "kv1",
		}, nil
	})
	c := newTestClient(meta)
	kv := newBatchTestKV()
	c.clients["kv1"] = kv
	c.leaders[1] = leaderEntry{addr: "kv1", until: time.Now().Add(time.Hour)}
	return c, kv
}

func batchKey(prefix string, i int) string {
	return fmt.Sprintf("%s/%03d", prefix, i)
}

// beginBatchTxns creates n single-key transactions. Begin allocates startTS
// before any of them commits, matching the intended independent-transaction
// workload.
func beginBatchTxns(t *testing.T, c *Client, prefix string, n int) []*Txn {
	t.Helper()
	ctx := context.Background()
	txns := make([]*Txn, n)
	for i := range txns {
		txn, err := c.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin(%d): %v", i, err)
		}
		txn.Set([]byte(batchKey(prefix, i)), []byte("value"))
		txns[i] = txn
	}
	return txns
}

func readBatchValue(t *testing.T, c *Client, key string) (string, error) {
	t.Helper()
	ctx := context.Background()
	txn, err := c.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin for read: %v", err)
	}
	got, err := txn.Get(ctx, []byte(key))
	if err != nil {
		return "", err
	}
	return string(got), nil
}

func notePeak(peak *atomic.Int64, v int64) {
	for {
		old := peak.Load()
		if v <= old || peak.CompareAndSwap(old, v) {
			return
		}
	}
}

func TestV2CommitBatchAllSucceedAndVisible(t *testing.T) {
	c, kv := newBatchTestClient(t)
	const n = 24
	txns := beginBatchTxns(t, c, "all", n)

	errs := c.CommitBatch(context.Background(), txns)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("CommitBatch slot %d = %v, want nil", i, err)
		}
	}
	for i := 0; i < n; i++ {
		key := batchKey("all", i)
		got, err := readBatchValue(t, c, key)
		if err != nil {
			t.Fatalf("Get(%q) after batch commit: %v", key, err)
		}
		if got != "value" {
			t.Fatalf("Get(%q) = %q, want value", key, got)
		}
	}
	if got := kv.commitCount(); got != n {
		t.Fatalf("CommitSingle calls = %d, want %d", got, n)
	}
}

func TestV2CommitBatchFailureIsIsolated(t *testing.T) {
	c, kv := newBatchTestClient(t)
	const n = 6
	failedIndex := 2
	failedKey := batchKey("isolated", failedIndex)
	txns := beginBatchTxns(t, c, "isolated", n)

	kv.setCommitSingleFn(func(_ context.Context, req *pbv2.CommitSingleRequest) (*pbv2.CommitSingleResponse, error) {
		if string(req.GetMutations()[0].GetKey()) == failedKey {
			return &pbv2.CommitSingleResponse{Error: "write conflict", Leader: "kv1"}, nil
		}
		kv.applyCommit(req)
		return &pbv2.CommitSingleResponse{Success: true, Leader: "kv1"}, nil
	})

	errs := c.CommitBatch(context.Background(), txns)
	for i, err := range errs {
		if i == failedIndex {
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("slot %d error = %v, want ErrConflict", i, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("slot %d = %v, want nil (a failed sibling must not abort others)", i, err)
		}
	}
	for i := 0; i < n; i++ {
		key := batchKey("isolated", i)
		_, ok := kv.value(key)
		if i == failedIndex {
			if ok {
				t.Fatalf("failed key %q was committed", key)
			}
			continue
		}
		if !ok {
			t.Fatalf("key %q from a successful sibling is not visible after the batch", key)
		}
	}
}

func TestV2CommitBatchPreservesInputOrder(t *testing.T) {
	c, kv := newBatchTestClient(t)
	const n = 6
	txns := beginBatchTxns(t, c, "order", n)

	// The transaction in slot 0 may only complete after slot 1 has completed,
	// so completion order is the reverse of input order for the first two
	// slots. The error in slot 1 must still land in slot 1.
	firstDone := make(chan struct{})
	var firstOnce sync.Once
	kv.setCommitSingleFn(func(_ context.Context, req *pbv2.CommitSingleRequest) (*pbv2.CommitSingleResponse, error) {
		switch string(req.GetMutations()[0].GetKey()) {
		case batchKey("order", 0):
			<-firstDone
		case batchKey("order", 1):
			firstOnce.Do(func() { close(firstDone) })
			return &pbv2.CommitSingleResponse{Error: "write conflict", Leader: "kv1"}, nil
		}
		kv.applyCommit(req)
		return &pbv2.CommitSingleResponse{Success: true, Leader: "kv1"}, nil
	})

	errs := c.CommitBatchWithLimit(context.Background(), txns, 2)
	if errs[0] != nil {
		t.Fatalf("slot 0 = %v, want nil even though it completed last", errs[0])
	}
	if !errors.Is(errs[1], ErrConflict) {
		t.Fatalf("slot 1 = %v, want ErrConflict", errs[1])
	}
	for i := 2; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("slot %d = %v, want nil", i, errs[i])
		}
	}
	if len(errs) != len(txns) {
		t.Fatalf("len(errs) = %d, want %d", len(errs), len(txns))
	}
}

func TestV2CommitBatchConcurrencyLimit(t *testing.T) {
	const n = 12
	for _, tc := range []struct {
		name  string
		limit int // 0 means CommitBatch's default limit
	}{
		{name: "default", limit: 0},
		{name: "limit-1", limit: 1},
		{name: "limit-3", limit: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, kv := newBatchTestClient(t)
			txns := beginBatchTxns(t, c, "limit-"+tc.name, n)

			want := tc.limit
			if want == 0 {
				want = DefaultCommitBatchConcurrency
				if want > n {
					want = n
				}
			}

			started := make(chan struct{}, n)
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(releaseAll)

			var active, peak atomic.Int64
			kv.setCommitSingleFn(func(_ context.Context, req *pbv2.CommitSingleRequest) (*pbv2.CommitSingleResponse, error) {
				cur := active.Add(1)
				notePeak(&peak, cur)
				started <- struct{}{}
				<-release
				active.Add(-1)
				kv.applyCommit(req)
				return &pbv2.CommitSingleResponse{Success: true, Leader: "kv1"}, nil
			})

			results := make(chan []error, 1)
			go func() {
				if tc.limit == 0 {
					results <- c.CommitBatch(context.Background(), txns)
					return
				}
				results <- c.CommitBatchWithLimit(context.Background(), txns, tc.limit)
			}()

			for i := 0; i < want; i++ {
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					releaseAll()
					t.Fatalf("only %d/%d transactions reached CommitSingle", i, want)
				}
			}
			// While all workers are blocked, no additional transaction may
			// enter the commit path: that would prove the limit is a lie.
			select {
			case <-started:
				releaseAll()
				t.Fatalf("more than %d transactions committed concurrently", want)
			case <-time.After(100 * time.Millisecond):
			}
			if got := active.Load(); got != int64(want) {
				releaseAll()
				t.Fatalf("active commits = %d, want %d", got, want)
			}
			if got := peak.Load(); got > int64(want) {
				releaseAll()
				t.Fatalf("peak concurrent commits = %d, want <= %d", got, want)
			}

			releaseAll()
			select {
			case errs := <-results:
				for i, err := range errs {
					if err != nil {
						t.Fatalf("slot %d = %v, want nil", i, err)
					}
				}
			case <-time.After(5 * time.Second):
				t.Fatal("CommitBatch did not return after releasing commits")
			}
			if got := kv.commitCount(); got != n {
				t.Fatalf("CommitSingle calls = %d, want %d", got, n)
			}
		})
	}
}

func TestV2CommitBatchContextCancellation(t *testing.T) {
	t.Run("before-start", func(t *testing.T) {
		c, kv := newBatchTestClient(t)
		txns := beginBatchTxns(t, c, "cancel-before", 5)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		errs := c.CommitBatchWithLimit(ctx, txns, 2)
		for i, err := range errs {
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("slot %d = %v, want context.Canceled", i, err)
			}
		}
		if got := kv.commitCount(); got != 0 {
			t.Fatalf("CommitSingle calls = %d, want 0 for a pre-cancelled batch", got)
		}
	})

	t.Run("mid-flight", func(t *testing.T) {
		c, kv := newBatchTestClient(t)
		const n = 6
		const limit = 2
		txns := beginBatchTxns(t, c, "cancel-mid", n)

		started := make(chan struct{}, limit)
		kv.setCommitSingleFn(func(ctx context.Context, _ *pbv2.CommitSingleRequest) (*pbv2.CommitSingleResponse, error) {
			started <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		})

		ctx, cancel := context.WithCancel(context.Background())
		results := make(chan []error, 1)
		go func() { results <- c.CommitBatchWithLimit(ctx, txns, limit) }()

		for i := 0; i < limit; i++ {
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				cancel()
				t.Fatalf("only %d/%d transactions started", i, limit)
			}
		}
		cancel()

		select {
		case errs := <-results:
			for i, err := range errs {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("slot %d = %v, want context.Canceled", i, err)
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatal("CommitBatch did not return after cancellation")
		}
	})
}

func TestV2CommitBatchFinishedAndNilTransactions(t *testing.T) {
	c, kv := newBatchTestClient(t)
	ctx := context.Background()

	txns := beginBatchTxns(t, c, "finished", 1)
	if errs := c.CommitBatch(ctx, txns); errs[0] != nil {
		t.Fatalf("first CommitBatch = %v, want nil", errs[0])
	}
	errs := c.CommitBatch(ctx, txns)
	if errs[0] == nil {
		t.Fatal("second CommitBatch of a finished transaction succeeded, want an error")
	}
	if !strings.Contains(errs[0].Error(), "transaction is finished") {
		t.Fatalf("second CommitBatch error = %v, want transaction is finished", errs[0])
	}
	if got := kv.commitCount(); got != 1 {
		t.Fatalf("CommitSingle calls = %d, want 1 (finished txn must not commit again)", got)
	}

	errs = c.CommitBatch(ctx, []*Txn{nil})
	if errs[0] == nil {
		t.Fatal("CommitBatch with a nil transaction returned nil error")
	}
	if !errors.Is(errs[0], errNilBatchTxn) {
		t.Fatalf("nil transaction error = %v, want errNilBatchTxn", errs[0])
	}

	if got := c.CommitBatch(ctx, nil); len(got) != 0 {
		t.Fatalf("empty batch returned %d errors, want 0", len(got))
	}
}

func TestV2CommitBatchRejectsDuplicateTransaction(t *testing.T) {
	c, kv := newBatchTestClient(t)
	txns := beginBatchTxns(t, c, "duplicate", 1)

	errs := c.CommitBatch(context.Background(), []*Txn{txns[0], txns[0]})
	for i, err := range errs {
		if !errors.Is(err, errDuplicateBatchTxn) {
			t.Fatalf("slot %d = %v, want errDuplicateBatchTxn", i, err)
		}
	}
	if got := kv.commitCount(); got != 0 {
		t.Fatalf("CommitSingle calls = %d, want 0 for a duplicated transaction", got)
	}
}

// TestV2CommitBatchConcurrentBatchesRace runs several CommitBatch calls at the
// same time. It is meaningful under -race: each batch only touches its own
// transactions, while all of them share one Client.
func TestV2CommitBatchConcurrentBatchesRace(t *testing.T) {
	c, kv := newBatchTestClient(t)
	const (
		batches = 4
		per     = 8
	)
	all := make([][]*Txn, batches)
	for b := 0; b < batches; b++ {
		all[b] = beginBatchTxns(t, c, fmt.Sprintf("race-%d", b), per)
	}
	kv.setCommitSingleFn(func(_ context.Context, req *pbv2.CommitSingleRequest) (*pbv2.CommitSingleResponse, error) {
		time.Sleep(time.Millisecond)
		kv.applyCommit(req)
		return &pbv2.CommitSingleResponse{Success: true, Leader: "kv1"}, nil
	})

	var wg sync.WaitGroup
	errCh := make(chan error, batches)
	for b := 0; b < batches; b++ {
		wg.Add(1)
		go func(b int) {
			defer wg.Done()
			for i, err := range c.CommitBatch(context.Background(), all[b]) {
				if err != nil {
					errCh <- fmt.Errorf("batch %d slot %d: %w", b, i, err)
					return
				}
			}
		}(b)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	for b := 0; b < batches; b++ {
		for i := 0; i < per; i++ {
			if _, ok := kv.value(batchKey(fmt.Sprintf("race-%d", b), i)); !ok {
				t.Fatalf("batch %d key %d is not visible", b, i)
			}
		}
	}
}
