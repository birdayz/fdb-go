package cascades

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustImplementFilterConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct implement-filter fixture: " + err.Error())
	}
	return value
}

func implementFilterRowType() *values.RecordType {
	return values.NewRecordType("", false, []values.Field{
		{Name: "active", FieldType: values.NotNullBoolean},
		{Name: "ID", FieldType: values.NotNullLong},
	})
}

func implementFilterScan(recordType string) *expressions.FullUnorderedScanExpression {
	return mustImplementFilterConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{recordType}, implementFilterRowType()))
}

func implementFilterField(quantifier expressions.Quantifier, ordinal int) values.Value {
	root := mustImplementFilterConstruct(quantifier.RequireFlowedObjectValue())
	return mustImplementFilterConstruct(values.ResolveFieldOrdinals(root, []int{ordinal}))
}

func TestImplementFilterRule_FiresAfterScanImplemented(t *testing.T) {
	t.Parallel()
	// Build Filter(P, Scan). Run PrimaryScanRule to add a physical
	// wrapper to the inner Reference. Then ImplementFilterRule should
	// fire and yield a FilterPlan.
	pred := predicates.NewConstantPredicate(predicates.TriFalse)
	scan := implementFilterScan("Order")
	innerRef := expressions.InitialOf(scan)
	filter := mustImplementFilterConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(innerRef),
	))
	topRef := expressions.InitialOf(filter)

	// Step 1: Implement the scan.
	scanRule := NewPrimaryScanRule()
	mustFireExpressionRule(t, scanRule, innerRef)
	// innerRef should now have 2 members (logical scan + physical wrapper).
	if got := len(innerRef.Members()); got != 2 {
		t.Fatalf("after PrimaryScanRule, innerRef has %d members, want 2", got)
	}

	// Step 2: Fire ImplementFilterRule on the top Reference.
	filterRule := NewImplementFilterRule()
	yielded := mustFireExpressionRule(t, filterRule, topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementFilterRule yielded %d, want 1", len(yielded))
	}
	wrap, ok := yielded[0].(*plans.RecordQueryPredicatesFilterPlan)
	if !ok {
		t.Fatalf("yield = %T, want *plans.RecordQueryPredicatesFilterPlan", yielded[0])
	}
	plan := wrap
	if plan.GetInner() == nil {
		t.Fatal("filter plan has no inner")
	}
	if got := len(plan.GetPredicates()); got != 1 {
		t.Fatalf("filter plan predicates = %d, want 1", got)
	}
	innerPlan, ok := plan.GetInner().(*plans.RecordQueryScanPlan)
	if !ok {
		t.Fatalf("filter plan inner = %T, want *RecordQueryScanPlan", plan.GetInner())
	}
	if rts := innerPlan.GetRecordTypes(); len(rts) != 1 || rts[0] != "Order" {
		t.Fatalf("inner scan record types = %v", rts)
	}
}

