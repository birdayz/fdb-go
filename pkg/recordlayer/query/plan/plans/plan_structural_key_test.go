package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
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

	// --- RFC-184 W3: the final 11 plans + the 5 new builder kinds
	// (ValuePtr / IntPtr / SortKeys / Equatable / Sub). ---

	// insert — targetRecordType (Str) distinguishes; targetType is equals-only.
	assertPlanKeyEqual(t,
		NewRecordQueryInsertPlan(scan, "T", values.UnknownType),
		NewRecordQueryInsertPlan(scan, "T", values.UnknownType))
	assertPlanKeyUnequal(t,
		NewRecordQueryInsertPlan(scan, "T", values.UnknownType),
		NewRecordQueryInsertPlan(scan, "U", values.UnknownType))

	// text_index — reverse + indexName + the decomposed TextScan string fields.
	ts := TextScan{IndexName: "ti", GroupingComparisons: "g", TextComparison: "t", SuffixComparisons: "s"}
	assertPlanKeyEqual(t,
		NewRecordQueryTextIndexPlan("ti", ts, false),
		NewRecordQueryTextIndexPlan("ti", ts, false))
	assertPlanKeyUnequal(t,
		NewRecordQueryTextIndexPlan("ti", ts, false),
		NewRecordQueryTextIndexPlan("ti", ts, true)) // reverse
	assertPlanKeyUnequal(t,
		NewRecordQueryTextIndexPlan("ti", ts, false),
		NewRecordQueryTextIndexPlan("ti", TextScan{IndexName: "ti", TextComparison: "OTHER"}, false)) // textScan field

	// explode — collectionValue by POINTER identity (ValuePtr): the SAME instance
	// is equal; two DISTINCT but semantically-equal Values are NOT (the pointer-vs-
	// semantic distinction the kind guards). withOrdinality is load-bearing.
	cv := values.NewNullValue(values.UnknownType)
	assertPlanKeyEqual(t, NewRecordQueryExplodePlan(cv), NewRecordQueryExplodePlan(cv))
	assertPlanKeyUnequal(t,
		NewRecordQueryExplodePlan(cv),
		NewRecordQueryExplodePlan(values.NewNullValue(values.UnknownType))) // distinct pointer, not semantic
	assertPlanKeyUnequal(t,
		NewRecordQueryExplodePlan(cv),
		NewRecordQueryExplodePlanWithOrdinality(cv, true)) // withOrdinality

	// table_function — streamValue by POINTER identity (ValuePtr).
	sv := values.NewNullValue(values.UnknownType)
	assertPlanKeyEqual(t, NewRecordQueryTableFunctionPlan(sv), NewRecordQueryTableFunctionPlan(sv))
	assertPlanKeyUnequal(t,
		NewRecordQueryTableFunctionPlan(sv),
		NewRecordQueryTableFunctionPlan(values.NewNullValue(values.UnknownType))) // distinct pointer

	// in_memory_sort — sort-key list (SortKeys) via sortKeyEqual; direction and
	// length are load-bearing.
	sk := SortKey{Field: "f", ValueExpr: nv}
	assertPlanKeyEqual(t,
		NewRecordQueryInMemorySortPlan(scan, []SortKey{sk}),
		NewRecordQueryInMemorySortPlan(scan, []SortKey{sk}))
	assertPlanKeyUnequal(t,
		NewRecordQueryInMemorySortPlan(scan, []SortKey{sk}),
		NewRecordQueryInMemorySortPlan(scan, []SortKey{{Field: "f", Desc: true, ValueExpr: nv}})) // Desc
	assertPlanKeyUnequal(t,
		NewRecordQueryInMemorySortPlan(scan, []SortKey{sk}),
		NewRecordQueryInMemorySortPlan(scan, []SortKey{sk, sk})) // length

	// selector — reverse + PlanSelector via its own .Equals()/.String() (Equatable).
	selChildren := []RecordQueryPlan{scan, scan}
	assertPlanKeyEqual(t,
		NewRecordQuerySelectorPlanWithProbabilities(selChildren, []int{50, 50}, false),
		NewRecordQuerySelectorPlanWithProbabilities(selChildren, []int{50, 50}, false))
	assertPlanKeyUnequal(t,
		NewRecordQuerySelectorPlanWithProbabilities(selChildren, []int{50, 50}, false),
		NewRecordQuerySelectorPlanWithProbabilities(selChildren, []int{30, 70}, false)) // selector identity
	assertPlanKeyUnequal(t,
		NewRecordQuerySelectorPlanWithProbabilities(selChildren, []int{50, 50}, false),
		NewRecordQuerySelectorPlanWithProbabilities(selChildren, []int{50, 50}, true)) // reverse

	// load_by_keys — KeysSource via its own .Equals()/.String() (Equatable).
	assertPlanKeyEqual(t,
		NewRecordQueryLoadByKeysPlanFromParameter("p"),
		NewRecordQueryLoadByKeysPlanFromParameter("p"))
	assertPlanKeyUnequal(t,
		NewRecordQueryLoadByKeysPlanFromParameter("p"),
		NewRecordQueryLoadByKeysPlanFromParameter("q")) // parameter name

	// in_join — sorted/reverse + the static IN-list (Equatable via inValuesEqual);
	// bindingName is EXCLUDED (alias-invariant identity, RFC-164 WS-4).
	ij := func(binding string, vals []any) *RecordQueryInJoinPlan {
		p := NewRecordQueryInJoinPlan(scan, binding, false, false)
		p.SetInValues(vals)
		return p
	}
	assertPlanKeyEqual(t,
		ij("b0", []any{int64(1), int64(2)}),
		ij("b1", []any{int64(1), int64(2)})) // binding name excluded
	assertPlanKeyUnequal(t,
		ij("b0", []any{int64(1), int64(2)}),
		ij("b0", []any{int64(3), int64(4)})) // inValues comparand included

	// in_union — reverse + bindingNames COUNT + comparisonKeys (Values) + inSources
	// (Equatable/DeepEqual); binding NAMES excluded (only count structural).
	iu := func(bindings []string, keys []values.Value, sources [][]any) *RecordQueryInUnionPlan {
		p := NewRecordQueryInUnionPlan(scan, bindings, keys, false)
		p.SetInSources(sources)
		return p
	}
	assertPlanKeyEqual(t,
		iu([]string{"x"}, []values.Value{nv}, [][]any{{int64(1)}}),
		iu([]string{"y"}, []values.Value{nv}, [][]any{{int64(1)}})) // binding names excluded, count equal
	assertPlanKeyUnequal(t,
		iu([]string{"x"}, []values.Value{nv}, [][]any{{int64(1)}}),
		iu([]string{"x", "z"}, []values.Value{nv}, [][]any{{int64(1)}})) // binding COUNT
	assertPlanKeyUnequal(t,
		iu([]string{"x"}, []values.Value{nv}, [][]any{{int64(1)}}),
		iu([]string{"x"}, []values.Value{nv}, [][]any{{int64(2)}})) // inSources literal (same dims → hash collides, eq splits)

	// aggregate_index — recordTypeName + aggregateFunction + the nested index plan
	// (Sub, compared via its OWN structuralKey, not by pointer).
	idx := func(name string) *RecordQueryIndexPlan {
		return NewRecordQueryIndexPlan(name, nil, []string{"R"}, values.UnknownType, false)
	}
	assertPlanKeyEqual(t,
		NewRecordQueryAggregateIndexPlan(idx("i"), "R", values.UnknownType, "COUNT"),
		NewRecordQueryAggregateIndexPlan(idx("i"), "R", values.UnknownType, "COUNT"))
	assertPlanKeyUnequal(t,
		NewRecordQueryAggregateIndexPlan(idx("i"), "R", values.UnknownType, "COUNT"),
		NewRecordQueryAggregateIndexPlan(idx("i"), "R", values.UnknownType, "SUM")) // aggregateFunction
	assertPlanKeyUnequal(t,
		NewRecordQueryAggregateIndexPlan(idx("i"), "R", values.UnknownType, "COUNT"),
		NewRecordQueryAggregateIndexPlan(idx("j"), "R", values.UnknownType, "COUNT")) // nested index (Sub)

	// vector_index_scan — index name + efSearch (IntPtr, nil vs non-nil) + query
	// vector / k (StructVal) + prefix comps + record types.
	ef5 := 5
	vec := func(name string, ef *int) *RecordQueryVectorIndexPlan {
		return NewRecordQueryVectorIndexPlan(name, nil, nv, nv,
			predicates.ComparisonDistanceRankLessThanOrEq, ef, nil, []string{"R"}, values.UnknownType)
	}
	assertPlanKeyEqual(t, vec("v", &ef5), vec("v", &ef5))
	assertPlanKeyUnequal(t, vec("v", &ef5), vec("v", nil))  // efSearch nil vs non-nil (IntPtr)
	assertPlanKeyUnequal(t, vec("v", &ef5), vec("w", &ef5)) // index name
}

