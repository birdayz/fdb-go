package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func fireConstraintRule(t testing.TB, rule ImplementationRule, ref *expressions.Reference, cm *ConstraintMap) {
	t.Helper()
	for _, member := range ref.AllMembers() {
		bindings := rule.Matcher().BindMatches(matching.NewBindings(), member)
		for _, b := range bindings {
			call := &ImplementationRuleCall{
				Bindings:       b,
				Reference:      ref,
				Context:        EmptyPlanContext(),
				Constraints:    cm,
				constraintOnly: true,
			}
			rule.OnMatch(call)
			if err := call.Err(); err != nil {
				t.Fatalf("%T.OnMatch: %v", rule, err)
			}
			call.applyPendingConstraints()
		}
	}
}

func referencedFieldsRowType() *values.RecordType {
	return values.NewRecordType("ReferencedFieldsRow", false, []values.Field{
		{Name: "X", FieldType: values.NullableLong},
		{Name: "A", FieldType: values.NullableLong},
		{Name: "B", FieldType: values.NullableLong},
		{Name: "COL", FieldType: values.NullableLong},
		{Name: "WHERE_COL", FieldType: values.NullableLong},
		{Name: "SELECT_COL", FieldType: values.NullableLong},
		{Name: "PK", FieldType: values.NotNullLong},
		{Name: "COL1", FieldType: values.NullableLong},
	})
}

func mustReferencedFieldsConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct referenced-fields fixture: " + err.Error())
	}
	return value
}

func referencedFieldsScanQ() (*expressions.Reference, expressions.Quantifier) {
	scan := mustReferencedFieldsConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, referencedFieldsRowType()))
	ref := expressions.InitialOf(scan)
	return ref, expressions.ForEachQuantifier(ref)
}

func referencedField(q expressions.Quantifier, ordinal int) values.Value {
	root := mustReferencedFieldsConstruct(q.RequireFlowedObjectValue())
	return mustReferencedFieldsConstruct(values.ResolveFieldOrdinals(root, []int{ordinal}))
}

func TestPushReferencedFieldsThroughFilter(t *testing.T) {
	t.Parallel()

	scanRef, scanQ := referencedFieldsScanQ()

	pred := &predicates.ComparisonPredicate{
		Operand: referencedField(scanQ, 0),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(5), Typ: values.NotNullLong},
		},
	}
	filter := mustReferencedFieldsConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred}, scanQ,
	))
	filterRef := expressions.InitialOf(filter)

	cm := NewConstraintMap()
	fireConstraintRule(t, NewPushReferencedFieldsThroughFilterRule(), filterRef, cm)

	rf, ok := Get(cm, scanRef, ReferencedFieldsConstraintKey)
	if !ok {
		t.Fatal("expected ReferencedFields constraint on child ref")
	}
	if !rf.Contains("X") {
		t.Fatal("expected field X in referenced fields")
	}
}

func TestPushReferencedFieldsThroughFilter_Multiple(t *testing.T) {
	t.Parallel()

	scanRef, scanQ := referencedFieldsScanQ()

	p1 := &predicates.ComparisonPredicate{
		Operand: referencedField(scanQ, 1),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: referencedField(scanQ, 2),
		},
	}
	filter := mustReferencedFieldsConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{p1}, scanQ,
	))
	filterRef := expressions.InitialOf(filter)

	cm := NewConstraintMap()
	fireConstraintRule(t, NewPushReferencedFieldsThroughFilterRule(), filterRef, cm)

	rf, ok := Get(cm, scanRef, ReferencedFieldsConstraintKey)
	if !ok {
		t.Fatal("expected constraint")
	}
	if !rf.Contains("A") || !rf.Contains("B") {
		t.Fatalf("expected fields A and B, got %v", rf.Fields())
	}
}

func TestPushReferencedFieldsThroughDistinct(t *testing.T) {
	t.Parallel()

	scanRef, scanQ := referencedFieldsScanQ()

	distinct := mustReferencedFieldsConstruct(expressions.NewLogicalDistinctExpression(scanQ))
	distinctRef := expressions.InitialOf(distinct)

	incoming := NewReferencedFields(map[string]struct{}{"COL": {}})
	cm := NewConstraintMap()
	Set(cm, distinctRef, ReferencedFieldsConstraintKey, incoming)

	fireConstraintRule(t, NewPushReferencedFieldsThroughDistinctRule(), distinctRef, cm)

	rf, ok := Get(cm, scanRef, ReferencedFieldsConstraintKey)
	if !ok {
		t.Fatal("expected constraint pushed through distinct")
	}
	if !rf.Contains("COL") {
		t.Fatal("expected COL in referenced fields")
	}
}