func TestFilterImplementationsSharePhysicalMemoIdentity(t *testing.T) {
	t.Parallel()
	for _, selectFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "filter_first", true: "select_first"}[selectFirst], func(t *testing.T) {
			t.Parallel()
			scan := mustImplementFilterConstruct(plans.NewRecordQueryScanPlan([]string{"T"}, implementFilterRowType(), false))
			inner := expressions.FinalOfAtStage(scan, expressions.StagePlanned)
			computeRefPlanProperties(inner)
			q := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("input"), inner)
			predicate := predicates.NewComparisonPredicate(implementFilterField(q, 1), predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(0)))
			filter := mustImplementFilterConstruct(expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{predicate}, q))
			selectExpr := mustImplementFilterConstruct(expressions.NewSelectExpression(mustImplementFilterConstruct(q.RequireFlowedObjectValue()), []expressions.Quantifier{q}, []predicates.QueryPredicate{predicate}))
			root := expressions.InitialOf(filter)
			root.Insert(selectExpr)
			root.AdvanceStagePreservingMembers(expressions.StagePlanned)
			p := NewPlanner(nil, nil)
			p.memo = NewMemo(root)
			p.constraintMap = NewConstraintMap()
			tasks := []Task{
				&TransformExprTask{Phase: PhasePlanning, Ref: root, Expr: filter, Rule: NewImplementFilterRule()},
				&TransformImplTask{Phase: PhasePlanning, Ref: root, Expr: selectExpr, Rule: NewImplementSimpleSelectRule()},
			}
			if selectFirst {
				tasks[0], tasks[1] = tasks[1], tasks[0]
			}
			for i, task := range tasks {
				task.Run(context.Background(), p)
				if p.capErr != nil {
					t.Fatal(p.capErr)
				}
				if len(root.FinalMembers()) != 1 {
					t.Fatalf("after producer %d, physical alternatives = %d, want 1", i, len(root.FinalMembers()))
				}
				physical := root.FinalMembers()[0].(*plans.RecordQueryPredicatesFilterPlan)
				if physical.GetInnerQuantifier().Kind() != expressions.QuantifierPhysical {
					t.Errorf("producer %d emitted logical edge kind %v", i, physical.GetInnerQuantifier().Kind())
				}
				if physical.GetInnerAlias() != q.GetAlias() {
					t.Fatal("physicalization changed the predicate binding alias")
				}
				if i == 0 && len(p.stack) == 0 {
					t.Fatal("first physical alternative scheduled no work")
				}
				if i == 1 && len(p.stack) != 0 {
					t.Fatalf("duplicate physical alternative scheduled %d tasks", len(p.stack))
				}
				for len(p.stack) > 0 {
					p.pop()
				}
			}
		})
	}
}

func TestFilterImplementationTautologiesYieldChildren(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		truth []predicates.TriBool
		elide bool
	}{
		{"empty", nil, true},
		{"true", []predicates.TriBool{predicates.TriTrue}, true},
		{"all_true", []predicates.TriBool{predicates.TriTrue, predicates.TriTrue}, true},
		{"false", []predicates.TriBool{predicates.TriFalse}, false},
		{"unknown", []predicates.TriBool{predicates.TriUnknown}, false},
		{"mixed", []predicates.TriBool{predicates.TriTrue, predicates.TriFalse}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ps := make([]predicates.QueryPredicate, len(tc.truth))
			for i, truth := range tc.truth {
				ps[i] = predicates.NewConstantPredicate(truth)
			}
			scans := []expressions.RelationalExpression{
				mustImplementFilterConstruct(plans.NewRecordQueryScanPlan([]string{"T"}, implementFilterRowType(), false)),
				mustImplementFilterConstruct(plans.NewRecordQueryScanPlan([]string{"T"}, implementFilterRowType(), true)),
			}
			inner := expressions.FinalOfAtStage(scans[0], expressions.StagePlanned)
			inner.InsertFinal(scans[1])
			q := expressions.ForEachQuantifier(inner)
			filter := mustImplementFilterConstruct(expressions.NewLogicalFilterExpression(ps, q))
			yields := mustFireExpressionRule(t, NewImplementFilterRule(), expressions.InitialOf(filter))
			if tc.elide {
				if len(yields) != len(scans) {
					t.Fatalf("yields = %d, want %d original children", len(yields), len(scans))
				}
				for _, scan := range scans {
					found := false
					for _, yielded := range yields {
						found = found || yielded == scan
					}
					if !found {
						t.Fatal("tautology did not yield an original child")
					}
				}
			} else {
				if len(yields) == 0 {
					t.Fatal("non-tautological filter produced no plans")
				}
				for _, yielded := range yields {
					physical, ok := yielded.(*plans.RecordQueryPredicatesFilterPlan)
					if !ok || physical.GetInnerAlias() != q.GetAlias() || len(physical.GetPredicates()) != len(ps) {
						t.Fatalf("non-tautological predicates or binding were lost: %T", yielded)
					}
				}
			}
		})
	}
}

