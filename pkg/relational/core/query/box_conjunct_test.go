package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestBakeGatedJoinPredicatesCheckedUsesExactLegWindows(t *testing.T) {
	t.Parallel()

	legType := &values.RecordType{Fields: []values.Field{
		{Name: "K", Ordinal: 0, FieldType: values.NotNullLong},
	}}
	mergedType := &values.RecordType{Fields: []values.Field{
		{Name: "A.K", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "B.K", Ordinal: 1, FieldType: values.NotNullLong},
	}}
	left := exactTestField(t, exactTestQOV(t, "A", legType), 0)
	right := exactTestField(t, exactTestQOV(t, "B", legType), 0)
	predicate := predicates.NewComparisonPredicate(left, predicates.Comparison{
		Type: predicates.ComparisonEquals, Operand: right,
	})
	legs := map[string]bakeLegType{
		"A": {typ: mergedType, leafOffset: 0, leafTyp: legType},
		"B": {typ: mergedType, leafOffset: 1, leafTyp: legType},
	}

	baked, drift := bakeGatedJoinPredicatesChecked([]predicates.QueryPredicate{predicate}, legs)
	if drift || len(baked) != 1 {
		t.Fatalf("exact bake = (%v, drift=%v), want one predicate without drift", baked, drift)
	}
	comparison, ok := baked[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("baked predicate = %T", baked[0])
	}
	leftField := exactTestFieldView(t, comparison.Operand)
	rightField := exactTestFieldView(t, comparison.Comparison.Operand)
	if got := leftField.Path().Ordinals(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("left baked path = %v, want [0]", got)
	}
	if got := rightField.Path().Ordinals(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("right baked path = %v, want [1]", got)
	}

	// Mutation-sensitive control: the recorded leaf window is the resolution
	// authority. Removing K must report drift and preserve the source reference,
	// never guess a slot from the merged row's display names.
	missingLeaf := &values.RecordType{Fields: []values.Field{
		{Name: "OTHER", Ordinal: 0, FieldType: values.NotNullLong},
	}}
	badLegs := map[string]bakeLegType{
		"A": {typ: mergedType, leafOffset: 0, leafTyp: legType},
		"B": {typ: mergedType, leafOffset: 1, leafTyp: missingLeaf},
	}
	if _, drift := bakeGatedJoinPredicatesChecked([]predicates.QueryPredicate{predicate}, badLegs); !drift {
		t.Fatal("missing B.K leaf window did not report bake drift")
	}
}
