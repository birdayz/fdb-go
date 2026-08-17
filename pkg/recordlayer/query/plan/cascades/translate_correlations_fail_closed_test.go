package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// translateFailClosedRow is a two-column row so the field below reads a
// NON-ZERO ordinal. That matters: an ordinal-0 read can survive some rebuilds
// by accident, and this test is about the rebuild failing, not about which
// slot it names.
func translateFailClosedRow() *values.RecordType {
	return values.NewRecordType("E", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong},
		{Name: "FNAME", FieldType: values.NullableString},
	})
}

// TestTranslateCorrelationsReportsAFailedRebuildInsteadOfMintingNil pins the
// contract that makes the correlation translators safe to call: when the
// substitution cannot be rebuilt around the replaced leaf, they say so, and
// the value they return is never assembled into a predicate.
//
// The failure mode this replaces was silent and travelled far. The leaf walk
// signals a failed rebuild by returning NIL, and the translators used to hand
// that nil straight into `&ComparisonPredicate{Operand: newOperand}`. The
// result is a predicate that cannot exist — `<nil> = 'alice'` — which then
// survives every later rewrite (each predicate spine copies the nil forward)
// and first surfaces during physical construction as
//
//	resolution error 55 at logical-source-name.value: logical source Value is nil
//
// reported as an unclassified planner failure, arbitrarily far from the rule
// that minted it. Reached in practice by PredicatePushDownRule pushing a
// residual into a Select whose result value is not a quantified object, on
// `SELECT d.dname FROM dept d LEFT JOIN emp e ON e.dept_id = d.id
// WHERE e.fname = 'alice' AND NOT EXISTS (...)`.
func TestTranslateCorrelationsReportsAFailedRebuildInsteadOfMintingNil(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("E")
	root, err := values.NewQuantifiedObjectValue(alias, translateFailClosedRow())
	if err != nil {
		t.Fatalf("building the source root: %v", err)
	}
	field, err := values.ResolveFieldOrdinals(root, []int{1})
	if err != nil {
		t.Fatalf("resolving E.FNAME: %v", err)
	}

	// The substitution a push performs: replace the whole source root with the
	// program the child publishes. A scalar cannot carry the field's ordinal,
	// so the rebuild fails — exactly as it does when a child Select's result
	// value is not a quantified object.
	tm := NewTranslationMapBuilder().
		When(alias).
		Then(func(values.CorrelationIdentifier, values.LeafValue) values.Value {
			return &values.ConstantValue{Value: int64(7)}
		}).
		Build()

	// VACUITY GUARD. If the substitution ever became rebuildable, both
	// assertions below would hold with the defect fully present, because
	// "translated fine" and "declined" are indistinguishable to a test that
	// only checks for a non-nil result.
	if rebuilt, ok := translateValueCorrelations(field, tm); ok {
		t.Fatalf("the substitution rebuilt into %v; this test needs a substitution that "+
			"CANNOT be rebuilt, or it asserts nothing about the failure path", rebuilt)
	}

	pred := predicates.NewComparisonPredicate(field, predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: &values.ConstantValue{Value: "alice"},
	})
	translated, ok := translatePredicateCorrelations(pred, tm)
	if ok {
		t.Fatalf("a predicate whose operand could not be translated reported success: %v", translated)
	}
	if translated != nil {
		t.Errorf("a declined translation returned a predicate (%v); callers compare against "+
			"the ORIGINAL to detect change, so any non-nil here can be published", translated)
	}
}

// TestTranslateCorrelationsDeclinesThroughEveryPredicateShape drives the
// decline through each connective, because the spine rebuilds them
// independently: an AND that keeps translating its remaining conjuncts after
// one fails publishes a predicate with a nil operand just as surely as the
// leaf arm did, and the leaf-only test above would still pass.
func TestTranslateCorrelationsDeclinesThroughEveryPredicateShape(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("E")
	root, err := values.NewQuantifiedObjectValue(alias, translateFailClosedRow())
	if err != nil {
		t.Fatalf("building the source root: %v", err)
	}
	field, err := values.ResolveFieldOrdinals(root, []int{1})
	if err != nil {
		t.Fatalf("resolving E.FNAME: %v", err)
	}
	tm := NewTranslationMapBuilder().
		When(alias).
		Then(func(values.CorrelationIdentifier, values.LeafValue) values.Value {
			return &values.ConstantValue{Value: int64(7)}
		}).
		Build()

	leaf := predicates.NewComparisonPredicate(field, predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: &values.ConstantValue{Value: "alice"},
	})
	alwaysTrue := predicates.NewConstantPredicate(predicates.TriTrue)

	for _, tc := range []struct {
		name string
		pred predicates.QueryPredicate
	}{
		{"comparison", leaf},
		{"value", predicates.NewValuePredicate(field)},
		// The untranslatable conjunct is SECOND in each connective: a spine
		// that stops at the first failure passes either way, while one that
		// only checks the first sub-predicate passes only with it first.
		{"and", predicates.NewAnd(alwaysTrue, leaf)},
		{"or", predicates.NewOr(alwaysTrue, leaf)},
		{"not", predicates.NewNot(leaf)},
		{"nested not-under-and", predicates.NewAnd(alwaysTrue, predicates.NewNot(leaf))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := translatePredicateCorrelations(tc.pred, tm)
			if ok {
				t.Fatalf("reported success: %v", got)
			}
			if got != nil {
				t.Errorf("returned %v on a declined translation, want nil", got)
			}
		})
	}
}