func TestPushFilterThroughFetchProducesPhysicalEdges(t *testing.T) {
	t.Parallel()
	for _, residual := range []bool{false, true} {
		t.Run(map[bool]string{false: "all_pushed", true: "partial_push"}[residual], func(t *testing.T) {
			t.Parallel()
			fetch := pushFetchFetch(pushFetchIndex("idx_x"), func(v values.Value, _, target values.CorrelationIdentifier) (values.Value, bool) {
				if field, ok := values.AsFieldValue(v); ok && field.DisplayName() == "x" {
					return pushFetchFieldForAlias(target, "x"), true
				}
				return nil, false
			})
			q := expressions.NewPhysicalQuantifier(expressions.FinalOfAtStage(fetch, expressions.StagePlanned))
			ps := []predicates.QueryPredicate{predicates.NewComparisonPredicate(pushFetchFieldForAlias(q.GetAlias(), "x"), predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)))}
			if residual {
				ps = append(ps, predicates.NewComparisonPredicate(pushFetchFieldForAlias(q.GetAlias(), "y"), predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(2))))
			}
			filter := mustPushFetchConstruct(plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(q, ps))
			yields := mustFireImplementationRule(t, NewPushFilterThroughFetchRule(), expressions.InitialOf(filter))
			if len(yields) != 1 {
				t.Fatalf("yield count = %d, want 1", len(yields))
			}
			current := yields[0]
			edges := 0
			for len(current.GetQuantifiers()) > 0 {
				quantifiers := current.GetQuantifiers()
				if len(quantifiers) != 1 {
					t.Fatalf("unexpected non-unary node %T", current)
				}
				edge := quantifiers[0]
				if edge.Kind() != expressions.QuantifierPhysical {
					t.Errorf("%T emitted logical edge kind %v", current, edge.Kind())
				}
				edges++
				current = edge.GetRangesOver().AllMembers()[0]
			}
			want := 2
			if residual {
				want++
			}
			if edges != want {
				t.Fatalf("physical path edges = %d, want %d", edges, want)
			}
		})
	}
}

func TestImplementFilterRule_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()
	// Filter(P, Scan) WITHOUT firing PrimaryScanRule first — inner
	// has no physical wrapper, so ImplementFilterRule should skip.
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := implementFilterScan("Order")
	filter := mustImplementFilterConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	))
	topRef := expressions.InitialOf(filter)

	filterRule := NewImplementFilterRule()
	yielded := mustFireExpressionRule(t, filterRule, topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementFilterRule fired without physical inner; yielded %d", len(yielded))
	}
}

// TestBatchA_CostExtraction_PicksPhysicalOverLogical pins that
// after Batch A rules fire and the OPTIMIZE phase runs, the
// planner's BestMember is the physical wrapper (not the logical
// expression). This validates the CostHinter wiring: physical
// wrappers' HintCost returns a discounted cost via
// physicalWrapperCostMultiplier=0.9, so cost extraction prefers
// them over the logical equivalent.
func TestBatchA_CostExtraction_PicksPhysicalOverLogical(t *testing.T) {
	t.Parallel()
	scan := implementFilterScan("Order")
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pred := predicates.NewValuePredicate(implementFilterField(innerQ, 0))
	filter := mustImplementFilterConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		innerQ,
	))
	ref := expressions.InitialOf(filter)

	// Physical wrappers are produced during PLANNING in the production
	// pipeline, so register the Batch A rules as planning rules.
	p := NewPlanner(DefaultExpressionRules(), nil).
		WithPlanningExpressionRules([]ExpressionRule{
			NewPrimaryScanRule(),
			NewImplementFilterRule(),
		})
	if _, _, err := p.Plan(ref); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// Plan's OPTIMIZE phase picks the cheapest. With CostHinter
	// applying the physical-wrapper discount, the physical filter
	// wrapper should win over the logical filter.
	best := p.BestMember(ref)
	if best == nil {
		t.Fatal("BestMember returned nil")
	}
	if _, ok := best.(*plans.RecordQueryPredicatesFilterPlan); !ok {
		t.Fatalf("BestMember = %T, want *plans.RecordQueryPredicatesFilterPlan (cost-driven extraction should pick physical)", best)
	}
}