func TestPushReferencedFieldsThroughSelect(t *testing.T) {
	t.Parallel()

	scanRef, scanQ := referencedFieldsScanQ()

	pred := &predicates.ComparisonPredicate{
		Operand: referencedField(scanQ, 4),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong},
		},
	}
	resultVal := referencedField(scanQ, 5)
	sel := mustReferencedFieldsConstruct(expressions.NewSelectExpression(
		resultVal,
		[]expressions.Quantifier{scanQ},
		[]predicates.QueryPredicate{pred},
	))
	selRef := expressions.InitialOf(sel)

	cm := NewConstraintMap()
	fireConstraintRule(t, NewPushReferencedFieldsThroughSelectRule(), selRef, cm)

	rf, ok := Get(cm, scanRef, ReferencedFieldsConstraintKey)
	if !ok {
		t.Fatal("expected constraint pushed through select")
	}
	if !rf.Contains("WHERE_COL") {
		t.Fatal("expected WHERE_COL from predicate")
	}
	if !rf.Contains("SELECT_COL") {
		t.Fatal("expected SELECT_COL from result value")
	}
}

func TestPushReferencedFieldsThroughUnique(t *testing.T) {
	t.Parallel()

	scanRef, scanQ := referencedFieldsScanQ()

	unique := mustReferencedFieldsConstruct(expressions.NewLogicalUniqueExpression(scanQ))
	uniqueRef := expressions.InitialOf(unique)

	incoming := NewReferencedFields(map[string]struct{}{"PK": {}})
	cm := NewConstraintMap()
	Set(cm, uniqueRef, ReferencedFieldsConstraintKey, incoming)

	fireConstraintRule(t, NewPushReferencedFieldsThroughUniqueRule(), uniqueRef, cm)

	rf, ok := Get(cm, scanRef, ReferencedFieldsConstraintKey)
	if !ok {
		t.Fatal("expected constraint pushed through unique")
	}
	if !rf.Contains("PK") {
		t.Fatal("expected PK in referenced fields")
	}
}

func TestReferencedFields_Union(t *testing.T) {
	t.Parallel()

	r1 := NewReferencedFields(map[string]struct{}{"A": {}, "B": {}})
	r2 := NewReferencedFields(map[string]struct{}{"B": {}, "C": {}})
	merged := r1.Union(r2)
	if merged.Size() != 3 {
		t.Fatalf("expected 3 fields, got %d", merged.Size())
	}
	for _, f := range []string{"A", "B", "C"} {
		if !merged.Contains(f) {
			t.Fatalf("missing field %s", f)
		}
	}
}

func TestReferencedFields_Empty(t *testing.T) {
	t.Parallel()

	r := EmptyReferencedFields()
	if !r.IsEmpty() {
		t.Fatal("should be empty")
	}
	if r.Size() != 0 {
		t.Fatalf("expected 0, got %d", r.Size())
	}
	if r.Contains("X") {
		t.Fatal("empty should not contain anything")
	}
}

func TestReferencedFields_NilSafe(t *testing.T) {
	t.Parallel()

	var r *ReferencedFields
	if !r.IsEmpty() {
		t.Fatal("nil should be empty")
	}
	if r.Size() != 0 {
		t.Fatal("nil size should be 0")
	}
	if r.Contains("X") {
		t.Fatal("nil should not contain anything")
	}

	other := NewReferencedFields(map[string]struct{}{"X": {}})
	merged := r.Union(other)
	if !merged.Contains("X") {
		t.Fatal("union with nil should return other")
	}
}

func TestFieldValuesFromValue(t *testing.T) {
	t.Parallel()

	_, q := referencedFieldsScanQ()
	v := referencedField(q, 7)
	rf := FieldValuesFromValue(v)
	if !rf.Contains("COL1") {
		t.Fatal("expected COL1")
	}
	if rf.Size() != 1 {
		t.Fatalf("expected 1 field, got %d", rf.Size())
	}
}
