package cascades

// Regression tests for the full-key ordering of index and primary scans:
// index entries are (index key, primary key), so an equality-prefixed scan
// provides an ordering that continues through the trimmed PK suffix, with
// the equality prefix FIXED (compatible with any requested direction).
// Mirrors Java ValueIndexLikeMatchCandidate.computeOrderingFromScanComparisons
// over ValueIndexExpansionVisitor.fullKey(index, primaryKey).
//
// Pinned bug: `SELECT id FROM t WHERE status = 'active' ORDER BY id` (index
// on status, PK id) planned InMemorySort(Fetch(IndexScan)) because the scan's
// derived ordering stopped at the index key ([STATUS FIXED]) — the ID suffix
// was missing, so the sort was not elided and MergeProjectionAndFetchRule
// (Project directly over Fetch) could not fire to produce the covering scan.
// Sibling bug: `WHERE a = 1 ORDER BY a DESC` over PK (a, b) kept the sort
// because the eq-bound PK prefix read as directional ASC instead of FIXED.

import (
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func equalityRange(t *testing.T, literal any) *predicates.ComparisonRange {
	t.Helper()
	cmp := predicates.NewLiteralComparison(predicates.ComparisonEquals, literal)
	res := predicates.EmptyComparisonRange().Merge(&cmp)
	if !res.Ok {
		t.Fatal("failed to build equality comparison range")
	}
	return res.Range
}

func pkSuffixField(
	t testing.TB,
	rowType *values.RecordType,
	name string,
) values.Value {
	t.Helper()
	root, ok := orderingKeyCarrier(rowType)
	if !ok {
		t.Fatalf("build ordering carrier for %s", rowType)
	}
	ordinal, unique := uniqueUpperFieldIndex(rowType, name)
	if !unique {
		t.Fatalf("ordering field %q is not unique in %s", name, rowType)
	}
	fieldValue, fieldErr := values.ResolveFieldOrdinals(root, []int{ordinal})
	return mustConstruct(t, fieldValue, fieldErr)
}

func requestedParts(
	t testing.TB,
	rowType *values.RecordType,
	dirs map[string]properties.RequestedSortOrder,
	order []string,
) []properties.RequestedOrderingPart {
	t.Helper()
	parts := make([]properties.RequestedOrderingPart, 0, len(order))
	for _, name := range order {
		parts = append(parts, properties.RequestedOrderingPart{
			Value:     pkSuffixField(t, rowType, name),
			SortOrder: dirs[name],
		})
	}
	return parts
}

func statusIDRowType() *values.RecordType {
	return values.NewRecordType("T", false, []values.Field{
		{Name: "STATUS", FieldType: values.NullableString, Ordinal: 0},
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 1},
	})
}

func abRowType() *values.RecordType {
	return values.NewRecordType("AB", false, []values.Field{
		{Name: "A", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "B", FieldType: values.NullableLong, Ordinal: 1},
	})
}

func abcRowType() *values.RecordType {
	return values.NewRecordType("KVW", false, []values.Field{
		{Name: "A", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "B", FieldType: values.NullableLong, Ordinal: 1},
		{Name: "C", FieldType: values.NullableLong, Ordinal: 2},
	})
}

func tagsIDRowType() *values.RecordType {
	return values.NewRecordType("T", false, []values.Field{
		{Name: "TAGS", FieldType: values.NewArrayType(true, values.NotNullString), Ordinal: 0},
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 1},
	})
}

