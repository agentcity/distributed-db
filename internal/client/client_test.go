package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRouter is a concurrency-safe scripted Router. fn receives the 1-based
// call number so tests can return a different leader on each refresh.
type fakeRouter struct {
	mu    sync.Mutex
	calls int
	keys  [][]byte
	fn    func(call int, ctx context.Context, key []byte) (string, error)
}

func (r *fakeRouter) Leader(ctx context.Context, key []byte) (string, error) {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.keys = append(r.keys, cloneBytes(key))
	fn := r.fn
	r.mu.Unlock()

	if fn != nil {
		return fn(call, ctx, key)
	}
	return "node-1", nil
}

func (r *fakeRouter) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type transportCall struct {
	op    string
	addr  string
	key   []byte
	value []byte
	start []byte
	end   []byte
	limit int
}

// fakeTransport records every call and delegates to optional per-operation
// functions. It is safe for concurrent use.
type fakeTransport struct {
	mu     sync.Mutex
	calls  []transportCall
	getFn  func(ctx context.Context, addr string, key []byte) ([]byte, error)
	putFn  func(ctx context.Context, addr string, key, value []byte) error
	delFn  func(ctx context.Context, addr string, key []byte) error
	scanFn func(ctx context.Context, addr string, start, end []byte, limit int) ([]KV, error)
}

func (t *fakeTransport) record(call transportCall) {
	t.mu.Lock()
	t.calls = append(t.calls, call)
	t.mu.Unlock()
}

func (t *fakeTransport) Get(ctx context.Context, addr string, key []byte) ([]byte, error) {
	t.record(transportCall{op: "get", addr: addr, key: cloneBytes(key)})
	t.mu.Lock()
	fn := t.getFn
	t.mu.Unlock()
	if fn != nil {
		return fn(ctx, addr, key)
	}
	return nil, nil
}

func (t *fakeTransport) Put(ctx context.Context, addr string, key, value []byte) error {
	t.record(transportCall{op: "put", addr: addr, key: cloneBytes(key), value: cloneBytes(value)})
	t.mu.Lock()
	fn := t.putFn
	t.mu.Unlock()
	if fn != nil {
		return fn(ctx, addr, key, value)
	}
	return nil
}

func (t *fakeTransport) Delete(ctx context.Context, addr string, key []byte) error {
	t.record(transportCall{op: "delete", addr: addr, key: cloneBytes(key)})
	t.mu.Lock()
	fn := t.delFn
	t.mu.Unlock()
	if fn != nil {
		return fn(ctx, addr, key)
	}
	return nil
}

func (t *fakeTransport) Scan(ctx context.Context, addr string, start, end []byte, limit int) ([]KV, error) {
	t.record(transportCall{op: "scan", addr: addr, start: cloneBytes(start), end: cloneBytes(end), limit: limit})
	t.mu.Lock()
	fn := t.scanFn
	t.mu.Unlock()
	if fn != nil {
		return fn(ctx, addr, start, end, limit)
	}
	return nil, nil
}

func (t *fakeTransport) snapshotCalls() []transportCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]transportCall, len(t.calls))
	copy(out, t.calls)
	return out
}

func (t *fakeTransport) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.calls)
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}

func testOptions() Options {
	return Options{
		MaxRetries: 3,
		Backoff:    time.Millisecond,
		Timeout:    5 * time.Second,
	}
}

func TestGetRetriesNotLeaderThenSucceeds(t *testing.T) {
	t.Parallel()

	router := &fakeRouter{fn: func(call int, _ context.Context, _ []byte) (string, error) {
		if call == 1 {
			return "old-leader", nil
		}
		return "new-leader", nil
	}}
	transport := &fakeTransport{getFn: func(_ context.Context, addr string, _ []byte) ([]byte, error) {
		switch addr {
		case "old-leader":
			return nil, fmt.Errorf("node moved: %w", ErrNotLeader)
		case "new-leader":
			return []byte("value"), nil
		default:
			return nil, fmt.Errorf("unexpected addr %q", addr)
		}
	}}

	c := New(router, transport, testOptions())
	got, err := c.Get(context.Background(), []byte("key-a"))
	if err != nil {
		t.Fatalf("Get() error = %v, want nil", err)
	}
	if string(got) != "value" {
		t.Fatalf("Get() = %q, want %q", got, "value")
	}
	if router.callCount() != 2 {
		t.Fatalf("router calls = %d, want 2 (refresh after not-leader)", router.callCount())
	}
	calls := transport.snapshotCalls()
	if len(calls) != 2 {
		t.Fatalf("transport calls = %d, want 2", len(calls))
	}
	if calls[0].addr != "old-leader" || calls[1].addr != "new-leader" {
		t.Fatalf("transport addresses = %q, %q; want old-leader, new-leader", calls[0].addr, calls[1].addr)
	}
}

