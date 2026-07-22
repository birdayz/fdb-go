package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Each operator's GetCorrelatedToWithoutChildren walks the
// node-information's Value / Predicate trees, collecting every
// QuantifiedObjectValue's CorrelationIdentifier. These tests pin that
// the wiring works — a Quantifier alias buried inside a predicate /
// projection / sort key surfaces in the correlation set.

func TestLogicalFilter_GetCorrelatedToWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	// Comparison predicate referencing q's flowed object.
	pred := predicates.NewComparisonPredicate(
		q.GetFlowedObjectValue(),
		predicates.Comparison{Type: predicates.ComparisonIsNull},
	)
	f := NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, q)
	got := f.GetCorrelatedToWithoutChildren()
	if _, ok := got[q.GetAlias()]; !ok {
		t.Fatalf("filter correlation set %v doesn't contain q's alias %v", got, q.GetAlias())
	}
}

func TestLogicalFilter_GetCorrelatedToWithoutChildren_NoCorrelation(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	// Pure constant predicate — no correlations.
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	f := NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, q)
	got := f.GetCorrelatedToWithoutChildren()
	if len(got) != 0 {
		t.Fatalf("filter over constant predicate has correlations: %v", got)
	}
}

func TestLogicalProjection_GetCorrelatedToWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	p := NewLogicalProjectionExpression([]values.Value{q.GetFlowedObjectValue()}, q)
	got := p.GetCorrelatedToWithoutChildren()
	if _, ok := got[q.GetAlias()]; !ok {
		t.Fatalf("projection correlation set %v doesn't contain q's alias", got)
	}
}

func TestLogicalSort_GetCorrelatedToWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	s := NewLogicalSortExpression([]SortKey{{Value: q.GetFlowedObjectValue(), Reverse: false}}, q)
	got := s.GetCorrelatedToWithoutChildren()
	if _, ok := got[q.GetAlias()]; !ok {
		t.Fatalf("sort correlation set %v doesn't contain q's alias", got)
	}
}

func TestSelect_GetCorrelatedToWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	pred := predicates.NewComparisonPredicate(
		q.GetFlowedObjectValue(),
		predicates.Comparison{Type: predicates.ComparisonIsNull},
	)
	rv := q.GetFlowedObjectValue()
	s := NewSelectExpression(rv, []Quantifier{q}, []predicates.QueryPredicate{pred})
	got := s.GetCorrelatedToWithoutChildren()
	if _, ok := got[q.GetAlias()]; !ok {
		t.Fatalf("select correlation set %v doesn't contain q's alias", got)
	}
}

func TestUpdate_GetCorrelatedToWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	upd := NewUpdateExpression(q, "Order", []UpdateTransform{
		{FieldPath: "name", NewValue: q.GetFlowedObjectValue()},
	})
	got := upd.GetCorrelatedToWithoutChildren()
	if _, ok := got[q.GetAlias()]; !ok {
		t.Fatalf("update correlation set %v doesn't contain q's alias", got)
	}
}

func TestLogicalIntersection_GetCorrelatedToWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	keys := []values.Value{q.GetFlowedObjectValue()} // references q's alias
	x := NewLogicalIntersectionExpression(
		[]Quantifier{q},
		keys,
	)
	got := x.GetCorrelatedToWithoutChildren()
	if _, ok := got[q.GetAlias()]; !ok {
		t.Fatalf("intersection correlation set %v doesn't contain comparison-key alias %v", got, q.GetAlias())
	}
}

func TestLeafExpressions_NoCorrelations(t *testing.T) {
	t.Parallel()
	scan := NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	if got := scan.GetCorrelatedToWithoutChildren(); len(got) != 0 {
		t.Fatalf("scan correlation set non-empty: %v", got)
	}

	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	d := NewLogicalDistinctExpression(q)
	if got := d.GetCorrelatedToWithoutChildren(); len(got) != 0 {
		t.Fatalf("distinct correlation set non-empty: %v", got)
	}
	u := NewLogicalUnionExpression([]Quantifier{q})
	if got := u.GetCorrelatedToWithoutChildren(); len(got) != 0 {
		t.Fatalf("union correlation set non-empty: %v", got)
	}
}

func TestCorrelationWalking_PicksUpDeepReference(t *testing.T) {
	t.Parallel()
	// Wrap q's flowed object inside an Arithmetic + Comparison, prove
	// the walker descends into nested Values.
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	deep := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  q.GetFlowedObjectValue(),
		Right: values.NewBooleanValue(true),
	}
	pred := predicates.NewComparisonPredicate(
		deep,
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.NewBooleanValue(true)},
	)
	f := NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, q)
	got := f.GetCorrelatedToWithoutChildren()
	if _, ok := got[q.GetAlias()]; !ok {
		t.Fatalf("walker didn't descend into nested Arithmetic — got %v", got)
	}
}