// TestIndexScanRichOrdering_PKSuffixSatisfiesOrderByPK: an eq-prefixed
// index scan (STATUS =) over PK (ID) provides [STATUS FIXED, ID ASC],
// which satisfies ORDER BY ID (forward scan) — the yamsql
// covering_index_pushdown regression at the property level.
func TestIndexScanRichOrdering_PKSuffixSatisfiesOrderByPK(t *testing.T) {
	t.Parallel()

	rowType := statusIDRowType()
	idxValue, idxErr := plans.NewRecordQueryIndexPlan(
		"IDX_STATUS",
		[]*predicates.ComparisonRange{equalityRange(t, "active")},
		[]string{"T"}, rowType, false)
	idx := mustConstruct(t, idxValue, idxErr).
		WithKeyComponentTypes([]values.Type{values.NullableString})
	w := idx.WithIndexMetadata([]string{"STATUS"}, []string{"ID"}, false).
		WithPrimaryKeyComponentTypes([]values.Type{values.NullableLong})

	ord := computeWrapperRichOrdering(w)
	if ord == nil {
		t.Fatal("nil rich ordering")
	}

	req := properties.NewRequestedOrdering(
		requestedParts(t, rowType, map[string]properties.RequestedSortOrder{"ID": properties.RequestedSortOrderAscending}, []string{"ID"}),
		properties.DistinctnessPreserveDistinctness, false)
	if !ord.Satisfies(req) {
		t.Fatal("eq-prefixed forward index scan with PK suffix must satisfy ORDER BY pk ASC")
	}

	// A forward scan's PK suffix is ASC — a DESC request is NOT satisfied.
	reqDesc := properties.NewRequestedOrdering(
		requestedParts(t, rowType, map[string]properties.RequestedSortOrder{"ID": properties.RequestedSortOrderDescending}, []string{"ID"}),
		properties.DistinctnessPreserveDistinctness, false)
	if ord.Satisfies(reqDesc) {
		t.Fatal("forward index scan must NOT satisfy ORDER BY pk DESC")
	}
}

// TestIndexScanRichOrdering_ReverseSatisfiesOrderByPKDesc: the REVERSE
// eq-prefixed scan provides [STATUS FIXED, ID DESC] and satisfies
// ORDER BY ID DESC.
func TestIndexScanRichOrdering_ReverseSatisfiesOrderByPKDesc(t *testing.T) {
	t.Parallel()

	rowType := statusIDRowType()
	idxValue, idxErr := plans.NewRecordQueryIndexPlan(
		"IDX_STATUS",
		[]*predicates.ComparisonRange{equalityRange(t, "active")},
		[]string{"T"}, rowType, true /* reverse */)
	idx := mustConstruct(t, idxValue, idxErr).
		WithKeyComponentTypes([]values.Type{values.NullableString})
	w := idx.WithIndexMetadata([]string{"STATUS"}, []string{"ID"}, false).
		WithPrimaryKeyComponentTypes([]values.Type{values.NullableLong})

	ord := computeWrapperRichOrdering(w)
	req := properties.NewRequestedOrdering(
		requestedParts(t, rowType, map[string]properties.RequestedSortOrder{"ID": properties.RequestedSortOrderDescending}, []string{"ID"}),
		properties.DistinctnessPreserveDistinctness, false)
	if !ord.Satisfies(req) {
		t.Fatal("reverse eq-prefixed index scan with PK suffix must satisfy ORDER BY pk DESC")
	}
}

// TestIndexScanRichOrdering_TrimPrimaryKey: PK components already in the
// index key are trimmed from the suffix (Java Index.trimPrimaryKey): index
// on B over PK (A, B, C) yields full key [B, A, C], so with B eq-bound the
// scan satisfies ORDER BY A, C.
func TestIndexScanRichOrdering_TrimPrimaryKey(t *testing.T) {
	t.Parallel()

	rowType := abcRowType()
	idxValue, idxErr := plans.NewRecordQueryIndexPlan(
		"KVW_B",
		[]*predicates.ComparisonRange{equalityRange(t, int64(20))},
		[]string{"KVW"}, rowType, false)
	idx := mustConstruct(t, idxValue, idxErr).
		WithKeyComponentTypes([]values.Type{values.NullableLong})
	w := idx.WithIndexMetadata([]string{"B"}, []string{"A", "B", "C"}, false).
		WithPrimaryKeyComponentTypes([]values.Type{
			values.NullableLong, values.NullableLong, values.NullableLong,
		})

	ord := computeWrapperRichOrdering(w)
	req := properties.NewRequestedOrdering(
		requestedParts(t, rowType, map[string]properties.RequestedSortOrder{
			"A": properties.RequestedSortOrderAscending,
			"C": properties.RequestedSortOrderAscending,
		}, []string{"A", "C"}),
		properties.DistinctnessPreserveDistinctness, false)
	if !ord.Satisfies(req) {
		t.Fatal("eq-bound index(B) over PK (A,B,C) must satisfy ORDER BY A, C (full key [B,A,C], B fixed)")
	}
	if got := len(ord.GetOrderingKeys()); got != 2 {
		t.Fatalf("trimmed suffix should add A and C only (B deduped): %d directional ordering keys, want 2", got)
	}
}

