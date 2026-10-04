package values

import (
	"fmt"
	"hash/fnv"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/internal/fnv64"
)

func TestSemanticValueHashChildCountEncoding(t *testing.T) {
	t.Parallel()
	for _, count := range []int{0, 1, 9, 10, 99, 100, 128} {
		t.Run(fmt.Sprintf("children=%d", count), func(t *testing.T) {
			t.Parallel()
			children := make([]Value, count)
			want := fnv.New64a()
			_, _ = fmt.Fprintf(want, "scalarfn:COALESCE(%d", count)
			for i := range children {
				children[i] = LiteralValue(int64(i))
				_, _ = fmt.Fprintf(want, ",const:int64=%d(0)", i)
			}
			_, _ = fmt.Fprint(want, ")")
			value := NewScalarFunctionValue("COALESCE", NotNullLong, children...)
			if got := SemanticHashCode(value); got != want.Sum64() {
				t.Fatalf("hash %x, want %x", got, want.Sum64())
			}
		})
	}
}

func BenchmarkSemanticValueHash(b *testing.B) {
	row := NewRecordType("R", false, []Field{{Name: "A", FieldType: NotNullLong}})
	root := mustQOV(b, NamedCorrelationIdentifier("q"), row)
	field, err := ResolveFieldOrdinals(root, []int{0})
	if err != nil {
		b.Fatal(err)
	}
	value := NewScalarFunctionValue("COALESCE", NotNullLong, field, LiteralValue(int64(7)))
	want := SemanticHashCode(value)
	b.ReportAllocs()
	for b.Loop() {
		if got := SemanticHashCode(value); got != want {
			b.Fatalf("hash changed: %x, want %x", got, want)
		}
	}
}

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

		viaString := fnv64.New()
		_, _ = viaString.WriteString(s)
		if viaString.Sum64() != want.Sum64() {
			t.Errorf("WriteString(%q) = %d, stdlib fnv = %d", s, viaString.Sum64(), want.Sum64())
		}
		viaBytes := fnv64.New()
		_, _ = viaBytes.Write([]byte(s))
		if viaBytes.Sum64() != want.Sum64() {
			t.Errorf("Write(%q) = %d, stdlib fnv = %d", s, viaBytes.Sum64(), want.Sum64())
		}
	}
	// And the two entry points must agree with each other on a split write,
	// since writeSemanticHash interleaves both.
	mixed := fnv64.New()
	_, _ = mixed.WriteString("qov:")
	_, _ = mixed.Write([]byte{1, 2, 3})
	_, _ = mixed.WriteString("(0)")
	ref := fnv.New64a()
	_, _ = ref.Write([]byte("qov:\x01\x02\x03(0)"))
	if mixed.Sum64() != ref.Sum64() {
		t.Errorf("interleaved writes = %d, stdlib = %d", mixed.Sum64(), ref.Sum64())
	}
}
