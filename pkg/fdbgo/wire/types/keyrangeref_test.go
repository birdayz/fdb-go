package types

import (
	"bytes"
	"testing"
)

// TestKeyRangeRef_RoundTrip pins the C++ single-key optimization +
// inverse reconstruction. Discovered 2026-04-25 by FuzzSplitRangeRequest:
// pre-fix, single-key-optimized payloads round-tripped with Begin/End
// swapped because the generated reader didn't know to invert the
// (end, empty) writer optimization.
func TestKeyRangeRef_RoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		begin []byte
		end   []byte
	}{
		{name: "empty/empty", begin: nil, end: nil},
		{name: "normal range", begin: []byte("aaa"), end: []byte("bbb")},
		{
			name:  "single-key optimization (begin+\\x00 == end)",
			begin: []byte("key1"),
			end:   []byte("key1\x00"),
		},
		{
			name:  "edge: empty begin, single-byte end",
			begin: nil,
			end:   []byte{0x00},
		},
		{name: "long keys", begin: bytes.Repeat([]byte("a"), 100), end: bytes.Repeat([]byte("z"), 100)},
		{
			name:  "single-key with binary",
			begin: []byte{0x01, 0x02, 0x03},
			end:   []byte{0x01, 0x02, 0x03, 0x00},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// We round-trip via SplitRangeRequest because KeyRangeRef
			// itself is a nested type without its own MarshalFDB.
			req := &SplitRangeRequest{
				Keys:      KeyRangeRef{Begin: tt.begin, End: tt.end},
				ChunkSize: 1000,
			}
			data := req.MarshalFDB()

			var got SplitRangeRequest
			if err := got.UnmarshalFDB(data); err != nil {
				t.Fatalf("UnmarshalFDB: %v", err)
			}
			if !bytesEqualOrBothEmpty(got.Keys.Begin, tt.begin) {
				t.Errorf("Begin: got %x, want %x", got.Keys.Begin, tt.begin)
			}
			if !bytesEqualOrBothEmpty(got.Keys.End, tt.end) {
				t.Errorf("End: got %x, want %x", got.Keys.End, tt.end)
			}
		})
	}
}

func bytesEqualOrBothEmpty(a, b []byte) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return bytes.Equal(a, b)
}
