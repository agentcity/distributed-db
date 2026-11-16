package v2client

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	pbv2 "distributed-db/internal/pb/v2"
)

type fakeKV struct {
	pbv2.KVServiceClient // unimplemented methods panic if used

	mu       sync.Mutex
	getCalls int
	epochs   []string
	failMsgs []string
	value    []byte
}

func (f *fakeKV) Get(ctx context.Context, _ *pbv2.GetRequest, _ ...grpc.CallOption) (*pbv2.GetResponse, error) {
	epoch := ""
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		if v := md.Get("x-range-epoch"); len(v) > 0 {
			epoch = v[0]
		}
	}
	f.mu.Lock()
	idx := f.getCalls
	f.getCalls++
	f.epochs = append(f.epochs, epoch)
	msg := ""
	if idx < len(f.failMsgs) {
		msg = f.failMsgs[idx]
	}
	value := append([]byte(nil), f.value...)
	f.mu.Unlock()
	if msg != "" {
		return &pbv2.GetResponse{Error: msg, Leader: "kv1"}, nil
	}
	return &pbv2.GetResponse{Found: true, Value: value, Leader: "kv1"}, nil
}

func (f *fakeKV) Status(context.Context, *pbv2.StatusRequest, ...grpc.CallOption) (*pbv2.StatusResponse, error) {
	return &pbv2.StatusResponse{Leader: "kv1", Role: "leader"}, nil
}

func (f *fakeKV) calls() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCalls, append([]string(nil), f.epochs...)
}

func newRetryTestClient(failMsgs []string) (*Client, *fakeKV, *fakeMeta) {
	fake := newFakeMeta("meta1")
	rangeCalls := 0
	fake.setGetRange(func([]byte) (*pbv2.RangeInfo, error) {
		rangeCalls++
		version := uint64(1)
		if rangeCalls > 1 {
			version = 2
		}
		return &pbv2.RangeInfo{
			Id: 1, Version: version, Replicas: []string{"kv1"}, Leader: "kv1",
		}, nil
	})
	c := newTestClient(fake)
	fk := &fakeKV{failMsgs: failMsgs, value: []byte("value")}
	c.clients["kv1"] = fk
	c.leaders[1] = leaderEntry{addr: "kv1", until: time.Now().Add(time.Hour)}
	return c, fk, fake
}

func TestRetryRefreshesRangeOnStaleEpoch(t *testing.T) {
	for _, msg := range []string{"stale epoch", "not hosted"} {
		t.Run(msg, func(t *testing.T) {
			c, fk, fake := newRetryTestClient([]string{msg})
			txn := &Txn{c: c, startTS: 1}
			got, err := txn.Get(context.Background(), []byte("k"))
			if err != nil {
				t.Fatalf("Get after %q: %v", msg, err)
			}
			if string(got) != "value" {
				t.Fatalf("Get = %q, want value", got)
			}
			calls, epochs := fk.calls()
			if calls != 2 {
				t.Fatalf("KV Get calls = %d, want 2 (one retry via meta)", calls)
			}
			if len(epochs) != 2 || epochs[0] != "1" || epochs[1] != "2" {
				t.Fatalf("range epochs = %v, want [1 2]", epochs)
			}
			if _, _, rangeCalls, _ := fake.stats(); rangeCalls != 2 {
				t.Fatalf("meta GetRange calls = %d, want 2", rangeCalls)
			}
		})
	}
}

func TestRetryStopsAfterOneMetaRefresh(t *testing.T) {
	c, fk, fake := newRetryTestClient([]string{"stale epoch", "stale epoch"})
	txn := &Txn{c: c, startTS: 1}
	if _, err := txn.Get(context.Background(), []byte("k")); err == nil {
		t.Fatal("Get succeeded while server kept returning stale epoch")
	} else if !strings.Contains(err.Error(), "stale epoch") {
		t.Fatalf("Get error = %v, want stale epoch", err)
	}
	if calls, _ := fk.calls(); calls != 2 {
		t.Fatalf("KV Get calls = %d, want 2 (only one meta refresh)", calls)
	}
	if _, _, rangeCalls, _ := fake.stats(); rangeCalls != 2 {
		t.Fatalf("meta GetRange calls = %d, want 2", rangeCalls)
	}
}
