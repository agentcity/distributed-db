package v2client

import (
	"context"
	"errors"
	"sync"
	"testing"

	"google.golang.org/grpc"
)

func TestMetaConnectionPoolReuse(t *testing.T) {
	fake := newFakeMeta("meta1")
	c := newTestClient(fake)
	c.metaPool = map[string]*metaEntry{}
	c.metaLeaderAddr = ""
	c.metaLeaderUntil = timeZero

	var mu sync.Mutex
	dialed := map[string]int{}
	c.metaDial = func(addr string) (*grpc.ClientConn, metaAPI, error) {
		mu.Lock()
		dialed[addr]++
		mu.Unlock()
		return nil, fake, nil
	}
	ctx := context.Background()

	// Reusing the already-connected client must not dial at all.
	for i := 0; i < 10; i++ {
		if _, _, err := c.metaClientFresh(ctx); err != nil {
			t.Fatalf("metaClientFresh: %v", err)
		}
	}
	mu.Lock()
	if total := dialed["meta1"] + dialed["meta2"]; total != 0 {
		mu.Unlock()
		t.Fatalf("dialed %d connections while reusing meta1", total)
	}
	mu.Unlock()

	// A learned leader is dialed exactly once and reused afterwards.
	fake.setLeader("meta2")
	for i := 0; i < 10; i++ {
		if _, _, err := c.metaClientFresh(ctx); err != nil {
			t.Fatalf("metaClientFresh to meta2: %v", err)
		}
	}
	mu.Lock()
	if dialed["meta2"] != 1 {
		mu.Unlock()
		t.Fatalf("dialed meta2 %d times, want 1", dialed["meta2"])
	}
	mu.Unlock()

	// Moving back to meta1 reuses the old connection instead of leaking a new
	// one on every failover.
	fake.setLeader("meta1")
	for i := 0; i < 10; i++ {
		if _, _, err := c.metaClientFresh(ctx); err != nil {
			t.Fatalf("metaClientFresh back to meta1: %v", err)
		}
	}
	mu.Lock()
	if dialed["meta1"] != 1 || dialed["meta2"] != 1 {
		total := dialed["meta1"] + dialed["meta2"]
		mu.Unlock()
		t.Fatalf("dialed connections after failover = %d (%v), want 2", total, dialed)
	}
	mu.Unlock()
	if len(c.metaPool) != 2 {
		t.Fatalf("meta pool size = %d, want 2", len(c.metaPool))
	}
}

func TestMetaSeedProbeReusesConnection(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setLeaderErr(errors.New("leader unavailable"))
	c := newTestClient(fake)
	c.metaPool = map[string]*metaEntry{}
	c.metaLeaderAddr = ""
	c.metaLeaderUntil = timeZero

	var mu sync.Mutex
	dialed := 0
	c.metaDial = func(string) (*grpc.ClientConn, metaAPI, error) {
		mu.Lock()
		dialed++
		mu.Unlock()
		return nil, fake, nil
	}
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		// The probe fails; the caller falls back to its cached connection.
		if _, _, err := c.metaClientFresh(ctx); err != nil {
			t.Fatalf("metaClientFresh: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if dialed != 1 {
		t.Fatalf("seed probes dialed %d connections, want 1", dialed)
	}
}
