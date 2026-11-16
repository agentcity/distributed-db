package kvserver

import (
	"bytes"
	"errors"
	"fmt"
	"sort"

	"distributed-db/internal/raft"
)

// RangeConfig describes one range hosted by a Server.
//
// Start is inclusive and End is exclusive. A nil Start means the beginning of
// the keyspace and a nil End means the end of the keyspace. Peers lists the
// other Raft members of the range (their RaftAddr); it does not include the
// local node.
type RangeConfig struct {
	ID    uint64   `json:"id"`
	Start []byte   `json:"start,omitempty"`
	End   []byte   `json:"end,omitempty"`
	Peers []string `json:"peers,omitempty"`
	Epoch uint64   `json:"epoch,omitempty"`
}

var (
	// ErrRangeNotFound is returned when a RangeHost does not host the
	// requested range (or the key does not belong to any hosted range).
	ErrRangeNotFound = errors.New("kvserver: range not hosted")
	// ErrRangeMismatch is returned when an operation spans more than one
	// range or its interval crosses the selected range's boundary.
	ErrRangeMismatch = errors.New("kvserver: range mismatch")
	// ErrRangeFrozen is returned while split/merge is copying a range.
	ErrRangeFrozen = errors.New("kvserver: range is splitting/merging")
	// ErrStaleEpoch is returned when a client used an old range map version.
	ErrStaleEpoch = errors.New("kvserver: stale range epoch")
)

// normalizeRanges expands the legacy single-range Config fields when Ranges is
// empty and validates the configured ranges. The result is sorted by range
// start so Groups and key routing are deterministic.
func normalizeRanges(cfg Config) ([]RangeConfig, error) {
	if len(cfg.Ranges) == 0 {
		id := cfg.RangeID
		if id == 0 {
			id = 1
		}
		epoch := cfg.RangeEpoch
		if epoch == 0 {
			epoch = 1
		}
		return []RangeConfig{{
			ID:    id,
			Start: cloneBytes(cfg.RangeStart),
			End:   cloneBytes(cfg.RangeEnd),
			Peers: cloneStrings(cfg.Peers),
			Epoch: epoch,
		}}, nil
	}

	out := make([]RangeConfig, 0, len(cfg.Ranges))
	seen := make(map[uint64]struct{}, len(cfg.Ranges))
	for i, rc := range cfg.Ranges {
		if rc.ID == 0 {
			return nil, fmt.Errorf("kvserver: ranges[%d]: ID must be non-zero", i)
		}
		if _, ok := seen[rc.ID]; ok {
			return nil, fmt.Errorf("kvserver: duplicate range ID %d", rc.ID)
		}
		seen[rc.ID] = struct{}{}
		if rc.Start != nil && rc.End != nil && bytes.Compare(rc.Start, rc.End) >= 0 {
			return nil, fmt.Errorf("kvserver: range %d: Start must be before End", rc.ID)
		}
		epoch := rc.Epoch
		if epoch == 0 {
			epoch = 1
		}
		out = append(out, RangeConfig{
			ID:    rc.ID,
			Start: cloneBytes(rc.Start),
			End:   cloneBytes(rc.End),
			Peers: cloneStrings(rc.Peers),
			Epoch: epoch,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		return compareRangeStart(out[i].Start, out[j].Start) < 0
	})
	for i := 1; i < len(out); i++ {
		prev, cur := out[i-1], out[i]
		if prev.End == nil || cur.Start == nil || bytes.Compare(cur.Start, prev.End) < 0 {
			return nil, fmt.Errorf("kvserver: ranges %d and %d overlap", prev.ID, cur.ID)
		}
	}
	return out, nil
}

// compareRangeStart orders ranges by their inclusive start; nil sorts first.
func compareRangeStart(a, b []byte) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	default:
		return bytes.Compare(a, b)
	}
}

// Groups returns the hosted range IDs in range order (ascending start key).
func (s *Server) Groups() []uint64 {
	s.hostMu.RLock()
	defer s.hostMu.RUnlock()
	out := make([]uint64, len(s.groupIDs))
	copy(out, s.groupIDs)
	return out
}

// Ranges returns a copy of the hosted range configurations in range order.
func (s *Server) Ranges() []RangeConfig {
	s.hostMu.RLock()
	defer s.hostMu.RUnlock()
	out := make([]RangeConfig, 0, len(s.cfg.Ranges))
	for _, rc := range s.cfg.Ranges {
		epoch := rc.Epoch
		if epoch == 0 {
			epoch = 1
		}
		out = append(out, RangeConfig{
			ID:    rc.ID,
			Start: cloneBytes(rc.Start),
			End:   cloneBytes(rc.End),
			Peers: cloneStrings(rc.Peers),
			Epoch: epoch,
		})
	}
	return out
}

// GroupForID returns the Raft node of the requested range. ID 0 selects the
// default (first) range for backward compatibility with single-range RPCs.
func (s *Server) GroupForID(id uint64) (*raft.Node, error) {
	g, err := s.groupForID(id)
	if err != nil {
		return nil, err
	}
	return g.node, nil
}

// GroupForKey returns the Raft node responsible for key.
func (s *Server) GroupForKey(key []byte) (*raft.Node, error) {
	g, err := s.groupForKey(key)
	if err != nil {
		return nil, err
	}
	return g.node, nil
}

func (s *Server) groupForID(id uint64) (*rangeGroup, error) {
	s.hostMu.RLock()
	defer s.hostMu.RUnlock()
	if id == 0 {
		id = s.defaultID
	}
	g, ok := s.groups[id]
	if !ok {
		return nil, fmt.Errorf("%w: id %d", ErrRangeNotFound, id)
	}
	return g, nil
}

func (s *Server) groupForKey(key []byte) (*rangeGroup, error) {
	s.hostMu.RLock()
	defer s.hostMu.RUnlock()
	for _, id := range s.groupIDs {
		if g := s.groups[id]; g.containsKey(key) {
			return g, nil
		}
	}
	return nil, fmt.Errorf("%w: key %q", ErrRangeNotFound, key)
}

// groupForKeys resolves the group of the first key and verifies that every
// other key belongs to the same range.
func (s *Server) groupForKeys(keys [][]byte) (*rangeGroup, error) {
	if len(keys) == 0 {
		g := s.defaultGroup()
		if g == nil {
			return nil, fmt.Errorf("%w: no default range", ErrRangeNotFound)
		}
		return g, nil
	}
	g, err := s.groupForKey(keys[0])
	if err != nil {
		return nil, err
	}
	for _, key := range keys[1:] {
		if !g.containsKey(key) {
			return nil, fmt.Errorf("%w: key %q is outside range %d", ErrRangeMismatch, key, g.cfg.ID)
		}
	}
	return g, nil
}

func (s *Server) defaultGroup() *rangeGroup {
	s.hostMu.RLock()
	defer s.hostMu.RUnlock()
	if s.groups == nil {
		return nil
	}
	return s.groups[s.defaultID]
}

// defaultLeader returns the default range's leader hint. It is used for error
// responses whose request did not resolve to a group.
func (s *Server) defaultLeader() string {
	if g := s.defaultGroup(); g != nil && g.node != nil {
		return g.node.Leader()
	}
	return ""
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string(nil), in...)
}
