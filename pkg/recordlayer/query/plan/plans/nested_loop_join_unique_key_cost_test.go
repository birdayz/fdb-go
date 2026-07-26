package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// fieldOfAlias builds a correlated FieldValue{Field, Child: QuantifiedObjectValue{alias}}
// — the shape a join predicate's per-side comparand takes when it reads a
// column off a named join leg.
func fieldOfAlias(field, alias string) values.Value {
	return &values.FieldValue{
		Field: field,
		Typ:   values.UnknownType,
		Child: &values.QuantifiedObjectValue{Correlation: values.NamedCorrelationIdentifier(alias)},
	}
}

func equalityJoinPredicate(lhs, rhs values.Value) predicates.QueryPredicate {
	return predicates.NewComparisonPredicate(lhs, predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: rhs,
	})
}

// TestNestedLoopJoinPlan_HintCost_UniqueKeyEqualityJoin_MatchesOuterCardinality
// pins the real production shape of the Finding 1 defect: a materialized
// NestedLoopJoin whose join predicate equality-binds the inner leg's FULL
// primary key must cost the same true cardinality (outerCard) the
// correlated-FlatMap shape of the SAME logical join would compute — not the
// flat-FilterSelectivity-inflated outerCard*innerCard*0.5.
func TestNestedLoopJoinPlan_HintCost_UniqueKeyEqualityJoin_MatchesOuterCardinality(t *testing.T) {
	t.Parallel()

	outerPlan := NewRecordQueryScanPlan([]string{"Outer"}, values.UnknownType, false)
	innerPlan := NewRecordQueryScanPlan([]string{"Inner"}, values.UnknownType, false).
		WithPrimaryKey([]values.Value{&values.FieldValue{Field: "ID", Typ: values.UnknownType}})

	pred := equalityJoinPredicate(fieldOfAlias("ID", "i"), fieldOfAlias("FK", "o"))
	plan := NewRecordQueryNestedLoopJoinPlan(
		outerPlan, innerPlan,
		[]predicates.QueryPredicate{pred},
		JoinInner, "o", "i", nil,
	)

	if n, ok := NestedLoopJoinUniqueKeyConjuncts(plan); !ok || n != 1 {
		t.Fatalf("NestedLoopJoinUniqueKeyConjuncts = (%v, %v), want (1, true)", n, ok)
	}

	got := plan.HintCost([]properties.Cost{{Cardinality: 10, CPU: 1}, {Cardinality: 1000, CPU: 5}}, nil)
	if got.Cardinality != 10 {
		t.Fatalf("Cardinality = %v, want 10 (outerCard — a unique-key equality join returns at most one inner row per outer row)", got.Cardinality)
	}
}

// TestNestedLoopJoinPlan_HintCost_UniqueKeyThroughFetch confirms the
// detection sees through a Fetch wrapper (RecordQueryFetchFromPartialRecordPlan)
// over a unique index scan — the ordinary "unique index, non-covering" shape
// a real plan takes, exactly like isProvablePointProbe already sees through
// Fetch for the leaf-scan case.
func TestNestedLoopJoinPlan_HintCost_UniqueKeyThroughFetch(t *testing.T) {
	t.Parallel()

	outerPlan := NewRecordQueryScanPlan([]string{"Outer"}, values.UnknownType, false)
	idxScan := NewRecordQueryIndexPlan("uq_idx", nil, []string{"Inner"}, values.UnknownType, false).
		WithIndexMetadata([]string{"ID"}, []string{"ID"}, true)
	innerPlan := NewRecordQueryFetchFromPartialRecordPlan(idxScan, nil, values.UnknownType, FetchIndexRecordsPrimaryKey)

	pred := equalityJoinPredicate(fieldOfAlias("ID", "i"), fieldOfAlias("FK", "o"))
	plan := NewRecordQueryNestedLoopJoinPlan(
		outerPlan, innerPlan,
		[]predicates.QueryPredicate{pred},
		JoinInner, "o", "i", nil,
	)

	got := plan.HintCost([]properties.Cost{{Cardinality: 10, CPU: 1}, {Cardinality: 1000, CPU: 5}}, nil)
	if got.Cardinality != 10 {
		t.Fatalf("Cardinality = %v, want 10", got.Cardinality)
	}
}

