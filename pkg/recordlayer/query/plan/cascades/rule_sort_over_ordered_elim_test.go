package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TestSortOverOrderedElim_IndexProvidesSortOrder verifies that
// Sort(col) over an index scan that provides col ordering eliminates
// the sort.
func TestSortOverOrderedElim_IndexProvidesSortOrder(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status_date",
		[]string{"Order"},
		[]string{"STATUS", "DATE"},
		[]values.CorrelationIdentifier{a1, a2},
		values.UnknownType,
		false,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	// Sort by DATE — this should be satisfiable by the index scan
	// (index on STATUS, DATE; STATUS is equality-bound → output
	// ordered by DATE).
	filterQ := expressions.ForEachQuantifier(filterRef)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "DATE", Typ: values.UnknownType}}},
		filterQ,
	)
	sortRef := expressions.InitialOf(sort)

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, ctx)
	if _, conv := p.Explore(sortRef); !conv {
		t.Fatal("planner did not converge")
	}

	// After exploration, the top Reference should have a member that
	// is the index scan (sort eliminated) or at least the index scan
	// should appear without a sort wrapper above it.
	foundIndexScanAtTop := false
	for _, m := range sortRef.Members() {
		if IsPhysicalIndexScan(m) {
			foundIndexScanAtTop = true
			break
		}
	}
	if !foundIndexScanAtTop {
		t.Fatal("sort should be eliminated when index provides the ordering")
	}
}

// TestSortOverOrderedElim_MultiKeySortMatchesIndex verifies that
// Sort(DATE, AMOUNT) is eliminated when the index on (STATUS, DATE, AMOUNT)
// with STATUS equality-bound provides (DATE, AMOUNT) ordering.
func TestSortOverOrderedElim_MultiKeySortMatchesIndex(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	a3 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status_date_amount",
		[]string{"Order"},
		[]string{"STATUS", "DATE", "AMOUNT"},
		[]values.CorrelationIdentifier{a1, a2, a3},
		values.UnknownType,
		false,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	filterQ := expressions.ForEachQuantifier(filterRef)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "DATE", Typ: values.UnknownType}},
			{Value: &values.FieldValue{Field: "AMOUNT", Typ: values.UnknownType}},
		},
		filterQ,
	)
	sortRef := expressions.InitialOf(sort)

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, ctx)
	if _, conv := p.Explore(sortRef); !conv {
		t.Fatal("planner did not converge")
	}

	foundIndexScanAtTop := false
	for _, m := range sortRef.Members() {
		if IsPhysicalIndexScan(m) {
			foundIndexScanAtTop = true
			break
		}
	}
	if !foundIndexScanAtTop {
		t.Fatal("multi-key sort should be eliminated when index provides the full ordering")
	}
}

// TestSortOverOrderedElim_PartialSortKeyMatch verifies that Sort(DATE, AMOUNT)
// is NOT eliminated when the index only provides (DATE) ordering (prefix
// of sort keys is not sufficient — need ALL sort keys satisfied).
func TestSortOverOrderedElim_PartialSortKeyMatch(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status_date",
		[]string{"Order"},
		[]string{"STATUS", "DATE"},
		[]values.CorrelationIdentifier{a1, a2},
		values.UnknownType,
		false,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	filterQ := expressions.ForEachQuantifier(filterRef)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "DATE", Typ: values.UnknownType}},
			{Value: &values.FieldValue{Field: "AMOUNT", Typ: values.UnknownType}},
		},
		filterQ,
	)
	sortRef := expressions.InitialOf(sort)

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, ctx)
	if _, conv := p.Explore(sortRef); !conv {
		t.Fatal("planner did not converge")
	}

	// Sort should NOT be eliminated — index provides (DATE) but sort
	// requires (DATE, AMOUNT).
	for _, m := range sortRef.Members() {
		if IsPhysicalIndexScan(m) {
			t.Fatal("sort should NOT be eliminated when index provides fewer ordering keys than sort requires")
		}
	}
}

// TestSortOverOrderedElim_RangeScanProvidesSortOrder verifies that
// Sort(STATUS) over a range predicate (status > 'a') with index on (STATUS)
// eliminates the sort — the index scan produces rows in STATUS order even
// for inequality bounds.
func TestSortOverOrderedElim_RangeScanProvidesSortOrder(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, "a"),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	filterQ := expressions.ForEachQuantifier(filterRef)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "STATUS", Typ: values.UnknownType}},
		},
		filterQ,
	)
	sortRef := expressions.InitialOf(sort)

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, ctx)
	if _, conv := p.Explore(sortRef); !conv {
		t.Fatal("planner did not converge")
	}

	foundIndexScanAtTop := false
	for _, m := range sortRef.Members() {
		if IsPhysicalIndexScan(m) {
			foundIndexScanAtTop = true
			break
		}
	}
	if !foundIndexScanAtTop {
		t.Fatal("sort should be eliminated when range-bound index scan provides the ordering")
	}
}

// TestSortOverOrderedElim_SortKeyNotProvidedByIndex verifies that
// Sort(AMOUNT) is NOT eliminated when the index provides DATE ordering.
func TestSortOverOrderedElim_SortKeyNotProvidedByIndex(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status_date",
		[]string{"Order"},
		[]string{"STATUS", "DATE"},
		[]values.CorrelationIdentifier{a1, a2},
		values.UnknownType,
		false,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	// Sort by AMOUNT — index provides DATE ordering, not AMOUNT.
	filterQ := expressions.ForEachQuantifier(filterRef)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "AMOUNT", Typ: values.UnknownType}}},
		filterQ,
	)
	sortRef := expressions.InitialOf(sort)

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, ctx)
	if _, conv := p.Explore(sortRef); !conv {
		t.Fatal("planner did not converge")
	}

	// The sort should NOT be eliminated — the index doesn't provide
	// AMOUNT ordering. Index scan should NOT appear directly in the
	// top Reference.
	for _, m := range sortRef.Members() {
		if IsPhysicalIndexScan(m) {
			t.Fatal("sort should NOT be eliminated when index doesn't provide the sort key")
		}
	}
}

// TestSortOverOrderedElim_DescSortEliminated verifies that a DESC
// sort over an index scan IS eliminated — the planner produces a
// reverse index scan whose descending ordering matches the sort.
func TestSortOverOrderedElim_DescSortEliminated(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "STATUS", Typ: values.UnknownType}, Reverse: true}},
		q,
	)
	sortRef := expressions.InitialOf(sort)

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, ctx)
	if _, conv := p.Explore(sortRef); !conv {
		t.Fatal("planner did not converge")
	}

	found := false
	for _, m := range sortRef.Members() {
		if IsPhysicalIndexScan(m) {
			found = true
		}
	}
	if !found {
		t.Fatal("DESC sort should be eliminated by a reverse index scan")
	}
}
