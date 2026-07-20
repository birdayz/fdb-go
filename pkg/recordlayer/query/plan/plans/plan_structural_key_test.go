package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestStructuralKey_MemoInvariantAndFieldSensitivity locks the contract every
// centralized plan key relies on: equal keys hash equal (the memo dedup
// invariant), every field is load-bearing, and variable-length parts cannot
// collide across a boundary. A regression here would silently corrupt memo
// grouping for every plan that migrated to the builder.
func TestStructuralKey_MemoInvariantAndFieldSensitivity(t *testing.T) {
	t.Parallel()
	base := func() *structuralKey { return newStructuralKey().Bool(true).Int(3).Str("x") }

	// Equal keys → Equal AND same hash (the invariant memo dedup depends on).
	k1, k2 := base(), base()
	if !k1.Equal(k2) {
		t.Fatal("identical keys reported unequal")
	}
	if h1, h2 := k1.Hash("d|"), k2.Hash("d|"); h1 != h2 {
		t.Fatal("equal keys must hash equal (memo invariant)")
	}

	// Every field is load-bearing — flipping any one breaks equality.
	if base().Equal(newStructuralKey().Bool(false).Int(3).Str("x")) {
		t.Error("bool field not load-bearing")
	}
	if base().Equal(newStructuralKey().Bool(true).Int(4).Str("x")) {
		t.Error("int field not load-bearing")
	}
	if base().Equal(newStructuralKey().Bool(true).Int(3).Str("y")) {
		t.Error("str field not load-bearing")
	}
	// A shorter key is never equal to a longer one.
	if base().Equal(newStructuralKey().Bool(true).Int(3)) {
		t.Error("length mismatch reported equal")
	}
	// The discriminator is folded — two plan classes with identical fields
	// still separate in the hash.
	if base().Hash("a|") == base().Hash("b|") {
		t.Error("discriminator not folded into the hash")
	}
	// Length-tagged variable parts: ["a","bc"] and ["ab","c"] must not collide
	// in either equality or hash.
	ka := newStructuralKey().Str("a").Str("bc")
	kb := newStructuralKey().Str("ab").Str("c")
	if ka.Equal(kb) {
		t.Error("string boundary collision in Equal")
	}
	if ka.Hash("d|") == kb.Hash("d|") {
		t.Error("string boundary collision in Hash")
	}
}

// TestLimitPlan_StructuralKeyContract exercises the invariant end-to-end
// through a migrated plan: EqualsPlanWithoutChildren / HashCodeWithoutChildren
// agree, and each identifying field distinguishes.
func TestLimitPlan_StructuralKeyContract(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan(nil, nil, false)
	assertPlanKeyEqual(t, NewRecordQueryLimitPlan(scan, 10, 5), NewRecordQueryLimitPlan(scan, 10, 5))
	assertPlanKeyUnequal(t, NewRecordQueryLimitPlan(scan, 10, 5), NewRecordQueryLimitPlan(scan, 11, 5)) // limit
	assertPlanKeyUnequal(t, NewRecordQueryLimitPlan(scan, 10, 5), NewRecordQueryLimitPlan(scan, 10, 6)) // offset
	// A different plan class with coincidentally-similar shape is never equal.
	assertPlanKeyUnequal(t, NewRecordQueryLimitPlan(scan, 10, 5), scan)
}

func assertPlanKeyEqual(t *testing.T, a, b RecordQueryPlan) {
	t.Helper()
	if !a.EqualsPlanWithoutChildren(b) {
		t.Errorf("expected EqualsPlanWithoutChildren true")
	}
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Errorf("equal plans must hash equal (memo invariant)")
	}
}

func assertPlanKeyUnequal(t *testing.T, a, b RecordQueryPlan) {
	t.Helper()
	if a.EqualsPlanWithoutChildren(b) {
		t.Errorf("expected EqualsPlanWithoutChildren false")
	}
}

// TestMigratedPlans_StructuralKeyContract pins each additionally-migrated plan:
// equal instances agree (and hash equal), and every identifying field is
// load-bearing. One block per plan — the per-plan net the reviewers asked for.
func TestMigratedPlans_StructuralKeyContract(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan(nil, nil, false)

	// distinct — Streaming is the sole identifying field.
	d1 := NewRecordQueryDistinctPlan(scan)
	assertPlanKeyEqual(t, d1, NewRecordQueryDistinctPlan(scan))
	dStream := NewRecordQueryDistinctPlan(scan)
	dStream.Streaming = true
	assertPlanKeyUnequal(t, d1, dStream)

	// typefilter — the record-type list (dedupSortedStrings-normalized at
	// construction, so equality is on the normalized set; length/content
	// distinguish).
	assertPlanKeyEqual(t,
		NewRecordQueryTypeFilterPlan([]string{"A", "B"}, scan),
		NewRecordQueryTypeFilterPlan([]string{"B", "A"}, scan)) // both normalize to [A,B]
	assertPlanKeyUnequal(t,
		NewRecordQueryTypeFilterPlan([]string{"A"}, scan),
		NewRecordQueryTypeFilterPlan([]string{"A", "B"}, scan))

	// default_on_empty — the default Value.
	nv := values.NewNullValue(values.UnknownType)
	assertPlanKeyEqual(t,
		NewRecordQueryDefaultOnEmptyPlan(scan, nv),
		NewRecordQueryDefaultOnEmptyPlan(scan, nv))

	// delete — targetRecordType.
	assertPlanKeyEqual(t, NewRecordQueryDeletePlan(scan, "T"), NewRecordQueryDeletePlan(scan, "T"))
	assertPlanKeyUnequal(t, NewRecordQueryDeletePlan(scan, "T"), NewRecordQueryDeletePlan(scan, "U"))

	// values — the column Value list (leaf); length is load-bearing.
	assertPlanKeyEqual(t,
		NewRecordQueryValuesPlan([]values.Value{nv}),
		NewRecordQueryValuesPlan([]values.Value{nv}))
	assertPlanKeyUnequal(t,
		NewRecordQueryValuesPlan([]values.Value{nv}),
		NewRecordQueryValuesPlan([]values.Value{nv, nv}))

	// map — resultValue.
	assertPlanKeyEqual(t, NewRecordQueryMapPlan(scan, nv), NewRecordQueryMapPlan(scan, nv))

	// projection — the projection Value list; length load-bearing.
	assertPlanKeyEqual(t,
		NewRecordQueryProjectionPlan([]values.Value{nv}, scan),
		NewRecordQueryProjectionPlan([]values.Value{nv}, scan))
	assertPlanKeyUnequal(t,
		NewRecordQueryProjectionPlan([]values.Value{nv}, scan),
		NewRecordQueryProjectionPlan([]values.Value{nv, nv}, scan))

	// first_or_default — the strict flag is load-bearing (strict vs non-strict
	// constructor variants).
	assertPlanKeyEqual(t,
		NewRecordQueryFirstOrDefaultPlan(scan, nv),
		NewRecordQueryFirstOrDefaultPlan(scan, nv))
	assertPlanKeyUnequal(t,
		NewRecordQueryFirstOrDefaultPlan(scan, nv),
		NewRecordQueryFirstOrDefaultPlanStrict(scan, nv))

	// unordered_pk_distinct — no identifying fields; two instances always match.
	assertPlanKeyEqual(t,
		NewRecordQueryUnorderedPrimaryKeyDistinctPlan(scan),
		NewRecordQueryUnorderedPrimaryKeyDistinctPlan(scan))
}