// TestSortKeyIdentityIgnoresDisplayName is the dimension the sort-key identity
// was never probed on: two sort keys over the SAME baked key Value, rendered
// under DIFFERENT display names, are ONE plan.
//
// Every other in_memory_sort case in TestPlanStructuralKeyDimensions holds the
// name equal, so none of them can tell a Value-keyed identity from a
// name-keyed one. Folding the name split one plan into two memo members whenever
// two producers named the same baked column differently — the RFC-197 conflation
// in its splitting direction — and the equal-implies-same-hash invariant is
// checked here too, because dropping the name from equality without dropping it
// from the hash is a silently broken memo.
func TestSortKeyIdentityIgnoresDisplayName(t *testing.T) {
	t.Parallel()

	scan := NewRecordQueryScanPlan(nil, nil, false)
	// ONE baked key value, addressed by ordinal, rendered two ways.
	key := values.NewFieldValueWithResolvedOrdinal("A", 0, values.UnknownType)
	rendered := values.NewFieldValueWithResolvedOrdinal("ALIASED", 0, values.UnknownType)

	assertPlanKeyEqual(t,
		NewRecordQueryInMemorySortPlan(scan, []SortKey{{Field: "A", ValueExpr: key}}),
		NewRecordQueryInMemorySortPlan(scan, []SortKey{{Field: "ALIASED", ValueExpr: rendered}}))

	// And the direction still separates them, so the relaxation is not blanket.
	assertPlanKeyUnequal(t,
		NewRecordQueryInMemorySortPlan(scan, []SortKey{{Field: "A", ValueExpr: key}}),
		NewRecordQueryInMemorySortPlan(scan, []SortKey{{Field: "A", Desc: true, ValueExpr: key}}))

	// A DIFFERENT ordinal under the SAME display name stays two plans — the
	// ordinal is what carries the identity now, so it has to be load-bearing in
	// both directions.
	other := values.NewFieldValueWithResolvedOrdinal("A", 1, values.UnknownType)
	assertPlanKeyUnequal(t,
		NewRecordQueryInMemorySortPlan(scan, []SortKey{{Field: "A", ValueExpr: key}}),
		NewRecordQueryInMemorySortPlan(scan, []SortKey{{Field: "A", ValueExpr: other}}))
}
