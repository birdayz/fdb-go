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

func requestedParts(dirs map[string]properties.RequestedSortOrder, order []string) []properties.RequestedOrderingPart {
	parts := make([]properties.RequestedOrderingPart, 0, len(order))
	for _, name := range order {
		parts = append(parts, properties.RequestedOrderingPart{
			Value:     &values.FieldValue{Field: name, Typ: values.UnknownType},
			SortOrder: dirs[name],
		})
	}
	return parts
}

// TestIndexScanRichOrdering_PKSuffixSatisfiesOrderByPK: an eq-prefixed
// index scan (STATUS =) over PK (ID) provides [STATUS FIXED, ID ASC],
// which satisfies ORDER BY ID (forward scan) — the yamsql
// covering_index_pushdown regression at the property level.
func TestIndexScanRichOrdering_PKSuffixSatisfiesOrderByPK(t *testing.T) {
	t.Parallel()

	idx := plans.NewRecordQueryIndexPlan(
		"IDX_STATUS",
		[]*predicates.ComparisonRange{equalityRange(t, "active")},
		[]string{"T"}, values.UnknownType, false)
	w := idx.WithIndexMetadata([]string{"STATUS"}, []string{"ID"}, false)

	ord := computeWrapperRichOrdering(w)
	if ord == nil {
		t.Fatal("nil rich ordering")
	}

	req := properties.NewRequestedOrdering(
		requestedParts(map[string]properties.RequestedSortOrder{"ID": properties.RequestedSortOrderAscending}, []string{"ID"}),
		properties.DistinctnessPreserveDistinctness, false)
	if !ord.Satisfies(req) {
		t.Fatal("eq-prefixed forward index scan with PK suffix must satisfy ORDER BY pk ASC")
	}

	// A forward scan's PK suffix is ASC — a DESC request is NOT satisfied.
	reqDesc := properties.NewRequestedOrdering(
		requestedParts(map[string]properties.RequestedSortOrder{"ID": properties.RequestedSortOrderDescending}, []string{"ID"}),
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

	idx := plans.NewRecordQueryIndexPlan(
		"IDX_STATUS",
		[]*predicates.ComparisonRange{equalityRange(t, "active")},
		[]string{"T"}, values.UnknownType, true /* reverse */)
	w := idx.WithIndexMetadata([]string{"STATUS"}, []string{"ID"}, false)

	ord := computeWrapperRichOrdering(w)
	req := properties.NewRequestedOrdering(
		requestedParts(map[string]properties.RequestedSortOrder{"ID": properties.RequestedSortOrderDescending}, []string{"ID"}),
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

	idx := plans.NewRecordQueryIndexPlan(
		"KVW_B",
		[]*predicates.ComparisonRange{equalityRange(t, int64(20))},
		[]string{"KVW"}, values.UnknownType, false)
	w := idx.WithIndexMetadata([]string{"B"}, []string{"A", "B", "C"}, false)

	ord := computeWrapperRichOrdering(w)
	req := properties.NewRequestedOrdering(
		requestedParts(map[string]properties.RequestedSortOrder{
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

	pkA := &values.FieldValue{Field: "A", Typ: values.UnknownType}
	pkB := &values.FieldValue{Field: "B", Typ: values.UnknownType}
	scan := plans.NewRecordQueryScanPlan([]string{"AB"}, values.UnknownType, false).
		WithPrimaryKey([]values.Value{pkA, pkB}).
		WithScanComparisons([]*predicates.ComparisonRange{equalityRange(t, int64(1))})
	w := scan

	ord := computeWrapperRichOrdering(w)
	if ord == nil {
		t.Fatal("nil rich ordering")
	}

	for _, dir := range []properties.RequestedSortOrder{properties.RequestedSortOrderAscending, properties.RequestedSortOrderDescending} {
		req := properties.NewRequestedOrdering(
			requestedParts(map[string]properties.RequestedSortOrder{"A": dir}, []string{"A"}),
			properties.DistinctnessPreserveDistinctness, false)
		if !ord.Satisfies(req) {
			t.Fatalf("eq-bound PK prefix must be FIXED and satisfy ORDER BY A %v", dir)
		}
	}

	// The unbound remainder stays directional: B DESC is NOT satisfied by
	// the forward scan.
	reqBDesc := properties.NewRequestedOrdering(
		requestedParts(map[string]properties.RequestedSortOrder{
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
	cand := NewValueIndexScanMatchCandidate(
		"IDX_STATUS", []string{"T"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{alias},
		values.UnknownType, false,
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
	fv, ok := idPart.GetValue().(*values.FieldValue)
	if !ok || fv.Field != "ID" {
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

// TestComputeMatchedOrderingParts_NoSuffixForFanOut: a fan-out
// (createsDuplicates) index gets NO PK suffix — positions past a
// duplicating key part are not sort-ordered (Java breaks its
// ordering-parts loop at a duplicating key expression).
func TestComputeMatchedOrderingParts_NoSuffixForFanOut(t *testing.T) {
	t.Parallel()

	alias := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"IDX_TAGS", []string{"T"},
		[]string{"TAGS"},
		[]values.CorrelationIdentifier{alias},
		values.UnknownType, false,
		[]string{"ID"})
	cand.createsDuplicates = true

	mi := NewRegularMatchInfo(
		map[values.CorrelationIdentifier]*predicates.ComparisonRange{alias: equalityRange(t, "x")},
		nil, nil, nil, nil, nil, nil, nil)

	parts := cand.ComputeMatchedOrderingParts(mi, []values.CorrelationIdentifier{alias}, false)
	if len(parts) != 1 {
		t.Fatalf("fan-out index must not gain a PK suffix ordering part: got %d parts", len(parts))
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

	pkA := &values.FieldValue{Field: "A", Typ: values.UnknownType}
	pkB := &values.FieldValue{Field: "B", Typ: values.UnknownType}
	scan := plans.NewRecordQueryScanPlan([]string{"AB"}, values.UnknownType, false).
		WithPrimaryKey([]values.Value{pkA, pkB}).
		WithScanComparisons([]*predicates.ComparisonRange{equalityRange(t, int64(1))})

	reqADesc := properties.NewRequestedOrdering(
		requestedParts(map[string]properties.RequestedSortOrder{"A": properties.RequestedSortOrderDescending}, []string{"A"}),
		properties.DistinctnessPreserveDistinctness, false)

	bare := &scanPlanExpression{plan: scan}
	if ord := computeWrapperRichOrdering(bare); ord == nil || !ord.Satisfies(reqADesc) {
		t.Fatal("scanPlanExpression over eq-bound PK scan must satisfy ORDER BY A DESC (A is FIXED)")
	}

	// Through the TypeFilter wrapper (order-preserving).
	tf := plans.NewRecordQueryTypeFilterPlan([]string{"AB"}, scan)
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
	cand := NewValueIndexScanMatchCandidate(
		"IDX_STATUS",
		[]string{"T"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
		[]string{"ID"})
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
		},
		q,
	)
	filterQ := expressions.ForEachQuantifier(expressions.InitialOf(filter))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "ID", Typ: values.UnknownType}}},
		filterQ,
	)
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
	if _, isSort := plan.(*physicalInMemorySortWrapper); isSort {
		ph, _ := plan.(physicalPlanExpression)
		t.Fatalf("ORDER BY pk over eq-prefixed index scan must elide the sort; got %s",
			ph.GetRecordQueryPlan().Explain())
	}
	if !IsPhysicalIndexScan(plan) && !IsPhysicalFetchFromPartialRecord(plan) && !IsPhysicalFilter(plan) {
		t.Fatalf("expected an index-scan-shaped plan, got %T", plan)
	}
}