// TestScanRichOrdering_EqualityPrefixIsFixed: a PK scan with the leading
// PK column equality-bound provides [A FIXED, B ASC]; FIXED is compatible
// with ANY requested direction, so both ORDER BY A ASC and ORDER BY A DESC
// are satisfied by the FORWARD scan (Java: Binding.fixed via
// computeOrderingFromScanComparisons — the order_by_elimination
// `WHERE a = 1 ORDER BY a DESC` stanza).
func TestScanRichOrdering_EqualityPrefixIsFixed(t *testing.T) {
	t.Parallel()

	rowType := abRowType()
	pkA := pkSuffixField(t, rowType, "A")
	pkB := pkSuffixField(t, rowType, "B")
	scanValue, scanErr := plans.NewRecordQueryScanPlan([]string{"AB"}, rowType, false)
	scan := mustConstruct(t, scanValue, scanErr).
		WithPrimaryKey([]values.Value{pkA, pkB}).
		WithScanComparisons([]*predicates.ComparisonRange{equalityRange(t, int64(1))}).
		WithKeyComponentTypes([]values.Type{values.NullableLong})
	w := scan

	ord := computeWrapperRichOrdering(w)
	if ord == nil {
		t.Fatal("nil rich ordering")
	}

	for _, dir := range []properties.RequestedSortOrder{properties.RequestedSortOrderAscending, properties.RequestedSortOrderDescending} {
		req := properties.NewRequestedOrdering(
			requestedParts(t, rowType, map[string]properties.RequestedSortOrder{"A": dir}, []string{"A"}),
			properties.DistinctnessPreserveDistinctness, false)
		if !ord.Satisfies(req) {
			t.Fatalf("eq-bound PK prefix must be FIXED and satisfy ORDER BY A %v", dir)
		}
	}

	// The unbound remainder stays directional: B DESC is NOT satisfied by
	// the forward scan.
	reqBDesc := properties.NewRequestedOrdering(
		requestedParts(t, rowType, map[string]properties.RequestedSortOrder{
			"A": properties.RequestedSortOrderDescending,
			"B": properties.RequestedSortOrderDescending,
		}, []string{"A", "B"}),
		properties.DistinctnessPreserveDistinctness, false)
	if ord.Satisfies(reqBDesc) {
		t.Fatal("forward scan must NOT satisfy ORDER BY A DESC, B DESC (B is ASC)")
	}
}