// TestImplementFilterRule_FiresOnFilterOverDistinct pins that Filter
// over Distinct can be physical-implemented WITHOUT push rules first.
func TestImplementFilterRule_FiresOnFilterOverDistinct(t *testing.T) {
	t.Parallel()
	scan := implementFilterScan("Order")

	distinctInner := mustImplementFilterConstruct(expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	))
	distinctRef := expressions.InitialOf(distinctInner)
	filterQ := expressions.ForEachQuantifier(distinctRef)
	pred := predicates.NewValuePredicate(implementFilterField(filterQ, 0))
	filter := mustImplementFilterConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		filterQ,
	))
	topRef := expressions.InitialOf(filter)

	scanRef := distinctInner.GetQuantifiers()[0].GetRangesOver()
	mustFireExpressionRule(t, NewPrimaryScanRule(), scanRef)
	// Directly create physical distinct wrapper (ImplementDistinctRule
	// removed in D-3; distinct now happens in PLANNING phase).
	scanPlan := findPhysicalPlan(scanRef)
	distPlan := mustImplementFilterConstruct(plans.NewRecordQueryDistinctPlan(scanPlan))
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(findPhysicalExpr(scanRef)))
	// Since RFC-184 W2 the memo holds the bare *plans.RecordQueryDistinctPlan (no
	// physicalDistinctWrapper).
	distinctRef.Insert(mustWithQuantifiers(t, distPlan, []expressions.Quantifier{innerQ}))

	yielded := mustFireExpressionRule(t, NewImplementFilterRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementFilterRule yielded %d, want 1 (Filter over physical Distinct)", len(yielded))
	}
	wrap, ok := yielded[0].(*plans.RecordQueryPredicatesFilterPlan)
	if !ok {
		t.Fatalf("yield = %T, want *plans.RecordQueryPredicatesFilterPlan", yielded[0])
	}
	innerPlan := wrap.GetInner()
	if _, ok := innerPlan.(*plans.RecordQueryDistinctPlan); !ok {
		t.Fatalf("filter inner plan = %T, want *RecordQueryDistinctPlan", innerPlan)
	}
}

// TestImplementFilterRule_FiresOverPhysicalIntersection is the
// symmetric companion to the Filter-over-Union case below.
// SQL pattern: SELECT ... FROM (A INTERSECT B) WHERE pred. The
// 7-wrapper symmetry fix lets Filter recognise physicalIntersection
// wrappers as physical inners.
func TestImplementFilterRule_FiresOverPhysicalIntersection(t *testing.T) {
	t.Parallel()
	rt := values.NewRecordType("T", false, []values.Field{{
		Name: "ID", FieldType: values.NotNullLong,
	}})
	keyRoot := mustImplementFilterConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("implement_filter_intersection_key"), rt))
	comparisonKey := mustImplementFilterConstruct(values.ResolveFieldOrdinals(keyRoot, []int{0}))
	scanA := mustImplementFilterConstruct(plans.NewRecordQueryScanPlan([]string{"T"}, rt, false)).
		WithKeyComponentTypes([]values.Type{values.NotNullLong}).
		WithPrimaryKey([]values.Value{comparisonKey})
	scanB := mustImplementFilterConstruct(plans.NewRecordQueryScanPlan([]string{"T"}, rt, false)).
		WithKeyComponentTypes([]values.Type{values.NotNullLong}).
		WithPrimaryKey([]values.Value{comparisonKey})
	refA := expressions.InitialOf(scanA)
	refB := expressions.InitialOf(scanB)
	intr := mustImplementFilterConstruct(expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(refA),
			expressions.ForEachQuantifier(refB),
		},
		[]values.Value{comparisonKey},
	))
	intrRef := expressions.InitialOf(intr)
	pred := predicates.NewConstantPredicate(predicates.TriFalse)
	filter := mustImplementFilterConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(intrRef),
	))
	topRef := expressions.InitialOf(filter)

	mustFireExpressionRule(t, NewImplementIntersectionRule(), intrRef)

	yielded := mustFireExpressionRule(t, NewImplementFilterRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementFilterRule yielded %d, want 1 (Filter over physical Intersection)", len(yielded))
	}
	wrap := yielded[0].(*plans.RecordQueryPredicatesFilterPlan)
	if _, ok := wrap.GetInner().(*plans.RecordQueryIntersectionPlan); !ok {
		t.Fatalf("inner = %T, want *RecordQueryIntersectionPlan", wrap.GetInner())
	}
}

