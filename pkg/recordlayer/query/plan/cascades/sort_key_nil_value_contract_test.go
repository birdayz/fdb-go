package cascades

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustNilSortKeyConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct nil-sort-key fixture: " + err.Error())
	}
	return value
}

func nilSortKeyScanAndFields() (*plans.RecordQueryScanPlan, values.Value, values.Value) {
	row := values.NewRecordType("NilSortKeyRow", false, []values.Field{
		{Name: "V1", FieldType: values.NotNullLong},
		{Name: "V2", FieldType: values.NotNullLong},
	})
	scan := mustNilSortKeyConstruct(plans.NewRecordQueryScanPlan(
		[]string{"T"}, row, false))
	root, ok := values.AsQuantifiedObjectValue(scan.GetResultValue())
	if !ok {
		panic("nil-sort-key scan result is not a QOV")
	}
	return scan,
		mustNilSortKeyConstruct(values.ResolveFieldOrdinals(root, []int{0})),
		mustNilSortKeyConstruct(values.ResolveFieldOrdinals(root, []int{1}))
}

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

	_, ok, _ := nilSortKeyScanAndFields()
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

// TestInMemorySortRejectsNilKeyAndHashesValidKeysDistinctly pins both sides of
// the checked sort-key contract. A nil ValueExpr is not a malformed plan state
// whose tie-break hash needs to remain meaningful: the constructor rejects it
// before admission, wherever it appears in the key list. The valid population
// still exercises the hash stream across empty/concatenable display labels,
// exact Value identities, directions, and one-/two-key sequences.
func TestInMemorySortRejectsNilKeyAndHashesValidKeysDistinctly(t *testing.T) {
	t.Parallel()

	scan, v1, v2 := nilSortKeyScanAndFields()
	if _, err := plans.NewRecordQueryInMemorySortPlan(scan, []plans.SortKey{{
		Field: "V1", ValueExpr: nil,
	}}); err == nil {
		t.Fatal("in-memory sort constructor accepted a nil leading ValueExpr")
	}
	if _, err := plans.NewRecordQueryInMemorySortPlan(scan, []plans.SortKey{
		{Field: "V1", ValueExpr: v1},
		{Field: "V2", ValueExpr: nil},
	}); err == nil {
		t.Fatal("in-memory sort constructor accepted a nil non-leading ValueExpr")
	}

	vals := []values.Value{
		v1,
		v2,
	}
	valNames := []string{"V1", "V2"}
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
		plan := mustNilSortKeyConstruct(plans.NewRecordQueryInMemorySortPlan(scan, ks))
		h := stablePlanNodeHash(plan)
		if prev, dup := seen[h]; dup {
			t.Fatalf("two DIFFERENT in-memory sorts hash alike (%d):\n  %s\n  %s\n"+
				"the #17 tie-break cannot separate these two valid exact key programs and "+
				"would rank them by arrival order instead of by content.", h, prev, name)
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
	// 16 one-key plans + 256 two-key plans. Stated so a refactor that silently
	// shrinks the alphabet cannot turn this into a green over a tiny population.
	if len(seen) != 272 {
		t.Fatalf("folded %d distinct plans, want 272 — the enumeration changed size, so the "+
			"no-collision result above is over a different population than the one claimed",
			len(seen))
	}
}
