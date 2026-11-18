package version

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors returned by Evaluate. Callers use errors.Is to classify an
// incompatible handshake without parsing text. The corresponding gRPC status
// code is FailedPrecondition.
var (
	// ErrMajorMismatch means the peer speaks a different, incompatible major
	// protocol version. Major versions change only when the wire contract
	// breaks, so there is no safe downgrade path.
	ErrMajorMismatch = errors.New("version: incompatible protocol major version")
	// ErrUnsupportedMinor means the peer is older than the N-1 minor window.
	// During a rolling upgrade this rejects a node that skipped a release,
	// rather than letting it corrupt state with a protocol the cluster no
	// longer supports.
	ErrUnsupportedMinor = errors.New("version: unsupported protocol minor version")
)

// Level classifies the compatibility outcome of one handshake. It is a
// reporting aid; Compatible and the returned error are authoritative.
type Level int

const (
	// LevelCompatible means the same major and minor, with no warnings.
	LevelCompatible Level = iota
	// LevelOlderMinor means the peer is one minor behind (N-1), accepted with
	// a warning for the duration of the rolling upgrade.
	LevelOlderMinor
	// LevelNewerMinor means the peer is ahead by at least one minor. It is
	// accepted for forward compatibility; unknown features are ignored.
	LevelNewerMinor
	// LevelLegacy means the peer sent no handshake metadata at all and was
	// assumed to speak LegacyVersion.
	LevelLegacy
	// LevelMajorMismatch means the major versions differ; the handshake is
	// rejected.
	LevelMajorMismatch
	// LevelTooOld means the major matches but the peer minor is below the N-1
	// window; the handshake is rejected.
	LevelTooOld
)

func (l Level) String() string {
	switch l {
	case LevelCompatible:
		return "compatible"
	case LevelOlderMinor:
		return "older-minor"
	case LevelNewerMinor:
		return "newer-minor"
	case LevelLegacy:
		return "legacy"
	case LevelMajorMismatch:
		return "major-mismatch"
	case LevelTooOld:
		return "too-old"
	default:
		return fmt.Sprintf("level(%d)", int(l))
	}
}

// PeerInfo is the protocol information a peer advertised. Legacy is true when
// the peer sent no version metadata; Version is then LegacyVersion and
// Features is empty.
type PeerInfo struct {
	Version  Version
	Features Feature
	Legacy   bool
}

// Report is the result of evaluating one peer against the local policy. It is
// always populated, including when the handshake is rejected, so it can be
// recorded and shown by status commands.
type Report struct {
	Local           Version
	LocalFeatures   Feature
	Peer            PeerInfo
	CommonFeatures  Feature
	UnknownFeatures Feature
	Level           Level
	Compatible      bool
	Warnings        []string
	Reason          string
}

// IncompatibleError describes a rejected handshake. It carries only version
// numbers and a fixed policy reason; peer metadata, credentials and request
// payloads are never included.
type IncompatibleError struct {
	Local  Version
	Peer   Version
	Level  Level
	Detail string
	Err    error
}

func (e *IncompatibleError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Detail != "" {
		return e.Detail
	}
	return fmt.Sprintf("%v: local %s, peer %s", e.Err, e.Local, e.Peer)
}

// Unwrap exposes ErrMajorMismatch or ErrUnsupportedMinor to errors.Is.
func (e *IncompatibleError) Unwrap() error { return e.Err }

// Evaluate applies the compatibility policy to one peer. A nil error means the
// peer is compatible (possibly with warnings for N-1, a newer minor or a
// legacy peer); a non-nil error is always an *IncompatibleError wrapping
// ErrMajorMismatch or ErrUnsupportedMinor.
func Evaluate(local Version, localFeatures Feature, peer PeerInfo) (Report, error) {
	rep := Report{
		Local:           local,
		LocalFeatures:   localFeatures,
		Peer:            peer,
		CommonFeatures:  peer.Features & localFeatures,
		UnknownFeatures: peer.Features &^ localFeatures,
		Compatible:      true,
	}

	if peer.Version.Major != local.Major {
		rep.Compatible = false
		rep.Level = LevelMajorMismatch
		err := &IncompatibleError{
			Local: local,
			Peer:  peer.Version,
			Level: LevelMajorMismatch,
			Detail: fmt.Sprintf(
				"%v: local %s, peer %s (major versions must match; mixed-major clusters are unsupported)",
				ErrMajorMismatch, local, peer.Version),
			Err: ErrMajorMismatch,
		}
		rep.Reason = err.Error()
		return rep, err
	}

	floor := SupportedMinorFloor(local)
	if peer.Version.Minor < floor {
		rep.Compatible = false
		rep.Level = LevelTooOld
		err := &IncompatibleError{
			Local: local,
			Peer:  peer.Version,
			Level: LevelTooOld,
			Detail: fmt.Sprintf(
				"%v: local %s supports minor >= %d, peer %s is too old",
				ErrUnsupportedMinor, local, floor, peer.Version),
			Err: ErrUnsupportedMinor,
		}
		rep.Reason = err.Error()
		return rep, err
	}

	switch {
	case peer.Legacy:
		rep.Level = LevelLegacy
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"peer sent no protocol metadata; assuming legacy protocol %s (supported window: minor >= %d)",
			peer.Version, floor))
	case peer.Version.Minor < local.Minor:
		rep.Level = LevelOlderMinor
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"peer protocol %s is older than local %s; N-1 minor is supported (minor >= %d)",
			peer.Version, local, floor))
	case peer.Version.Minor > local.Minor:
		rep.Level = LevelNewerMinor
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"peer protocol %s is newer than local %s; accepting for forward compatibility and ignoring unknown features",
			peer.Version, local))
	default:
		rep.Level = LevelCompatible
	}
	return rep, nil
}

// String renders one report without peer-provided text. It is safe for logs and
// status output: only version numbers, feature masks and policy warnings are
// included.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "local=%s peer=%s compatible=%t level=%s legacy=%t common_features=0x%x unknown_features=0x%x",
		r.Local, r.Peer.Version, r.Compatible, r.Level, r.Peer.Legacy,
		uint64(r.CommonFeatures), uint64(r.UnknownFeatures))
	if len(r.Warnings) > 0 {
		fmt.Fprintf(&b, " warnings=%q", strings.Join(r.Warnings, "; "))
	}
	if r.Reason != "" {
		fmt.Fprintf(&b, " reason=%q", r.Reason)
	}
	return b.String()
}