// TestComputeMatchedOrderingParts_PKSuffix: the matched ordering parts of
// a value-index candidate continue into the trimmed PK suffix (unbound,
// directional), so SatisfiesRequestedOrdering can resolve an ORDER BY on
// the PK. Java: computeMatchedOrderingParts walks getFullKeyExpression().
func TestComputeMatchedOrderingParts_PKSuffix(t *testing.T) {
	t.Parallel()

	alias := values.UniqueCorrelationIdentifier()
	cand := newKnownDistinctValueIndexCandidate(
		"IDX_STATUS", []string{"T"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{alias},
		statusIDRowType(), false,
		[]string{"ID"})

	mi := NewRegularMatchInfo(
		map[values.CorrelationIdentifier]*predicates.ComparisonRange{alias: equalityRange(t, "active")},
		nil, nil, nil, nil, nil, nil, nil)

	parts := cand.ComputeMatchedOrderingParts(mi, []values.CorrelationIdentifier{alias}, false)
	if len(parts) != 2 {
		t.Fatalf("want 2 ordering parts (STATUS eq + ID suffix), got %d", len(parts))
	}
	if !parts[0].GetComparisonRange().IsEquality() {
		t.Fatal("STATUS part must carry the equality range")
	}
	idPart := parts[1]
	fv, ok := values.AsFieldValue(idPart.GetValue())
	if !ok || fv.DisplayName() != "ID" {
		t.Fatalf("PK suffix part must be FieldValue(ID), got %v", idPart.GetValue())
	}
	if idPart.GetComparisonRange().IsEquality() {
		t.Fatal("PK suffix part must be unbound (empty comparison range)")
	}
	if idPart.GetMatchedSortOrder() != MatchedSortOrderAscending {
		t.Fatalf("forward PK suffix part must be ASCENDING, got %v", idPart.GetMatchedSortOrder())
	}

	// Reverse: the suffix flips to DESCENDING.
	revParts := cand.ComputeMatchedOrderingParts(mi, []values.CorrelationIdentifier{alias}, true)
	if len(revParts) != 2 || revParts[1].GetMatchedSortOrder() != MatchedSortOrderDescending {
		t.Fatalf("reverse PK suffix part must be DESCENDING: %v", revParts)
	}
}

// TestComputeMatchedOrderingParts_NoSuffixForFanOut pins Go's conservative
// PK-suffix limitation: an equality-fixed fan-out position contributes no
// ordering part, and Go synthesizes no PK suffix for any fan-out candidate.
// Java can skip that equality-bound duplicate-producing position and continue
// through its full-key PK alias; Go keeps PK aliases outside sortParameterIDs
// and cannot prove the same positional continuation here.
func TestComputeMatchedOrderingParts_NoSuffixForFanOut(t *testing.T) {
	t.Parallel()

	alias := values.UniqueCorrelationIdentifier()
	cand := newKnownDistinctValueIndexCandidate(
		"IDX_TAGS", []string{"T"},
		[]string{"TAGS"},
		[]values.CorrelationIdentifier{alias},
		tagsIDRowType(), false,
		[]string{"ID"})
	cand.createsDuplicates = true
	cand.WithRootKeyExpression(candidateTestKeyField("TAGS", gen.Field_FAN_OUT))

	mi := NewRegularMatchInfo(
		map[values.CorrelationIdentifier]*predicates.ComparisonRange{alias: equalityRange(t, "x")},
		nil, nil, nil, nil, nil, nil, nil)

	parts := cand.ComputeMatchedOrderingParts(mi, []values.CorrelationIdentifier{alias}, false)
	if len(parts) != 0 {
		t.Fatalf("fan-out index must expose neither TAGS nor a PK suffix: got %d parts", len(parts))
	}
	fetch, ok := cand.ToScanPlan(
		map[values.CorrelationIdentifier]*predicates.ComparisonRange{
			alias: equalityRange(t, "x"),
		}, false,
	).(*plans.RecordQueryFetchFromPartialRecordPlan)
	if !ok {
		t.Fatalf("fan-out scan = %T, want fetch over index", cand.ToScanPlan(nil, false))
	}
	indexPlan, ok := fetch.GetInner().(*plans.RecordQueryIndexPlan)
	if !ok {
		t.Fatalf("fan-out fetch inner = %T, want index plan", fetch.GetInner())
	}
	if pk := indexPlan.GetPKColumnNames(); len(pk) != 1 || pk[0] != "ID" {
		t.Fatalf("fan-out plan lost coordinate-safe PK coverage names: %v", pk)
	}
	if ordering := indexPlan.HintOrdering(); ordering.IsKnown {
		t.Fatalf("fan-out plan advertised ordering despite retained coverage PK: %#v", ordering)
	}
}

// TestScanPlanExpressionRichOrdering_EqualityPrefixIsFixed: the
// data-access path memoizes a SARGed primary scan as the plan-backed
// leaf scanPlanExpression (NOT the bare RecordQueryScanPlan expression) —
// the ordering hints must be present there too, including through the
// TypeFilter wrapper the primary candidate adds when available and queried
// record types differ. This was the actual carrier of the
// order_by_elimination `WHERE a = 1 ORDER BY a DESC` bug: the bare
// RecordQueryScanPlan expression had an ordering hint, the scanPlanExpression
// the planner actually held did not.
func TestScanPlanExpressionRichOrdering_EqualityPrefixIsFixed(t *testing.T) {
	t.Parallel()

	rowType := abRowType()
	pkA := pkSuffixField(t, rowType, "A")
	pkB := pkSuffixField(t, rowType, "B")
	scanValue, scanErr := plans.NewRecordQueryScanPlan([]string{"AB"}, rowType, false)
	scan := mustConstruct(t, scanValue, scanErr).
		WithPrimaryKey([]values.Value{pkA, pkB}).
		WithScanComparisons([]*predicates.ComparisonRange{equalityRange(t, int64(1))}).
		WithKeyComponentTypes([]values.Type{values.NullableLong})

	reqADesc := properties.NewRequestedOrdering(
		requestedParts(t, rowType, map[string]properties.RequestedSortOrder{"A": properties.RequestedSortOrderDescending}, []string{"A"}),
		properties.DistinctnessPreserveDistinctness, false)

	bare := &scanPlanExpression{plan: scan}
	if ord := computeWrapperRichOrdering(bare); ord == nil || !ord.Satisfies(reqADesc) {
		t.Fatal("scanPlanExpression over eq-bound PK scan must satisfy ORDER BY A DESC (A is FIXED)")
	}

	// Through the TypeFilter wrapper (order-preserving).
	tfValue, tfErr := plans.NewRecordQueryTypeFilterPlan([]string{"AB"}, scan)
	tf := mustConstruct(t, tfValue, tfErr)
	wrapped := &scanPlanExpression{plan: tf}
	if ord := computeWrapperRichOrdering(wrapped); ord == nil || !ord.Satisfies(reqADesc) {
		t.Fatal("scanPlanExpression over TypeFilter(eq-bound PK scan) must satisfy ORDER BY A DESC")
	}
}

// TestSortElim_EqPrefixIndexScanSatisfiesOrderByPK is the end-to-end
// planner pin for the covering_index_pushdown regression: Sort(ID) over
// Filter(STATUS = 'active') with an index on STATUS over PK (ID) must
// elide the sort — the eq-prefixed index scan's full-key ordering
// [STATUS FIXED, ID ASC] satisfies the request. Red before the PK-suffix
// ordering fix (planned InMemorySort over Fetch(IndexScan)).
func TestSortElim_EqPrefixIndexScanSatisfiesOrderByPK(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := newKnownDistinctValueIndexCandidate(
		"IDX_STATUS",
		[]string{"T"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		statusIDRowType(),
		false,
		[]string{"ID"})
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := mustFullUnorderedScan(t, []string{"T"}, statusIDRowType())
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	filterValue, filterErr := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				func() values.Value {
					rootValue, rootErr := q.RequireFlowedObjectValue()
					root := mustConstruct(t, rootValue, rootErr)
					fieldValue, fieldErr := values.ResolveFieldOrdinals(root, []int{0})
					return mustConstruct(t, fieldValue, fieldErr)
				}(),
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
		},
		q,
	)
	filter := mustConstruct(t, filterValue, filterErr)
	filterQ := expressions.ForEachQuantifier(expressions.InitialOf(filter))
	filterRootValue, filterRootErr := filterQ.RequireFlowedObjectValue()
	filterRoot := mustConstruct(t, filterRootValue, filterRootErr)
	idValue, idErr := values.ResolveFieldOrdinals(filterRoot, []int{1})
	id := mustConstruct(t, idValue, idErr)
	sortValue, sortErr := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: id}},
		filterQ,
	)
	sort := mustConstruct(t, sortValue, sortErr)
	sortRef := expressions.InitialOf(sort)

	p := NewPlanner(DefaultExpressionRules(), ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(sortRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if _, isSort := plan.(*plans.RecordQueryInMemorySortPlan); isSort {
		ph, _ := plan.(physicalPlanExpression)
		t.Fatalf("ORDER BY pk over eq-prefixed index scan must elide the sort; got %s",
			ph.GetRecordQueryPlan().Explain())
	}
	if !IsPhysicalIndexScan(plan) && !IsPhysicalFetchFromPartialRecord(plan) && !IsPhysicalFilter(plan) {
		t.Fatalf("expected an index-scan-shaped plan, got %T", plan)
	}
}

// THE SAME GAP, ONE PLAN TYPE OVER: an INDEX access memoized as the plan-backed
// leaf must state its ordering too.
//
// The test above closed the primary-scan half — the data-access path memoizes a
// SARGed scan as scanPlanExpression, not as the bare plan expression, so the
// ordering hints have to be present there. The adapter's hints then narrowed to
// *RecordQueryScanPlan and answered "no ordering" for every index access.
//
// The rich form is where that hurts, because it is the only one carrying FIXED
// bindings. Two members of the same group — a forward index probe and its
// REVERSE twin — both answered EMPTY, so nothing at the sort boundary could
// tell them apart and the reverse alternative was never selected:
// `ORDER BY <equality-bound float> DESC` over a correlated range set lost its
// reverse composite probe to an InMemorySort.
//
// Driven in BOTH directions, because an empty ordering trivially "differs" from
// nothing: the forward member must NOT satisfy the DESC request and the reverse
// member must.
func TestScanPlanExpressionRichOrdering_IndexAccessStatesItsOrder(t *testing.T) {
	t.Parallel()

	rowType := abRowType()
	index := func(reverse bool) *plans.RecordQueryIndexPlan {
		value, err := plans.NewRecordQueryIndexPlan(
			"IDX_A", []*predicates.ComparisonRange{equalityRange(t, int64(1))},
			[]string{"AB"}, rowType, reverse)
		return mustConstruct(t, value, err).
			WithKeyComponentTypes([]values.Type{values.NullableLong}).
			WithIndexMetadata([]string{"A"}, []string{"B"}, false)
	}
	forward, reverse := index(false), index(true)

	reqBDesc := properties.NewRequestedOrdering(
		requestedParts(t, rowType,
			map[string]properties.RequestedSortOrder{"B": properties.RequestedSortOrderDescending},
			[]string{"B"}),
		properties.DistinctnessPreserveDistinctness, false)
	reqBAsc := properties.NewRequestedOrdering(
		requestedParts(t, rowType,
			map[string]properties.RequestedSortOrder{"B": properties.RequestedSortOrderAscending},
			[]string{"B"}),
		properties.DistinctnessPreserveDistinctness, false)

	// CONTROL: the bare plan expression has always stated this. If it stops,
	// the adapter assertions below prove nothing about the adapter.
	if ord := computeWrapperRichOrdering(forward); ord == nil || len(ord.GetKeys()) == 0 {
		t.Fatal("the bare index plan expression states no ordering — the control for " +
			"this test is gone, so nothing below is about the adapter")
	}

	for _, tc := range []struct {
		name    string
		plan    plans.RecordQueryPlan
		wantAsc bool
	}{
		{"forward", forward, true},
		{"reverse", reverse, false},
		// Through the order-preserving TypeFilter wrapper the primary candidate
		// adds, which is the shape the adapter's unwrap exists for.
		{"forward under a type filter", typeFilterOver(t, forward), true},
		{"reverse under a type filter", typeFilterOver(t, reverse), false},
	} {
		adapter := &scanPlanExpression{plan: tc.plan}
		ord := computeWrapperRichOrdering(adapter)
		if ord == nil || len(ord.GetKeys()) == 0 {
			t.Fatalf("%s: the adapter reports NO ordering for an index access.\n"+
				"  The data-access path memoizes the access through this adapter, so an\n"+
				"  empty answer here is what the sort boundary sees — and it is the same\n"+
				"  answer the REVERSE twin gives, which is why neither can be selected.",
				tc.name)
		}
		if got := ord.Satisfies(reqBAsc); got != tc.wantAsc {
			t.Fatalf("%s: satisfies(ORDER BY B ASC) = %v, want %v", tc.name, got, tc.wantAsc)
		}
		if got := ord.Satisfies(reqBDesc); got == tc.wantAsc {
			t.Fatalf("%s: satisfies(ORDER BY B DESC) = %v, want %v — the two directions must "+
				"NOT answer the same, or the scan direction is not being reported at all",
				tc.name, got, !tc.wantAsc)
		}
	}
}

func typeFilterOver(t *testing.T, inner plans.RecordQueryPlan) plans.RecordQueryPlan {
	t.Helper()
	value, err := plans.NewRecordQueryTypeFilterPlan([]string{"AB"}, inner)
	return mustConstruct(t, value, err)
}
