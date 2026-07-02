package predicates

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RFC-173 S3-W2 commit-1 pin — TranslateLeafPredicates: embedded comparison
// operands rebase through the TranslationMap (leaf-only, whole-tree per
// embedded Value), a baked LHS fuses over the replacement via the rebuild
// arm, and an unrelated predicate comes back pointer-identical through the
// shared spine.
func TestRFC173S3_TranslateLeafPredicates(t *testing.T) {
	t.Parallel()
	legA := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
	})
	legB := values.NewRecordType("", false, []values.Field{
		{Name: "W", FieldType: values.NotNullLong, Ordinal: 0},
	})
	merged := values.NewRecordType("", false, []values.Field{
		{Name: values.OrdinalFieldName(0), FieldType: legA, Ordinal: 0},
		{Name: values.OrdinalFieldName(1), FieldType: legB, Ordinal: 1},
	})
	qovA := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("a"), legA)
	qovB := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("b"), legB)
	upperQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("u"), merged)
	legOrdinal := map[values.CorrelationIdentifier]int{qovA.Correlation: 0, qovB.Correlation: 1}
	m := values.NewTranslationMapBuilder().
		When(qovA.Correlation).Then(func(a values.CorrelationIdentifier, _ values.Value) values.Value {
		fv, err := values.NewFieldValueOfOrdinal(upperQOV, legOrdinal[a])
		if err != nil {
			t.Fatalf("merge-map bake: %v", err)
		}
		return fv
	}).
		When(qovB.Correlation).Then(func(a values.CorrelationIdentifier, _ values.Value) values.Value {
		fv, err := values.NewFieldValueOfOrdinal(upperQOV, legOrdinal[a])
		if err != nil {
			t.Fatalf("merge-map bake: %v", err)
		}
		return fv
	}).
		Build()

	lhs, err := values.NewFieldValueOfOrdinal(qovA, 0) // a.ID baked
	if err != nil {
		t.Fatalf("bake: %v", err)
	}
	rhs := values.NewFieldValue(qovB, "W", values.NotNullLong) // b.W lazy
	pred := NewAnd(&ComparisonPredicate{
		Operand:    lhs,
		Comparison: Comparison{Type: ComparisonEquals, Operand: rhs},
	})

	out := TranslateLeafPredicates(pred, m)
	if out == pred {
		t.Fatal("predicate with map-matched aliases must be rebuilt")
	}
	cmp := out.(*AndPredicate).SubPredicates[0].(*ComparisonPredicate)
	newLHS := cmp.Operand.(*values.FieldValue)
	if newLHS.Child != upperQOV || newLHS.Resolved == nil || len(newLHS.Resolved.Accessors) != 2 {
		t.Fatalf("LHS did not rebase+fuse over the upper QOV: %+v", newLHS)
	}
	newRHS := cmp.Comparison.Operand.(*values.FieldValue)
	if newRHS.Resolved != nil {
		t.Fatal("lazy RHS must not fuse")
	}
	if inner, ok := newRHS.Child.(*values.FieldValue); !ok || inner.Child != upperQOV {
		t.Fatalf("lazy RHS child = %v, want the baked replacement over the upper QOV", newRHS.Child)
	}

	// Unrelated predicate: pointer-identical through the whole spine, and the
	// identity map short-circuits before any walk.
	other := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("zz"), values.NotNullLong)
	unrelated := NewAnd(NewValuePredicate(values.NewFieldValue(other, "X", values.NotNullLong)))
	if got := TranslateLeafPredicates(unrelated, m); got != unrelated {
		t.Fatal("predicate with no matching aliases must return the input pointer")
	}
	if got := TranslateLeafPredicates(pred, values.NewTranslationMapBuilder().Build()); got != pred {
		t.Fatal("identity map must return the input pointer")
	}
}
