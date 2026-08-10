package cascades

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// plans.SortKey.ValueExpr is REQUIRED, and its upstream expressions.SortKey.Value
// is non-nil at every producer. The contract had been spelled THREE different
// ways in three places, each of which quietly tolerated the nil it says cannot
// happen. A tolerance is what makes a contract untrue, but the three were NOT
// equally bad and only measurement separates them:
//
//   - the advertiser (plans.HintOrdering) FABRICATED an ordering claim from a
//     display string, over-claiming past what the executor will run. Fixed;
//     pinned next door in plans.
//   - the rule (sortKeysAreOrderable) MANUFACTURED the malformed plan by
//     skipping the key it could not use. Fixed; pinned below.
//   - the cost model's hash OMITS a nil Value, which reads like the sharpest of
//     the three and MEASURES as harmless. Refuted rather than fixed, and the
//     measurement is committed below rather than argued.
//
// Two of the three are therefore fixes with a red-going mutation behind them, and
// the third is a negative result with an enumeration behind it.

// TestSortKeysAreOrderable_DeclinesANilValue pins the rule's spelling.
//
// It used to `continue` past a nil, which is the worst of the three arms even
// though it looks like the mildest: skipping the key does not stop the loop
// below from building plans.SortKey{ValueExpr: nil}, so the tolerance MANUFACTURED
// exactly the malformed plan the executor then rejects at runtime. Declining
// yields no plan, which is what this rule already does for an unorderable key.
func TestSortKeysAreOrderable_DeclinesANilValue(t *testing.T) {
	t.Parallel()

	ok := values.NewFlatFieldValue("AMOUNT", values.UnknownType)
	if !sortKeysAreOrderable([]expressions.SortKey{{Value: ok}}) {
		t.Fatal("control: an ordinary scalar key was declined, so the assertions below " +
			"would pass for the wrong reason — this rule would never yield a plan at all")
	}
	if sortKeysAreOrderable([]expressions.SortKey{{Value: nil}}) {
		t.Fatal("a nil sort-key Value was ACCEPTED. The key loop then hands plans.SortKey a " +
			"nil ValueExpr and the executor rejects the whole plan as malformed at runtime; " +
			"declining here is what stops the malformed plan from being built")
	}
	if sortKeysAreOrderable([]expressions.SortKey{{Value: ok}, {Value: nil}}) {
		t.Fatal("a nil Value in a NON-LEADING position was accepted. The nil must decline " +
			"wherever it sits, not only where it happens to be looked at first")
	}
}

// TestStablePlanNodeHash_NilSortKeyValueDoesNotCollide is a NEGATIVE RESULT,
// committed rather than discarded because it is what keeps the third spelling
// classified as unreachable-and-harmless instead of latent.
//
// stablePlanNodeHash tolerates a nil ValueExpr by silently OMITTING the key's
// Value from the #17 tie-break hash. That reads like the sharpest of the three
// spellings — an omission sounds like it should make a malformed plan hash
// IDENTICALLY to a well-formed one with the same display Field, which would make
// two different plans indistinguishable to the tie-break. MEASURED, it does not:
// the hash is a byte STREAM, so the presence or absence of the fixed 8-byte
// value write is itself distinguishing, and every plan below hashes uniquely.
//
// What makes it unreachable rather than merely harmless is upstream: nil is
// declined at sortKeysAreOrderable (above) and at the executor, so no planner
// path builds a plan whose hash this arm would decide. What makes it HARMLESS is
// this test, and the property is not obvious enough to assert without measuring.
//
// If this ever goes red, the omission has become a real collision and the fix is
// to fold a discriminator for the nil case rather than to omit it.
func TestStablePlanNodeHash_NilSortKeyValueDoesNotCollide(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	vals := []values.Value{
		nil,
		values.NewFlatFieldValue("V1", values.UnknownType),
		values.NewFlatFieldValue("V2", values.UnknownType),
	}
	valNames := []string{"nil", "V1", "V2"}
	// "" and "AB" are in the alphabet on purpose: an empty Field and a Field that
	// is the CONCATENATION of two others are how a stream with no length prefix
	// would give a nil key's omission somewhere to hide.
	fields := []string{"", "A", "B", "AB"}

	key := func(fi, vi, d int) (plans.SortKey, string) {
		return plans.SortKey{Field: fields[fi], ValueExpr: vals[vi], Desc: d == 1},
			fmt.Sprintf("{%q,%s,desc=%v}", fields[fi], valNames[vi], d == 1)
	}
	seen := map[uint64]string{}
	fold := func(ks []plans.SortKey, name string) {
		h := stablePlanNodeHash(plans.NewRecordQueryInMemorySortPlan(scan, ks))
		if prev, dup := seen[h]; dup {
			t.Fatalf("two DIFFERENT in-memory sorts hash alike (%d):\n  %s\n  %s\n"+
				"stablePlanNodeHash omits a nil ValueExpr from the fold, and that omission has "+
				"now become a real collision — the #17 tie-break cannot separate these two and "+
				"ranks them by arrival order instead of by content. Fold a discriminator for "+
				"the nil case rather than omitting it.", h, prev, name)
		}
		seen[h] = name
	}
	for fi := range fields {
		for vi := range vals {
			for d := 0; d < 2; d++ {
				k, n := key(fi, vi, d)
				fold([]plans.SortKey{k}, n)
				for fj := range fields {
					for vj := range vals {
						for e := 0; e < 2; e++ {
							k2, n2 := key(fj, vj, e)
							fold([]plans.SortKey{k, k2}, n+n2)
						}
					}
				}
			}
		}
	}
	// 24 one-key plans + 576 two-key plans. Stated so a refactor that silently
	// shrinks the alphabet cannot turn this into a green over a tiny population.
	if len(seen) != 600 {
		t.Fatalf("folded %d distinct plans, want 600 — the enumeration changed size, so the "+
			"no-collision result above is over a different population than the one claimed",
			len(seen))
	}
}