// TestImplementFilterRule_FiresOverPhysicalUnion pins that
// ImplementFilterRule fires when the inner Reference contains a bare
// physical RecordQueryUnionPlan — closing the 7-wrapper symmetry gap left
// by physical Union not yet being one of the covered wrapper shapes.
//
// Filter(Union(Scan, Scan)) should physically implement to
// FilterPlan(UnionPlan(ScanPlan, ScanPlan)) once both scans + the
// union are physically implemented.
func TestImplementFilterRule_FiresOverPhysicalUnion(t *testing.T) {
	t.Parallel()
	scanA := implementFilterScan("A")
	scanB := implementFilterScan("B")
	refA := expressions.InitialOf(scanA)
	refB := expressions.InitialOf(scanB)
	union := mustImplementFilterConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	}))
	unionRef := expressions.InitialOf(union)
	pred := predicates.NewConstantPredicate(predicates.TriFalse)
	filter := mustImplementFilterConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(unionRef),
	))
	topRef := expressions.InitialOf(filter)

	// Step 1: Implement both scans.
	mustFireExpressionRule(t, NewPrimaryScanRule(), refA)
	mustFireExpressionRule(t, NewPrimaryScanRule(), refB)
	// Step 2: put a PHYSICAL union in the inner reference, built directly.
	// No rule implements a bare UNION ALL as RecordQueryUnionPlan any more
	// (see BatchAExpressionRules), and this test is about the FILTER's
	// wrapper coverage, not about how the union got there.
	physicalUnion := mustImplementFilterConstruct(plans.NewRecordQueryUnionPlanFromQuantifiers(
		[]expressions.Quantifier{
			expressions.NewPhysicalQuantifier(refA),
			expressions.NewPhysicalQuantifier(refB),
		}))
	if !unionRef.InsertFinal(physicalUnion) {
		t.Fatal("inserting the physical union into its reference did not take")
	}
	// Step 3: Now Filter's inner Reference has a bare RecordQueryUnionPlan.
	yielded := mustFireExpressionRule(t, NewImplementFilterRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementFilterRule yielded %d, want 1 (Filter over physical Union)", len(yielded))
	}
	wrap, ok := yielded[0].(*plans.RecordQueryPredicatesFilterPlan)
	if !ok {
		t.Fatalf("yield = %T, want *plans.RecordQueryPredicatesFilterPlan", yielded[0])
	}
	innerPlan := wrap.GetInner()
	if _, ok := innerPlan.(*plans.RecordQueryUnionPlan); !ok {
		t.Fatalf("filter inner plan = %T, want *RecordQueryUnionPlan", innerPlan)
	}
}

func TestPlannerWithBatchA_ImplementsFilterOverScan(t *testing.T) {
	t.Parallel()
	// End-to-end through the task-stack Planner: Filter(P, Scan) with
	// PrimaryScanRule + ImplementFilterRule as PLANNING rules yields a
	// Reference holding a physical FilterPlan-over-ScanPlan member
	// alongside the logical shapes.
	pred := predicates.NewConstantPredicate(predicates.TriFalse)
	scan := implementFilterScan("Order")
	filter := mustImplementFilterConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	))
	ref := expressions.InitialOf(filter)

	// Physical wrappers are produced during PLANNING in the production
	// pipeline, so register the Batch A rules as planning rules.
	p := NewPlanner(DefaultExpressionRules(), nil).
		WithPlanningExpressionRules([]ExpressionRule{
			NewPrimaryScanRule(),
			NewImplementFilterRule(),
		})
	if _, _, err := p.Plan(ref); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	foundPhysFilter := false
	for _, m := range ref.AllMembers() {
		if _, ok := m.(*plans.RecordQueryPredicatesFilterPlan); ok {
			foundPhysFilter = true
			break
		}
	}
	if !foundPhysFilter {
		t.Fatalf("planner did not produce a physical PredicatesFilterPlan wrapper after rules; %d members", len(ref.AllMembers()))
	}
}
