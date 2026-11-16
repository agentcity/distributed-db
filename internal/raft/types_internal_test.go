package raft

import (
	"errors"
	"testing"
)

func TestLostQuorumErrorClassification(t *testing.T) {
	if !errors.Is(errLostQuorum, ErrNotLeader) {
		t.Fatal("lost-quorum error does not match ErrNotLeader")
	}
	if !errors.Is(errLostQuorum, ErrTimeout) {
		t.Fatal("lost-quorum error does not match ErrTimeout")
	}
}
