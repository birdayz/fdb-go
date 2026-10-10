package types

import (
	"bytes"
	"testing"
)

// equalsKeyAfterReference is the pre-change Go loop, kept as the oracle.
func equalsKeyAfterReference(begin, end []byte) bool {
	if len(end) != len(begin)+1 || end[len(end)-1] != 0 {
		return false
	}
	for i := range begin {
		if begin[i] != end[i] {
			return false
		}
	}
	return true
}

var equalsKeyAfterSeeds = []struct {
	name       string
	begin, end []byte
	want       bool
}{
	{"both empty", nil, nil, false},
	{"empty begin, zero end", nil, []byte{0}, true},
	{"empty begin, nonzero end", nil, []byte{1}, false},
	{"empty begin, two zeros", nil, []byte{0, 0}, false},
	{"key after", []byte("k"), []byte("k\x00"), true},
	{"zero-suffixed key after", []byte("k\x00"), []byte("k\x00\x00"), true},
	{"trailing nonzero", []byte("k"), []byte("k\x01"), false},
	{"prefix mismatch", []byte("k"), []byte("j\x00"), false},
	{"end shorter", []byte("kk"), []byte("k"), false},
	{"end equal length", []byte("k\x00"), []byte("k\x00"), false},
	{"end two longer", []byte("k"), []byte("k\x00\x00"), false},
	{"first byte differs, long", append([]byte{1}, bytes.Repeat([]byte("a"), 99)...), append(append([]byte{2}, bytes.Repeat([]byte("a"), 99)...), 0), false},
	{"last byte differs, long", append(bytes.Repeat([]byte("a"), 99), 1), append(append(bytes.Repeat([]byte("a"), 99), 2), 0), false},
	{"long key after", bytes.Repeat([]byte{0xff}, 100), append(bytes.Repeat([]byte{0xff}, 100), 0), true},
}

func TestKeyRangeEqualsKeyAfterSeeds(t *testing.T) {
	t.Parallel()
	for _, tc := range equalsKeyAfterSeeds {
		if got := keyRangeEqualsKeyAfter(tc.begin, tc.end); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		if ref := equalsKeyAfterReference(tc.begin, tc.end); ref != tc.want {
			t.Errorf("%s: reference got %v, want %v", tc.name, ref, tc.want)
		}
	}
}

// The serialized bytes depend on this predicate (single-key ranges ship as
// (end, empty)), so any disagreement with the reference is a wire change.
func FuzzKeyRangeEqualsKeyAfter(f *testing.F) {
	for _, tc := range equalsKeyAfterSeeds {
		f.Add(tc.begin, tc.end)
	}
	f.Fuzz(func(t *testing.T, begin, end []byte) {
		if got, want := keyRangeEqualsKeyAfter(begin, end), equalsKeyAfterReference(begin, end); got != want {
			t.Fatalf("begin=%x end=%x: got %v, reference %v", begin, end, got, want)
		}
	})
}