// TestReference_GetCorrelatedTo_OwnAliasNotFree pins Java's
// AbstractRelationalExpressionWithChildren.computeCorrelatedTo semantics on
// the Reference aggregate: an expression's OWN predicates referencing its OWN
// quantifier are BOUND, not free — the alias must not surface in the
// reference's correlation set. Go reuses human-readable aliases (Java mints
// unique ones), so before this filter a dissolved outer-join box — whose
// null-on-empty quantifier reuses the null-supplying leg's alias — reported
// its inner select as "correlated to" the alias it binds, and
// SelectMergeRule's retained-quantifier translation captured the inner
// binding (the 42804 wrong-window bake on LEFT JOIN + correlated EXISTS).
func TestReference_GetCorrelatedTo_OwnAliasNotFree(t *testing.T) {
	t.Parallel()
	inner := ForEachQuantifier(InitialOf(&leafScan{name: "E"}))
	outer := values.NamedCorrelationIdentifier("D")
	// Select over inner with a predicate referencing BOTH its own quantifier
	// (bound) and an outer alias (free).
	pred := predicates.NewComparisonPredicate(
		inner.GetFlowedObjectValue(),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: values.NewQuantifiedObjectValue(outer),
		},
	)
	sel := NewSelectExpression(inner.GetFlowedObjectValue(), []Quantifier{inner}, []predicates.QueryPredicate{pred})
	ref := InitialOf(sel)
	got := ref.GetCorrelatedTo()
	if _, leak := got[inner.GetAlias()]; leak {
		t.Fatalf("own quantifier alias %v leaked as a FREE correlation: %v (Java filters bound aliases)", inner.GetAlias(), got)
	}
	if _, free := got[outer]; !free {
		t.Fatalf("outer alias %v missing from the free correlation set: %v", outer, got)
	}
}

// TestQuantifier_GetCorrelatedTo_Transitive pins RFC-189 A4 (finding 7):
// Quantifier.GetCorrelatedTo() returned the EMPTY set (an under-approximation —
// a correlated leg reported as free-standing), where Java's
// Quantifier.getCorrelatedTo() delegates to getRangesOver().getCorrelatedTo().
// A quantifier ranging over a reference that correlates to an external alias
// must report that alias transitively; the reference's own bound alias must not
// leak.
func TestQuantifier_GetCorrelatedTo_Transitive(t *testing.T) {
	t.Parallel()
	innerQ := ForEachQuantifier(InitialOf(&leafScan{name: "T"}))
	x := values.NamedCorrelationIdentifier("X")
	// Select over innerQ whose predicate references the external alias X (free).
	pred := predicates.NewComparisonPredicate(
		innerQ.GetFlowedObjectValue(),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: values.NewQuantifiedObjectValue(x),
		},
	)
	sel := NewSelectExpression(innerQ.GetFlowedObjectValue(), []Quantifier{innerQ}, []predicates.QueryPredicate{pred})
	ref := InitialOf(sel)

	// A quantifier ranging over that reference must surface X transitively.
	q := ForEachQuantifier(ref)
	got := q.GetCorrelatedTo()
	if _, ok := got[x]; !ok {
		t.Fatalf("quantifier correlation set %v must contain external alias %v (transitive delegation, was empty pre-fix)", got, x)
	}
	if _, leak := got[innerQ.GetAlias()]; leak {
		t.Fatalf("bound inner alias %v leaked into quantifier correlation set %v", innerQ.GetAlias(), got)
	}
}

// TestReference_GetCorrelatedTo_NonCorrelatableParentRetains pins the OTHER
// half of Java's computeCorrelatedTo (the `!canCorrelate() || !bound`
// disjunct on child correlations): a parent that CANNOT anchor correlation
// (a union — evaluating one branch never binds another's alias) must RETAIN
// a child's correlation even when it coincides with a sibling quantifier's
// alias — filtering it would hide a genuinely FREE dependency behind a name
// coincidence. Contrast: a SelectExpression (canCorrelate) filters the same
// alias, because there the sibling genuinely binds it.
func TestReference_GetCorrelatedTo_NonCorrelatableParentRetains(t *testing.T) {
	t.Parallel()
	// Branch 1: plain scan quantifier, aliased A.
	qA := NamedForEachQuantifier(values.NamedCorrelationIdentifier("A"), InitialOf(&leafScan{name: "T"}))
	// Branch 2: a select whose predicate references A — a FREE correlation
	// from branch 2's perspective (nothing in its own subtree binds A).
	inner := ForEachQuantifier(InitialOf(&leafScan{name: "U"}))
	pred := predicates.NewComparisonPredicate(
		inner.GetFlowedObjectValue(),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("A")),
		},
	)
	branch2 := NewSelectExpression(inner.GetFlowedObjectValue(), []Quantifier{inner}, []predicates.QueryPredicate{pred})
	qB := ForEachQuantifier(InitialOf(branch2))
	union := NewLogicalUnionExpression([]Quantifier{qA, qB})
	got := InitialOf(union).GetCorrelatedTo()
	if _, retained := got[values.NamedCorrelationIdentifier("A")]; !retained {
		t.Fatalf("union (CanCorrelate=false) must RETAIN the branch's free correlation A despite the sibling alias coincidence: %v", got)
	}
}
