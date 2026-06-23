package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// fieldOverAlias builds a FieldValue `alias.field` (a QOV-anchored field) for a
// null-on-empty quantifier's alias — the shape a WHERE predicate over an
// outer-join leg carries.
func fieldOverAlias(q expressions.Quantifier, field string) values.Value {
	return &values.FieldValue{
		Field: field,
		Child: values.NewQuantifiedObjectValue(q.GetAlias()),
		Typ:   values.UnknownType,
	}
}

// yieldedNullOnEmpty returns whether the single yielded SelectExpression still
// carries a null-on-empty ForEach quantifier; the bool reports a yield happened.
func yieldedNullOnEmpty(yielded []expressions.RelationalExpression) (stillNullOnEmpty bool, yieldedOne bool) {
	if len(yielded) == 0 {
		return false, false
	}
	sel, ok := yielded[0].(*expressions.SelectExpression)
	if !ok {
		return false, false
	}
	for _, q := range sel.GetQuantifiers() {
		if q.Kind() == expressions.QuantifierForEach && q.IsNullOnEmpty() {
			return true, true
		}
	}
	return false, true
}

// TestEliminateNullOnEmpty_RejectingPredicateEliminates pins the core semantics:
// a null-REJECTING predicate (`alias.field = 'x'`, which is NULL when the alias
// row is the injected null tuple) makes the null-on-empty flag eliminable. The
// rule yields a SelectExpression whose quantifier is a PLAIN ForEach.
func TestEliminateNullOnEmpty_RejectingPredicateEliminates(t *testing.T) {
	t.Parallel()

	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachNullOnEmptyQuantifier(scanRef)
	// `q.name = 'x'` — when q is the injected null tuple, q.name is NULL, so
	// `NULL = 'x'` folds to UNKNOWN (rejects the row). Null-rejecting.
	pred := &predicates.ComparisonPredicate{
		Operand: fieldOverAlias(q, "name"),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "x", Typ: values.UnknownType},
		},
	}
	sel := expressions.NewSelectExpression(
		q.GetFlowedObjectValue(),
		[]expressions.Quantifier{q},
		[]predicates.QueryPredicate{pred},
	)
	selRef := expressions.InitialOf(sel)

	yielded := FireExpressionRule(NewEliminateNullOnEmptyRule(), selRef)
	stillNOE, yieldedOne := yieldedNullOnEmpty(yielded)
	if !yieldedOne {
		t.Fatalf("expected a yield for a null-rejecting predicate, got none")
	}
	if stillNOE {
		t.Fatalf("null-rejecting predicate must eliminate the null-on-empty flag")
	}
}

// TestEliminateNullOnEmpty_AcceptingPredicateKept is the RFC-144 BC1 regression:
// a null-ACCEPTING predicate (`alias.field IS NULL`, which is TRUE for the
// injected null tuple) must NOT eliminate the null-on-empty flag. The buggy
// PullUpNullOnEmptyRule would (it ignored null-acceptance); EliminateNullOnEmptyRule
// keeps it. The rule yields nothing (no eligible alias).
func TestEliminateNullOnEmpty_AcceptingPredicateKept(t *testing.T) {
	t.Parallel()

	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachNullOnEmptyQuantifier(scanRef)
	pred := &predicates.ComparisonPredicate{
		Operand:    fieldOverAlias(q, "name"),
		Comparison: predicates.Comparison{Type: predicates.ComparisonIsNull},
	}
	sel := expressions.NewSelectExpression(
		q.GetFlowedObjectValue(),
		[]expressions.Quantifier{q},
		[]predicates.QueryPredicate{pred},
	)
	selRef := expressions.InitialOf(sel)

	yielded := FireExpressionRule(NewEliminateNullOnEmptyRule(), selRef)
	if len(yielded) != 0 {
		// A yield that KEEPS the flag is acceptable; a yield that drops it is the bug.
		stillNOE, _ := yieldedNullOnEmpty(yielded)
		if !stillNOE {
			t.Fatalf("null-ACCEPTING (IS NULL) predicate must NOT eliminate the null-on-empty flag (RFC-144 BC1)")
		}
	}
}

// TestEliminateNullOnEmpty_PerAliasOnlyRejectingLeg pins the per-alias behaviour:
// with two null-on-empty quantifiers A and B and predicates `A.k = 1` (rejecting
// A) + `B.k IS NULL` (accepting B), only A's flag is eliminated; B's is kept.
func TestEliminateNullOnEmpty_PerAliasOnlyRejectingLeg(t *testing.T) {
	t.Parallel()

	scanA := &expressions.FullUnorderedScanExpression{}
	scanARef := expressions.InitialOf(scanA)
	qA := expressions.ForEachNullOnEmptyQuantifier(scanARef)

	scanB := &expressions.FullUnorderedScanExpression{}
	scanBRef := expressions.InitialOf(scanB)
	qB := expressions.ForEachNullOnEmptyQuantifier(scanBRef)

	predA := &predicates.ComparisonPredicate{
		Operand: fieldOverAlias(qA, "k"),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(1), Typ: values.UnknownType},
		},
	}
	predB := &predicates.ComparisonPredicate{
		Operand:    fieldOverAlias(qB, "k"),
		Comparison: predicates.Comparison{Type: predicates.ComparisonIsNull},
	}
	sel := expressions.NewSelectExpression(
		qA.GetFlowedObjectValue(),
		[]expressions.Quantifier{qA, qB},
		[]predicates.QueryPredicate{predA, predB},
	)
	selRef := expressions.InitialOf(sel)

	yielded := FireExpressionRule(NewEliminateNullOnEmptyRule(), selRef)
	if len(yielded) == 0 {
		t.Fatalf("expected a yield (A is null-rejecting)")
	}
	sel2, ok := yielded[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected SelectExpression yield, got %T", yielded[0])
	}
	var aNOE, bNOE bool
	for _, q := range sel2.GetQuantifiers() {
		switch q.GetAlias() {
		case qA.GetAlias():
			aNOE = q.IsNullOnEmpty()
		case qB.GetAlias():
			bNOE = q.IsNullOnEmpty()
		}
	}
	if aNOE {
		t.Errorf("A (null-rejecting) flag must be eliminated")
	}
	if !bNOE {
		t.Errorf("B (null-accepting IS NULL) flag must be KEPT (per-alias)")
	}
}

// TestEliminateNullOnEmpty_NoPredicatesNoElim: an empty WHERE (the TRUE
// predicate, null-accepting) eliminates nothing.
func TestEliminateNullOnEmpty_NoPredicatesNoElim(t *testing.T) {
	t.Parallel()

	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachNullOnEmptyQuantifier(scanRef)
	sel := expressions.NewSelectExpression(
		q.GetFlowedObjectValue(),
		[]expressions.Quantifier{q},
		nil,
	)
	selRef := expressions.InitialOf(sel)
	yielded := FireExpressionRule(NewEliminateNullOnEmptyRule(), selRef)
	if len(yielded) != 0 {
		t.Fatalf("no-predicate select must not eliminate the null-on-empty flag, got %d yields", len(yielded))
	}
}
