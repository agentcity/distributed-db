package edgeset

import (
	"encoding/json"
	"testing"
)

func TestHashHelpers(t *testing.T) {
	h := HashOf([]byte("hello edgeset"))
	text := h.String()
	if len(text) != 64 {
		t.Fatalf("Hash.String length = %d, want 64", len(text))
	}
	parsed, err := ParseHash(text)
	if err != nil {
		t.Fatalf("ParseHash: %v", err)
	}
	if parsed != h {
		t.Fatalf("ParseHash round trip = %s, want %s", parsed, h)
	}
	if _, err := ParseHash(text[:63]); err == nil {
		t.Fatal("ParseHash accepted a short hash")
	}
	if _, err := ParseHash("zz" + text[2:]); err == nil {
		t.Fatal("ParseHash accepted non-hex characters")
	}

	encoded, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("json.Marshal(Hash): %v", err)
	}
	var decoded Hash
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal(Hash): %v", err)
	}
	if decoded != h {
		t.Fatalf("JSON round trip = %s, want %s", decoded, h)
	}

	var nilHash *Hash
	if err := nilHash.UnmarshalText([]byte(text)); err == nil {
		t.Fatal("UnmarshalText on a nil receiver succeeded")
	}
}
