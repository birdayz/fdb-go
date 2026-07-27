package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func eqComp(op values.Value) *predicates.Comparison {
	return &predicates.Comparison{Type: predicates.ComparisonEquals, Operand: op}
}

// TestSargComparisonEqual_StructuralIdentity pins RFC-188 finding 3: the SARG
// set compares by BARE Comparison identity (type + comparand + escape + param),
// the analog of Java Comparisons.Comparison.equals — NOT rendered text or an
// alias-blind hash. Distinct constants that render alike (int64(1) vs float64(1))
// and correlated composite-key operands with the same structure but different
// aliases must NOT be treated as equal.
func TestSargComparisonEqual_StructuralIdentity(t *testing.T) {
	t.Parallel()

	if !sargComparisonEqual(eqComp(values.LiteralValue(int64(5))), eqComp(values.LiteralValue(int64(5)))) {
		t.Fatal("same type + comparand must be equal")
	}
	if sargComparisonEqual(eqComp(values.LiteralValue(int64(5))), eqComp(values.LiteralValue(int64(7)))) {
		t.Fatal("different comparand must NOT be equal")
	}
	// int64(1) vs float64(1): render identically ("1") but are different
	// comparands — must not collide.
	if sargComparisonEqual(eqComp(values.LiteralValue(int64(1))), eqComp(values.LiteralValue(float64(1)))) {
		t.Fatal("int64(1) and float64(1) must NOT be equal (structural, not display text)")
	}
	// Different comparison type.
	gt5 := &predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(5))}
	if sargComparisonEqual(eqComp(values.LiteralValue(int64(5))), gt5) {
		t.Fatal("different comparison type must NOT be equal")
	}
	// Different Escape — a Comparison identity field that a type+comparand key omits.
	esc := &predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(5)), Escape: '\\'}
	if sargComparisonEqual(eqComp(values.LiteralValue(int64(5))), esc) {
		t.Fatal("different Escape must NOT be equal")
	}
	// Correlated operands over DIFFERENT quantifier aliases must NOT be equal
	// (ValuesStructurallyEqual is alias-sensitive).
	a := eqComp(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("OUTERA")))
	b := eqComp(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("OUTERB")))
	if sargComparisonEqual(a, b) {
		t.Fatal("correlated operands over different aliases must NOT be equal (alias-sensitive)")
	}

	// Unary comparisons (IS NULL) ignore the comparand — two IS NULLs are equal
	// regardless of any (semantically-absent) operand.
	null1 := &predicates.Comparison{Type: predicates.ComparisonIsNull, Operand: values.LiteralValue(int64(1))}
	null2 := &predicates.Comparison{Type: predicates.ComparisonIsNull, Operand: values.LiteralValue(int64(99))}
	if !sargComparisonEqual(null1, null2) {
		t.Fatal("unary IS NULL comparisons must be equal (comparand semantically ignored)")
	}

	// A text-search variant identity field (tokenizer) distinguishes otherwise-
	// identical comparisons.
	tokA := &predicates.Comparison{Type: predicates.ComparisonTextContainsAll, TextTokenizerName: "std"}
	tokB := &predicates.Comparison{Type: predicates.ComparisonTextContainsAll, TextTokenizerName: "ngram"}
	if sargComparisonEqual(tokA, tokB) {
		t.Fatal("different text tokenizer must NOT be equal (variant identity field)")
	}
}

// TestDistinctSargCount pins the scale criterion #7's contested band ranks on:
// the CARDINALITY of the comparison SET, so a comparison repeated across columns
// or across the nodes of one concrete plan tree counts once, exactly as it does
// in Java's Set<Comparisons.Comparison>.
func TestDistinctSargCount(t *testing.T) {
	t.Parallel()

	pk := rungEqualityRange(t, values.LiteralValue(int64(1)))
	rt := rungEqualityRange(t, values.LiteralValue(int64(99)))
	same := rungEqualityRange(t, values.LiteralValue(int64(1)))

	none := plans.NewRecordQueryIndexPlan("idx_sargcount_none", nil,
		[]string{"T"}, values.UnknownType, false)
	if got := distinctSargCount(none); got != 0 {
		t.Fatalf("no comparisons → %d, want 0", got)
	}

	two := plans.NewRecordQueryIndexPlan("idx_sargcount_two",
		[]*predicates.ComparisonRange{pk, rt}, []string{"T"}, values.UnknownType, false)
	if got := distinctSargCount(two); got != 2 {
		t.Fatalf("two distinct comparisons → %d, want 2", got)
	}

	// The same comparison on two columns is ONE set member.
	repeated := plans.NewRecordQueryIndexPlan("idx_sargcount_repeat",
		[]*predicates.ComparisonRange{pk, same}, []string{"T"}, values.UnknownType, false)
	if got := distinctSargCount(repeated); got != 1 {
		t.Fatalf("a comparison repeated across columns → %d, want 1", got)
	}

	// The count is taken over the whole concrete tree, and de-duplicates across
	// nodes: a fetch over the index contributes nothing new.
	fetched := plans.NewRecordQueryFetchFromPartialRecordPlan(
		two, nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey)
	if got := distinctSargCount(fetched); got != 2 {
		t.Fatalf("fetch over a two-comparison index → %d, want 2", got)
	}

	// A type filter over a scan does not hide the scan's comparisons.
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithScanComparisons([]*predicates.ComparisonRange{pk})
	if got := distinctSargCount(plans.NewRecordQueryTypeFilterPlan([]string{"T"}, scan)); got != 1 {
		t.Fatalf("type filter over a one-comparison scan → %d, want 1", got)
	}
}
