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
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan(nil, exactTestRecordType(), false)
	})
	assertPlanKeyEqual(t, mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(scan, 10, 5)
	}), mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(scan, 10, 5)
	}))
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(scan, 10, 5)
	}), mustChecked(t, func() (*RecordQueryLimitPlan, // limit
		error,
	) {
		return NewRecordQueryLimitPlan(scan, 11, 5)
	}))
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(scan, 10, 5)
	}), mustChecked(t, func() (*RecordQueryLimitPlan, // offset
		error,
	) {
		// A different plan class with coincidentally-similar shape is never equal.
		return NewRecordQueryLimitPlan(scan, 10, 6)
	}))

	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(scan, 10, 5)
	}), scan)
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
// load-bearing. One block per plan — a per-plan net that pins every
// identifying field individually.
func TestMigratedPlans_StructuralKeyContract(t *testing.T) {
	t.Parallel()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan(nil, exactTestRecordType(

		// distinct — Streaming is the sole identifying field.
		), false)
	})

	d1 := mustChecked(t, func() (*RecordQueryDistinctPlan, error) {
		return NewRecordQueryDistinctPlan(scan)
	})
	assertPlanKeyEqual(t, d1, mustChecked(t, func() (*RecordQueryDistinctPlan, error) {
		return NewRecordQueryDistinctPlan(scan)
	}))
	dStream := mustChecked(t, func() (*RecordQueryDistinctPlan, error) {
		return NewRecordQueryDistinctPlan(scan)
	})
	dStream.Streaming = true
	assertPlanKeyUnequal(t, d1, dStream)

	// typefilter — the record-type list (dedupSortedStrings-normalized at
	// construction, so equality is on the normalized set; length/content
	// distinguish).
	assertPlanKeyEqual(t, mustChecked(t, func() (*RecordQueryTypeFilterPlan, error) {
		return NewRecordQueryTypeFilterPlan([]string{"A", "B"}, scan)
	}), mustChecked(t, func() (*RecordQueryTypeFilterPlan, error) {
		return NewRecordQueryTypeFilterPlan([]string{"B", "A"}, scan)
	})) // both normalize to [A,B]
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQueryTypeFilterPlan, error) {
		return NewRecordQueryTypeFilterPlan([]string{"A"}, scan)
	}), mustChecked(t, func() (*RecordQueryTypeFilterPlan, error) {
		return NewRecordQueryTypeFilterPlan([]string{"A", "B"}, scan)
	}))

	// default_on_empty — the default Value.
	nv := values.NewNullValue(values.NullableLong)
	recordNull := values.NewNullValue(values.WithNullability(exactTestRecordType(), true))
	assertPlanKeyEqual(t, mustChecked(t, func() (*RecordQueryDefaultOnEmptyPlan, error) {
		return NewRecordQueryDefaultOnEmptyPlan(scan, recordNull)
	}), mustChecked(t, func() (*RecordQueryDefaultOnEmptyPlan, error) {
		return NewRecordQueryDefaultOnEmptyPlan(scan, recordNull)
	}))

	// delete — targetRecordType.
	assertPlanKeyEqual(t, mustChecked(t, func() (*RecordQueryDeletePlan, error) {
		return NewRecordQueryDeletePlan(scan, "T")
	}), mustChecked(t, func() (*RecordQueryDeletePlan, error) {
		return NewRecordQueryDeletePlan(scan, "T")
	}))
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQueryDeletePlan, error) {
		return NewRecordQueryDeletePlan(scan, "T")
	}), mustChecked(t, func() (*RecordQueryDeletePlan, error) {
		return NewRecordQueryDeletePlan(scan, "U")
	}))

	// values — the column Value list (leaf); length is load-bearing.
	assertPlanKeyEqual(t, mustChecked(t, func() (*RecordQueryValuesPlan, error) {
		return NewRecordQueryValuesPlan([]values.Value{nv})
	}), mustChecked(t, func() (*RecordQueryValuesPlan, error) {
		return NewRecordQueryValuesPlan([]values.Value{nv})
	}))
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQueryValuesPlan, error) {
		return NewRecordQueryValuesPlan([]values.Value{nv})
	}), mustChecked(t, func() (*RecordQueryValuesPlan, error) {
		return NewRecordQueryValuesPlan([]values.Value{nv, nv})
	}))

	// map — resultValue.
	assertPlanKeyEqual(t, mustChecked(t, func() (*RecordQueryMapPlan, error) {
		return NewRecordQueryMapPlan(scan, nv)
	}), mustChecked(t, func() (*RecordQueryMapPlan,

		// projection — the projection Value list; length load-bearing.
		error,
	) {
		return NewRecordQueryMapPlan(scan, nv)
	}))

	assertPlanKeyEqual(t, mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan([]values.Value{nv}, scan)
	}), mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan([]values.Value{nv}, scan)
	}))
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan([]values.Value{nv}, scan)
	}), mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan([]values.Value{nv, nv}, scan)
	}))

	// first_or_default — the strict flag is load-bearing (strict vs non-strict
	// constructor variants).
	assertPlanKeyEqual(t, mustChecked(t, func() (*RecordQueryFirstOrDefaultPlan, error) {
		return NewRecordQueryFirstOrDefaultPlan(scan, nv)
	}), mustChecked(t, func() (*RecordQueryFirstOrDefaultPlan, error) {
		return NewRecordQueryFirstOrDefaultPlan(scan, nv)
	}))
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQueryFirstOrDefaultPlan, error) {
		return NewRecordQueryFirstOrDefaultPlan(scan, nv)
	}), mustChecked(t, func() (*RecordQueryFirstOrDefaultPlan, error) {
		return NewRecordQueryFirstOrDefaultPlanStrict(scan, nv)
	}))

	// unordered_pk_distinct — no identifying fields; two instances always match.
	assertPlanKeyEqual(t, mustChecked(t, func() (*RecordQueryUnorderedPrimaryKeyDistinctPlan, error) {
		return NewRecordQueryUnorderedPrimaryKeyDistinctPlan(scan)
	}), mustChecked(t, func() (*RecordQueryUnorderedPrimaryKeyDistinctPlan, error) {
		// --- RFC-184 W3: the final 11 plans + the 5 new builder kinds
		// (ValuePtr / IntPtr / SortKeys / Equatable / Sub). ---
		return NewRecordQueryUnorderedPrimaryKeyDistinctPlan(scan)
	}))

	// insert — targetRecordType (Str) distinguishes; targetType is equals-only.
	assertPlanKeyEqual(t, mustChecked(t, func() (*RecordQueryInsertPlan, error) {
		return NewRecordQueryInsertPlan(scan, "T", exactTestRecordType())
	}), mustChecked(t, func() (*RecordQueryInsertPlan, error) {
		return NewRecordQueryInsertPlan(scan, "T", exactTestRecordType())
	}))
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQueryInsertPlan, error) {
		return NewRecordQueryInsertPlan(scan, "T", exactTestRecordType())
	}), mustChecked(t, func() (*RecordQueryInsertPlan, error) {
		return NewRecordQueryInsertPlan(scan, "U", exactTestRecordType())
	}))

	// text_index — reverse + indexName + the decomposed TextScan string fields.
	ts := TextScan{IndexName: "ti", GroupingComparisons: "g", TextComparison: "t", SuffixComparisons: "s"}
	assertPlanKeyEqual(t, mustChecked(t, func() (*RecordQueryTextIndexPlan, error) {
		return NewRecordQueryTextIndexPlan("ti", ts, exactTestRecordType(), false)
	}), mustChecked(t, func() (*RecordQueryTextIndexPlan, error) {
		return NewRecordQueryTextIndexPlan("ti", ts, exactTestRecordType(), false)
	}))
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQueryTextIndexPlan, error) {
		return NewRecordQueryTextIndexPlan("ti", ts, exactTestRecordType(), false)
	}), mustChecked(t, func() (*RecordQueryTextIndexPlan,
		// reverse
		error,
	) {
		return NewRecordQueryTextIndexPlan("ti", ts, exactTestRecordType(), true)
	}))
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQueryTextIndexPlan, error) {
		return NewRecordQueryTextIndexPlan("ti", ts, exactTestRecordType(), false)
	}), mustChecked(t, func() (*RecordQueryTextIndexPlan, error) {
		return NewRecordQueryTextIndexPlan("ti", TextScan{IndexName: "ti", TextComparison: "OTHER"}, exactTestRecordType( // textScan field
		), false)
	}))

	// explode — collectionValue by POINTER identity (ValuePtr): the SAME instance
	// is equal; two DISTINCT but semantically-equal Values are NOT (the pointer-vs-
	// semantic distinction the kind guards). withOrdinality is load-bearing.
	arrayType := &values.ArrayType{ElementType: values.NullableLong}
	cv := &values.ConstantValue{Value: []any(nil), Typ: arrayType}
	assertPlanKeyEqual(t, mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(cv)
	}), mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(cv)
	}))
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(cv)
	}), mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(&values.ConstantValue{Value: []any(nil), Typ: arrayType})
	})) // distinct pointer, not semantic
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(cv)
	}), mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlanWithOrdinality(cv, true)
	})) // withOrdinality

	// table_function — streamValue by POINTER identity (ValuePtr).
	sv := &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}
	assertPlanKeyEqual(t, mustChecked(t, func() (*RecordQueryTableFunctionPlan, error) {
		return NewRecordQueryTableFunctionPlan(sv)
	}), mustChecked(t, func() (*RecordQueryTableFunctionPlan, error) {
		return NewRecordQueryTableFunctionPlan(sv)
	}))
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQueryTableFunctionPlan, error) {
		return NewRecordQueryTableFunctionPlan(sv)
	}), mustChecked(t, func() (*RecordQueryTableFunctionPlan, error) {
		return NewRecordQueryTableFunctionPlan(&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong})
	})) // distinct pointer

	// in_memory_sort — sort-key list (SortKeys) via sortKeyEqual; direction and
	// length are load-bearing.
	sk := SortKey{Field: "f", ValueExpr: nv}
	assertPlanKeyEqual(t, mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(scan, []SortKey{sk})
	}), mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(scan, []SortKey{sk})
	}))
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(scan, []SortKey{sk})
	}), mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(scan, []SortKey{{Field: "f", Desc: true, ValueExpr: nv}})
	})) // Desc
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(scan, []SortKey{sk})
	}), mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(scan, []SortKey{sk, sk})
	})) // length

	// selector — reverse + PlanSelector via its own .Equals()/.String() (Equatable).
	selChildren := []RecordQueryPlan{scan, scan}
	assertPlanKeyEqual(t, mustChecked(t, func() (*RecordQuerySelectorPlan, error) {
		return NewRecordQuerySelectorPlanWithProbabilities(selChildren, []int{50, 50}, false)
	}), mustChecked(t, func() (*RecordQuerySelectorPlan, error) {
		return NewRecordQuerySelectorPlanWithProbabilities(selChildren, []int{50, 50}, false)
	}))
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQuerySelectorPlan, error) {
		return NewRecordQuerySelectorPlanWithProbabilities(selChildren, []int{50, 50}, false)
	}), mustChecked(t, func() (*RecordQuerySelectorPlan, error) {
		return NewRecordQuerySelectorPlanWithProbabilities(selChildren, []int{30, 70}, false)
	})) // selector identity
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQuerySelectorPlan, error) {
		return NewRecordQuerySelectorPlanWithProbabilities(selChildren, []int{50, 50}, false)
	}), mustChecked(t, func() (*RecordQuerySelectorPlan, error) {
		return NewRecordQuerySelectorPlanWithProbabilities(selChildren, []int{50, 50}, true)
	})) // reverse

	// load_by_keys — KeysSource via its own .Equals()/.String() (Equatable).
	assertPlanKeyEqual(t, mustChecked(t, func() (*RecordQueryLoadByKeysPlan, error) {
		return NewRecordQueryLoadByKeysPlanFromParameter("p", exactTestRecordType())
	}), mustChecked(t, func() (*RecordQueryLoadByKeysPlan, error) {
		return NewRecordQueryLoadByKeysPlanFromParameter("p", exactTestRecordType())
	}))
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQueryLoadByKeysPlan, error) {
		return NewRecordQueryLoadByKeysPlanFromParameter("p", exactTestRecordType())
	}), mustChecked(t, func() (*RecordQueryLoadByKeysPlan, error) {
		// parameter name
		return NewRecordQueryLoadByKeysPlanFromParameter("q", exactTestRecordType())
	}))

	// in_join — sorted/reverse + the static IN-list (Equatable via inValuesEqual);
	// bindingName is EXCLUDED (alias-invariant identity, RFC-164 WS-4).
	ij := func(binding string, vals []any) *RecordQueryInJoinPlan {
		p := mustChecked(t, func() (*RecordQueryInJoinPlan, error) {
			return NewRecordQueryInJoinPlan(scan, binding, false, false)
		})
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
		p := mustChecked(t, func() (*RecordQueryInUnionPlan, error) {
			return NewRecordQueryInUnionPlan(scan, bindings, keys, false)
		})
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
		return mustChecked(t, func() (*RecordQueryIndexPlan, error) {
			return NewRecordQueryIndexPlan(name, nil, []string{"R"}, exactTestRecordType(), false)
		})
	}
	assertPlanKeyEqual(t, mustChecked(t, func() (*RecordQueryAggregateIndexPlan, error) {
		return NewRecordQueryAggregateIndexPlan(idx("i"), "R", exactTestRecordType(), "COUNT")
	}), mustChecked(t, func() (*RecordQueryAggregateIndexPlan, error) {
		return NewRecordQueryAggregateIndexPlan(idx("i"), "R", exactTestRecordType(), "COUNT")
	}))
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQueryAggregateIndexPlan, error) {
		return NewRecordQueryAggregateIndexPlan(idx("i"), "R", exactTestRecordType(), "COUNT")
	}), mustChecked(t, func() (*RecordQueryAggregateIndexPlan, error) {
		return NewRecordQueryAggregateIndexPlan(idx("i"), "R", exactTestRecordType(), "SUM")
	})) // aggregateFunction
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQueryAggregateIndexPlan, error) {
		return NewRecordQueryAggregateIndexPlan(idx("i"), "R", exactTestRecordType(), "COUNT")
	}), mustChecked(t, func() (*RecordQueryAggregateIndexPlan, error) {
		return NewRecordQueryAggregateIndexPlan(idx("j"), "R", exactTestRecordType(), "COUNT")
	})) // nested index (Sub)

	// vector_index_scan — index name + efSearch (IntPtr, nil vs non-nil) + query
	// vector / k (StructVal) + prefix comps + record types.
	ef5 := 5
	vec := func(name string, ef *int) *RecordQueryVectorIndexPlan {
		return mustChecked(t, func() (*RecordQueryVectorIndexPlan, error) {
			return NewRecordQueryVectorIndexPlan(name, nil, nv, nv,
				predicates.ComparisonDistanceRankLessThanOrEq, ef, nil, []string{"R"}, exactTestRecordType())
		})
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

	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan(nil, exactTestRecordType(

		// ONE baked key value, addressed by ordinal, rendered two ways.
		), false)
	})

	keyLayout := values.NewRecordType("sort_key", false, []values.Field{
		{Name: "A", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "B", FieldType: values.NullableLong, Ordinal: 1},
	})
	key := testFieldIn(t, keyLayout, "sort_key", "A")

	assertPlanKeyEqual(t, mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(scan, []SortKey{{Field: "A", ValueExpr: key}})
	}), mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(scan, []SortKey{{Field: "ALIASED", ValueExpr: key}})
	}))

	// And the direction still separates them, so the relaxation is not blanket.
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(scan, []SortKey{{Field: "A", ValueExpr: key}})
	}), mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(scan, []SortKey{{Field: "A", Desc: true, ValueExpr: key}})
	}))

	// A DIFFERENT ordinal under the SAME display name stays two plans — the
	// ordinal is what carries the identity now, so it has to be load-bearing in
	// both directions.
	other := testFieldIn(t, keyLayout, "sort_key", "B")
	assertPlanKeyUnequal(t, mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(scan, []SortKey{{Field: "A", ValueExpr: key}})
	}), mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(scan, []SortKey{{Field: "A", ValueExpr: other}})
	}))
}
