package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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

	// Multi-element IN now produces SelectExpression, not Union.
	sel, ok := results[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected *SelectExpression, got %T", results[0])
	}

	// SelectExpression must have no predicates (ImplementInJoinRule requirement).
	if sel.HasPredicates() {
		t.Fatal("SelectExpression should have no predicates")
	}

	// Must have exactly 2 quantifiers: inner + explode.
	qs := sel.GetQuantifiers()
	if len(qs) != 2 {
		t.Fatalf("expected 2 quantifiers (inner + explode), got %d", len(qs))
	}

	// ResultValue must be a QOV referencing the inner quantifier.
	qov, ok := sel.GetResultValue().(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("expected QOV result value, got %T", sel.GetResultValue())
	}
	if qov.Correlation != qs[0].GetAlias() {
		t.Fatalf("QOV should reference inner quantifier alias %v, got %v",
			qs[0].GetAlias(), qov.Correlation)
	}

	// One quantifier should range over an ExplodeExpression.
	foundExplode := false
	for _, qq := range qs {
		ref := qq.GetRangesOver()
		if ref == nil {
			continue
		}
		for _, m := range ref.Members() {
			if _, ok := m.(*expressions.ExplodeExpression); ok {
				foundExplode = true
			}
		}
	}
	if !foundExplode {
		t.Fatal("expected one quantifier to range over ExplodeExpression")
	}

	// The inner quantifier should contain a LogicalFilterExpression
	// with the equality predicate (col = QOV(explodeAlias)).
	innerRef := qs[0].GetRangesOver()
	if innerRef == nil {
		t.Fatal("inner quantifier has no reference")
	}
	foundInnerFilter := false
	for _, m := range innerRef.Members() {
		lf, ok := m.(*expressions.LogicalFilterExpression)
		if !ok {
			continue
		}
		foundInnerFilter = true
		preds := lf.GetPredicates()
		if len(preds) != 1 {
			t.Fatalf("inner filter should have 1 predicate, got %d", len(preds))
		}
		cp, ok := preds[0].(*predicates.ComparisonPredicate)
		if !ok {
			t.Fatalf("expected *ComparisonPredicate, got %T", preds[0])
		}
		if cp.Comparison.Type != predicates.ComparisonEquals {
			t.Fatalf("expected ComparisonEquals, got %v", cp.Comparison.Type)
		}
		// RHS should be a QOV referencing the explode quantifier.
		rhsQOV, ok := cp.Comparison.Operand.(*values.QuantifiedObjectValue)
		if !ok {
			t.Fatalf("equality RHS should be *QuantifiedObjectValue, got %T",
				cp.Comparison.Operand)
		}
		if rhsQOV.Correlation != qs[1].GetAlias() {
			t.Fatalf("equality QOV should correlate to explode alias %v, got %v",
				qs[1].GetAlias(), rhsQOV.Correlation)
		}
	}
	if !foundInnerFilter {
		t.Fatal("inner quantifier should contain LogicalFilterExpression")
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

	// Multi-element IN with other predicates → SelectExpression.
	sel, ok := results[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected *SelectExpression, got %T", results[0])
	}

	// SelectExpression must have no predicates.
	if sel.HasPredicates() {
		t.Fatal("SelectExpression should have no predicates")
	}

	// The inner quantifier's LogicalFilterExpression must carry
	// both the equality predicate and the other predicate.
	qs := sel.GetQuantifiers()
	if len(qs) != 2 {
		t.Fatalf("expected 2 quantifiers, got %d", len(qs))
	}

	innerRef := qs[0].GetRangesOver()
	if innerRef == nil {
		t.Fatal("inner quantifier has no reference")
	}
	for _, m := range innerRef.Members() {
		lf, ok := m.(*expressions.LogicalFilterExpression)
		if !ok {
			continue
		}
		preds := lf.GetPredicates()
		if len(preds) != 2 {
			t.Fatalf("inner filter should have 2 predicates (eq + other), got %d", len(preds))
		}
		// First predicate: equality with QOV RHS.
		cp0, ok := preds[0].(*predicates.ComparisonPredicate)
		if !ok {
			t.Fatalf("predicate[0]: expected *ComparisonPredicate, got %T", preds[0])
		}
		if cp0.Comparison.Type != predicates.ComparisonEquals {
			t.Fatalf("predicate[0]: expected ComparisonEquals, got %v", cp0.Comparison.Type)
		}
		// Second predicate: AMOUNT > 100.
		cp1, ok := preds[1].(*predicates.ComparisonPredicate)
		if !ok {
			t.Fatalf("predicate[1]: expected *ComparisonPredicate, got %T", preds[1])
		}
		if cp1.Comparison.Type != predicates.ComparisonGreaterThan {
			t.Fatalf("predicate[1]: expected ComparisonGreaterThan, got %v", cp1.Comparison.Type)
		}
		return
	}
	t.Fatal("inner quantifier should contain LogicalFilterExpression")
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
		nil,
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

	rules := DefaultExpressionRules()
	rules = append(rules, NewInComparisonToExplodeRule())
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules())
	if _, conv := p.Explore(ref); !conv {
		t.Fatal("planner did not converge")
	}

	// The exploration should produce at least a SelectExpression with
	// an ExplodeExpression quantifier (the new shape), or index scans
	// if index matching fires. Verify the tree contains at least one
	// ExplodeExpression or index scan.
	explodeCount := 0
	indexScanCount := 0
	var walk func(r *expressions.Reference, visited map[*expressions.Reference]bool)
	walk = func(r *expressions.Reference, visited map[*expressions.Reference]bool) {
		if r == nil || visited[r] {
			return
		}
		visited[r] = true
		for _, m := range r.Members() {
			if _, ok := m.(*expressions.ExplodeExpression); ok {
				explodeCount++
			}
			if IsPhysicalIndexScan(m) || IsPhysicalFetchFromPartialRecord(m) {
				indexScanCount++
			}
			for _, q := range m.GetQuantifiers() {
				walk(q.GetRangesOver(), visited)
			}
		}
	}
	walk(ref, map[*expressions.Reference]bool{})

	if explodeCount == 0 && indexScanCount == 0 {
		t.Fatal("IN-explode rule did not produce any ExplodeExpressions or index scans")
	}
}

