package plans

import (
	"testing"
	"unsafe"
)

// TestStructuralKeyStaysSmall guards the one property that makes the inline
// part array a win instead of a cost. A structuralKey is built and thrown away
// on EVERY memo dedup comparison — once for each side of an equality, once more
// for a hash — so its size IS the planner's dominant allocation term. Eight
// inline parts at 312 bytes each made it 2520 bytes, 24% of the whole planner's
// allocated bytes, and more than the growing-slice version the inline array was
// introduced to beat.
//
// The alarm here is GROWTH. A new payload field on `part` costs every part of
// every key, whether or not any plan folds that kind; the ceilings below are
// what says "put it in ref/list instead". They are not tight bounds — they sit
// a little above the current values so a field reordering does not fail the
// build — and they are not a target to grow into.
func TestStructuralKeyStaysSmall(t *testing.T) {
	t.Parallel()

	const (
		maxPartSize = 112
		maxKeySize  = 1024
	)
	if got := unsafe.Sizeof(part{}); got > maxPartSize {
		t.Errorf("sizeof(part) = %d, want <= %d.\n"+
			"  A part carries exactly ONE payload; a new field beside `ref`/`list` "+
			"is paid by every part of every key. Put the payload in ref (single "+
			"object) or list (slice) and read it back through a typed accessor.",
			got, maxPartSize)
	}
	if got := unsafe.Sizeof(structuralKey{}); got > maxKeySize {
		t.Errorf("sizeof(structuralKey) = %d, want <= %d.\n"+
			"  This is allocated once per dedup comparison. If part cannot shrink, "+
			"lower structuralKeyInlineParts (currently %d) rather than letting the "+
			"inline array carry the growth.",
			got, maxKeySize, structuralKeyInlineParts)
	}
}
