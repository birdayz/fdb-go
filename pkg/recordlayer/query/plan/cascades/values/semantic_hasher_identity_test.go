package values

import (
	"hash/fnv"
	"testing"
)

// TestSemanticHasherIsBitIdenticalToStdlibFNV pins that swapping hash/fnv for
// the allocation-free hasher did not change a single hash value.
//
// The memo buckets on these, so a silent change of the arithmetic would not
// fail anything visibly — it would just redistribute buckets and quietly change
// which expressions are compared. Equality against the stdlib is the only check
// that can see it.
func TestSemanticHasherIsBitIdenticalToStdlibFNV(t *testing.T) {
	t.Parallel()
	cases := []string{
		"", "q", "qov:", "record:", "fieldpath:", "scalarfn:COALESCE",
		"v:quantifier", "\x00\xff\x80", "a much longer tag with spaces and 0123456789",
	}
	for _, s := range cases {
		want := fnv.New64a()
		_, _ = want.Write([]byte(s))

		viaString := newSemanticHasher()
		_, _ = viaString.WriteString(s)
		if viaString.Sum64() != want.Sum64() {
			t.Errorf("WriteString(%q) = %d, stdlib fnv = %d", s, viaString.Sum64(), want.Sum64())
		}
		viaBytes := newSemanticHasher()
		_, _ = viaBytes.Write([]byte(s))
		if viaBytes.Sum64() != want.Sum64() {
			t.Errorf("Write(%q) = %d, stdlib fnv = %d", s, viaBytes.Sum64(), want.Sum64())
		}
	}
	// And the two entry points must agree with each other on a split write,
	// since writeSemanticHash interleaves both.
	mixed := newSemanticHasher()
	_, _ = mixed.WriteString("qov:")
	_, _ = mixed.Write([]byte{1, 2, 3})
	_, _ = mixed.WriteString("(0)")
	ref := fnv.New64a()
	_, _ = ref.Write([]byte("qov:\x01\x02\x03(0)"))
	if mixed.Sum64() != ref.Sum64() {
		t.Errorf("interleaved writes = %d, stdlib = %d", mixed.Sum64(), ref.Sum64())
	}
}
