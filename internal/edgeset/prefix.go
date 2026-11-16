package edgeset

import "fmt"

// prefixOf returns the 16-bit segment id for h.
//
// PrefixBytes == 1 uses only the first byte (256 segments). Every larger value
// uses the first two bytes (65536 segments), because SegmentMeta.Prefix and the
// on-disk segment id are uint16. See BuildOptions.PrefixBytes.
func prefixOf(h Hash, prefixBytes int) uint16 {
	if prefixBytes <= 1 {
		return uint16(h[0])
	}
	return uint16(h[0])<<8 | uint16(h[1])
}

// segmentFileFor is the canonical manifest path for a segment. Manifest paths
// always use forward slashes so a store directory is portable and so manifest
// validation can reject traversal without filesystem-dependent semantics.
func segmentFileFor(prefix uint16) string {
	return fmt.Sprintf("segments/%04x.seg", prefix)
}
