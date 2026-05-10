package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TestInExplode_SingleElement verifies that IN('x') produces a simple
// equality filter (not a union).
func TestInExplode_SingleElement(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	inList := &values.ConstantValue{Value: []any{"active"}, Typ: values.TypeUnknown}
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

	if len(results) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(results))
	}
	f, ok := results[0].(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("expected *LogicalFilterExpression, got %T", results[0])
	}
	preds := f.GetPredicates()
	if len(preds) != 1 {
		t.Fatalf("expected 1 predicate, got %d", len(preds))
	}
	cp := preds[0].(*predicates.ComparisonPredicate)
	if cp.Comparison.Type != predicates.ComparisonEquals {
		t.Fatalf("expected ComparisonEquals, got %v", cp.Comparison.Type)
	}
}

// TestInExplode_MultiColumnIndex verifies that IN on the first column
// of a compound index, plus equality on the second, produces N index
// scans each consuming both predicates.
func TestInExplode_MultiColumnIndex(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status_amount",
		[]string{"Order"},
		[]string{"STATUS", "AMOUNT"},
		[]values.CorrelationIdentifier{a1, a2},
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
	eqPred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "AMOUNT", Typ: values.TypeInt},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(50)),
	)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{inPred, eqPred},
		q,
	)
	ref := expressions.InitialOf(filter)

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
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
			for _, qq := range m.GetQuantifiers() {
				walk(qq.GetRangesOver(), visited)
			}
		}
	}
	walk(ref, map[*expressions.Reference]bool{})
	if indexScanCount < 2 {
		t.Fatalf("expected at least 2 index scans (one per IN element), got %d", indexScanCount)
	}
}

// TestIndexScan_GapInPrefix verifies that a predicate on the SECOND
// column of a compound index (without a predicate on the first) does
// NOT produce an index scan (prefix must be contiguous from position 0).
func TestIndexScan_GapInPrefix(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status_amount",
		[]string{"Order"},
		[]string{"STATUS", "AMOUNT"},
		[]values.CorrelationIdentifier{a1, a2},
		values.UnknownType,
		false,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "AMOUNT", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(100)),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	rule := NewImplementIndexScanRule()
	results := FireExpressionRuleWithMemo(rule, filterRef, ctx, nil)

	if len(results) != 0 {
		t.Fatalf("expected 0 yields (gap in prefix — no predicate on first column), got %d", len(results))
	}
}

// TestIndexScan_InequalityStopsPrefix verifies that after an
// inequality on column 1, a predicate on column 2 becomes residual
// (inequality terminates the prefix).
func TestIndexScan_InequalityStopsPrefix(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status_amount",
		[]string{"Order"},
		[]string{"STATUS", "AMOUNT"},
		[]values.CorrelationIdentifier{a1, a2},
		values.UnknownType,
		false,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(5)),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "AMOUNT", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(100)),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	rule := NewImplementIndexScanRule()
	results := FireExpressionRuleWithMemo(rule, filterRef, ctx, nil)

	if len(results) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(results))
	}
	// Should be filter-over-index-scan (AMOUNT predicate is residual
	// because the inequality on STATUS terminates the prefix).
	if _, ok := results[0].(*physicalIndexScanWrapper); ok {
		t.Fatal("expected filter wrapper (residual), got bare index scan")
	}
}

// TestIndexScan_AllPredicatesResidual verifies that if ALL predicates
// are on non-indexed columns, no index scan is produced.
func TestIndexScan_AllPredicatesResidual(t *testing.T) {
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
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "UNINDEXED_COL", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	rule := NewImplementIndexScanRule()
	results := FireExpressionRuleWithMemo(rule, filterRef, ctx, nil)

	if len(results) != 0 {
		t.Fatalf("expected 0 yields (predicate on non-indexed column), got %d", len(results))
	}
}

// TestPlanContext_FromIndexDefs_UpperCaseColumnNames verifies that the
// PlanContextBuilder uppercases column names for SQL-standard matching.
func TestPlanContext_FromIndexDefs_UpperCaseColumnNames(t *testing.T) {
	t.Parallel()

	stub := stubDef{
		name:  "Order$status_amount",
		cols:  []string{"status", "Amount"},
		types: []string{"Order"},
	}

	ctx := NewPlanContextFromIndexDefs([]IndexDef{stub})
	cands := ctx.GetMatchCandidates()
	if len(cands) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(cands))
	}
	cols := cands[0].GetColumnNames()
	if cols[0] != "STATUS" || cols[1] != "AMOUNT" {
		t.Fatalf("column names not uppercased: %v", cols)
	}
}

