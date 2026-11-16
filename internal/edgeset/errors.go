package edgeset

import "errors"

var (
	// ErrSegmentNotFound is returned by Store.Segment and Store.SegmentPath
	// when the prefix is not present in the manifest.
	ErrSegmentNotFound = errors.New("edgeset: segment not found")

	// ErrManifestSignature reports a missing, malformed or mismatching
	// manifest HMAC.
	ErrManifestSignature = errors.New("edgeset: manifest signature verification failed")

	// ErrInvalidManifest reports a structurally invalid manifest.
	ErrInvalidManifest = errors.New("edgeset: invalid manifest")

	// ErrCorruptSegment reports a segment whose bytes do not match its
	// manifest metadata (size, checksum, header, ordering, or Merkle root).
	ErrCorruptSegment = errors.New("edgeset: corrupt segment")

	// ErrInvalidOption reports invalid BuildOptions.
	ErrInvalidOption = errors.New("edgeset: invalid option")
)
