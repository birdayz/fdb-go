package query

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestCheckBuriedExistentialPredicate_RefusesADanglingExistential pins the
// second condition of the existential backstop: a directly-handled
// `EXISTS(q)` whose quantifier q is NOT owned by the Select that carries it is
// refused with DanglingExistentialPredicateError. The planner has nothing to
// peel for such a marker and drops the predicate, so the rows the EXISTS
// should have excluded come back. The control is the same Select with the
// quantifier attached, which the guard admits; a NOT EXISTS marker is held
// to the same rule.
func TestCheckBuriedExistentialPredicate_RefusesADanglingExistential(t *testing.T) {
	t.Parallel()

	rowType := &values.RecordType{Fields: []values.Field{{Name: "ID", Ordinal: 0, FieldType: values.NullableLong}}}
	scan := func(name string) *expressions.Reference {
		s, err := expressions.NewFullUnorderedScanExpression([]string{name}, rowType)
		if err != nil {
			t.Fatal(err)
		}
		return expressions.InitialOf(s)
	}
	outerQ := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("A"), scan("A"))
	subAlias := values.NamedCorrelationIdentifier("q$2")
	existsPred, err := predicates.NewExistentialAlias(subAlias, rowType)
	if err != nil {
		t.Fatal(err)
	}
	result, err := outerQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}

	build := func(quantifiers []expressions.Quantifier, preds []predicates.QueryPredicate) *expressions.Reference {
		sel, err := expressions.NewSelectExpression(result, quantifiers, preds)
		if err != nil {
			t.Fatal(err)
		}
		return expressions.InitialOf(sel)
	}

	dangling := build([]expressions.Quantifier{outerQ}, []predicates.QueryPredicate{existsPred})
	var want *DanglingExistentialPredicateError
	if err := CheckBuriedExistentialPredicate(dangling); !errors.As(err, &want) || want.Alias != subAlias {
		t.Fatalf("dangling EXISTS(q$2) with no q$2 quantifier: got %v, want DanglingExistentialPredicateError for q$2", err)
	}

	danglingNot := build([]expressions.Quantifier{outerQ}, []predicates.QueryPredicate{predicates.NewNot(existsPred)})
	if err := CheckBuriedExistentialPredicate(danglingNot); !errors.As(err, &want) {
		t.Fatalf("dangling NOT EXISTS(q$2): got %v, want DanglingExistentialPredicateError", err)
	}

	existQ := expressions.NamedExistentialQuantifier(subAlias, scan("D"))
	attached := build([]expressions.Quantifier{outerQ, existQ}, []predicates.QueryPredicate{existsPred})
	if err := CheckBuriedExistentialPredicate(attached); err != nil {
		t.Fatalf("control: an EXISTS whose quantifier the Select owns must pass, got %v", err)
	}

	// A buried existential takes precedence over a dangling one, so the
	// older message keeps its meaning for the shapes it names.
	buried := build([]expressions.Quantifier{outerQ}, []predicates.QueryPredicate{
		predicates.NewOr(existsPred, predicates.NewConstantPredicate(predicates.TriTrue)),
	})
	var buriedErr *BuriedExistentialPredicateError
	if err := CheckBuriedExistentialPredicate(buried); !errors.As(err, &buriedErr) {
		t.Fatalf("EXISTS under OR: got %v, want BuriedExistentialPredicateError", err)
	}
}
