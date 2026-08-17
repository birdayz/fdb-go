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
// what says "put it in ref/list instead".
//
// They are TIGHT, and they have to be. Measured at part=96, structuralKey=408
// with structuralKeyInlineParts=4: the previous 112 admitted exactly one new
// `any` field on `part` (96 -> 112), the precise growth the first message exists
// to stop, and the previous 1024 admitted structuralKeyInlineParts=10 (984) —
// a 2.5x rise in the constant the second message tells you to LOWER. A ceiling
// that permits the change it warns about is not a guard. 448 leaves less than one
// part of headroom (a fifth inline part is 504), which is the point: field
// reordering cannot change these sizes, so there is nothing to leave slack for.
func TestStructuralKeyStaysSmall(t *testing.T) {
	t.Parallel()

	const (
		maxPartSize = 96
		maxKeySize  = 448
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