func TestLeaderCacheReusedAcrossCalls(t *testing.T) {
	t.Parallel()

	router := &fakeRouter{}
	transport := &fakeTransport{getFn: func(_ context.Context, _ string, key []byte) ([]byte, error) {
		return append([]byte("v:"), key...), nil
	}}
	c := New(router, transport, testOptions())

	for i := 0; i < 3; i++ {
		got, err := c.Get(context.Background(), []byte("cached-key"))
		if err != nil {
			t.Fatalf("Get() #%d error = %v", i, err)
		}
		if string(got) != "v:cached-key" {
			t.Fatalf("Get() #%d = %q", i, got)
		}
	}
	if router.callCount() != 1 {
		t.Fatalf("router calls = %d after repeated key, want 1 (cache hit)", router.callCount())
	}
	if transport.count() != 3 {
		t.Fatalf("transport calls = %d, want 3", transport.count())
	}

	// A key with a different routing prefix must consult the router.
	if _, err := c.Get(context.Background(), []byte("other-key")); err != nil {
		t.Fatalf("Get(other-key) error = %v", err)
	}
	if router.callCount() != 2 {
		t.Fatalf("router calls = %d after a different key, want 2", router.callCount())
	}
}

func TestLeaderCacheInvalidatedOnNotLeader(t *testing.T) {
	t.Parallel()

	router := &fakeRouter{fn: func(call int, _ context.Context, _ []byte) (string, error) {
		if call == 1 {
			return "stale-leader", nil
		}
		return "fresh-leader", nil
	}}
	transport := &fakeTransport{getFn: func(_ context.Context, addr string, _ []byte) ([]byte, error) {
		if addr == "stale-leader" {
			return nil, ErrNotLeader
		}
		return []byte("fresh"), nil
	}}
	c := New(router, transport, testOptions())

	if _, err := c.Get(context.Background(), []byte("key")); err != nil {
		t.Fatalf("first Get() error = %v", err)
	}
	if router.callCount() != 2 {
		t.Fatalf("router calls = %d, want 2", router.callCount())
	}

	// The successful retry cached fresh-leader; the stale address must be gone.
	if _, err := c.Get(context.Background(), []byte("key")); err != nil {
		t.Fatalf("second Get() error = %v", err)
	}
	if router.callCount() != 2 {
		t.Fatalf("router calls = %d after cache reuse, want 2", router.callCount())
	}
	calls := transport.snapshotCalls()
	if len(calls) != 3 {
		t.Fatalf("transport calls = %d, want 3", len(calls))
	}
	for i, call := range calls[1:] {
		if call.addr != "fresh-leader" {
			t.Fatalf("transport call #%d addr = %q, want fresh-leader", i+1, call.addr)
		}
	}
}

func TestMaxRetries(t *testing.T) {
	t.Parallel()

	router := &fakeRouter{}
	transport := &fakeTransport{getFn: func(_ context.Context, _ string, _ []byte) ([]byte, error) {
		return nil, fmt.Errorf("still following: %w", ErrNotLeader)
	}}
	opts := Options{MaxRetries: 2, Backoff: -1, Timeout: time.Second}
	c := New(router, transport, opts)

	_, err := c.Get(context.Background(), []byte("key"))
	if !errors.Is(err, ErrNotLeader) {
		t.Fatalf("Get() error = %v, want ErrNotLeader", err)
	}
	if transport.count() != 3 {
		t.Fatalf("transport calls = %d, want 3 (initial + 2 retries)", transport.count())
	}
	if router.callCount() != 3 {
		t.Fatalf("router calls = %d, want 3 (refresh on every retry)", router.callCount())
	}
	if !strings.Contains(err.Error(), "after 3 attempt") {
		t.Fatalf("Get() error = %q, want attempt count", err)
	}
}

