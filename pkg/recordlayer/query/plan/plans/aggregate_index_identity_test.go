package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// aggregateIdentityFixture builds a fresh aggregate-index plan. Fresh each call
// rather than shared: structuralKey folds the child index plan's key, so two
// comparands must be independently built or the test would be comparing a plan
// with itself.
func aggregateIdentityFixture(t *testing.T) *RecordQueryAggregateIndexPlan {
	t.Helper()
	eq := scanCostRange(t, predicates.ComparisonEquals, int64(1))
	index, err := NewRecordQueryIndexPlan(
		"idx", []*predicates.ComparisonRange{eq}, []string{"T"}, exactTestRecordType(), false)
	if err != nil {
		t.Fatalf("index plan: %v", err)
	}
	agg, err := NewRecordQueryAggregateIndexPlan(index, "T", exactTestRecordType(), "COUNT")
	if err != nil {
		t.Fatalf("aggregate index plan: %v", err)
	}
	return agg
}

// TestAggregateIndexPlan_IdentitySeparatesExecutorVisibleFields pins the two
// fields whose omission from structuralKey let plans producing DIFFERENT ROWS
// share one memo identity.
//
// groupCols and aggColumn are what the executor's aggregateIndexCursor uses to
// map index entries onto result rows, so GROUP BY alpha and GROUP BY beta —
// and SUM(price) and SUM(quantity) — were EQUAL and hashed the same. The memo
// interns those into one expression and serves whichever arrived first.
//
// What made it survive is that the neighbouring folds looked like coverage.
// GetPhysicalGroupingPrefixCount() returns len(groupCols) when the count is not
// independently known, so it folds the group columns' ARITY — differing arity
// separated correctly, and only same-arity different NAMES collapsed. And
// liveGroupsOnly was folded with a comment explaining this exact hazard, so the
// principle was understood and these fields were simply missed.
//
// The arity and aggregateFunction rows below are CONTROLS, not coverage: they
// discriminate in the same shape as the failing rows, so a run where everything
// reports unequal cannot be read as a pass.
func TestAggregateIndexPlan_IdentitySeparatesExecutorVisibleFields(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		build func(*testing.T) (*RecordQueryAggregateIndexPlan, *RecordQueryAggregateIndexPlan)
		why   string
	}{
		{
			name: "group columns differing only in name",
			build: func(t *testing.T) (a, b *RecordQueryAggregateIndexPlan) {
				return aggregateIdentityFixture(t).WithGroupColumns([]string{"alpha"}, "price"),
					aggregateIdentityFixture(t).WithGroupColumns([]string{"beta"}, "price")
			},
			why: "GROUP BY alpha and GROUP BY beta produce different rows",
		},
		{
			name: "aggregated column",
			build: func(t *testing.T) (a, b *RecordQueryAggregateIndexPlan) {
				return aggregateIdentityFixture(t).WithGroupColumns([]string{"alpha"}, "price"),
					aggregateIdentityFixture(t).WithGroupColumns([]string{"alpha"}, "quantity")
			},
			why: "SUM(price) and SUM(quantity) produce different rows",
		},
		{
			name: "CONTROL group-column arity",
			build: func(t *testing.T) (a, b *RecordQueryAggregateIndexPlan) {
				return aggregateIdentityFixture(t).WithGroupColumns([]string{"alpha"}, "price"),
					aggregateIdentityFixture(t).WithGroupColumns([]string{"alpha", "beta"}, "price")
			},
			why: "already separated before the fix, via the physical grouping prefix count",
		},
		{
			name: "CONTROL aggregate function",
			build: func(t *testing.T) (a, b *RecordQueryAggregateIndexPlan) {
				base := aggregateIdentityFixture(t).WithGroupColumns([]string{"alpha"}, "price")
				sum, err := NewRecordQueryAggregateIndexPlan(
					base.GetIndexPlan(), "T", exactTestRecordType(), "SUM")
				if err != nil {
					t.Fatalf("sum plan: %v", err)
				}
				return base, sum.WithGroupColumns([]string{"alpha"}, "price")
			},
			why: "already separated before the fix",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, b := tc.build(t)
			if a.EqualsPlanWithoutChildren(b) {
				t.Errorf("plans differing only in %s are EqualsPlanWithoutChildren — the memo "+
					"will intern them as one expression and serve whichever arrived first (%s)",
					tc.name, tc.why)
			}
			if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
				t.Errorf("plans differing only in %s hash identically; equality separates them "+
					"but the hash does not, so they land in the same memo bucket", tc.name)
			}
		})
	}
}

