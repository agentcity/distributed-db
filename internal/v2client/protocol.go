package v2client

import (
	"context"
	"errors"
	"fmt"
	"strings"

	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/version"
)

// ProtocolInfo returns the most recent protocol handshake record for addr.
//
// A record is created by any RPC to that peer (meta or KV) and is keyed by the
// gRPC target address the client dialed. The second result is false when no
// handshake has completed with addr yet; a status command can force one with
// ProbeProtocol.
func (c *Client) ProtocolInfo(addr string) (version.PeerRecord, bool) {
	if c == nil {
		return version.PeerRecord{}, false
	}
	return c.protocol.Lookup(addr)
}

// ProtocolRecords returns all peer handshake records sorted by address.
func (c *Client) ProtocolRecords() []version.PeerRecord {
	if c == nil {
		return nil
	}
	return c.protocol.Snapshot()
}

// NegotiatedVersion returns the peer version from the most recent compatible
// handshake. A rejected or incompatible peer is not reported as negotiated.
func (c *Client) NegotiatedVersion() (version.Version, bool) {
	if c == nil {
		return version.Version{}, false
	}
	var best version.PeerRecord
	found := false
	for _, rec := range c.protocol.Snapshot() {
		if !rec.Compatible {
			continue
		}
		if !found || rec.LastSeen.After(best.LastSeen) {
			best, found = rec, true
		}
	}
	if !found {
		return version.Version{}, false
	}
	return best.PeerVersion, true
}

// ProtocolSummary renders the negotiated protocol state for a status command.
// It contains version numbers, feature masks and compatibility warnings only:
// credentials and request data are never included.
func (c *Client) ProtocolSummary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "protocol local=%s", version.Current)
	recs := c.ProtocolRecords()
	if len(recs) == 0 {
		b.WriteString(" (no peer handshake yet)")
		return b.String()
	}
	for _, rec := range recs {
		b.WriteString("; ")
		b.WriteString(rec.String())
	}
	return b.String()
}

// ProbeProtocol forces a handshake with a KV node by issuing the read-only
// Status RPC and returns the resulting record. It is intended for status
// commands that must show a peer version even when no data operation has run
// yet; ordinary client calls populate the same registry automatically.
//
// A client built by the public constructors always has the handshake enabled.
// A zero-value Client assembled directly by tests does not, and this method
// reports that instead of pretending the peer is legacy.
func (c *Client) ProbeProtocol(ctx context.Context, addr string) (version.PeerRecord, error) {
	if c == nil || c.protocol == nil {
		return version.PeerRecord{}, errors.New("v2client: protocol handshake is not enabled")
	}
	cl, err := c.node(addr)
	if err != nil {
		return version.PeerRecord{}, err
	}
	if _, err := cl.Status(ctx, &pbv2.StatusRequest{}); err != nil {
		rec, _ := c.ProtocolInfo(addr)
		return rec, fmt.Errorf("v2client: protocol probe %s: %w", addr, err)
	}
	rec, ok := c.ProtocolInfo(addr)
	if !ok {
		return version.PeerRecord{}, fmt.Errorf("v2client: no protocol record for %s after handshake", addr)
	}
	return rec, nil
}