func (d stubDef) IndexName() string          { return d.name }
func (d stubDef) IndexColumnNames() []string { return d.cols }
func (d stubDef) IndexRecordTypes() []string { return d.types }
func (d stubDef) IndexIsUnique() bool        { return d.unique }

type stubDef struct {
	name   string
	cols   []string
	types  []string
	unique bool
}

// TestPlanContext_FromIndexDefs_UniqueFlag checks that unique=true
// propagates from IndexDef to the MatchCandidate.
func TestPlanContext_FromIndexDefs_UniqueFlag(t *testing.T) {
	t.Parallel()

	ctx := NewPlanContextFromIndexDefs([]IndexDef{stubDef{
		name:   "Order$pk",
		cols:   []string{"id"},
		types:  []string{"Order"},
		unique: true,
	}})
	cands := ctx.GetMatchCandidates()
	if len(cands) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(cands))
	}
	if !cands[0].IsUnique() {
		t.Fatal("candidate should be unique")
	}
}

// TestPlanner_MemoDeduplicatesEquivalentScans verifies that when two
// rules produce structurally-identical full scans, the Memo collapses
// them into one Reference (no bloat).
func TestPlanner_MemoDeduplicatesEquivalentScans(t *testing.T) {
	t.Parallel()

	scan1 := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scan2 := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	ref1 := expressions.InitialOf(scan1)
	ref2 := expressions.InitialOf(scan2)

	q1 := expressions.ForEachQuantifier(ref1)
	q2 := expressions.ForEachQuantifier(ref2)
	filter1 := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewConstantPredicate(predicates.TriTrue),
		},
		q1,
	)
	filter2 := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewConstantPredicate(predicates.TriTrue),
		},
		q2,
	)
	topRef := expressions.InitialOf(filter1)
	topRef.Insert(filter2)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext())
	tasksRun, conv := p.Explore(topRef)
	if !conv {
		t.Fatal("planner did not converge")
	}
	if tasksRun == 0 {
		t.Fatal("planner ran 0 tasks (nothing happened)")
	}
}

// TestInExplode_DuplicateElements verifies that IN(1,1,2) produces a
// SelectExpression with an ExplodeExpression containing all 3 elements
// (no dedup — that's the executor's job via DISTINCT if needed).
func TestInExplode_DuplicateElements(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	inList := &values.ConstantValue{Value: []any{int64(1), int64(1), int64(2)}, Typ: values.TypeUnknown}
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
	sel, ok := results[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected *SelectExpression, got %T", results[0])
	}

	// Verify the ExplodeExpression carries all 3 elements (no dedup).
	qs := sel.GetQuantifiers()
	if len(qs) != 2 {
		t.Fatalf("expected 2 quantifiers, got %d", len(qs))
	}
	// Find the explode quantifier.
	for _, qq := range qs {
		ref := qq.GetRangesOver()
		if ref == nil {
			continue
		}
		for _, m := range ref.Members() {
			if explode, ok := m.(*expressions.ExplodeExpression); ok {
				cv := explode.GetCollectionValue()
				vals := cv.Evaluate(nil)
				list, ok := vals.([]any)
				if !ok {
					t.Fatalf("collection value is %T, expected []any", vals)
				}
				if len(list) != 3 {
					t.Fatalf("expected 3 elements in explode list (no dedup), got %d", len(list))
				}
				return
			}
		}
	}
	t.Fatal("no ExplodeExpression found in SelectExpression quantifiers")
}

// TestIndexScan_ThreeColumnPrefix tests a 3-column index with all
// three columns having equalities — should produce a single index
// scan with 3-position prefix.
func TestIndexScan_ThreeColumnPrefix(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	a3 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$compound",
		[]string{"Order"},
		[]string{"A", "B", "C"},
		[]values.CorrelationIdentifier{a1, a2, a3},
		values.UnknownType,
		false,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "C", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(3)),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "A", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "B", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(2)),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	rule := NewImplementIndexScanRule()
	results := FireExpressionRuleWithMemo(rule, filterRef, ctx, nil)

	if len(results) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(results))
	}
	wrapper, ok := results[0].(*physicalIndexScanWrapper)
	if !ok {
		t.Fatalf("expected bare index scan (all 3 consumed), got %T", results[0])
	}
	comps := wrapper.plan.GetScanComparisons()
	if len(comps) != 3 {
		t.Fatalf("expected 3 scan comparisons, got %d", len(comps))
	}
	for i, c := range comps {
		if !c.IsEquality() {
			t.Fatalf("comparison %d should be equality", i)
		}
	}
}