// TestInComparisonToExplodeRule_ImplementInJoinShape verifies the produced
// SelectExpression shape is compatible with ImplementInJoinRule's expectations:
// no predicates, QOV result value referencing inner, exactly one inner + one
// explode quantifier.
func TestInComparisonToExplodeRule_ImplementInJoinShape(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	inList := &values.ConstantValue{Value: []any{int64(10), int64(20)}, Typ: values.TypeUnknown}
	inPred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "ID", Typ: values.TypeInt},
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

	sel, ok := results[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected *SelectExpression, got %T", results[0])
	}

	// Verify: no predicates.
	if sel.HasPredicates() {
		t.Fatal("SelectExpression must have no predicates for ImplementInJoinRule")
	}

	// Verify: exactly 2 quantifiers.
	qs := sel.GetQuantifiers()
	if len(qs) != 2 {
		t.Fatalf("expected 2 quantifiers, got %d", len(qs))
	}

	// Verify: result value is QOV referencing inner quantifier's alias.
	qov, ok := sel.GetResultValue().(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("result value should be *QuantifiedObjectValue, got %T", sel.GetResultValue())
	}
	// The inner quantifier is first.
	innerAlias := qs[0].GetAlias()
	if qov.Correlation != innerAlias {
		t.Fatalf("QOV correlation %v doesn't match inner quantifier alias %v",
			qov.Correlation, innerAlias)
	}

	// Verify: explode quantifier ranges over ExplodeExpression.
	explodeRef := qs[1].GetRangesOver()
	if explodeRef == nil {
		t.Fatal("explode quantifier has no reference")
	}
	foundExplode := false
	for _, m := range explodeRef.Members() {
		if explode, ok := m.(*expressions.ExplodeExpression); ok {
			foundExplode = true
			// Verify the collection value contains our IN-list.
			cv := explode.GetCollectionValue()
			if cv == nil {
				t.Fatal("ExplodeExpression has nil collection value")
			}
			vals, err := cv.Evaluate(nil)
			if err != nil {
				t.Fatalf("collection value evaluate: %v", err)
			}
			list, ok := vals.([]any)
			if !ok {
				t.Fatalf("collection value evaluated to %T, expected []any", vals)
			}
			if len(list) != 2 {
				t.Fatalf("expected 2 elements in explode list, got %d", len(list))
			}
		}
	}
	if !foundExplode {
		t.Fatal("second quantifier should range over ExplodeExpression")
	}

	// Verify: inner quantifier's equality predicate correlates to
	// the explode quantifier's alias.
	innerFilterRef := qs[0].GetRangesOver()
	if innerFilterRef == nil {
		t.Fatal("inner quantifier has no reference")
	}
	explodeAlias := qs[1].GetAlias()
	for _, m := range innerFilterRef.Members() {
		lf, ok := m.(*expressions.LogicalFilterExpression)
		if !ok {
			continue
		}
		for _, p := range lf.GetPredicates() {
			cp, ok := p.(*predicates.ComparisonPredicate)
			if !ok {
				continue
			}
			if cp.Comparison.Type != predicates.ComparisonEquals {
				continue
			}
			rhsQOV, ok := cp.Comparison.Operand.(*values.QuantifiedObjectValue)
			if !ok {
				continue
			}
			if rhsQOV.Correlation != explodeAlias {
				t.Fatalf("equality QOV correlates to %v, expected explode alias %v",
					rhsQOV.Correlation, explodeAlias)
			}
			// Verify the correlation set includes the explode alias.
			correlated := cp.Comparison.GetCorrelatedTo()
			if _, ok := correlated[explodeAlias]; !ok {
				t.Fatalf("Comparison.GetCorrelatedTo() does not include explode alias %v",
					explodeAlias)
			}
			return // found the expected equality
		}
	}
	t.Fatal("inner filter should have equality predicate correlating to explode alias")
}

// TestDistinctInListValues covers IN-list dedup, including the panic-safety
// case: array / vector IN literals fold to non-comparable
// slices ([]float64, []any), so the comparator must not use a bare `==`.
func TestDistinctInListValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []any
		want int
	}{
		{"scalars_with_dups", []any{int64(1), int64(1), int64(2)}, 2},
		{"strings", []any{"a", "b", "a"}, 2},
		{"bytes_by_content", []any{[]byte{1, 2}, []byte{1, 2}, []byte{3}}, 2},
		{"nil_collapses", []any{nil, nil, int64(1)}, 2},
		// Non-comparable slice literals (array/vector IN list) — must dedupe
		// structurally without panicking on ==.
		{"float_slices", []any{[]float64{1, 0}, []float64{1, 0}, []float64{0, 1}}, 2},
		{"any_slices", []any{[]any{int64(1)}, []any{int64(1)}, []any{int64(2)}}, 2},
		{"mixed_slice_and_scalar", []any{[]float64{1}, int64(1), []float64{1}}, 2},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := distinctInListValues(c.in) // must not panic
			if len(got) != c.want {
				t.Fatalf("distinctInListValues(%v) = %v (len %d), want len %d", c.in, got, len(got), c.want)
			}
		})
	}
}
