package kvserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDurabilityModeConstants pins the wire values shared between the CLI flag
// (-durability=sync|fast) and Config.DurabilityMode.
func TestDurabilityModeConstants(t *testing.T) {
	if DurabilitySync != "sync" {
		t.Errorf("DurabilitySync = %q, want %q", DurabilitySync, "sync")
	}
	if DurabilityFast != "fast" {
		t.Errorf("DurabilityFast = %q, want %q", DurabilityFast, "fast")
	}
}

// TestResolveSyncWrites covers the documented precedence: the mode defaults to
// sync, "fast" is an explicit opt-in, and SyncStateMachine=true always forces
// sync while its zero value never overrides the mode. Unknown values are
// rejected even when SyncStateMachine is true, so a typo cannot silently
// weaken durability.
func TestResolveSyncWrites(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		want    bool
		wantErr string // substring; empty means no error
	}{
		{
			name: "zero Config defaults to sync",
			cfg:  Config{},
			want: true,
		},
		{
			name: "empty mode is sync",
			cfg:  Config{DurabilityMode: ""},
			want: true,
		},
		{
			name: "explicit sync",
			cfg:  Config{DurabilityMode: DurabilitySync},
			want: true,
		},
		{
			name: "fast is opt-in",
			cfg:  Config{DurabilityMode: DurabilityFast},
			want: false,
		},
		{
			name: "SyncStateMachine true alone forces sync",
			cfg:  Config{SyncStateMachine: true},
			want: true,
		},
		{
			name: "SyncStateMachine true wins over fast",
			cfg:  Config{DurabilityMode: DurabilityFast, SyncStateMachine: true},
			want: true,
		},
		{
			name: "SyncStateMachine false does not override fast",
			cfg:  Config{DurabilityMode: DurabilityFast, SyncStateMachine: false},
			want: false,
		},
		{
			name:    "unknown mode rejected",
			cfg:     Config{DurabilityMode: "durable"},
			wantErr: `unknown durability mode "durable"`,
		},
		{
			name:    "mode is case-sensitive",
			cfg:     Config{DurabilityMode: "FAST"},
			wantErr: "unknown durability mode",
		},
		{
			name:    "mode is not trimmed",
			cfg:     Config{DurabilityMode: " fast"},
			wantErr: "unknown durability mode",
		},
		{
			name:    "unknown mode rejected even with SyncStateMachine",
			cfg:     Config{DurabilityMode: "fsync", SyncStateMachine: true},
			wantErr: "unknown durability mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.cfg.resolveSyncWrites()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("resolveSyncWrites() = %v, want error containing %q", got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolveSyncWrites() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveSyncWrites() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolveSyncWrites() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestNewResolvesDurability checks that New, and therefore the Pebble engine
// it opens, uses the resolved setting: sync by default and for explicit sync,
// fast only when requested, and sync when SyncStateMachine overrides fast.
func TestNewResolvesDurability(t *testing.T) {
	tests := []struct {
		name      string
		mode      string
		forceSync bool
		wantSync  bool
	}{
		{name: "default is sync", mode: "", forceSync: false, wantSync: true},
		{name: "explicit sync", mode: DurabilitySync, forceSync: false, wantSync: true},
		{name: "fast opt-in", mode: DurabilityFast, forceSync: false, wantSync: false},
		{name: "SyncStateMachine wins over fast", mode: DurabilityFast, forceSync: true, wantSync: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, err := New(Config{
				ID:               "n1",
				RaftAddr:         "127.0.0.1:0",
				DataDir:          t.TempDir(),
				NoRanges:         true,
				DurabilityMode:   tt.mode,
				SyncStateMachine: tt.forceSync,
				Logger:           testLogger(),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			stopTestServer(t, srv)

			if srv.syncWrites != tt.wantSync {
				t.Fatalf("syncWrites = %v, want %v", srv.syncWrites, tt.wantSync)
			}
			if srv.cfg.DurabilityMode != tt.mode {
				t.Fatalf("cfg.DurabilityMode = %q, want %q", srv.cfg.DurabilityMode, tt.mode)
			}
		})
	}
}

// TestNewRejectsUnknownDurabilityMode verifies that a bad mode fails during
// validation, before the data directory is created or opened.
func TestNewRejectsUnknownDurabilityMode(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	_, err := New(Config{
		ID:             "n1",
		RaftAddr:       "127.0.0.1:0",
		DataDir:        dataDir,
		NoRanges:       true,
		DurabilityMode: "bogus",
		Logger:         testLogger(),
	})
	if err == nil {
		t.Fatal("New with unknown DurabilityMode succeeded, want error")
	}
	if !strings.Contains(err.Error(), "unknown durability mode") {
		t.Fatalf("New error = %v, want it to mention unknown durability mode", err)
	}
	if _, statErr := os.Stat(dataDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("data dir %q exists after rejected config (stat err = %v)", dataDir, statErr)
	}
}

// stopTestServer stops a server built by New in these tests, which may never
// have been started.
func stopTestServer(t *testing.T, srv *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Errorf("Stop: %v", err)
	}
}
