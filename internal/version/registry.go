package version

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// PeerRecord is the server- or client-side record of one completed handshake.
// Addr is the peer address used as the registry key (a dial target on the
// client, the remote address on the server). All fields come from the version
// handshake; credentials and request payloads are never stored.
type PeerRecord struct {
	Addr            string
	LocalVersion    Version
	PeerVersion     Version
	PeerFeatures    Feature
	CommonFeatures  Feature
	UnknownFeatures Feature
	Legacy          bool
	Compatible      bool
	Level           Level
	Warnings        []string
	Reason          string
	FirstSeen       time.Time
	LastSeen        time.Time
}

// PeerRecord converts a Report into a registry record for addr. Timestamps are
// filled in by Registry.Record.
func (r Report) PeerRecord(addr string) PeerRecord {
	return PeerRecord{
		Addr:            addr,
		LocalVersion:    r.Local,
		PeerVersion:     r.Peer.Version,
		PeerFeatures:    r.Peer.Features,
		CommonFeatures:  r.CommonFeatures,
		UnknownFeatures: r.UnknownFeatures,
		Legacy:          r.Peer.Legacy,
		Compatible:      r.Compatible,
		Level:           r.Level,
		Warnings:        append([]string(nil), r.Warnings...),
		Reason:          r.Reason,
	}
}

// String renders a record without credentials or peer-provided text. It is
// suitable for `kv2 status` output and logs.
func (p PeerRecord) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: local=%s peer=%s compatible=%t level=%s legacy=%t common_features=0x%x",
		p.Addr, p.LocalVersion, p.PeerVersion, p.Compatible, p.Level, p.Legacy,
		uint64(p.CommonFeatures))
	if p.UnknownFeatures != 0 {
		fmt.Fprintf(&b, " unknown_features=0x%x", uint64(p.UnknownFeatures))
	}
	if len(p.Warnings) > 0 {
		fmt.Fprintf(&b, " warnings=%q", strings.Join(p.Warnings, "; "))
	}
	if p.Reason != "" {
		fmt.Fprintf(&b, " reason=%q", p.Reason)
	}
	return b.String()
}

// Registry is a concurrency-safe map of peer address to the most recent
// handshake record. Servers use it to expose "which versions have I seen?" and
// clients use it to expose the negotiated version of every connected peer.
type Registry struct {
	mu    sync.Mutex
	peers map[string]PeerRecord
	now   func() time.Time
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{peers: make(map[string]PeerRecord), now: time.Now}
}

// Record stores rec, preserving FirstSeen for an address already present and
// replacing the rest with the newest handshake. It is a no-op on a nil
// registry, which keeps the interceptor safe for callers that do not need
// peer tracking.
func (r *Registry) Record(rec PeerRecord) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	if old, ok := r.peers[rec.Addr]; ok {
		rec.FirstSeen = old.FirstSeen
	} else {
		rec.FirstSeen = now()
	}
	rec.LastSeen = now()
	if rec.Warnings != nil {
		rec.Warnings = append([]string(nil), rec.Warnings...)
	}
	if r.peers == nil {
		r.peers = make(map[string]PeerRecord)
	}
	r.peers[rec.Addr] = rec
}

// Lookup returns the most recent record for addr.
func (r *Registry) Lookup(addr string) (PeerRecord, bool) {
	if r == nil {
		return PeerRecord{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.peers[addr]
	if ok && rec.Warnings != nil {
		rec.Warnings = append([]string(nil), rec.Warnings...)
	}
	return rec, ok
}

// Snapshot returns all records sorted by address. The returned slice and
// warnings are copies.
func (r *Registry) Snapshot() []PeerRecord {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]PeerRecord, 0, len(r.peers))
	for _, rec := range r.peers {
		if rec.Warnings != nil {
			rec.Warnings = append([]string(nil), rec.Warnings...)
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}

// Latest returns the record with the newest LastSeen. It is used by callers
// that need one "negotiated version" value rather than all peers.
func (r *Registry) Latest() (PeerRecord, bool) {
	if r == nil {
		return PeerRecord{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var best PeerRecord
	found := false
	for _, rec := range r.peers {
		if !found || rec.LastSeen.After(best.LastSeen) {
			best, found = rec, true
		}
	}
	if found && best.Warnings != nil {
		best.Warnings = append([]string(nil), best.Warnings...)
	}
	return best, found
}

// Len returns the number of distinct peers observed.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.peers)
}
