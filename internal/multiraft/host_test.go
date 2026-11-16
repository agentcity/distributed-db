package multiraft

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"distributed-db/internal/model"
)

var errFakeGroupStopped = errors.New("fake group stopped")

// fakeGroup is an in-memory Group used to exercise Host routing and lifecycle.
type fakeGroup struct {
	id GroupID

	mu        sync.Mutex
	stopped   bool
	stopCount int
	proposals []model.Entry
	proposeFn func(context.Context, model.Entry) (uint64, error)
}

func (g *fakeGroup) ID() GroupID { return g.id }

func (g *fakeGroup) Propose(ctx context.Context, cmd model.Entry) (uint64, error) {
	g.mu.Lock()
	fn := g.proposeFn
	g.mu.Unlock()
	if fn != nil {
		return fn(ctx, cmd)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return 0, errFakeGroupStopped
	}
	g.proposals = append(g.proposals, cmd)
	return uint64(len(g.proposals)), nil
}

func (g *fakeGroup) Status() Status {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Status{ID: g.id, Leader: "fake-leader", Term: 1, CommitIndex: uint64(len(g.proposals))}
}

func (g *fakeGroup) Stop() {
	g.mu.Lock()
	g.stopped = true
	g.stopCount++
	g.mu.Unlock()
}

func (g *fakeGroup) setProposeFn(fn func(context.Context, model.Entry) (uint64, error)) {
	g.mu.Lock()
	g.proposeFn = fn
	g.mu.Unlock()
}

func (g *fakeGroup) stoppedNow() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stopped
}

func (g *fakeGroup) stops() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stopCount
}

func (g *fakeGroup) gotProposals() []model.Entry {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]model.Entry(nil), g.proposals...)
}

type fakeFactory struct {
	mu       sync.Mutex
	err      error
	nilGroup bool
	groups   map[GroupID]*fakeGroup
	order    []GroupID
	peers    map[GroupID][]string
}

func newFakeFactory() *fakeFactory {
	return &fakeFactory{
		groups: make(map[GroupID]*fakeGroup),
		peers:  make(map[GroupID][]string),
	}
}

func (f *fakeFactory) NewGroup(id GroupID, peers []string) (Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return nil, f.err
	}
	if f.nilGroup {
		return nil, nil
	}
	g := &fakeGroup{id: id}
	f.groups[id] = g
	f.order = append(f.order, id)
	f.peers[id] = append([]string(nil), peers...)
	return g, nil
}

func (f *fakeFactory) get(id GroupID) (*fakeGroup, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.groups[id]
	return g, ok
}

func (f *fakeFactory) createdIDs() []GroupID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]GroupID(nil), f.order...)
}

func (f *fakeFactory) peerSet(id GroupID) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.peers[id]...)
}

func (f *fakeFactory) setError(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeFactory) setNilGroup(v bool) {
	f.mu.Lock()
	f.nilGroup = v
	f.mu.Unlock()
}

func TestCreateAndGet(t *testing.T) {
	f := newFakeFactory()
	h := NewHost(f)

	peers := []string{"n1", "n2", "n3"}
	if err := h.Create(7, peers); err != nil {
		t.Fatalf("Create: %v", err)
	}

	g, ok := h.Get(7)
	if !ok {
		t.Fatal("Get(7) = false, want true")
	}
	want, ok := f.get(7)
	if !ok {
		t.Fatal("factory did not create group 7")
	}
	if g != want {
		t.Fatalf("Get returned %v, want the factory's group", g)
	}
	if got := g.ID(); got != 7 {
		t.Fatalf("group ID = %d, want 7", got)
	}
	if got := f.peerSet(7); !reflect.DeepEqual(got, peers) {
		t.Fatalf("factory peers = %v, want %v", got, peers)
	}

	if _, ok := h.Get(8); ok {
		t.Fatal("Get(8) = true, want false")
	}
}