func TestGetRetriesUnavailableThenSucceeds(t *testing.T) {
	t.Parallel()

	router := &fakeRouter{}
	var calls int
	var mu sync.Mutex
	transport := &fakeTransport{getFn: func(_ context.Context, _ string, _ []byte) ([]byte, error) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if call == 1 {
			return nil, fmt.Errorf("temporarily down: %w", ErrUnavailable)
		}
		return []byte("recovered"), nil
	}}
	c := New(router, transport, testOptions())

	got, err := c.Get(context.Background(), []byte("key"))
	if err != nil {
		t.Fatalf("Get() error = %v, want nil", err)
	}
	if string(got) != "recovered" {
		t.Fatalf("Get() = %q, want recovered", got)
	}
	if transport.count() != 2 {
		t.Fatalf("transport calls = %d, want 2", transport.count())
	}
	if router.callCount() != 2 {
		t.Fatalf("router calls = %d, want 2 (refresh after unavailable)", router.callCount())
	}
}

func TestTransportPermanentErrorNotRetried(t *testing.T) {
	t.Parallel()

	boom := errors.New("disk on fire")
	router := &fakeRouter{}
	transport := &fakeTransport{getFn: func(_ context.Context, _ string, _ []byte) ([]byte, error) {
		return nil, boom
	}}
	c := New(router, transport, testOptions())

	_, err := c.Get(context.Background(), []byte("some-key"))
	if !errors.Is(err, boom) {
		t.Fatalf("Get() error = %v, want wrapped transport error", err)
	}
	if transport.count() != 1 {
		t.Fatalf("transport calls = %d, want 1 (permanent errors are not retried)", transport.count())
	}
	if !strings.Contains(err.Error(), `key "some-key"`) {
		t.Fatalf("Get() error = %q, want key context", err)
	}
}

func TestRouterUnavailableRetried(t *testing.T) {
	t.Parallel()

	router := &fakeRouter{fn: func(_ int, _ context.Context, _ []byte) (string, error) {
		return "", fmt.Errorf("meta unavailable: %w", ErrUnavailable)
	}}
	transport := &fakeTransport{}
	opts := Options{MaxRetries: 2, Backoff: -1, Timeout: time.Second}
	c := New(router, transport, opts)

	_, err := c.Get(context.Background(), []byte("key"))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Get() error = %v, want ErrUnavailable", err)
	}
	if router.callCount() != 3 {
		t.Fatalf("router calls = %d, want 3", router.callCount())
	}
	if transport.count() != 0 {
		t.Fatalf("transport calls = %d, want 0", transport.count())
	}
}

func TestRouterPermanentErrorNotRetried(t *testing.T) {
	t.Parallel()

	boom := errors.New("bad router configuration")
	router := &fakeRouter{fn: func(_ int, _ context.Context, _ []byte) (string, error) {
		return "", boom
	}}
	transport := &fakeTransport{}
	c := New(router, transport, testOptions())

	_, err := c.Get(context.Background(), []byte("some-key"))
	if err == nil {
		t.Fatal("Get() error = nil, want failure")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("Get() error = %v, want wrapped boom", err)
	}
	if router.callCount() != 1 {
		t.Fatalf("router calls = %d, want 1 (no retry for permanent errors)", router.callCount())
	}
	if !strings.Contains(err.Error(), "get") || !strings.Contains(err.Error(), "some-key") {
		t.Fatalf("Get() error = %q, want operation and key context", err)
	}
}

func TestNotFoundNotRetried(t *testing.T) {
	t.Parallel()

	router := &fakeRouter{}
	transport := &fakeTransport{getFn: func(_ context.Context, _ string, _ []byte) ([]byte, error) {
		return nil, fmt.Errorf("no such key: %w", ErrNotFound)
	}}
	c := New(router, transport, testOptions())

	_, err := c.Get(context.Background(), []byte("missing"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
	if router.callCount() != 1 {
		t.Fatalf("router calls = %d, want 1", router.callCount())
	}
	if transport.count() != 1 {
		t.Fatalf("transport calls = %d, want 1 (not-found is not retried)", transport.count())
	}
}

func TestInvalidArgumentsNotRetried(t *testing.T) {
	t.Parallel()

	router := &fakeRouter{}
	transport := &fakeTransport{}
	c := New(router, transport, testOptions())
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
		want string
	}{
		{name: "get empty", call: func() error { _, err := c.Get(ctx, nil); return err }, want: "get"},
		{name: "put empty", call: func() error { return c.Put(ctx, []byte{}, []byte("v")) }, want: "put"},
		{name: "delete empty", call: func() error { return c.Delete(ctx, []byte{}) }, want: "delete"},
		{name: "scan reversed", call: func() error { _, err := c.Scan(ctx, []byte("z"), []byte("a"), 0); return err }, want: "scan"},
		{name: "scan negative limit", call: func() error { _, err := c.Scan(ctx, nil, nil, -1); return err }, want: "scan"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("error = nil, want validation failure")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want operation %q", err, tc.want)
			}
		})
	}

	if router.callCount() != 0 {
		t.Fatalf("router calls = %d, want 0 for invalid arguments", router.callCount())
	}
	if transport.count() != 0 {
		t.Fatalf("transport calls = %d, want 0 for invalid arguments", transport.count())
	}
}