// TestAggregateIndexPlan_IdentityStillDedups is the other half: folding MORE
// fields can only make identity stricter, and the failure mode of stricter
// identity is that two genuinely identical plans stop deduping — no correctness
// cost, unbounded memo cost.
//
// Be precise about what this does NOT establish, because the first version of
// this comment claimed it and was wrong. It does not guard resultValue. That
// field is described as a per-instance QuantifiedObjectValue, but the uniqueness
// lives only on newPlanExprBaseForType's ERASED-record branch, which mints a
// UniqueCorrelationIdentifier; an exact record type like this fixture's takes
// the layout branch and gets a deterministic carrier instead. Measured by
// mutation: folding resultValue here — through Value() and through the
// alias-sensitive StructVal() — leaves this test GREEN both times, because the
// two fixtures' result values are structurally identical.
//
// So this guards the general property (independently-built identical plans
// still intern), and resultValue's exclusion rests on the erased-type branch,
// which no fixture here constructs.
func TestAggregateIndexPlan_IdentityStillDedups(t *testing.T) {
	t.Parallel()

	a := aggregateIdentityFixture(t).WithGroupColumns([]string{"alpha"}, "price").
		WithGroupColumnLayout(exactTestRecordType())
	b := aggregateIdentityFixture(t).WithGroupColumns([]string{"alpha"}, "price").
		WithGroupColumnLayout(exactTestRecordType())

	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("two independently-built identical aggregate index plans must still be equal; " +
			"identity has become per-instance and the memo will never intern anything")
	}
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("two independently-built identical plans must hash identically")
	}
	// The fixtures must genuinely be two plans, or "they dedup" says nothing.
	if a == b {
		t.Fatal("the fixture returned the same plan twice, so interning is trivially true here")
	}
}

// TestAggregateIndexPlan_IdentityExclusionsAreDeliberate pins the fields
// structuralKey does NOT fold, so that each stays an argued decision rather
// than drifting into the same unnoticed omission groupCols and aggColumn were.
//
// Writing this test is how the fix got narrowed. A census found six unfolded
// fields and a probe showed four of them collapsing; folding all four looked
// like the complete answer. Two were wrong to fold:
//
//   - groupColLayout carries an explicit field comment saying it is excluded
//     BECAUSE it is derived from the index's record type, so two plans over one
//     index cannot disagree — and all three production callers do pass
//     <candidate>.GetBaseRowType(). Folding it keys the memo on a type token
//     for no discrimination.
//   - resultType is computed by aggregateIndexOutputType from groupCols, the
//     aggregate function and the index's key component types. Every one of
//     those is folded, so it is a function of the key rather than an addition
//     to it.
//
// The probe showed both "collapsing" only because it constructed states the
// planner cannot produce. A census locates candidates; it does not establish
// that any of them is a defect.
func TestAggregateIndexPlan_IdentityExclusionsAreDeliberate(t *testing.T) {
	t.Parallel()

	otherType := values.NewRecordType("other_row", false, []values.Field{
		{Name: "ZZZ", FieldType: values.NotNullLong, Ordinal: 0},
	})

	t.Run("groupColLayout stays out", func(t *testing.T) {
		t.Parallel()
		a := aggregateIdentityFixture(t).WithGroupColumns([]string{"alpha"}, "price").
			WithGroupColumnLayout(exactTestRecordType())
		b := aggregateIdentityFixture(t).WithGroupColumns([]string{"alpha"}, "price").
			WithGroupColumnLayout(otherType)
		if !a.EqualsPlanWithoutChildren(b) {
			t.Error("groupColLayout is now folded into identity. If that is intended, the " +
				"field comment saying it is excluded because it is derived from the index's " +
				"record type must be updated too — one of the two is now wrong.")
		}
	})

	t.Run("resultType stays out", func(t *testing.T) {
		t.Parallel()
		base := aggregateIdentityFixture(t)
		other, err := NewRecordQueryAggregateIndexPlan(base.GetIndexPlan(), "T", otherType, "COUNT")
		if err != nil {
			t.Fatalf("other-resulttype plan: %v", err)
		}
		if !base.EqualsPlanWithoutChildren(other) {
			t.Error("resultType is now folded into identity. It is derived by " +
				"aggregateIndexOutputType from groupCols, the aggregate function and the " +
				"index key types — all already folded — so folding it adds nothing; if it " +
				"stopped being derived, say so at structuralKey.")
		}
	})
}