func TestCreateCopiesPeers(t *testing.T) {
	f := newFakeFactory()
	h := NewHost(f)

	peers := []string{"n1", "n2"}
	if err := h.Create(1, peers); err != nil {
		t.Fatalf("Create: %v", err)
	}
	peers[0] = "mutated"

	if got := f.peerSet(1); !reflect.DeepEqual(got, []string{"n1", "n2"}) {
		t.Fatalf("factory peers = %v, want the original slice contents", got)
	}
}

func TestDuplicateCreate(t *testing.T) {
	f := newFakeFactory()
	h := NewHost(f)

	if err := h.Create(1, nil); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	err := h.Create(1, nil)
	if !errors.Is(err, ErrGroupExists) {
		t.Fatalf("second Create error = %v, want ErrGroupExists", err)
	}
	if !errors.Is(err, ErrDuplicateGroup) {
		t.Fatalf("second Create error = %v, want ErrDuplicateGroup alias", err)
	}
	if got := len(f.createdIDs()); got != 1 {
		t.Fatalf("factory called %d times, want 1", got)
	}
}

func TestProposeRouting(t *testing.T) {
	f := newFakeFactory()
	h := NewHost(f)
	if err := h.Create(1, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.Create(2, nil); err != nil {
		t.Fatal(err)
	}

	cmd1 := model.Entry{Type: model.CommandPut, Key: "a", Value: []byte("1")}
	cmd2 := model.Entry{Type: model.CommandDelete, Key: "b"}

	idx, err := h.Propose(context.Background(), 1, cmd1)
	if err != nil {
		t.Fatalf("Propose(1): %v", err)
	}
	if idx != 1 {
		t.Fatalf("Propose(1) index = %d, want 1", idx)
	}
	idx, err = h.Propose(context.Background(), 2, cmd2)
	if err != nil {
		t.Fatalf("Propose(2): %v", err)
	}
	if idx != 1 {
		t.Fatalf("Propose(2) index = %d, want 1", idx)
	}

	g1, _ := f.get(1)
	g2, _ := f.get(2)
	if got := g1.gotProposals(); !reflect.DeepEqual(got, []model.Entry{cmd1}) {
		t.Fatalf("group 1 proposals = %v, want [%v]", got, cmd1)
	}
	if got := g2.gotProposals(); !reflect.DeepEqual(got, []model.Entry{cmd2}) {
		t.Fatalf("group 2 proposals = %v, want [%v]", got, cmd2)
	}

	idx, err = h.Propose(context.Background(), 99, cmd1)
	if !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("Propose(99) error = %v, want ErrGroupNotFound", err)
	}
	if idx != 0 {
		t.Fatalf("Propose(99) index = %d, want 0", idx)
	}
}

func TestProposePropagatesGroupError(t *testing.T) {
	f := newFakeFactory()
	h := NewHost(f)
	if err := h.Create(1, nil); err != nil {
		t.Fatal(err)
	}
	g, _ := f.get(1)

	boom := errors.New("boom")
	g.setProposeFn(func(context.Context, model.Entry) (uint64, error) {
		return 42, boom
	})

	idx, err := h.Propose(context.Background(), 1, model.Entry{})
	if !errors.Is(err, boom) {
		t.Fatalf("Propose error = %v, want boom", err)
	}
	if idx != 42 {
		t.Fatalf("Propose index = %d, want 42", idx)
	}
}

func TestProposeRespectsContext(t *testing.T) {
	f := newFakeFactory()
	h := NewHost(f)
	if err := h.Create(1, nil); err != nil {
		t.Fatal(err)
	}
	g, _ := f.get(1)

	started := make(chan struct{})
	g.setProposeFn(func(ctx context.Context, _ model.Entry) (uint64, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.Propose(ctx, 1, model.Entry{})
		done <- err
	}()

	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Propose error = %v, want context.Canceled", err)
	}
}