func TestContextCancellationBeforeCall(t *testing.T) {
	t.Parallel()

	router := &fakeRouter{}
	transport := &fakeTransport{}
	c := New(router, transport, testOptions())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Get(ctx, []byte("key"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Get() error = %v, want context.Canceled", err)
	}
	if router.callCount() != 0 {
		t.Fatalf("router calls = %d, want 0", router.callCount())
	}
	if transport.count() != 0 {
		t.Fatalf("transport calls = %d, want 0", transport.count())
	}
}

func TestTimeoutBoundsOperation(t *testing.T) {
	t.Parallel()

	router := &fakeRouter{}
	transport := &fakeTransport{getFn: func(ctx context.Context, _ string, _ []byte) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	opts := Options{MaxRetries: 5, Backoff: time.Millisecond, Timeout: 30 * time.Millisecond}
	c := New(router, transport, opts)

	start := time.Now()
	_, err := c.Get(context.Background(), []byte("slow-key"))
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get() error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Get() took %v, timeout was not respected", elapsed)
	}
	if transport.count() != 1 {
		t.Fatalf("transport calls = %d, want 1 (deadline is not retried)", transport.count())
	}
}

func TestCancellationDuringBackoff(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	var once sync.Once
	router := &fakeRouter{}
	transport := &fakeTransport{getFn: func(_ context.Context, _ string, _ []byte) ([]byte, error) {
		once.Do(func() { close(started) })
		return nil, ErrUnavailable
	}}
	opts := Options{MaxRetries: 5, Backoff: time.Second, Timeout: 5 * time.Second}
	c := New(router, transport, opts)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()

	start := time.Now()
	_, err := c.Get(ctx, []byte("key"))
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Get() error = %v, want context.Canceled", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Get() took %v, cancellation did not interrupt backoff", elapsed)
	}
	if transport.count() != 1 {
		t.Fatalf("transport calls = %d, want 1", transport.count())
	}
}

