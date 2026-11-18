package version

import (
	"strings"
	"testing"
)

func TestCurrentVersion(t *testing.T) {
	if Current.Major != CurrentMajor || Current.Minor != CurrentMinor || Current.Patch != CurrentPatch {
		t.Fatalf("Current = %s, consts = %d.%d.%d", Current, CurrentMajor, CurrentMinor, CurrentPatch)
	}
	if got := Current.String(); got != "2.1.0" {
		t.Fatalf("Current.String() = %q, want 2.1.0", got)
	}
	if LegacyVersion.Compare(Current) >= 0 {
		t.Fatalf("LegacyVersion %s should be older than Current %s", LegacyVersion, Current)
	}
	if floor := SupportedMinorFloor(Current); floor != CurrentMinor-SupportedMinorBack {
		t.Fatalf("SupportedMinorFloor = %d, want %d", floor, CurrentMinor-SupportedMinorBack)
	}
	if floor := SupportedMinorFloor(Version{Major: 2, Minor: 0}); floor != 0 {
		t.Fatalf("SupportedMinorFloor(minor=0) = %d, want 0", floor)
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in   string
		want Version
		ok   bool
	}{
		{in: "2.1.0", want: Version{2, 1, 0}, ok: true},
		{in: "v1.2.3", want: Version{1, 2, 3}, ok: true},
		{in: " 2.1.0\n", want: Version{2, 1, 0}, ok: true},
		{in: "0.0.0", want: Version{0, 0, 0}, ok: true},
		{in: "2.1", ok: false},
		{in: "2.1.0.1", ok: false},
		{in: "2..0", ok: false},
		{in: "2.1.x", ok: false},
		{in: "-1.0.0", ok: false},
		{in: "", ok: false},
		{in: "v", ok: false},
	}
	for _, tc := range tests {
		got, err := Parse(tc.in)
		if tc.ok {
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("Parse(%q) = %s, want %s", tc.in, got, tc.want)
			}
			continue
		}
		if err == nil {
			t.Fatalf("Parse(%q) = %s, want error", tc.in, got)
		}
	}
}

func TestVersionCompare(t *testing.T) {
	tests := []struct {
		a, b Version
		want int
	}{
		{Version{2, 1, 0}, Version{2, 1, 0}, 0},
		{Version{2, 1, 0}, Version{2, 1, 1}, -1},
		{Version{2, 2, 0}, Version{2, 1, 9}, 1},
		{Version{1, 9, 9}, Version{2, 0, 0}, -1},
	}
	for _, tc := range tests {
		if got := tc.a.Compare(tc.b); got != tc.want {
			t.Fatalf("%s.Compare(%s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestFeatureBits(t *testing.T) {
	if FeatureTxnAtomic != 1 || FeatureSnapshotChunk != 2 || FeatureRangeEpoch != 4 ||
		FeatureBackupScan != 8 || FeatureLockWait != 16 {
		t.Fatalf("feature bits are not contiguous: %x %x %x %x %x",
			FeatureTxnAtomic, FeatureSnapshotChunk, FeatureRangeEpoch, FeatureBackupScan, FeatureLockWait)
	}
	if CurrentFeatures != 0x1f {
		t.Fatalf("CurrentFeatures = %#x, want 0x1f", CurrentFeatures)
	}
	if !CurrentFeatures.Has(FeatureTxnAtomic) {
		t.Fatal("CurrentFeatures does not include FeatureTxnAtomic")
	}
	if FeatureNone.Has(FeatureTxnAtomic) {
		t.Fatal("FeatureNone.Has(FeatureTxnAtomic) = true")
	}
	if got := FeatureTxnAtomic.String(); !strings.Contains(got, "txn-atomic") {
		t.Fatalf("FeatureTxnAtomic.String() = %q", got)
	}
	unknown := CurrentFeatures | Feature(1<<40)
	if got := unknown.String(); !strings.Contains(got, "unknown:0x") {
		t.Fatalf("unknown feature String() = %q, want unknown mask", got)
	}

	encoded := FormatFeatures(CurrentFeatures)
	parsed, err := ParseFeatures(encoded)
	if err != nil {
		t.Fatalf("ParseFeatures(%q): %v", encoded, err)
	}
	if parsed != CurrentFeatures {
		t.Fatalf("ParseFeatures round trip = %x, want %x", parsed, CurrentFeatures)
	}
	if f, err := ParseFeatures(""); err != nil || f != FeatureNone {
		t.Fatalf("ParseFeatures(empty) = %v, %v; want none, nil", f, err)
	}
	if _, err := ParseFeatures("0x1f"); err == nil {
		t.Fatal("ParseFeatures accepted a non-decimal mask")
	}
	if _, err := ParseFeatures("-1"); err == nil {
		t.Fatal("ParseFeatures accepted a negative mask")
	}
}

func TestPeerFromMetadataMissingAndMalformed(t *testing.T) {
	// nil metadata is the pre-handshake case, not an error.
	peer, err := PeerFromMetadata(nil)
	if err != nil {
		t.Fatalf("PeerFromMetadata(nil): %v", err)
	}
	if !peer.Legacy || peer.Version != LegacyVersion || peer.Features != FeatureNone {
		t.Fatalf("PeerFromMetadata(nil) = %+v, want legacy %s", peer, LegacyVersion)
	}

	// An empty version value is also legacy, not malformed.
	peer, err = PeerFromMetadata(map[string][]string{VersionMetadataKey: {""}})
	if err != nil || !peer.Legacy {
		t.Fatalf("empty version = %+v, %v; want legacy", peer, err)
	}

	if _, err := PeerFromMetadata(map[string][]string{VersionMetadataKey: {"not-a-version"}}); err == nil {
		t.Fatal("PeerFromMetadata accepted malformed version")
	}
	if _, err := PeerFromMetadata(map[string][]string{
		VersionMetadataKey:  {"2.1.0"},
		FeaturesMetadataKey: {"not-a-mask"},
	}); err == nil {
		t.Fatal("PeerFromMetadata accepted malformed features")
	}
}