// TestTranslateCorrelationsDeclinesAShapeChangingSubstitution pins the type
// half of the same contract, which is the half that produced WRONG ROWS rather
// than a loud failure.
//
// A correlation substitution replaces a value denoting a ROW with another value
// denoting THE SAME row. Nothing in the leaf walk enforced that, and the
// ordinals in the surrounding expression cross it untouched — so substituting a
// leg's row by the join BOX it sits inside leaves every `#0` addressing a
// different column. `SELECT d.dname FROM dept d LEFT JOIN emp e ON e.dept_id =
// d.id WHERE e.id IS NULL AND NOT EXISTS (…)` turned `E.ID#0 IS NULL` into the
// box's ordinal 0 — `D.ID#0` — which the access path then matched as a scan
// range on DEPT's primary key. The query returned no rows, the plan showed no
// trace of the conjunct, and nothing failed.
func TestTranslateCorrelationsDeclinesAShapeChangingSubstitution(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("E")
	leg, err := values.NewQuantifiedObjectValue(alias, translateFailClosedRow())
	if err != nil {
		t.Fatalf("building the leg root: %v", err)
	}
	field, err := values.ResolveFieldOrdinals(leg, []int{0})
	if err != nil {
		t.Fatalf("resolving E.ID: %v", err)
	}

	// The box: the leg's row preceded by another leg's columns, so ordinal 0
	// means something different in it. This is the shape whose substitution
	// must not happen.
	box, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("BOX"),
		values.NewRecordType("BOX", false, []values.Field{
			{Name: "D_ID", FieldType: values.NullableLong},
			{Name: "DNAME", FieldType: values.NullableString},
			{Name: "ID", FieldType: values.NullableLong},
			{Name: "FNAME", FieldType: values.NullableString},
		}),
	)
	if err != nil {
		t.Fatalf("building the box root: %v", err)
	}
	tm := NewTranslationMapBuilder().
		When(alias).
		Then(func(values.CorrelationIdentifier, values.LeafValue) values.Value { return box }).
		Build()

	if rebuilt, ok := translateValueCorrelations(field, tm); ok {
		t.Fatalf("substituting a 2-column leg row by a 4-column box succeeded, producing %s — "+
			"the ordinal now addresses a different column", values.ExplainValue(rebuilt))
	}

	// The SAME substitution with an agreeing shape still goes through, so the
	// guard is about the shape and not about substitution in general.
	twin, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("TWIN"), translateFailClosedRow())
	if err != nil {
		t.Fatalf("building the twin root: %v", err)
	}
	agreeing := NewTranslationMapBuilder().
		When(alias).
		Then(func(values.CorrelationIdentifier, values.LeafValue) values.Value { return twin }).
		Build()
	rebuilt, ok := translateValueCorrelations(field, agreeing)
	if !ok {
		t.Fatal("a same-shaped substitution was declined")
	}
	if _, stillLeg := values.GetCorrelatedToOfValue(rebuilt)[alias]; stillLeg {
		t.Errorf("the same-shaped substitution left the original alias in place: %s",
			values.ExplainValue(rebuilt))
	}
}

// TestTranslateCorrelationsStillTranslatesWhatItCan is the other side of the
// guard: fail-closed must not become fail-always. A substitution that CAN be
// rebuilt still is, and an untouched alias still passes through by pointer.
func TestTranslateCorrelationsStillTranslatesWhatItCan(t *testing.T) {
	t.Parallel()

	source := values.NamedCorrelationIdentifier("E")
	target := values.NamedCorrelationIdentifier("Q")
	sourceRoot, err := values.NewQuantifiedObjectValue(source, translateFailClosedRow())
	if err != nil {
		t.Fatalf("building the source root: %v", err)
	}
	targetRoot, err := values.NewQuantifiedObjectValue(target, translateFailClosedRow())
	if err != nil {
		t.Fatalf("building the target root: %v", err)
	}
	field, err := values.ResolveFieldOrdinals(sourceRoot, []int{1})
	if err != nil {
		t.Fatalf("resolving E.FNAME: %v", err)
	}
	tm := NewTranslationMapBuilder().
		When(source).
		Then(func(values.CorrelationIdentifier, values.LeafValue) values.Value {
			return targetRoot
		}).
		Build()

	pred := predicates.NewComparisonPredicate(field, predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: &values.ConstantValue{Value: "alice"},
	})
	translated, ok := translatePredicateCorrelations(pred, tm)
	if !ok {
		t.Fatal("a rebuildable substitution was declined")
	}
	cmp, isCmp := translated.(*predicates.ComparisonPredicate)
	if !isCmp {
		t.Fatalf("translated to %T, want *ComparisonPredicate", translated)
	}
	if cmp.Operand == nil {
		t.Fatal("translated operand is nil")
	}
	correlated := values.GetCorrelatedToOfValue(cmp.Operand)
	if _, stillSource := correlated[source]; stillSource {
		t.Errorf("operand still references the source alias %q after translation", source.Name())
	}
	if _, onTarget := correlated[target]; !onTarget {
		t.Errorf("operand does not reference the target alias %q; correlations = %v",
			target.Name(), correlated)
	}

	// An alias the map says nothing about is returned by POINTER, which is the
	// contract the callers' change detection rests on.
	untouched := predicates.NewComparisonPredicate(
		mustResolveOrdinal(t, targetRoot, 1),
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: "bob"}},
	)
	same, sameOK := translatePredicateCorrelations(untouched, tm)
	if !sameOK {
		t.Fatal("a predicate the map does not mention was declined")
	}
	if same != untouched {
		t.Error("a predicate with no mapped alias was rebuilt instead of passed through")
	}
}

func mustResolveOrdinal(t *testing.T, root values.QuantifiedObjectValue, ordinal int) values.Value {
	t.Helper()
	resolved, err := values.ResolveFieldOrdinals(root, []int{ordinal})
	if err != nil {
		t.Fatalf("resolving ordinal %d: %v", ordinal, err)
	}
	return resolved
}