// TestNestedLoopJoinPlan_HintCost_CompositeUniqueKey_BothColumnsBound proves
// the fix composes correctly across a multi-column unique key: TWO join
// predicate conjuncts are consumed (one per key column), and their COMBINED
// selectivity is 1/innerCard exactly — not (1/innerCard) applied per
// conjunct, and not one conjunct getting the correction while the other
// falls through to FilterSelectivity.
func TestNestedLoopJoinPlan_HintCost_CompositeUniqueKey_BothColumnsBound(t *testing.T) {
	t.Parallel()

	outerPlan := NewRecordQueryScanPlan([]string{"Outer"}, values.UnknownType, false)
	innerPlan := NewRecordQueryScanPlan([]string{"Inner"}, values.UnknownType, false).
		WithPrimaryKey([]values.Value{
			&values.FieldValue{Field: "TENANT", Typ: values.UnknownType},
			&values.FieldValue{Field: "ORDER", Typ: values.UnknownType},
		})

	preds := []predicates.QueryPredicate{
		equalityJoinPredicate(fieldOfAlias("TENANT", "i"), fieldOfAlias("TENANT_FK", "o")),
		equalityJoinPredicate(fieldOfAlias("ORDER", "i"), fieldOfAlias("ORDER_FK", "o")),
	}
	plan := NewRecordQueryNestedLoopJoinPlan(outerPlan, innerPlan, preds, JoinInner, "o", "i", nil)

	if n, ok := NestedLoopJoinUniqueKeyConjuncts(plan); !ok || n != 2 {
		t.Fatalf("NestedLoopJoinUniqueKeyConjuncts = (%v, %v), want (2, true)", n, ok)
	}
	got := plan.HintCost([]properties.Cost{{Cardinality: 10}, {Cardinality: 1000}}, nil)
	if got.Cardinality != 10 {
		t.Fatalf("Cardinality = %v, want 10", got.Cardinality)
	}
}

// TestNestedLoopJoinPlan_HintCost_UniqueKeyPlusResidualPredicate proves the
// remaining (non-key) conjunct still pays its own FilterSelectivity factor on
// top of the exact 1/innerCard the key bind contributes — the correction
// explains only ITS OWN conjuncts, never any other residual predicate's.
func TestNestedLoopJoinPlan_HintCost_UniqueKeyPlusResidualPredicate(t *testing.T) {
	t.Parallel()

	outerPlan := NewRecordQueryScanPlan([]string{"Outer"}, values.UnknownType, false)
	innerPlan := NewRecordQueryScanPlan([]string{"Inner"}, values.UnknownType, false).
		WithPrimaryKey([]values.Value{&values.FieldValue{Field: "ID", Typ: values.UnknownType}})

	keyPred := equalityJoinPredicate(fieldOfAlias("ID", "i"), fieldOfAlias("FK", "o"))
	residual := predicates.NewComparisonPredicate(fieldOfAlias("STATUS", "i"), predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: &values.ConstantValue{Value: "ACTIVE"},
	})
	plan := NewRecordQueryNestedLoopJoinPlan(
		outerPlan, innerPlan,
		[]predicates.QueryPredicate{keyPred, residual},
		JoinInner, "o", "i", nil,
	)

	if n, ok := NestedLoopJoinUniqueKeyConjuncts(plan); !ok || n != 1 {
		t.Fatalf("NestedLoopJoinUniqueKeyConjuncts = (%v, %v), want (1, true)", n, ok)
	}
	outer := properties.Cost{Cardinality: 10, CPU: 1}
	inner := properties.Cost{Cardinality: 1000, CPU: 5}
	got := plan.HintCost([]properties.Cost{outer, inner}, nil)
	want := outer.Cardinality * properties.FilterSelectivity // inner cancels: 10*1000*(1/1000)*0.5
	if got.Cardinality != want {
		t.Fatalf("Cardinality = %v, want %v", got.Cardinality, want)
	}
}

