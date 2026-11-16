package node

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestNormalizeConfigDefaultsAndPeers(t *testing.T) {
	cfg, err := normalizeConfig(Config{
		RaftAddr: " 127.0.0.1:7001 ",
		DataDir:  t.TempDir(),
		Peers:    []string{"a:7001", "a:7001", "", "127.0.0.1:7001", "b:7001"},
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("normalizeConfig: %v", err)
	}
	if cfg.ID != "127.0.0.1:7001" {
		t.Errorf("ID = %q, want RaftAddr default", cfg.ID)
	}
	want := []string{"a:7001", "b:7001"}
	if len(cfg.Peers) != len(want) {
		t.Fatalf("Peers = %v, want %v", cfg.Peers, want)
	}
	for i := range want {
		if cfg.Peers[i] != want[i] {
			t.Errorf("Peers[%d] = %q, want %q", i, cfg.Peers[i], want[i])
		}
	}
}

func TestNormalizeConfigValidation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	base := Config{
		RaftAddr: ":7001",
		DataDir:  t.TempDir(),
		Logger:   logger,
	}

	t.Run("missing raft addr", func(t *testing.T) {
		cfg := base
		cfg.RaftAddr = ""
		if _, err := normalizeConfig(cfg); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("missing data dir", func(t *testing.T) {
		cfg := base
		cfg.DataDir = ""
		if _, err := normalizeConfig(cfg); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("cert without key", func(t *testing.T) {
		cfg := base
		cfg.TLSCert = "/tmp/cert.pem"
		if _, err := normalizeConfig(cfg); err == nil || !strings.Contains(err.Error(), "TLSCert") {
			t.Fatalf("expected TLSCert/TLSKey error, got %v", err)
		}
	})

	t.Run("invalid auth hash", func(t *testing.T) {
		cfg := base
		cfg.AuthUser = "u"
		cfg.AuthHash = "not-a-bcrypt-hash"
		if _, err := normalizeConfig(cfg); err == nil {
			t.Fatal("expected an invalid AuthHash error")
		}
	})

	t.Run("rate burst defaults", func(t *testing.T) {
		cfg := base
		cfg.RateLimit = 2.5
		got, err := normalizeConfig(cfg)
		if err != nil {
			t.Fatalf("normalizeConfig: %v", err)
		}
		if got.RateBurst != 3 {
			t.Errorf("RateBurst = %d, want ceil(2.5) = 3", got.RateBurst)
		}
	})
}
