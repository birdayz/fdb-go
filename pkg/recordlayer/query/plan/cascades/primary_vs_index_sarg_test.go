package cascades

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustSargConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct SARG fixture: " + err.Error())
	}
	return value
}

func sargRowType() values.Type {
	return values.NewRecordType("PrimaryVsIndexSargRow", false, []values.Field{
		{Name: "K0", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "K1", FieldType: values.NullableLong, Ordinal: 1},
	})
}

func sargLiteral(lit any) values.Value {
	var typ values.Type
	switch lit.(type) {
	case int64:
		typ = values.NotNullLong
	case float64:
		typ = values.NotNullDouble
	default:
		panic(fmt.Sprintf("unsupported SARG literal type %T", lit))
	}
	return &values.ConstantValue{Value: lit, Typ: typ}
}

func sargIndex(name string, ranges []*predicates.ComparisonRange) *plans.RecordQueryIndexPlan {
	return mustSargConstruct(plans.NewRecordQueryIndexPlan(
		name, ranges, []string{"T"}, sargRowType(), false))
}

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

	if !sargComparisonEqual(eqComp(sargLiteral(int64(5))), eqComp(sargLiteral(int64(5)))) {
		t.Fatal("same type + comparand must be equal")
	}
	if sargComparisonEqual(eqComp(sargLiteral(int64(5))), eqComp(sargLiteral(int64(7)))) {
		t.Fatal("different comparand must NOT be equal")
	}
	// int64(1) vs float64(1): render identically ("1") but are different
	// comparands — must not collide.
	if sargComparisonEqual(eqComp(sargLiteral(int64(1))), eqComp(sargLiteral(float64(1)))) {
		t.Fatal("int64(1) and float64(1) must NOT be equal (structural, not display text)")
	}
	// Different comparison type.
	gt5 := &predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: sargLiteral(int64(5))}
	if sargComparisonEqual(eqComp(sargLiteral(int64(5))), gt5) {
		t.Fatal("different comparison type must NOT be equal")
	}
	// Different Escape — a Comparison identity field that a type+comparand key omits.
	esc := &predicates.Comparison{Type: predicates.ComparisonEquals, Operand: sargLiteral(int64(5)), Escape: '\\'}
	if sargComparisonEqual(eqComp(sargLiteral(int64(5))), esc) {
		t.Fatal("different Escape must NOT be equal")
	}
	// Correlated operands over DIFFERENT quantifier aliases must NOT be equal
	// (ValuesStructurallyEqual is alias-sensitive).
	a := eqComp(mustSargConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("OUTERA"), values.NotNullLong)))
	b := eqComp(mustSargConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("OUTERB"), values.NotNullLong)))
	if sargComparisonEqual(a, b) {
		t.Fatal("correlated operands over different aliases must NOT be equal (alias-sensitive)")
	}

	// Unary comparisons (IS NULL) ignore the comparand — two IS NULLs are equal
	// regardless of any (semantically-absent) operand.
	null1 := &predicates.Comparison{Type: predicates.ComparisonIsNull, Operand: sargLiteral(int64(1))}
	null2 := &predicates.Comparison{Type: predicates.ComparisonIsNull, Operand: sargLiteral(int64(99))}
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

	pk := rungEqualityRange(t, sargLiteral(int64(1)))
	rt := rungEqualityRange(t, sargLiteral(int64(99)))
	same := rungEqualityRange(t, sargLiteral(int64(1)))

	none := sargIndex("idx_sargcount_none", nil)
	if got := distinctSargCount(none); got != 0 {
		t.Fatalf("no comparisons → %d, want 0", got)
	}

	two := sargIndex("idx_sargcount_two", []*predicates.ComparisonRange{pk, rt})
	if got := distinctSargCount(two); got != 2 {
		t.Fatalf("two distinct comparisons → %d, want 2", got)
	}

	// The same comparison on two columns is ONE set member.
	repeated := sargIndex("idx_sargcount_repeat", []*predicates.ComparisonRange{pk, same})
	if got := distinctSargCount(repeated); got != 1 {
		t.Fatalf("a comparison repeated across columns → %d, want 1", got)
	}

	// The count is taken over the whole concrete tree, and de-duplicates across
	// nodes: a fetch over the index contributes nothing new.
	fetched := mustSargConstruct(plans.NewRecordQueryFetchFromPartialRecordPlan(
		two, nil, sargRowType(), plans.FetchIndexRecordsPrimaryKey))
	if got := distinctSargCount(fetched); got != 2 {
		t.Fatalf("fetch over a two-comparison index → %d, want 2", got)
	}

	// A type filter over a scan does not hide the scan's comparisons.
	scan := mustSargConstruct(plans.NewRecordQueryScanPlan(
		[]string{"T"}, sargRowType(), false)).
		WithScanComparisons([]*predicates.ComparisonRange{pk})
	typeFilter := mustSargConstruct(plans.NewRecordQueryTypeFilterPlan([]string{"T"}, scan))
	if got := distinctSargCount(typeFilter); got != 1 {
		t.Fatalf("type filter over a one-comparison scan → %d, want 1", got)
	}
}