func TestProposeAlreadyCancelledContext(t *testing.T) {
	f := newFakeFactory()
	h := NewHost(f)
	if err := h.Create(1, nil); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	idx, err := h.Propose(ctx, 1, model.Entry{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Propose error = %v, want context.Canceled", err)
	}
	if idx != 0 {
		t.Fatalf("Propose index = %d, want 0", idx)
	}
}

func TestRemove(t *testing.T) {
	f := newFakeFactory()
	h := NewHost(f)
	if err := h.Create(1, nil); err != nil {
		t.Fatal(err)
	}
	g, _ := f.get(1)

	if err := h.Remove(1); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !g.stoppedNow() {
		t.Fatal("removed group was not stopped")
	}
	if got := g.stops(); got != 1 {
		t.Fatalf("group stopped %d times, want 1", got)
	}
	if _, ok := h.Get(1); ok {
		t.Fatal("Get after Remove = true, want false")
	}
	if err := h.Remove(1); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("second Remove error = %v, want ErrGroupNotFound", err)
	}
	if _, err := h.Propose(context.Background(), 1, model.Entry{}); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("Propose after Remove error = %v, want ErrGroupNotFound", err)
	}
}

func TestStopStopsAllAndIsIdempotent(t *testing.T) {
	f := newFakeFactory()
	h := NewHost(f)
	const n = 5
	for i := 1; i <= n; i++ {
		if err := h.Create(GroupID(i), nil); err != nil {
			t.Fatal(err)
		}
	}

	h.Stop()
	h.Stop()

	for i := 1; i <= n; i++ {
		id := GroupID(i)
		g, ok := f.get(id)
		if !ok {
			t.Fatalf("group %d missing from factory", id)
		}
		if !g.stoppedNow() {
			t.Fatalf("group %d was not stopped", id)
		}
		if got := g.stops(); got != 1 {
			t.Fatalf("group %d stopped %d times, want 1", id, got)
		}
		if _, ok := h.Get(id); ok {
			t.Fatalf("Get(%d) after Stop = true, want false", id)
		}
		if _, err := h.Propose(context.Background(), id, model.Entry{}); !errors.Is(err, ErrGroupNotFound) {
			t.Fatalf("Propose(%d) after Stop error = %v, want ErrGroupNotFound", id, err)
		}
	}

	if err := h.Create(100, nil); !errors.Is(err, ErrHostStopped) {
		t.Fatalf("Create after Stop error = %v, want ErrHostStopped", err)
	}
	if _, ok := f.get(100); ok {
		t.Fatal("factory created a group after Stop")
	}
}

func TestConcurrentRemoveAndStopStopOnce(t *testing.T) {
	for i := 0; i < 50; i++ {
		f := newFakeFactory()
		h := NewHost(f)
		if err := h.Create(1, nil); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); _ = h.Remove(1) }()
		go func() { defer wg.Done(); h.Stop() }()
		go func() { defer wg.Done(); h.Stop() }()
		wg.Wait()

		g, ok := f.get(1)
		if !ok {
			t.Fatal("factory group missing")
		}
		if got := g.stops(); got != 1 {
			t.Fatalf("iteration %d: group stopped %d times, want 1", i, got)
		}
	}
}

func TestFactoryError(t *testing.T) {
	f := newFakeFactory()
	h := NewHost(f)
	boom := errors.New("factory boom")
	f.setError(boom)

	if err := h.Create(1, nil); !errors.Is(err, boom) {
		t.Fatalf("Create error = %v, want factory error", err)
	}
	if _, ok := h.Get(1); ok {
		t.Fatal("Get after failed Create = true, want false")
	}

	f.setError(nil)
	if err := h.Create(1, nil); err != nil {
		t.Fatalf("Create after clearing factory error: %v", err)
	}
}

