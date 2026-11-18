// Package version defines the semantic protocol version, feature bits and
// compatibility policy used by the v2 protocol handshake.
//
// The handshake is wire-additive. A client sends its protocol version and
// feature bitmask as gRPC metadata on every RPC; a participating server
// validates the peer, records it and echoes its own version in the response
// headers. Servers that predate the handshake send no response metadata and are
// treated as the legacy protocol version. No protobuf messages are changed, so
// old clients, old servers and mixed-version clusters keep the existing wire
// format.
//
// The policy is deliberately simple:
//
//   - the major version must match exactly; a mismatch is an error;
//   - minor N and N-1 are supported (SupportedMinorBack), anything older is an
//     error, and a newer minor is accepted for forward compatibility;
//   - patch differences are ignored;
//   - unknown feature bits are ignored (they only shrink the common set);
//   - a peer that sends no metadata is treated as LegacyVersion and accepted
//     when that version is inside the supported minor window.
package version

import (
	"fmt"
	"strconv"
	"strings"
)

// Current protocol version components. They are constants so version checks
// cannot be changed at runtime by accidentally assigning to a variable.
const (
	CurrentMajor uint32 = 2
	CurrentMinor uint32 = 1
	CurrentPatch uint32 = 0
)

// SupportedMinorBack is the number of minor revisions older than Current that
// remain supported. N-1 is the rolling-upgrade window: during an upgrade every
// node must be able to talk to a node that still runs the previous minor.
const SupportedMinorBack uint32 = 1

// Current is the protocol version implemented by this build. It is derived
// from the constants above and must be treated as read-only.
var Current = Version{Major: CurrentMajor, Minor: CurrentMinor, Patch: CurrentPatch}

// LegacyVersion is the protocol version assumed for a peer that sends no
// handshake metadata at all (a client or server built before the handshake).
// It is the last protocol revision released before handshake support, which
// places it at the N-1 minor for the current build.
var LegacyVersion = Version{Major: 2, Minor: 0, Patch: 0}

// Version is a semantic protocol version: major.minor.patch. The major version
// changes only for incompatible wire or behavioral changes; the minor version
// changes for wire-additive features; the patch version changes for fixes that
// do not alter the protocol contract.
type Version struct {
	Major uint32
	Minor uint32
	Patch uint32
}

// IsZero reports whether v is the zero version. Zero is never a valid
// on-the-wire version; callers that need the legacy default use
// LegacyVersion.
func (v Version) IsZero() bool {
	return v == Version{}
}

// Compare returns -1, 0 or 1 when v is older than, equal to or newer than o.
func (v Version) Compare(o Version) int {
	switch {
	case v.Major != o.Major:
		return compareUint32(v.Major, o.Major)
	case v.Minor != o.Minor:
		return compareUint32(v.Minor, o.Minor)
	default:
		return compareUint32(v.Patch, o.Patch)
	}
}

func compareUint32(a, b uint32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// String renders the version as "major.minor.patch".
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Parse parses a "major.minor.patch" string. An optional leading "v" is
// accepted, and surrounding whitespace is ignored. All three components are
// required: a partial version cannot be interpreted unambiguously by a peer.
func Parse(s string) (Version, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Version{}, fmt.Errorf("version: empty protocol version")
	}
	raw = strings.TrimPrefix(raw, "v")
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("version: invalid protocol version %q: want major.minor.patch", s)
	}
	var out Version
	fields := []*uint32{&out.Major, &out.Minor, &out.Patch}
	for i, part := range parts {
		if part == "" {
			return Version{}, fmt.Errorf("version: invalid protocol version %q: empty component", s)
		}
		n, err := strconv.ParseUint(part, 10, 32)
		if err != nil {
			return Version{}, fmt.Errorf("version: invalid protocol version %q: %w", s, err)
		}
		*fields[i] = uint32(n)
	}
	return out, nil
}

// SupportedMinorFloor returns the oldest minor version that remains compatible
// with local under the N-1 policy. A local minor of zero has no older supported
// minor, so the floor is zero.
func SupportedMinorFloor(local Version) uint32 {
	if local.Minor < SupportedMinorBack {
		return 0
	}
	return local.Minor - SupportedMinorBack
}

// Feature is a bitmask of optional, wire-additive protocol capabilities. A
// feature bit means "this process understands this capability"; it is
// advisory, not a switch that changes the meaning of existing messages.
// Unknown bits are ignored so a newer peer never breaks an older one.
type Feature uint64

// FeatureNone is the empty feature set.
const FeatureNone Feature = 0

const (
	// FeatureTxnAtomic means Prewrite/Commit/Rollback transactional writes.
	FeatureTxnAtomic Feature = 1 << iota
	// FeatureSnapshotChunk means chunked Raft snapshot transfer.
	FeatureSnapshotChunk
	// FeatureRangeEpoch means range-epoch validation and propagation.
	FeatureRangeEpoch
	// FeatureBackupScan means the lock-aware ScanForBackup RPC.
	FeatureBackupScan
	// FeatureLockWait means bounded lock-wait conflict resolution.
	FeatureLockWait

	// CurrentFeatures is the feature set implemented by this build.
	CurrentFeatures = FeatureTxnAtomic | FeatureSnapshotChunk |
		FeatureRangeEpoch | FeatureBackupScan | FeatureLockWait
)

type featureName struct {
	bit  Feature
	name string
}

// knownFeatures is the stable, human-readable order used by String.
var knownFeatures = []featureName{
	{FeatureTxnAtomic, "txn-atomic"},
	{FeatureSnapshotChunk, "snapshot-chunk"},
	{FeatureRangeEpoch, "range-epoch"},
	{FeatureBackupScan, "backup-scan"},
	{FeatureLockWait, "lock-wait"},
}

// Has reports whether all bits in x are set in f.
func (f Feature) Has(x Feature) bool {
	return f&x == x
}

// String renders known feature names, plus any unknown bits as a hex mask. It
// is intended for status output and logs, never for wire encoding.
func (f Feature) String() string {
	if f == FeatureNone {
		return "none"
	}
	var parts []string
	for _, kf := range knownFeatures {
		if f.Has(kf.bit) {
			parts = append(parts, kf.name)
		}
	}
	if unknown := f &^ CurrentFeatures; unknown != 0 {
		parts = append(parts, fmt.Sprintf("unknown:0x%x", uint64(unknown)))
	}
	return strings.Join(parts, "|")
}

// ParseFeatures parses a decimal uint64 feature bitmask. An empty value means
// no features. Hex is intentionally not accepted so the wire representation
// stays canonical; unknown bits are preserved and interpreted later.
func ParseFeatures(s string) (Feature, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return FeatureNone, nil
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return FeatureNone, fmt.Errorf("version: invalid feature mask %q: %w", s, err)
	}
	return Feature(n), nil
}

// FormatFeatures renders a feature bitmask in its canonical wire form.
func FormatFeatures(f Feature) string {
	return strconv.FormatUint(uint64(f), 10)
}
