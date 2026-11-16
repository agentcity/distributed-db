package node

import (
	"context"
	"testing"

	"distributed-db/internal/raft"
)

func TestPeerTransportCloseIsIdempotent(t *testing.T) {
	tr, err := NewPeerTransport(Config{})
	if err != nil {
		t.Fatalf("NewPeerTransport: %v", err)
	}
	tr.Close()
	tr.Close()

	if _, err := tr.RequestVote(context.Background(), "127.0.0.1:1", &raft.RequestVoteRequest{}); err == nil {
		t.Fatal("RequestVote after Close: expected an error")
	}
	if _, err := tr.AppendEntries(context.Background(), "127.0.0.1:1", &raft.AppendEntriesRequest{}); err == nil {
		t.Fatal("AppendEntries after Close: expected an error")
	}
}

func TestTransportCredentialsModes(t *testing.T) {
	creds, err := clientTransportCredentials(Config{})
	if err != nil {
		t.Fatalf("insecure client credentials: %v", err)
	}
	if creds == nil {
		t.Fatal("client credentials are nil")
	}
	if got := creds.Info().SecurityProtocol; got != "insecure" {
		t.Errorf("SecurityProtocol = %q, want insecure", got)
	}

	creds, err = serverTransportCredentials(Config{})
	if err != nil {
		t.Fatalf("disabled server credentials: %v", err)
	}
	if creds != nil {
		t.Fatal("server credentials should be nil when TLS is disabled")
	}
}
