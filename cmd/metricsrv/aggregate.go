package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"distributed-db/internal/metrics"
)

// maxSnapshotBytes bounds one federated JSON snapshot so a misbehaving peer
// cannot exhaust this process's memory.
const maxSnapshotBytes = 16 << 20

// Aggregator holds snapshots fetched from JSON files or peer metricsrv
// instances. Every aggregated series gets a source=<name> label so it cannot
// collide with local metrics that have the same name. Aggregator is safe for
// concurrent use.
type Aggregator struct {
	mu      sync.RWMutex
	sources map[string]*metrics.Snapshot
	order   []string
}

// NewAggregator returns an empty aggregator.
func NewAggregator() *Aggregator {
	return &Aggregator{sources: make(map[string]*metrics.Snapshot)}
}

// SetSnapshot replaces the snapshot for name. An empty name defaults to
// "upstream". The snapshot is treated as immutable by the aggregator.
func (a *Aggregator) SetSnapshot(name string, snap *metrics.Snapshot) {
	if name == "" {
		name = "upstream"
	}
	if snap == nil {
		snap = &metrics.Snapshot{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.sources[name]; !ok {
		a.order = append(a.order, name)
	}
	a.sources[name] = snap
}

// Snapshot returns the merged view of all sources, with source labels added.
// It returns an empty (non-nil) snapshot when no source is configured.
func (a *Aggregator) Snapshot() *metrics.Snapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()

	out := &metrics.Snapshot{Metrics: make([]metrics.MetricValue, 0)}
	for _, name := range a.order {
		snap := a.sources[name]
		if snap == nil {
			continue
		}
		out.Merge(snap.WithLabel("source", name))
	}
	out.Sort()
	return out
}

// LoadFile reads a JSON snapshot from path and stores it under name. An empty
// name uses the file's base name.
func (a *Aggregator) LoadFile(name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("metricsrv: open snapshot file: %w", err)
	}
	defer f.Close()

	snap, err := metrics.ReadSnapshot(f)
	if err != nil {
		return fmt.Errorf("metricsrv: read snapshot file %s: %w", path, err)
	}
	if name == "" {
		name = filepath.Base(path)
	}
	a.SetSnapshot(name, snap)
	return nil
}

// Fetch downloads one JSON snapshot from url (normally /debug/snapshot on a
// peer metricsrv) and stores it under name. A nil client uses a default client
// with a 10s timeout.
func (a *Aggregator) Fetch(ctx context.Context, client *http.Client, name, url string) error {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("metricsrv: create upstream request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("metricsrv: fetch snapshot from %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
		return fmt.Errorf("metricsrv: fetch snapshot from %s: status %s", url, resp.Status)
	}

	snap, err := metrics.ReadSnapshot(io.LimitReader(resp.Body, maxSnapshotBytes))
	if err != nil {
		return fmt.Errorf("metricsrv: decode snapshot from %s: %w", url, err)
	}
	if name == "" {
		name = "upstream"
	}
	a.SetSnapshot(name, snap)
	return nil
}

// refreshLoop polls one upstream snapshot URL until ctx is canceled. It logs
// failures but keeps the last successful snapshot.
func refreshLoop(ctx context.Context, agg *Aggregator, client *http.Client, name, url string, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := agg.Fetch(fetchCtx, client, name, url)
			cancel()
			if err != nil {
				log.Warn("upstream metrics refresh failed", "url", url, "error", err)
				continue
			}
			log.Debug("upstream metrics refreshed", "url", url, "source", name)
		}
	}
}
