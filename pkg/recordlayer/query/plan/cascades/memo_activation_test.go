package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// aliasVariantFilter builds Filter(QOV(q)=1, →scanRef) with a FRESH quantifier
// alias each call — two such filters are equivalent but differ in the alias
// their predicate references.
func aliasVariantFilter(scanRef *expressions.Reference) expressions.RelationalExpression {
	q := expressions.ForEachQuantifier(scanRef)
	pred := predicates.NewComparisonPredicate(values.NewQuantifiedObjectValue(q.GetAlias()),
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1)}})
	return expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, q)
}

// TestMemoActivation_InternsAliasVariants proves the PR-A activation: the memo
// now interns two alias-variant expressions into the SAME Reference via
// MemoizeExpression — which it could NOT before (alias-sensitive interning).
func TestMemoActivation_InternsAliasVariants(t *testing.T) {
	t.Parallel()
	scanRef := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
	m := NewMemo(nil)
	m.RegisterReference(scanRef)

	refA := m.MemoizeExpression(aliasVariantFilter(scanRef))
	refB := m.MemoizeExpression(aliasVariantFilter(scanRef))

	if refA.Canonical() != refB.Canonical() {
		t.Fatal("ACTIVATION FAILED: alias-variant filters should intern to the SAME Reference now")
	}
}

// TestMemoActivation_BroadInterningCollapsesK proves the activation fires
// BROADLY (not ~once): K equivalent-but-differently-aliased sub-expressions,
// memoized the way rules build them (MemoizeExpression), all collapse to ONE
// Reference. Pre-activation each fresh alias produced a DISTINCT Reference.
func TestMemoActivation_BroadInterningCollapsesK(t *testing.T) {
	t.Parallel()
	const k = 6
	scanRef := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
	m := NewMemo(nil)
	m.RegisterReference(scanRef)

	canon := map[*expressions.Reference]struct{}{}
	for i := 0; i < k; i++ {
		ref := m.MemoizeExpression(aliasVariantFilter(scanRef)) // fresh alias each time
		canon[ref.Canonical()] = struct{}{}
	}
	if len(canon) != 1 {
		t.Fatalf("ACTIVATION INEFFECTIVE: %d alias-variant equivalents interned to %d References, want 1", k, len(canon))
	}
	t.Logf("activation: %d alias-variant equivalents → 1 shared Reference", k)
}