func TestScanAndPutDeleteUseResolvedLeader(t *testing.T) {
	t.Parallel()

	router := &fakeRouter{}
	var (
		mu       sync.Mutex
		seenAddr string
	)
	transport := &fakeTransport{
		putFn: func(_ context.Context, addr string, key, value []byte) error {
			mu.Lock()
			seenAddr = addr
			mu.Unlock()
			if string(key) != "put-key" || string(value) != "put-value" {
				return fmt.Errorf("unexpected put %q=%q", key, value)
			}
			return nil
		},
		delFn: func(_ context.Context, addr string, key []byte) error {
			if string(key) != "del-key" {
				return fmt.Errorf("unexpected delete key %q", key)
			}
			return nil
		},
		scanFn: func(_ context.Context, addr string, start, end []byte, limit int) ([]KV, error) {
			if string(start) != "a" || string(end) != "z" || limit != 7 {
				return nil, fmt.Errorf("unexpected scan %q..%q limit %d", start, end, limit)
			}
			return []KV{{Key: []byte("a"), Value: []byte("1")}}, nil
		},
	}
	c := New(router, transport, testOptions())
	ctx := context.Background()

	if err := c.Put(ctx, []byte("put-key"), []byte("put-value")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if err := c.Delete(ctx, []byte("del-key")); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	got, err := c.Scan(ctx, []byte("a"), []byte("z"), 7)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(got) != 1 || string(got[0].Key) != "a" || string(got[0].Value) != "1" {
		t.Fatalf("Scan() = %#v", got)
	}

	mu.Lock()
	putAddr := seenAddr
	mu.Unlock()
	if putAddr != "node-1" {
		t.Fatalf("Put() used addr %q, want node-1", putAddr)
	}
	calls := transport.snapshotCalls()
	if len(calls) != 3 {
		t.Fatalf("transport calls = %d, want 3", len(calls))
	}
	for _, call := range calls {
		if call.addr != "node-1" {
			t.Fatalf("%s used addr %q, want node-1", call.op, call.addr)
		}
	}
	// put/del/scan use different short keys, so each resolves its leader once
	// and then reuses the cached entry for later operations on that prefix.
	if router.callCount() != 3 {
		t.Fatalf("router calls = %d, want 3", router.callCount())
	}
}

func TestCacheStaysBounded(t *testing.T) {
	t.Parallel()

	router := &fakeRouter{}
	transport := &fakeTransport{getFn: func(_ context.Context, _ string, key []byte) ([]byte, error) {
		return key, nil
	}}
	c := New(router, transport, testOptions())

	const keys = leaderCacheEntries + 200
	for i := 0; i < keys; i++ {
		key := []byte(fmt.Sprintf("key-%08x", i))
		if _, err := c.Get(context.Background(), key); err != nil {
			t.Fatalf("Get(%q) error = %v", key, err)
		}
	}
	if got := c.leaders.len(); got != leaderCacheEntries {
		t.Fatalf("cache size = %d, want %d", got, leaderCacheEntries)
	}
}

func TestConcurrentCalls(t *testing.T) {
	t.Parallel()

	router := &fakeRouter{}
	transport := &fakeTransport{
		getFn: func(_ context.Context, _ string, key []byte) ([]byte, error) {
			return append([]byte("v:"), key...), nil
		},
		scanFn: func(_ context.Context, _ string, start, end []byte, _ int) ([]KV, error) {
			return []KV{{Key: cloneBytes(start), Value: cloneBytes(end)}}, nil
		},
	}
	c := New(router, transport, Options{MaxRetries: 3, Backoff: -1, Timeout: 30 * time.Second})
	ctx := context.Background()

	const (
		goroutines = 16
		iterations = 100
	)
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				key := []byte(fmt.Sprintf("key-%02d-%03d", g, i))
				got, err := c.Get(ctx, key)
				if err != nil {
					errCh <- fmt.Errorf("Get(%q): %w", key, err)
					return
				}
				if want := "v:" + string(key); string(got) != want {
					errCh <- fmt.Errorf("Get(%q) = %q, want %q", key, got, want)
					return
				}
				if err := c.Put(ctx, key, got); err != nil {
					errCh <- fmt.Errorf("Put(%q): %w", key, err)
					return
				}
				if err := c.Delete(ctx, key); err != nil {
					errCh <- fmt.Errorf("Delete(%q): %w", key, err)
					return
				}
				start := key
				end := append(cloneBytes(key), 'z')
				pairs, err := c.Scan(ctx, start, end, 1)
				if err != nil {
					errCh <- fmt.Errorf("Scan(%q): %w", start, err)
					return
				}
				if len(pairs) != 1 || string(pairs[0].Key) != string(start) {
					errCh <- fmt.Errorf("Scan(%q) = %#v", start, pairs)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	// 16*100 unique keys, each queried exactly once; all paths are distinct
	// enough that the router has to resolve each one.
	if got, want := router.callCount(), goroutines*iterations; got != want {
		t.Fatalf("router calls = %d, want %d", got, want)
	}
	if got, want := c.leaders.len(), leaderCacheEntries; got != want {
		t.Fatalf("cache size = %d, want %d", got, want)
	}
}

func TestConcurrentSameKeyUsesCache(t *testing.T) {
	t.Parallel()

	router := &fakeRouter{}
	transport := &fakeTransport{getFn: func(_ context.Context, _ string, key []byte) ([]byte, error) {
		return key, nil
	}}
	c := New(router, transport, Options{MaxRetries: 2, Backoff: -1, Timeout: 30 * time.Second})
	ctx := context.Background()

	// Warm the cache once, then hammer it concurrently.
	if _, err := c.Get(ctx, []byte("hot-key")); err != nil {
		t.Fatalf("warm-up Get() error = %v", err)
	}

	const goroutines = 32
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := c.Get(ctx, []byte("hot-key")); err != nil {
					t.Errorf("Get(hot-key) error = %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if got := router.callCount(); got != 1 {
		t.Fatalf("router calls = %d, want 1 (cache hit under concurrency)", got)
	}
	if got := transport.count(); got != 1+goroutines*50 {
		t.Fatalf("transport calls = %d, want %d", got, 1+goroutines*50)
	}
}

func TestNilClientFailsWithoutPanic(t *testing.T) {
	t.Parallel()

	var c *Client
	if _, err := c.Get(context.Background(), []byte("key")); err == nil {
		t.Fatal("nil Client Get() error = nil")
	}

	c = New(nil, nil, Options{})
	if _, err := c.Get(context.Background(), []byte("key")); err == nil {
		t.Fatal("Client without router/transport Get() error = nil")
	}
	if err := c.Put(context.Background(), []byte("key"), []byte("value")); err == nil {
		t.Fatal("Client without router/transport Put() error = nil")
	}
}