func TestFactoryReturnsNilGroup(t *testing.T) {
	f := newFakeFactory()
	h := NewHost(f)
	f.setNilGroup(true)

	if err := h.Create(1, nil); err == nil {
		t.Fatal("Create with nil group error = nil, want error")
	}
	if _, ok := h.Get(1); ok {
		t.Fatal("Get after nil-group Create = true, want false")
	}

	f.setNilGroup(false)
	if err := h.Create(1, nil); err != nil {
		t.Fatalf("Create after clearing nil group: %v", err)
	}
}

func TestNilFactory(t *testing.T) {
	h := NewHost(nil)
	if err := h.Create(1, nil); err == nil {
		t.Fatal("Create with nil factory error = nil, want error")
	}
	if _, ok := h.Get(1); ok {
		t.Fatal("Get after failed Create = true, want false")
	}
	h.Stop()
}

func TestConcurrentAccess(t *testing.T) {
	f := newFakeFactory()
	h := NewHost(f)

	const n = 64
	var failures struct {
		mu   sync.Mutex
		msgs []string
	}
	record := func(format string, args ...any) {
		failures.mu.Lock()
		failures.msgs = append(failures.msgs, fmt.Sprintf(format, args...))
		failures.mu.Unlock()
	}

	// Concurrent creation and initial use of distinct IDs.
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := GroupID(i + 1)
			if err := h.Create(id, []string{"p1", "p2"}); err != nil {
				record("Create(%d): %v", id, err)
				return
			}
			if _, ok := h.Get(id); !ok {
				record("Get(%d) = false after Create", id)
			}
			if _, err := h.Propose(context.Background(), id, model.Entry{Key: fmt.Sprintf("k%d", i)}); err != nil {
				record("Propose(%d): %v", id, err)
			}
		}(i)
	}
	wg.Wait()

	// Concurrent duplicate creation: exactly one must win.
	const racers = 32
	const dupID GroupID = 1000
	var winners atomic.Int32
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch err := h.Create(dupID, nil); {
			case err == nil:
				winners.Add(1)
			case errors.Is(err, ErrGroupExists):
			default:
				record("duplicate Create(%d): %v", dupID, err)
			}
		}()
	}
	wg.Wait()
	if got := winners.Load(); got != 1 {
		record("duplicate create winners = %d, want 1", got)
	}

	// Concurrent reads, proposals, removals and creations. A proposal may
	// race with the removal of its group, so either error is acceptable
	// there; anything else is a failure.
	var ops sync.WaitGroup
	for i := 0; i < n; i++ {
		id := GroupID(i + 1)
		ops.Add(3)
		go func() {
			defer ops.Done()
			_, _ = h.Get(id)
		}()
		go func() {
			defer ops.Done()
			_, err := h.Propose(context.Background(), id, model.Entry{Type: model.CommandNoop})
			if err != nil && !errors.Is(err, ErrGroupNotFound) && !errors.Is(err, errFakeGroupStopped) {
				record("concurrent Propose(%d): %v", id, err)
			}
		}()
		go func() {
			defer ops.Done()
			if err := h.Remove(id); err != nil && !errors.Is(err, ErrGroupNotFound) {
				record("concurrent Remove(%d): %v", id, err)
			}
		}()
	}
	ops.Wait()

	h.Stop()

	// The duplicate-created group must also have been stopped exactly once.
	allIDs := f.createdIDs()
	if len(allIDs) != n+1 {
		record("factory created %d groups, want %d", len(allIDs), n+1)
	}
	for _, id := range allIDs {
		g, ok := f.get(id)
		if !ok {
			record("group %d missing", id)
			continue
		}
		if !g.stoppedNow() {
			record("group %d not stopped after Stop", id)
		}
		if got := g.stops(); got != 1 {
			record("group %d stopped %d times, want 1", id, got)
		}
	}

	// Stops and duplicate creates after Stop must fail cleanly.
	if err := h.Create(2000, nil); !errors.Is(err, ErrHostStopped) {
		record("Create after Stop: %v", err)
	}

	failures.mu.Lock()
	defer failures.mu.Unlock()
	for _, msg := range failures.msgs {
		t.Error(msg)
	}
}
