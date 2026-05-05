package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestInComparisonToExplodeRule_BasicExplode(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	inList := &values.ConstantValue{Value: []any{int64(1), int64(2), int64(3)}, Typ: values.TypeUnknown}
	inPred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "STATUS", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonIn, Operand: inList},
	)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{inPred},
		q,
	)
	ref := expressions.InitialOf(filter)

	rule := NewInComparisonToExplodeRule()
	results := FireExpressionRuleWithMemo(rule, ref, EmptyPlanContext(), nil)

	if len(results) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(results))
	}
	union, ok := results[0].(*expressions.LogicalUnionExpression)
	if !ok {
		t.Fatalf("expected *LogicalUnionExpression, got %T", results[0])
	}
	qs := union.GetQuantifiers()
	if len(qs) != 3 {
		t.Fatalf("expected 3 union legs (one per IN element), got %d", len(qs))
	}
}

func TestInComparisonToExplodeRule_SingleElement(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	inList := &values.ConstantValue{Value: []any{int64(6)}, Typ: values.TypeUnknown}
	inPred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "B", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonIn, Operand: inList},
	)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{inPred},
		q,
	)
	ref := expressions.InitialOf(filter)

	rule := NewInComparisonToExplodeRule()
	results := FireExpressionRuleWithMemo(rule, ref, EmptyPlanContext(), nil)

	if len(results) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(results))
	}
	f, ok := results[0].(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("expected *LogicalFilterExpression for single-element IN, got %T", results[0])
	}
	preds := f.GetPredicates()
	if len(preds) != 1 {
		t.Fatalf("expected 1 predicate (equality), got %d", len(preds))
	}
	cp, ok := preds[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate, got %T", preds[0])
	}
	if cp.Comparison.Type != predicates.ComparisonEquals {
		t.Fatalf("expected ComparisonEquals, got %v", cp.Comparison.Type)
	}
}

func TestInComparisonToExplodeRule_PreservesOtherPredicates(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	inList := &values.ConstantValue{Value: []any{"a", "b"}, Typ: values.TypeUnknown}
	inPred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
		predicates.Comparison{Type: predicates.ComparisonIn, Operand: inList},
	)
	otherPred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "AMOUNT", Typ: values.TypeInt},
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(100)),
	)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{inPred, otherPred},
		q,
	)
	ref := expressions.InitialOf(filter)

	rule := NewInComparisonToExplodeRule()
	results := FireExpressionRuleWithMemo(rule, ref, EmptyPlanContext(), nil)

	if len(results) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(results))
	}
	union, ok := results[0].(*expressions.LogicalUnionExpression)
	if !ok {
		t.Fatalf("expected *LogicalUnionExpression, got %T", results[0])
	}
	qs := union.GetQuantifiers()
	if len(qs) != 2 {
		t.Fatalf("expected 2 union legs, got %d", len(qs))
	}
	for i, lq := range qs {
		legRef := lq.GetRangesOver()
		for _, m := range legRef.Members() {
			if lf, ok := m.(*expressions.LogicalFilterExpression); ok {
				if len(lf.GetPredicates()) != 2 {
					t.Fatalf("leg %d: expected 2 predicates, got %d", i, len(lf.GetPredicates()))
				}
				return
			}
		}
	}
	t.Fatal("no leg filter found")
}

func TestInComparisonToExplodeRule_NoInPredicate(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	eqPred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
	)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{eqPred},
		q,
	)
	ref := expressions.InitialOf(filter)

	rule := NewInComparisonToExplodeRule()
	results := FireExpressionRuleWithMemo(rule, ref, EmptyPlanContext(), nil)

	if len(results) != 0 {
		t.Fatalf("expected 0 yields (no IN predicate), got %d", len(results))
	}
}

func TestInComparisonToExplodeRule_EmptyInList(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	inList := &values.ConstantValue{Value: []any{}, Typ: values.TypeUnknown}
	inPred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
		predicates.Comparison{Type: predicates.ComparisonIn, Operand: inList},
	)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{inPred},
		q,
	)
	ref := expressions.InitialOf(filter)

	rule := NewInComparisonToExplodeRule()
	results := FireExpressionRuleWithMemo(rule, ref, EmptyPlanContext(), nil)

	if len(results) != 0 {
		t.Fatalf("expected 0 yields (empty IN list), got %d", len(results))
	}
}

func TestInComparisonToExplodeRule_PlannerIntegration(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	inList := &values.ConstantValue{Value: []any{"active", "pending"}, Typ: values.TypeUnknown}
	inPred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
		predicates.Comparison{Type: predicates.ComparisonIn, Operand: inList},
	)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{inPred},
		q,
	)
	ref := expressions.InitialOf(filter)

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	rules = append(rules, NewInComparisonToExplodeRule())
	p := NewPlanner(rules, ctx)
	if _, conv := p.Explore(ref); !conv {
		t.Fatal("planner did not converge")
	}

	indexScanCount := 0
	var walk func(r *expressions.Reference, visited map[*expressions.Reference]bool)
	walk = func(r *expressions.Reference, visited map[*expressions.Reference]bool) {
		if r == nil || visited[r] {
			return
		}
		visited[r] = true
		for _, m := range r.Members() {
			if IsPhysicalIndexScan(m) {
				indexScanCount++
			}
			for _, q := range m.GetQuantifiers() {
				walk(q.GetRangesOver(), visited)
			}
		}
	}
	walk(ref, map[*expressions.Reference]bool{})
	if indexScanCount == 0 {
		t.Fatal("IN-explode + index scan rule did not produce any index scans")
	}
}
