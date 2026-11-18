package version

import (
	"errors"
	"strings"
	"testing"
)

func TestEvaluateCompatibilityPolicy(t *testing.T) {
	const unknownBit Feature = 1 << 62

	tests := []struct {
		name         string
		local        Version
		localFeats   Feature
		peer         PeerInfo
		wantLevel    Level
		wantOK       bool
		wantErr      error
		wantWarnText string
		wantCommon   Feature
		wantUnknown  Feature
	}{
		{
			name:       "same version",
			local:      Current,
			localFeats: CurrentFeatures,
			peer:       PeerInfo{Version: Current, Features: CurrentFeatures},
			wantLevel:  LevelCompatible,
			wantOK:     true,
			wantCommon: CurrentFeatures,
		},
		{
			name:         "older minor is N-1 and supported",
			local:        Version{2, 1, 0},
			localFeats:   CurrentFeatures,
			peer:         PeerInfo{Version: Version{2, 0, 0}, Features: CurrentFeatures},
			wantLevel:    LevelOlderMinor,
			wantOK:       true,
			wantWarnText: "older",
			wantCommon:   CurrentFeatures,
		},
		{
			name:         "newer minor is forward compatible",
			local:        Version{2, 1, 0},
			localFeats:   CurrentFeatures,
			peer:         PeerInfo{Version: Version{2, 2, 0}, Features: CurrentFeatures | unknownBit},
			wantLevel:    LevelNewerMinor,
			wantOK:       true,
			wantWarnText: "newer",
			wantCommon:   CurrentFeatures,
			wantUnknown:  unknownBit,
		},
		{
			name:       "legacy peer assumes the pre-handshake version",
			local:      Current,
			localFeats: CurrentFeatures,
			peer:       PeerInfo{Version: LegacyVersion, Legacy: true},
			wantLevel:  LevelLegacy,
			wantOK:     true,
			// Current is 2.1 and LegacyVersion is 2.0, so legacy is also N-1.
			wantWarnText: "legacy",
		},
		{
			name:       "patch is ignored",
			local:      Version{2, 1, 0},
			localFeats: CurrentFeatures,
			peer:       PeerInfo{Version: Version{2, 1, 7}, Features: CurrentFeatures},
			wantLevel:  LevelCompatible,
			wantOK:     true,
			wantCommon: CurrentFeatures,
		},
		{
			name:        "unknown features are ignored",
			local:       Version{2, 1, 0},
			localFeats:  FeatureTxnAtomic,
			peer:        PeerInfo{Version: Version{2, 1, 0}, Features: FeatureTxnAtomic | unknownBit},
			wantLevel:   LevelCompatible,
			wantOK:      true,
			wantCommon:  FeatureTxnAtomic,
			wantUnknown: unknownBit,
		},
		{
			name:       "missing features are an empty set",
			local:      Version{2, 1, 0},
			localFeats: CurrentFeatures,
			peer:       PeerInfo{Version: Version{2, 1, 0}},
			wantLevel:  LevelCompatible,
			wantOK:     true,
		},
		{
			name:       "older major is rejected",
			local:      Current,
			localFeats: CurrentFeatures,
			peer:       PeerInfo{Version: Version{1, 9, 0}},
			wantLevel:  LevelMajorMismatch,
			wantOK:     false,
			wantErr:    ErrMajorMismatch,
		},
		{
			name:       "newer major is rejected",
			local:      Current,
			localFeats: CurrentFeatures,
			peer:       PeerInfo{Version: Version{3, 0, 0}},
			wantLevel:  LevelMajorMismatch,
			wantOK:     false,
			wantErr:    ErrMajorMismatch,
		},
		{
			name:       "legacy peer with a changed local major is rejected",
			local:      Version{3, 0, 0},
			localFeats: CurrentFeatures,
			peer:       PeerInfo{Version: LegacyVersion, Legacy: true},
			wantLevel:  LevelMajorMismatch,
			wantOK:     false,
			wantErr:    ErrMajorMismatch,
		},
		{
			name:         "N-1 boundary is supported",
			local:        Version{2, 3, 0},
			localFeats:   CurrentFeatures,
			peer:         PeerInfo{Version: Version{2, 2, 0}, Features: CurrentFeatures},
			wantLevel:    LevelOlderMinor,
			wantOK:       true,
			wantWarnText: "older",
			wantCommon:   CurrentFeatures,
		},
		{
			name:       "older than N-1 is rejected",
			local:      Version{2, 3, 0},
			localFeats: CurrentFeatures,
			peer:       PeerInfo{Version: Version{2, 1, 0}},
			wantLevel:  LevelTooOld,
			wantOK:     false,
			wantErr:    ErrUnsupportedMinor,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rep, err := Evaluate(tc.local, tc.localFeats, tc.peer)
			if tc.wantOK && err != nil {
				t.Fatalf("Evaluate: %v (report %s)", err, rep)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("Evaluate accepted incompatible peer: %s", rep)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if rep.Level != tc.wantLevel {
				t.Fatalf("Level = %s, want %s (report %s)", rep.Level, tc.wantLevel, rep)
			}
			if rep.Compatible != tc.wantOK {
				t.Fatalf("Compatible = %t, want %t", rep.Compatible, tc.wantOK)
			}
			if tc.wantWarnText != "" {
				joined := strings.Join(rep.Warnings, "; ")
				if !strings.Contains(joined, tc.wantWarnText) {
					t.Fatalf("warnings = %q, want substring %q", joined, tc.wantWarnText)
				}
			}
			if tc.wantCommon != 0 && rep.CommonFeatures != tc.wantCommon {
				t.Fatalf("CommonFeatures = %x, want %x", rep.CommonFeatures, tc.wantCommon)
			}
			if rep.UnknownFeatures != tc.wantUnknown {
				t.Fatalf("UnknownFeatures = %x, want %x", rep.UnknownFeatures, tc.wantUnknown)
			}
			if !tc.wantOK && rep.Reason == "" {
				t.Fatal("rejected report has no Reason")
			}
		})
	}
}

func TestEvaluateReportDoesNotIncludePeerText(t *testing.T) {
	// The report is built only from parsed numbers; a peer-supplied version
	// string containing a credential must never be reproduced in output.
	rep, err := Evaluate(Current, CurrentFeatures, PeerInfo{Version: Version{2, 0, 0}})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if strings.Contains(rep.String(), "Bearer") || strings.Contains(rep.String(), "authorization") {
		t.Fatalf("report leaked credential-like text: %s", rep)
	}
}

func TestIncompatibleErrorUnwrap(t *testing.T) {
	_, err := Evaluate(Current, CurrentFeatures, PeerInfo{Version: Version{Major: 9}})
	if !errors.Is(err, ErrMajorMismatch) {
		t.Fatalf("errors.Is(%v, ErrMajorMismatch) = false", err)
	}
	var ierr *IncompatibleError
	if !errors.As(err, &ierr) {
		t.Fatalf("error %v is not *IncompatibleError", err)
	}
	if ierr.Local != Current || ierr.Peer.Major != 9 {
		t.Fatalf("IncompatibleError = %+v", ierr)
	}
}
