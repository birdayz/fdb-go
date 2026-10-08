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

func TestReferencedFieldsFromRangeOperands(t *testing.T) {
	t.Parallel()
	_, q := referencedFieldsScanQ()
	p := predicates.NewPredicateWithValueAndRanges(referencedField(q, 0), []*predicates.RangeConstraints{
		predicates.NewRangeConstraints(nil, []predicates.Comparison{
			{Type: predicates.ComparisonGreaterThan, Operand: referencedField(q, 1)},
			{Type: predicates.ComparisonLessThan, Operand: referencedField(q, 2)},
		}),
	})
	out := map[string]struct{}{}
	collectPredicateFieldValues(p, out)
	for _, name := range []string{"X", "A", "B"} {
		if _, ok := out[name]; !ok {
			t.Errorf("range lost referenced field %s: %v", name, out)
		}
	}
	if len(out) != 3 {
		t.Fatalf("referenced fields = %v, want exactly X, A, B", out)
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

func TestReferencedFieldPassThroughPreservesConstraintPresence(t *testing.T) {
	t.Parallel()
	for _, operator := range []string{"unique", "distinct"} {
		for _, state := range []string{"absent", "empty", "populated"} {
			t.Run(operator+"/"+state, func(t *testing.T) {
				t.Parallel()
				child, q := referencedFieldsScanQ()
				var parent expressions.RelationalExpression
				var rule ImplementationRule
				if operator == "unique" {
					parent = mustReferencedFieldsConstruct(expressions.NewLogicalUniqueExpression(q))
					rule = NewPushReferencedFieldsThroughUniqueRule()
				} else {
					parent = mustReferencedFieldsConstruct(expressions.NewLogicalDistinctExpression(q))
					rule = NewPushReferencedFieldsThroughDistinctRule()
				}
				ref := expressions.InitialOf(parent)
				cm := NewConstraintMap()
				incoming := EmptyReferencedFields()
				if state == "populated" {
					incoming = NewReferencedFields(map[string]struct{}{"PK": {}})
				}
				if state != "absent" {
					Set(cm, ref, ReferencedFieldsConstraintKey, incoming)
				}
				child.ConstraintsMap().SetExplored()
				tick := child.ConstraintsMap().CurrentTick()
				fireConstraintRule(t, rule, ref, cm)
				got, present := Get(cm, child, ReferencedFieldsConstraintKey)
				if state == "absent" {
					if present || child.ConstraintsMap().CurrentTick() != tick || child.NeedsExploration() {
						t.Fatalf("absent constraint was manufactured: present=%v tick=%d->%d needsExploration=%v",
							present, tick, child.ConstraintsMap().CurrentTick(), child.NeedsExploration())
					}
					return
				}
				if !present || got != incoming || !child.NeedsExploration() {
					t.Fatalf("present %s constraint was not propagated: present=%v fields=%v needsExploration=%v",
						state, present, got, child.NeedsExploration())
				}
				propagatedTick := child.ConstraintsMap().CurrentTick()
				child.ConstraintsMap().SetExplored()
				fireConstraintRule(t, rule, ref, cm)
				if child.ConstraintsMap().CurrentTick() != propagatedTick || child.NeedsExploration() {
					t.Fatal("identical constraint push rearmed the child")
				}
			})
		}
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
