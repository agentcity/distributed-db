package v2client

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	pbv2 "distributed-db/internal/pb/v2"
)

func TestListRangesMapsDescriptors(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setListRanges(func() ([]*pbv2.RangeInfo, error) {
		return []*pbv2.RangeInfo{
			{
				Id: 2, Start: []byte("m"), Replicas: []string{"n1", "n2"},
				Leader: "n1", Version: 5,
			},
			{
				Id: 1, End: []byte("m"), Replicas: []string{"n2"},
				Leader: "n2", Version: 3,
			},
		}, nil
	})
	c := newTestClient(fake)

	got, err := c.ListRanges(context.Background())
	if err != nil {
		t.Fatalf("ListRanges: %v", err)
	}
	want := []RangeInfo{
		{ID: 2, Start: []byte("m"), Replicas: []string{"n1", "n2"}, Leader: "n1", Version: 5},
		{ID: 1, End: []byte("m"), Replicas: []string{"n2"}, Leader: "n2", Version: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListRanges = %#v, want %#v", got, want)
	}
}

func TestListRangesWithoutKVNodes(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setListRanges(func() ([]*pbv2.RangeInfo, error) {
		return []*pbv2.RangeInfo{{Id: 1, Version: 1}}, nil
	})
	c := newTestClient(fake)
	c.addrs = nil
	c.clients = nil

	got, err := c.ListRanges(context.Background())
	if err != nil {
		t.Fatalf("ListRanges: %v", err)
	}
	if len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("ListRanges = %#v", got)
	}
}

func TestListRangesTransportError(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setListRanges(func() ([]*pbv2.RangeInfo, error) {
		return nil, errors.New("meta unavailable")
	})
	c := newTestClient(fake)

	if _, err := c.ListRanges(context.Background()); err == nil || !strings.Contains(err.Error(), "meta unavailable") {
		t.Fatalf("ListRanges error = %v, want meta unavailable", err)
	}
}

func TestSplitRangeSendsMetaRequest(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setSplitRange(func(req *pbv2.MetaSplitRangeRequest) (*pbv2.MetaSplitRangeResponse, error) {
		if req.GetRangeId() != 7 || string(req.GetSplitKey()) != "m" {
			t.Errorf("SplitRange request = %+v", req)
		}
		return &pbv2.MetaSplitRangeResponse{Success: true, NewRangeId: 9}, nil
	})
	c := newTestClient(fake)

	newID, err := c.SplitRange(context.Background(), 7, []byte("m"))
	if err != nil {
		t.Fatalf("SplitRange: %v", err)
	}
	if newID != 9 {
		t.Fatalf("new range id = %d, want 9", newID)
	}
	_, splits, _ := fake.adminStats()
	if len(splits) != 1 {
		t.Fatalf("SplitRange RPCs = %d, want 1", len(splits))
	}
}

func TestSplitRangeServerErrorIsNotRetried(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setSplitRange(func(*pbv2.MetaSplitRangeRequest) (*pbv2.MetaSplitRangeResponse, error) {
		return &pbv2.MetaSplitRangeResponse{
			Success: false,
			Error:   "split range 7: range not found",
		}, nil
	})
	c := newTestClient(fake)

	newID, err := c.SplitRange(context.Background(), 7, []byte("m"))
	if err == nil || err.Error() != "split range 7: range not found" {
		t.Fatalf("SplitRange error = %v", err)
	}
	if newID != 0 {
		t.Fatalf("new range id = %d, want 0", newID)
	}
	_, splits, _ := fake.adminStats()
	if len(splits) != 1 {
		t.Fatalf("SplitRange RPCs = %d, want 1 (server errors are not retried)", len(splits))
	}
}

func TestSplitRangeTransportError(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setSplitRange(func(*pbv2.MetaSplitRangeRequest) (*pbv2.MetaSplitRangeResponse, error) {
		return nil, errors.New("connection refused")
	})
	c := newTestClient(fake)

	if _, err := c.SplitRange(context.Background(), 7, []byte("m")); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("SplitRange error = %v", err)
	}
}

func TestMergeRangesSendsMetaRequest(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setMergeRanges(func(req *pbv2.MetaMergeRangesRequest) (*pbv2.MetaMergeRangesResponse, error) {
		if req.GetLeftRangeId() != 2 || req.GetRightRangeId() != 3 {
			t.Errorf("MergeRanges request = %+v", req)
		}
		return &pbv2.MetaMergeRangesResponse{Success: true}, nil
	})
	c := newTestClient(fake)

	if err := c.MergeRanges(context.Background(), 2, 3); err != nil {
		t.Fatalf("MergeRanges: %v", err)
	}
	_, _, merges := fake.adminStats()
	if len(merges) != 1 {
		t.Fatalf("MergeRanges RPCs = %d, want 1", len(merges))
	}
}

func TestMergeRangesServerErrorIsNotRetried(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setMergeRanges(func(*pbv2.MetaMergeRangesRequest) (*pbv2.MetaMergeRangesResponse, error) {
		return &pbv2.MetaMergeRangesResponse{
			Success: false,
			Error:   "merge: ranges 2 and 3 are not adjacent",
		}, nil
	})
	c := newTestClient(fake)

	err := c.MergeRanges(context.Background(), 2, 3)
	if err == nil || err.Error() != "merge: ranges 2 and 3 are not adjacent" {
		t.Fatalf("MergeRanges error = %v", err)
	}
	_, _, merges := fake.adminStats()
	if len(merges) != 1 {
		t.Fatalf("MergeRanges RPCs = %d, want 1 (server errors are not retried)", len(merges))
	}
}

func TestMetaLeader(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setLeaderInfo("meta2", 7)
	c := newTestClient(fake)

	leader, term, err := c.MetaLeader(context.Background())
	if err != nil {
		t.Fatalf("MetaLeader: %v", err)
	}
	if leader != "meta2" || term != 7 {
		t.Fatalf("MetaLeader = %q/%d, want meta2/7", leader, term)
	}
}

func TestMetaLeaderError(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setLeaderErr(errors.New("meta down"))
	c := newTestClient(fake)

	if _, _, err := c.MetaLeader(context.Background()); err == nil || !strings.Contains(err.Error(), "meta down") {
		t.Fatalf("MetaLeader error = %v", err)
	}
}

func TestNewMetaOnlyRequiresMetaButNoNodes(t *testing.T) {
	if _, err := New("meta1", nil); err == nil {
		t.Fatal("New with no nodes succeeded, want error")
	}
	c, err := NewMetaOnly("meta1")
	if err != nil {
		t.Fatalf("NewMetaOnly: %v", err)
	}
	defer func() { _ = c.Close() }()
	if len(c.addrs) != 0 {
		t.Fatalf("NewMetaOnly addrs = %v, want none", c.addrs)
	}
}