// TestNestedLoopJoinPlan_HintCost_NonUniqueEqualityJoin_Unaffected pins the
// gate's other side: a non-unique index equality join must keep the flat
// FilterSelectivity fallback exactly as before — the correction must never
// fire for a join key that is not provably unique in the inner.
func TestNestedLoopJoinPlan_HintCost_NonUniqueEqualityJoin_Unaffected(t *testing.T) {
	t.Parallel()

	outerPlan := NewRecordQueryScanPlan([]string{"Outer"}, values.UnknownType, false)
	innerPlan := NewRecordQueryIndexPlan("cat_idx", nil, []string{"Inner"}, values.UnknownType, false).
		WithIndexMetadata([]string{"CATEGORY"}, []string{"ID"}, false) // NOT unique

	pred := equalityJoinPredicate(fieldOfAlias("CATEGORY", "i"), fieldOfAlias("CAT_FK", "o"))
	plan := NewRecordQueryNestedLoopJoinPlan(
		outerPlan, innerPlan,
		[]predicates.QueryPredicate{pred},
		JoinInner, "o", "i", nil,
	)

	if n, ok := NestedLoopJoinUniqueKeyConjuncts(plan); ok || n != 0 {
		t.Fatalf("NestedLoopJoinUniqueKeyConjuncts = (%v, %v), want (0, false) — index is not unique", n, ok)
	}

	outer := properties.Cost{Cardinality: 10}
	inner := properties.Cost{Cardinality: 1000}
	got := plan.HintCost([]properties.Cost{outer, inner}, nil)
	want := outer.Cardinality * inner.Cardinality * properties.FilterSelectivity
	if got.Cardinality != want {
		t.Fatalf("Cardinality = %v, want %v (unchanged flat-selectivity formula)", got.Cardinality, want)
	}
}

// TestNestedLoopJoinPlan_HintCost_PartialCompositeKeyBind_Unaffected pins the
// other under-detection edge: a composite PRIMARY KEY with only ONE of two
// columns bound by the join predicate must NOT be treated as a point-probe
// bind — a partial prefix does not prove uniqueness.
func TestNestedLoopJoinPlan_HintCost_PartialCompositeKeyBind_Unaffected(t *testing.T) {
	t.Parallel()

	outerPlan := NewRecordQueryScanPlan([]string{"Outer"}, values.UnknownType, false)
	innerPlan := NewRecordQueryScanPlan([]string{"Inner"}, values.UnknownType, false).
		WithPrimaryKey([]values.Value{
			&values.FieldValue{Field: "TENANT", Typ: values.UnknownType},
			&values.FieldValue{Field: "ORDER", Typ: values.UnknownType},
		})

	pred := equalityJoinPredicate(fieldOfAlias("TENANT", "i"), fieldOfAlias("TENANT_FK", "o"))
	plan := NewRecordQueryNestedLoopJoinPlan(outerPlan, innerPlan, []predicates.QueryPredicate{pred}, JoinInner, "o", "i", nil)

	if n, ok := NestedLoopJoinUniqueKeyConjuncts(plan); ok || n != 0 {
		t.Fatalf("NestedLoopJoinUniqueKeyConjuncts = (%v, %v), want (0, false) — only one of two PK columns bound", n, ok)
	}
	outer := properties.Cost{Cardinality: 10}
	inner := properties.Cost{Cardinality: 1000}
	got := plan.HintCost([]properties.Cost{outer, inner}, nil)
	want := outer.Cardinality * inner.Cardinality * properties.FilterSelectivity
	if got.Cardinality != want {
		t.Fatalf("Cardinality = %v, want %v", got.Cardinality, want)
	}
}
