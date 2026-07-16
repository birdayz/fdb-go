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

// TestValueIndexScanMatchCandidate_PushValueThroughFetch_ChainedFieldDeclines pins
// that PushValueThroughFetch translates ONLY a top-level bare field over the source
// quantifier by covered-column name. A chained accessor (ADDR.CITY) whose LEAF name
// collides with a covered top-level column (CITY) must NOT translate to a flat read
// of the index entry's CITY column — that is a silent wrong-rows/wrong-value bug.
//
// Java ref: ScanWithFetchMatchCandidate.pushValueThroughFetch matches the WHOLE value
// tree (accessor chain included) against a provided index Value via semanticEquals and
// accepts only when no source correlation remains; a chained ADDR.CITY does not
// semantically equal the flat top-level CITY, so it is rejected.
//
// Revert-proof: before the fix the FieldValue arm matched by leaf name only and
// rebuilt a flat FieldValue{CITY, QOV(TGT)}, silently dropping the ADDR accessor —
// ok==true with the wrong value. After the fix the chained shape returns ok==false,
// while the legitimate bare shape still translates.
func TestValueIndexScanMatchCandidate_PushValueThroughFetch_ChainedFieldDeclines(t *testing.T) {
	t.Parallel()

	src := values.NamedCorrelationIdentifier("SRC")
	tgt := values.NamedCorrelationIdentifier("TGT")
	sarg := values.UniqueCorrelationIdentifier()
	c := NewValueIndexScanMatchCandidate(
		"idx_city",
		[]string{"T"},
		[]string{"CITY"},
		[]values.CorrelationIdentifier{sarg},
		values.UnknownType,
		false,
		nil,
	)

	// Chained ADDR.CITY: FieldValue{CITY, Child: FieldValue{ADDR, Child: QOV(SRC)}}.
	// Its leaf name (CITY) collides with the covered top-level column but the value
	// is a different indexed Value — must decline.
	inner := values.NewFieldValue(values.NewQuantifiedObjectValue(src), "ADDR", values.UnknownType)
	chained := values.NewFieldValue(inner, "CITY", values.UnknownType)
	got, ok := c.PushValueThroughFetch(chained, src, tgt)
	if ok {
		t.Fatalf("chained ADDR.CITY must NOT push through fetch (leaf-name collision), got ok=true value=%v", got)
	}

	// Bare top-level CITY over the source: FieldValue{CITY, Child: QOV(SRC)} — the
	// legitimate case that must STILL translate (guard against over-restriction).
	bare := values.NewFieldValue(values.NewQuantifiedObjectValue(src), "CITY", values.UnknownType)
	gotBare, okBare := c.PushValueThroughFetch(bare, src, tgt)
	if !okBare {
		t.Fatal("bare top-level CITY over the source must push through fetch")
	}
	fv, isFV := gotBare.(*values.FieldValue)
	if !isFV || fv.Field != "CITY" {
		t.Fatalf("translated value=%v, want FieldValue{CITY, ...}", gotBare)
	}
	childQOV, isQOV := fv.Child.(*values.QuantifiedObjectValue)
	if !isQOV || childQOV.Correlation != tgt {
		t.Fatalf("translated child=%v, want QOV(TGT)", fv.Child)
	}
}
