package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestValueIndexScanMatchCandidate_PrefixMap_AllEquality(t *testing.T) {
	t.Parallel()
	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	c := NewValueIndexScanMatchCandidate(
		"Order$status_date",
		[]string{"Order"},
		[]string{"STATUS", "DATE"},
		[]values.CorrelationIdentifier{a1, a2},
		values.UnknownType,
		false,
		nil,
	)
	eq1 := predicates.EmptyComparisonRange()
	eq1.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))})
	eq2 := predicates.EmptyComparisonRange()
	eq2.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(2))})

	bindings := map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		a1: eq1.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))}).Range,
		a2: eq2.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(2))}).Range,
	}
	prefix := c.ComputeBoundParameterPrefixMap(bindings)
	if len(prefix) != 2 {
		t.Fatalf("expected 2 prefix entries, got %d", len(prefix))
	}
}

func TestValueIndexScanMatchCandidate_PrefixMap_StopsAtEmpty(t *testing.T) {
	t.Parallel()
	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	a3 := values.UniqueCorrelationIdentifier()
	c := NewValueIndexScanMatchCandidate(
		"idx",
		[]string{"T"},
		[]string{"A", "B", "C"},
		[]values.CorrelationIdentifier{a1, a2, a3},
		values.UnknownType,
		false,
		nil,
	)
	eq1 := predicates.EmptyComparisonRange()
	res := eq1.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))})

	bindings := map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		a1: res.Range,
		// a2 is unbound — prefix should stop here
	}
	prefix := c.ComputeBoundParameterPrefixMap(bindings)
	if len(prefix) != 1 {
		t.Fatalf("expected 1 prefix entry (stop at unbound a2), got %d", len(prefix))
	}
}

func TestValueIndexScanMatchCandidate_PrefixMap_StopsAfterInequality(t *testing.T) {
	t.Parallel()
	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	a3 := values.UniqueCorrelationIdentifier()
	c := NewValueIndexScanMatchCandidate(
		"idx",
		[]string{"T"},
		[]string{"A", "B", "C"},
		[]values.CorrelationIdentifier{a1, a2, a3},
		values.UnknownType,
		false,
		nil,
	)
	eq := predicates.EmptyComparisonRange()
	eqRes := eq.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))})
	ineq := predicates.EmptyComparisonRange()
	ineqRes := ineq.Merge(&predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(5))})
	eq3 := predicates.EmptyComparisonRange()
	eq3Res := eq3.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(9))})

	bindings := map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		a1: eqRes.Range,
		a2: ineqRes.Range,
		a3: eq3Res.Range, // should NOT be in prefix (after inequality)
	}
	prefix := c.ComputeBoundParameterPrefixMap(bindings)
	if len(prefix) != 2 {
		t.Fatalf("expected 2 prefix entries (eq + ineq, stop before a3), got %d", len(prefix))
	}
	if _, ok := prefix[a3]; ok {
		t.Fatal("a3 should NOT be in prefix — it's after the inequality")
	}
}

func TestValueIndexScanMatchCandidate_ToScanPlan(t *testing.T) {
	t.Parallel()
	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	c := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS", "DATE"},
		[]values.CorrelationIdentifier{a1, a2},
		values.UnknownType,
		false,
		nil,
	)
	eq := predicates.EmptyComparisonRange()
	eqRes := eq.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue("active")})

	prefix := map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		a1: eqRes.Range,
	}
	plan := c.ToScanPlan(prefix, false)
	fetchPlan, ok := plan.(*plans.RecordQueryFetchFromPartialRecordPlan)
	if !ok {
		t.Fatalf("expected *RecordQueryFetchFromPartialRecordPlan, got %T", plan)
	}
	idxPlan, ok := fetchPlan.GetInner().(*plans.RecordQueryIndexPlan)
	if !ok {
		t.Fatalf("expected inner *RecordQueryIndexPlan, got %T", fetchPlan.GetInner())
	}
	if idxPlan.GetIndexName() != "Order$status" {
		t.Fatalf("index name=%q, want Order$status", idxPlan.GetIndexName())
	}
	comps := idxPlan.GetScanComparisons()
	if len(comps) != 2 {
		t.Fatalf("expected 2 scan comparisons (one per column), got %d", len(comps))
	}
	if !comps[0].IsEquality() {
		t.Fatal("first comparison should be equality")
	}
	if !comps[1].IsEmpty() {
		t.Fatal("second comparison should be empty (unbound)")
	}
}
