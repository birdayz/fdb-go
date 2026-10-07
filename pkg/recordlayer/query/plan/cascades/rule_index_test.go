package cascades

import (
	"fmt"
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// orderedScanTestRowType is the one exact descriptor shared by the ordered
// primary/index and bare-primary scan fixtures.  Each test selects the subset
// of columns it needs, while a common layout keeps query, candidate, and
// physical-plan roots type-identical.
func orderedScanTestRowType() *values.RecordType {
	address := values.NewRecordType("ORDER_ADDRESS", false, []values.Field{{
		Name:      "CITY",
		FieldType: values.NullableString,
		Ordinal:   0,
	}})
	return values.NewRecordType("ORDER_ROW", false, []values.Field{
		{Name: "TENANT_ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "STATUS", FieldType: values.NotNullString, Ordinal: 2},
		{Name: "DATE", FieldType: values.NotNullLong, Ordinal: 3},
		{Name: "AMOUNT", FieldType: values.NotNullLong, Ordinal: 4},
		{Name: "TAGS", FieldType: values.NewArrayType(true, values.NotNullString), Ordinal: 5},
		{Name: "CITY", FieldType: values.NullableString, Ordinal: 6},
		{Name: "ADDR", FieldType: address, Ordinal: 7},
	})
}

func mustOrderedScanFull(
	t testing.TB,
	recordTypes []string,
) *expressions.FullUnorderedScanExpression {
	t.Helper()
	scan, err := expressions.NewFullUnorderedScanExpression(recordTypes, orderedScanTestRowType())
	return mustConstruct(t, scan, err)
}

func mustOrderedScanInitial(
	t testing.TB,
	expression expressions.RelationalExpression,
) *expressions.Reference {
	t.Helper()
	reference, err := InitialOf(expression)
	return mustConstruct(t, reference, err)
}

func mustOrderedScanSort(
	t testing.TB,
	keys []expressions.SortKey,
	inner expressions.Quantifier,
) *expressions.LogicalSortExpression {
	t.Helper()
	sort, err := expressions.NewLogicalSortExpression(keys, inner)
	return mustConstruct(t, sort, err)
}

func mustOrderedScanFilter(
	t testing.TB,
	queryPredicates []predicates.QueryPredicate,
	inner expressions.Quantifier,
) *expressions.LogicalFilterExpression {
	t.Helper()
	filter, err := expressions.NewLogicalFilterExpression(queryPredicates, inner)
	return mustConstruct(t, filter, err)
}

func mustOrderedScanFlowed(
	t testing.TB,
	quantifier expressions.Quantifier,
) values.QuantifiedObjectValue {
	t.Helper()
	flowed, err := quantifier.RequireFlowedObjectValue()
	return mustConstruct(t, flowed, err)
}

func mustOrderedScanQOV(
	t testing.TB,
	alias values.CorrelationIdentifier,
) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(alias, orderedScanTestRowType())
	return mustConstruct(t, qov, err)
}

func mustOrderedScanField(
	t testing.TB,
	root values.Value,
	name string,
) values.Value {
	t.Helper()
	request, err := values.FieldByName(name)
	request = mustConstruct(t, request, err)
	field, err := values.ResolveFieldAccess(root, []values.FieldRequest{request})
	return mustConstruct(t, field, err)
}

func mustOrderedScanPlan(
	t testing.TB,
	recordTypes []string,
	reverse bool,
) *plans.RecordQueryScanPlan {
	t.Helper()
	plan, err := plans.NewRecordQueryScanPlan(recordTypes, orderedScanTestRowType(), reverse)
	return mustConstruct(t, plan, err)
}

type indexProbeRule struct {
	m matching.BindingMatcher
}

func (r *indexProbeRule) Matcher() matching.BindingMatcher { return r.m }
func (r *indexProbeRule) OnMatch(call *ExpressionRuleCall) {}
func (r *indexProbeRule) String() string                   { return "indexProbeRule" }
func probeRule(m matching.BindingMatcher) *indexProbeRule  { return &indexProbeRule{m: m} }

func TestRuleIndex_BucketsByRootOperator(t *testing.T) {
	t.Parallel()

	sortRule := probeRule(NewExpressionMatcher[*expressions.LogicalSortExpression]("s"))
	filterRule := probeRule(NewExpressionMatcher[*expressions.LogicalFilterExpression]("f"))
	// Interface-typed matcher — the match-anything shape MatchLeafRule /
	// MatchIntermediateRule use. It MUST land in the always bucket: bucketing
	// it under the interface type would key it where no concrete expression
	// ever looks, silently disabling the matching infrastructure (this
	// exact failure took out every data-access plan when the index first
	// went in — sorts stopped eliding because MatchLeafRule never fired).
	anyRule := probeRule(NewExpressionMatcher[expressions.RelationalExpression]("any"))

	ix := newRuleIndex([]ExpressionRule{sortRule, filterRule, anyRule})

	if len(ix.always) != 1 {
		t.Fatalf("interface-rooted rule must be an always rule; always=%d", len(ix.always))
	}
	if got := len(ix.byRoot[reflect.TypeFor[*expressions.LogicalSortExpression]()]); got != 1 {
		t.Fatalf("sort bucket size = %d, want 1", got)
	}

	sortScan := mustOrderedScanFull(t, []string{"T"})
	sortExpr := mustOrderedScanSort(t, nil, expressions.ForEachQuantifier(
		mustOrderedScanInitial(t, sortScan)))

	got := ix.rulesFor(sortExpr)
	if len(got) != 2 || got[0] != ExpressionRule(sortRule) || got[1] != ExpressionRule(anyRule) {
		t.Fatalf("rulesFor(sort) = %d rules, want [sortRule, anyRule] (indexed first, then always, registration order)", len(got))
	}
	for _, r := range got {
		if r == ExpressionRule(filterRule) {
			t.Fatal("a filter-rooted rule must not be offered to a sort expression")
		}
	}

	// A type with no indexed rules gets exactly the always bucket.
	scan := mustOrderedScanFull(t, []string{"T"})
	if got := ix.rulesFor(scan); len(got) != 1 || got[0] != ExpressionRule(anyRule) {
		t.Fatalf("rulesFor(scan) = %d rules, want just the always rule", len(got))
	}

	// The cache returns the identical slice on re-query.
	a := ix.rulesFor(sortExpr)
	b := ix.rulesFor(sortExpr)
	if &a[0] != &b[0] {
		t.Fatal("rulesFor must serve the cached list on re-query")
	}
}

func TestPreorderSchedulingRequiresLiveConstraintInput(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"unique_fields", "distinct_fields", "unique_order", "distinct_order", "filter_order"} {
		for _, state := range []string{"absent", "empty", "present", "subsumed", "empty_subsumed", "empty_child", "growing_child", "earlier", "later", "earlier_growth", "later_growth"} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				t.Parallel()
				child, q := referencedFieldsScanQ()
				var parent expressions.RelationalExpression
				var rule ImplementationRule
				fields := kind == "unique_fields" || kind == "distinct_fields"
				switch kind {
				case "unique_fields", "unique_order":
					parent = mustReferencedFieldsConstruct(expressions.NewLogicalUniqueExpression(q))
					if fields {
						rule = NewPushReferencedFieldsThroughUniqueRule()
					} else {
						rule = NewPushRequestedOrderingThroughUniqueRule()
					}
				case "distinct_fields", "distinct_order":
					parent = mustReferencedFieldsConstruct(expressions.NewLogicalDistinctExpression(q))
					if fields {
						rule = NewPushReferencedFieldsThroughDistinctRule()
					} else {
						rule = NewPushRequestedOrderingThroughDistinctRule()
					}
				case "filter_order":
					parent = mustReferencedFieldsConstruct(expressions.NewLogicalFilterExpression(nil, q))
					rule = NewPushRequestedOrderingThroughFilterRule()
				}
				ref := expressions.InitialOf(parent)
				p := NewPlanner(nil, EmptyPlanContext())
				p.constraintMap = NewConstraintMap()
				p.implementationRules = []ImplementationRule{rule}
				seedAt := func(ref *expressions.Reference, empty bool) {
					if fields {
						value := EmptyReferencedFields()
						if !empty {
							value = NewReferencedFields(map[string]struct{}{"PK": {}})
						}
						Set(p.constraintMap, ref, ReferencedFieldsConstraintKey, value)
					} else {
						var orderings []*properties.RequestedOrdering
						if !empty {
							orderings = []*properties.RequestedOrdering{properties.PreserveOrdering()}
						}
						Set(p.constraintMap, ref, RequestedOrderingConstraintKey, orderings)
					}
				}
				seed := func() { seedAt(ref, state == "empty" || state == "empty_subsumed") }
				if state != "absent" && state != "earlier" && state != "later" {
					seed()
				}
				if state == "subsumed" || state == "empty_subsumed" || state == "empty_child" {
					seedAt(child, state != "subsumed")
				}
				if state == "growing_child" {
					if fields {
						Set(p.constraintMap, child, ReferencedFieldsConstraintKey, NewReferencedFields(map[string]struct{}{"Y": {}}))
					} else {
						Set(p.constraintMap, child, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{
							properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
								{Value: constraintField("Y"), SortOrder: properties.RequestedSortOrderAscending},
							}, properties.DistinctnessNotDistinct, false),
						})
					}
				}
				if state == "earlier_growth" || state == "later_growth" {
					seedAt(ref, true)
					seedAt(child, false)
				}
				if state == "earlier" || state == "later" || state == "earlier_growth" || state == "later_growth" {
					grow := seed
					if state == "earlier_growth" || state == "later_growth" {
						grow = func() {
							if fields {
								Set(p.constraintMap, ref, ReferencedFieldsConstraintKey, NewReferencedFields(map[string]struct{}{"Y": {}}))
							} else {
								Set(p.constraintMap, ref, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{
									properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
										{Value: constraintField("Y"), SortOrder: properties.RequestedSortOrderAscending},
									}, properties.DistinctnessNotDistinct, false),
								})
							}
						}
					}
					growth := &growInputRule{indexProbeRule: probeRule(rule.Matcher()), grow: grow, preorder: true}
					if state == "earlier" || state == "earlier_growth" {
						p.implementationRules = append([]ImplementationRule{growth}, p.implementationRules...)
					} else {
						p.implementationRules = append(p.implementationRules, growth)
					}
				}
				tick := child.ConstraintsMap().CurrentTick()
				(&ExploreExprTask{Phase: PhasePlanning, Ref: ref, Expr: parent}).Run(t.Context(), p)
				if child.ConstraintsMap().CurrentTick() != tick {
					t.Fatal("admission mutated the child's constraints")
				}
				var transform *TransformImplTask
				for _, task := range p.stack {
					if candidate, ok := task.(*TransformImplTask); ok && candidate.Rule == rule {
						if transform != nil {
							t.Fatal("constraint propagation was queued twice")
						}
						transform = candidate
					}
				}
				want := state == "present" || state == "empty_child" || state == "growing_child" || state == "earlier" || state == "earlier_growth" || (state == "empty" && kind != "filter_order")
				if (transform != nil) != want {
					t.Fatalf("constraint propagation queued=%v, want %v", transform != nil, want)
				}
				if !want {
					return
				}
				for len(p.stack) > 0 {
					task := p.pop()
					task.Run(t.Context(), p)
					if p.capErr != nil {
						t.Fatal(p.capErr)
					}
					if task == transform {
						break
					}
				}
				if fields {
					got, ok := Get(p.constraintMap, child, ReferencedFieldsConstraintKey)
					source, _ := Get(p.constraintMap, ref, ReferencedFieldsConstraintKey)
					_, missing := CombineReferencedFields(got, source)
					if !ok || missing {
						t.Fatal("retained field propagation lost its live input")
					}
				} else {
					got, ok := Get(p.constraintMap, child, RequestedOrderingConstraintKey)
					source, _ := Get(p.constraintMap, ref, RequestedOrderingConstraintKey)
					_, missing := properties.CombineRequestedOrderings(got, source)
					if !ok || missing {
						t.Fatal("retained ordering propagation lost its live input")
					}
				}
			})
		}
	}
}

func TestPreorderAdmissionStopsAfterFirstActiveRule(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"both_absent", "fields_only", "ordering_only", "child_populates_parent"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			// The child as REWRITING leaves it: one final at the canonical stage,
			// which PLANNING crosses (Java's advancePlannerStage needs exactly one).
			child := expressions.FinalOfAtStage(mustReferencedFieldsConstruct(expressions.NewFullUnorderedScanExpression(
				[]string{"T"}, referencedFieldsRowType())), expressions.StageCanonical)
			q := expressions.ForEachQuantifier(child)
			parent := mustReferencedFieldsConstruct(expressions.NewLogicalUniqueExpression(q))
			ref := expressions.InitialOf(parent)
			p := NewPlanner(nil, EmptyPlanContext())
			p.constraintMap = NewConstraintMap()
			p.implementationRules = []ImplementationRule{NewPushRequestedOrderingThroughUniqueRule(), NewPushReferencedFieldsThroughUniqueRule()}
			fields := NewReferencedFields(map[string]struct{}{"PK": {}})
			if mode == "fields_only" {
				Set(p.constraintMap, ref, ReferencedFieldsConstraintKey, fields)
			}
			if mode == "ordering_only" || mode == "child_populates_parent" {
				Set(p.constraintMap, ref, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{properties.PreserveOrdering()})
			}
			if mode == "child_populates_parent" {
				p.planningExpressionRules = []ExpressionRule{&seedChildMatchExpressionRule{
					indexProbeRule: probeRule(NewExpressionMatcher[*expressions.FullUnorderedScanExpression]("child")),
					seed:           func() { Set(p.constraintMap, ref, ReferencedFieldsConstraintKey, fields) },
				}}
			}
			(&ExploreExprTask{Phase: PhasePlanning, Ref: ref, Expr: parent}).Run(t.Context(), p)
			queued := 0
			for _, task := range p.stack {
				if _, ok := task.(*TransformImplTask); ok {
					queued++
				}
			}
			want := 2
			if mode == "both_absent" {
				want = 0
			} else if mode == "fields_only" {
				want = 1
			}
			if queued != want {
				t.Fatalf("queued %d preorder tasks, want %d", queued, want)
			}
			for len(p.stack) > 0 {
				p.pop().Run(t.Context(), p)
				if p.capErr != nil {
					t.Fatal(p.capErr)
				}
			}
			_, pushedFields := Get(p.constraintMap, child, ReferencedFieldsConstraintKey)
			if wantFields := mode == "fields_only" || mode == "child_populates_parent"; pushedFields != wantFields {
				t.Fatalf("fields propagated=%v, want %v", pushedFields, wantFields)
			}
		})
	}
}

func TestMatchRuleSchedulingSeparatesLeaves(t *testing.T) {
	t.Parallel()
	for _, physical := range []bool{false, true} {
		t.Run(fmt.Sprintf("physical=%t", physical), func(t *testing.T) {
			t.Parallel()
			var leaf, parent expressions.RelationalExpression
			if physical {
				scan := mustOrderedScanPlan(t, []string{"T"}, false)
				distinct, err := plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(scan)
				if err != nil {
					t.Fatal(err)
				}
				leaf, parent = scan, distinct
			} else {
				leaf = mustOrderedScanFull(t, []string{"T"})
				parent = mustOrderedScanFilter(t, nil, expressions.ForEachQuantifier(expressions.InitialOf(leaf)))
			}
			leafRef := parent.GetQuantifiers()[0].GetRangesOver()
			parentRef := expressions.InitialOf(parent)
			candidate := &testMatchCandidate{name: "same_tree", traversal: NewTraversal(parentRef)}
			ctx := testPlanContextForMatching{candidates: []MatchCandidate{candidate}}
			leafRule, intermediateRule := NewMatchLeafRule(), NewMatchIntermediateRule()
			for _, expr := range []expressions.RelationalExpression{leaf, parent} {
				ref := leafRef
				if expr == parent {
					ref = parentRef
				}
				planner := NewPlanner(nil, ctx).
					WithPlanningExpressionRules([]ExpressionRule{leafRule, intermediateRule})
				(&ExploreExprTask{Phase: PhasePlanning, Ref: ref, Expr: expr}).Run(t.Context(), planner)
				counts := map[ExpressionRule]int{}
				for _, task := range append([]Task(nil), planner.stack...) {
					if transform, ok := task.(*TransformExprTask); ok {
						counts[transform.Rule]++
						transform.Run(t.Context(), planner)
					}
				}
				for _, rule := range []ExpressionRule{leafRule, intermediateRule} {
					want := (rule == leafRule) == (expr == leaf)
					if got := counts[rule]; (want && got != 1) || (!want && got != 0) {
						t.Errorf("%T on %T: queued %d tasks, want admitted=%t", rule, expr, got, want)
					}
					if got := len(rule.Matcher().BindMatches(matching.NewBindings(), expr)) != 0; got != want {
						t.Errorf("%T on %T: matcher admitted=%t, want %t", rule, expr, got, want)
					}
				}
				if got := len(GetPartialMatchesForCandidate(ref, candidate)); got == 0 {
					t.Errorf("%T produced no partial match", expr)
				}
			}
		})
	}
}

func TestCoalescedLeafInputPrepassRetainsPhysicalMatching(t *testing.T) {
	t.Parallel()
	leaf, candidateLeaf := pushFetchScan(), pushFetchScan()
	parent := mustPushFetchConstruct(plans.NewRecordQueryLimitPlan(leaf, 1, 0))
	ref := expressions.FinalOf(parent)
	ref.InsertFinal(leaf)
	candidate := &testMatchCandidate{name: "physical", traversal: NewTraversal(expressions.FinalOf(candidateLeaf))}
	p := NewPlanner(nil, testPlanContextForMatching{candidates: []MatchCandidate{candidate}})
	p.constraintMap = NewConstraintMap()
	p.planningExpressionRules = []ExpressionRule{NewMatchLeafRule()}
	p.push(&OptimizeInputsTask{Phase: PhasePlanning, Ref: ref, Expr: parent})
	p.push(&OptimizeInputsTask{Phase: PhasePlanning, Ref: ref, Expr: leaf})
	if len(p.stack) != 1 {
		t.Fatalf("adjacent input prepasses retained %d tasks, want 1", len(p.stack))
	}
	p.push(&ExploreExprTask{Phase: PhasePlanning, Ref: ref, Expr: leaf})
	if len(p.stack) != 2 {
		t.Fatal("coalesced input task suppressed physical leaf exploration")
	}
	for len(p.stack) > 1 && len(GetPartialMatchesForCandidate(ref, candidate)) == 0 {
		p.pop().Run(t.Context(), p)
		if p.capErr != nil {
			t.Fatal(p.capErr)
		}
	}
	if got := len(GetPartialMatchesForCandidate(ref, candidate)); got != 1 {
		t.Fatalf("physical leaf matching produced %d matches, want 1", got)
	}
	consumption := false
	for _, task := range p.stack {
		if consume, ok := task.(*ConsumeMatchPartitionTask); ok && consume.Ref == ref {
			consumption = true
		}
	}
	if !consumption {
		t.Fatal("physical leaf match lost its data-access consumption")
	}
}

type crossTypeLeafForScheduling struct {
	expressions.RelationalExpression
}

func (e *crossTypeLeafForScheduling) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	_, ok := other.(*plans.RecordQueryScanPlan)
	return ok
}

func TestMatchLeafSchedulingUsesCandidateOperators(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"logical_scan", "explode", "physical_scan", "index", "covering", "custom"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			makeLeaf := func() expressions.RelationalExpression {
				switch name {
				case "logical_scan":
					return mustOrderedScanFull(t, []string{"T"})
				case "explode":
					return mustMatchExplode(t, inExplodeList([]any{int64(1)}, values.NotNullLong))
				case "physical_scan", "custom":
					return mustOrderedScanPlan(t, []string{"T"}, false)
				default:
					index, err := plans.NewRecordQueryIndexPlan("idx", nil, []string{"T"}, orderedScanTestRowType(), false)
					if err != nil {
						t.Fatal(err)
					}
					if name == "index" {
						return index
					}
					covering, err := plans.NewRecordQueryCoveringIndexPlan(index)
					if err != nil {
						t.Fatal(err)
					}
					return covering
				}
			}
			candidateLeaf := makeLeaf()
			// The matching leaf lives in the final lane below a non-leaf root.
			candidateLeafRef := expressions.FinalOf(candidateLeaf)
			candidateRoot := mustOrderedScanFilter(t, nil, expressions.ForEachQuantifier(candidateLeafRef))
			candidate := &testMatchCandidate{name: "nested_leaf", traversal: NewTraversal(expressions.InitialOf(candidateRoot))}
			for _, populated := range []bool{false, true} {
				var candidates []MatchCandidate
				if populated {
					candidates = []MatchCandidate{candidate}
				}
				queryLeaf := makeLeaf()
				if name == "custom" {
					queryLeaf = &crossTypeLeafForScheduling{RelationalExpression: mustOrderedScanFull(t, []string{"T"})}
				}
				ref := expressions.InitialOf(queryLeaf)
				rule := NewMatchLeafRule()
				planner := NewPlanner(nil, testPlanContextForMatching{candidates: candidates}).
					WithPlanningExpressionRules([]ExpressionRule{rule})
				(&ExploreExprTask{Phase: PhasePlanning, Ref: ref, Expr: queryLeaf}).Run(t.Context(), planner)
				queued := 0
				for _, task := range append([]Task(nil), planner.stack...) {
					if transform, ok := task.(*TransformExprTask); ok && transform.Rule == rule {
						queued++
						transform.Run(t.Context(), planner)
					}
				}
				wantTask := populated || name == "custom"
				if (wantTask && queued != 1) || (!wantTask && queued != 0) {
					t.Errorf("populated=%t queued %d match tasks, want admitted=%t", populated, queued, wantTask)
				}
				if got := len(GetPartialMatchesForCandidate(ref, candidate)); (populated && got != 1) || (!populated && got != 0) {
					t.Errorf("populated=%t produced %d matches", populated, got)
				}
			}
		})
	}
}

func TestMatchLeafSchedulingOnlyFiltersOperators(t *testing.T) {
	t.Parallel()
	candidate := &testMatchCandidate{name: "logical", traversal: NewTraversal(expressions.InitialOf(mustOrderedScanFull(t, []string{"T"})))}
	rule := NewMatchLeafRule()
	planner := NewPlanner(nil, testPlanContextForMatching{candidates: []MatchCandidate{candidate}})
	planner.planningExpressionRules = []ExpressionRule{rule}
	for _, physical := range []bool{true, false} {
		var expr expressions.RelationalExpression = mustOrderedScanFull(t, []string{"OTHER"})
		if physical {
			expr = mustOrderedScanPlan(t, []string{"T"}, false)
		}
		ref := expressions.InitialOf(expr)
		planner.stack = nil
		(&ExploreExprTask{Phase: PhasePlanning, Ref: ref, Expr: expr}).Run(t.Context(), planner)
		queued := 0
		for _, task := range append([]Task(nil), planner.stack...) {
			if transform, ok := task.(*TransformExprTask); ok && transform.Rule == rule {
				queued++
				transform.Run(t.Context(), planner)
			}
		}
		if (physical && queued != 0) || (!physical && queued != 1) {
			t.Errorf("physical=%t queued %d tasks; only the operator should control admission", physical, queued)
		}
		if got := len(GetPartialMatchesForCandidate(ref, candidate)); got != 0 {
			t.Errorf("different operator or record types produced %d partial matches", got)
		}
	}
}

type seedChildMatchExpressionRule struct {
	*indexProbeRule
	seed func()
}

func (r *seedChildMatchExpressionRule) OnMatch(*ExpressionRuleCall) { r.seed() }

type seedChildMatchPreorderRule struct {
	*indexProbeRule
	seed func()
}

func (r *seedChildMatchPreorderRule) IsPreOrder() bool                { return true }
func (r *seedChildMatchPreorderRule) OnMatch(*ImplementationRuleCall) { r.seed() }

type crossTypeParentForScheduling struct {
	expressions.RelationalExpression
}

func (*crossTypeParentForScheduling) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	_, ok := other.(*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan)
	return ok
}

func TestIntermediateMatchSchedulingRetainsFutureChildMatches(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"no_matches", "preseeded", "unexplored", "canonical", "preorder", "expression_before", "pending_group", "pending_consumption", "nil_traversal", "candidate_root", "different_parent_operator", "custom_query", "pinned_preseeded"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			queryLeaf := mustOrderedScanPlan(t, []string{"T"}, false)
			child := expressions.FinalOf(queryLeaf)
			if mode == "pinned_preseeded" {
				child = expressions.PinnedFinalOf(queryLeaf)
			}
			child.ConstraintsMap().SetExplored()
			if mode == "unexplored" {
				child = expressions.FinalOf(queryLeaf)
			} else if mode == "canonical" {
				child = expressions.FinalOfAtStage(queryLeaf, expressions.StageCanonical)
				child.ConstraintsMap().SetExplored()
			}
			queryParent, err := plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlanFromQuantifier(expressions.NewPhysicalQuantifier(child))
			queryParent = mustConstruct(t, queryParent, err)
			var queryExpr expressions.RelationalExpression = queryParent
			if mode == "custom_query" {
				queryExpr = &crossTypeParentForScheduling{queryParent}
			}
			ref := expressions.FinalOf(queryExpr)
			candidateLeaf := mustOrderedScanPlan(t, []string{"T"}, false)
			candidateChild := expressions.FinalOf(candidateLeaf)
			candidateParent, err := plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlanFromQuantifier(expressions.NewPhysicalQuantifier(candidateChild))
			candidateParent = mustConstruct(t, candidateParent, err)
			candidateRoot := expressions.FinalOf(candidateParent)
			if mode == "candidate_root" {
				candidateRoot = candidateChild
			} else if mode == "different_parent_operator" {
				other, err := plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(expressions.NewPhysicalQuantifier(candidateChild), nil)
				candidateRoot = expressions.FinalOf(mustConstruct(t, other, err))
			}
			candidate := &testMatchCandidate{name: "external", traversal: NewTraversal(candidateRoot)}
			seed := func() {
				matches := matchLeafWithCandidate(queryLeaf, candidateLeaf)
				if len(matches) != 1 {
					t.Fatalf("child fixture yielded %d matches", len(matches))
				}
				match := matches[0]
				AddPartialMatchForCandidate(child, candidate, NewPartialMatch(match.boundAliasMap, candidate, child, queryLeaf, candidateChild, match.matchInfo))
			}
			if mode == "preseeded" || mode == "nil_traversal" || mode == "candidate_root" || mode == "different_parent_operator" || mode == "custom_query" || mode == "pinned_preseeded" {
				seed()
				if mode == "nil_traversal" {
					candidate.traversal = nil
				}
			}
			// The context intentionally does not declare this preseeded candidate.
			p := NewPlanner(nil, EmptyPlanContext())
			p.constraintMap = NewConstraintMap()
			rule := NewMatchIntermediateRule()
			p.planningExpressionRules = []ExpressionRule{NewMatchLeafRule(), rule}
			probe := probeRule(NewExpressionMatcher[*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan]("parent"))
			switch mode {
			case "unexplored":
				p.planningExpressionRules = append(p.planningExpressionRules, &seedChildMatchExpressionRule{
					indexProbeRule: probeRule(NewExpressionMatcher[*plans.RecordQueryScanPlan]("child")), seed: seed,
				})
			case "preorder":
				p.implementationRules = []ImplementationRule{&seedChildMatchPreorderRule{indexProbeRule: probe, seed: seed}}
			case "expression_before":
				p.planningExpressionRules = append([]ExpressionRule{&seedChildMatchExpressionRule{indexProbeRule: probe, seed: seed}}, p.planningExpressionRules...)
			case "pending_group":
				p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: child})
			case "pending_consumption":
				p.queueDataAccessTask(child)
			}
			exploration := &ExploreExprTask{Phase: PhasePlanning, Ref: ref, Expr: queryExpr}
			p.push(exploration)
			if len(p.stack) == 0 || p.pop() != exploration {
				t.Fatal("parent exploration discarded its real or future matches")
			}
			exploration.Run(t.Context(), p)
			var matchingTask *TransformExprTask
			for _, task := range p.stack {
				if transform, ok := task.(*TransformExprTask); ok && transform.Rule == rule {
					if matchingTask != nil {
						t.Fatal("intermediate matching was queued twice")
					}
					matchingTask = transform
				}
			}
			want := mode != "no_matches" && mode != "nil_traversal" && mode != "candidate_root" && mode != "different_parent_operator"
			if (matchingTask != nil) != want {
				t.Fatalf("matching task queued=%t, want %t", matchingTask != nil, want)
			}
			if !want {
				return
			}
			if mode == "unexplored" {
				for len(p.stack) != 0 {
					task := p.pop()
					if task == matchingTask {
						break
					}
					task.Run(t.Context(), p)
					if p.capErr != nil {
						t.Fatal(p.capErr)
					}
				}
			} else {
				seed()
			}
			matchingTask.Run(t.Context(), p)
			if p.capErr != nil {
				t.Fatal(p.capErr)
			}
			if got := len(GetPartialMatchesForCandidate(ref, candidate)); got != 1 {
				t.Fatalf("physical parent produced %d matches, want 1", got)
			}
		})
	}
}

func TestIntermediateOperatorAdmissionKeepsRealParentMatches(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"unique", "union", "filter", "fetch", "physical_union", "distinct"} {
		t.Run(shape, func(t *testing.T) {
			t.Parallel()
			build := func(child *expressions.Reference) expressions.RelationalExpression {
				q := expressions.NewPhysicalQuantifier(child)
				switch shape {
				case "unique":
					return mustPushFetchConstruct(expressions.NewLogicalUniqueExpression(q))
				case "union":
					return mustPushFetchConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{q}))
				case "filter":
					return mustPushFetchConstruct(plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(q, nil))
				case "fetch":
					return mustPushFetchConstruct(plans.NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(q, nil,
						pushFetchRowType(), plans.FetchIndexRecordsPrimaryKey))
				case "physical_union":
					return mustPushFetchConstruct(plans.NewRecordQueryUnorderedUnionPlanFromQuantifiers([]expressions.Quantifier{q}))
				default:
					return mustPushFetchConstruct(plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlanFromQuantifier(q))
				}
			}
			queryLeaf, candidateLeaf := pushFetchScan(), pushFetchScan()
			child, candidateChild := expressions.FinalOf(queryLeaf), expressions.FinalOf(candidateLeaf)
			child.ConstraintsMap().SetExplored()
			queryParent, candidateParent := build(child), build(candidateChild)
			ref := expressions.FinalOf(queryParent)
			candidate := &testMatchCandidate{name: shape, traversal: NewTraversal(expressions.FinalOf(candidateParent))}
			matches := matchLeafWithCandidate(queryLeaf, candidateLeaf)
			if len(matches) != 1 {
				t.Fatalf("child fixture produced %d matches", len(matches))
			}
			match := matches[0]
			AddPartialMatchForCandidate(child, candidate, NewPartialMatch(match.boundAliasMap, candidate, child, queryLeaf, candidateChild, match.matchInfo))
			p := NewPlanner(nil, EmptyPlanContext())
			rule := NewMatchIntermediateRule()
			p.planningExpressionRules = []ExpressionRule{rule}
			(&ExploreExprTask{Phase: PhasePlanning, Ref: ref, Expr: queryParent}).Run(t.Context(), p)
			var transform *TransformExprTask
			for _, task := range p.stack {
				if candidate, ok := task.(*TransformExprTask); ok && candidate.Rule == rule {
					transform = candidate
				}
			}
			if transform == nil {
				t.Fatal("matching operator was discarded")
			}
			transform.Run(t.Context(), p)
			if p.capErr != nil {
				t.Fatal(p.capErr)
			}
			if got := len(GetPartialMatchesForCandidate(ref, candidate)); got != 1 {
				t.Fatalf("parent produced %d matches, want 1", got)
			}
			if intermediateOperatorsMayMatch(queryParent, queryLeaf) {
				t.Fatal("different concrete operator was admitted")
			}
		})
	}
}

func TestFetchRuleSchedulingChecksSettledInputOperators(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"filter", "distinct", "fetch"} {
		t.Run(shape, func(t *testing.T) {
			t.Parallel()
			index := pushFetchIndex("input_pattern")
			child := expressions.FinalOf(index)
			child.ConstraintsMap().SetExplored()
			q := expressions.NewPhysicalQuantifier(child)
			var rule ImplementationRule
			var parent, matchingChild expressions.RelationalExpression
			switch shape {
			case "filter":
				rule = NewPushFilterThroughFetchRule()
				parent = mustPushFetchConstruct(plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(q, nil))
				matchingChild = pushFetchFetch(index, nil)
			case "distinct":
				rule = NewPushDistinctThroughFetchRule()
				parent = mustPushFetchConstruct(plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlanFromQuantifier(q))
				matchingChild = pushFetchFetch(index, nil)
			case "fetch":
				rule = NewMergeFetchIntoCoveringIndexRule()
				parent = mustPushFetchConstruct(plans.NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(q, nil,
					pushFetchRowType(), plans.FetchIndexRecordsPrimaryKey))
				matchingChild = mustPushFetchConstruct(plans.NewRecordQueryCoveringIndexPlan(index))
			}
			ref := expressions.FinalOf(parent)
			for _, positive := range []bool{false, true} {
				if positive {
					child.InsertFinal(matchingChild)
					child.ConstraintsMap().SetExplored()
				}
				bindings := rule.Matcher().BindMatches(matching.NewBindings(), parent)
				if (len(bindings) == 1) != positive {
					t.Errorf("matching input present=%t: bindings=%d", positive, len(bindings))
				}
				p := NewPlanner(nil, EmptyPlanContext())
				p.planningExpressionRules = []ExpressionRule{NewMatchLeafRule(), NewMatchIntermediateRule()}
				p.implementationRules = []ImplementationRule{rule}
				(&ExploreExprTask{Phase: PhasePlanning, Ref: ref, Expr: parent}).Run(t.Context(), p)
				queued := 0
				for _, task := range p.stack {
					if transform, ok := task.(*TransformImplTask); ok && transform.Rule == rule {
						queued++
					}
				}
				if (queued == 1) != positive {
					t.Errorf("matching input present=%t: queued=%d", positive, queued)
				}
			}
		})
	}
}

func TestUnorderedUnionFetchSchedulingRequiresTwoFetchLegs(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"no_fetch", "one_fetch", "two_in_one_leg", "two_legs", "three_legs", "forwarded_leg"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			var qs []expressions.Quantifier
			for i := range 3 {
				child := expressions.FinalOf(pushFetchIndex(fmt.Sprintf("leg%d", i)))
				if mode != "no_fetch" && (i == 0 || ((mode == "two_legs" || mode == "forwarded_leg") && i == 1) || mode == "three_legs") {
					child.InsertFinal(pushFetchFetch(pushFetchIndex(fmt.Sprintf("fetch%d", i)), nil))
				}
				if mode == "two_in_one_leg" && i == 0 {
					child.InsertFinal(pushFetchFetch(pushFetchIndex("second_fetch"), nil))
				}
				if mode == "forwarded_leg" && i == 0 {
					forwarded := expressions.FinalOf(pushFetchScan())
					child.Absorb(forwarded)
					child = forwarded
				}
				child.ConstraintsMap().SetExplored()
				qs = append(qs, expressions.NewPhysicalQuantifier(child))
			}
			parent := mustPushFetchConstruct(plans.NewRecordQueryUnorderedUnionPlanFromQuantifiers(qs))
			rule := NewPushUnorderedUnionThroughFetchRule()
			want := mode == "two_legs" || mode == "three_legs" || mode == "forwarded_leg"
			if got := len(rule.Matcher().BindMatches(matching.NewBindings(), parent)); (got != 0) != want {
				t.Errorf("bindings=%d, want admitted=%t", got, want)
			}
			p := NewPlanner(nil, EmptyPlanContext())
			p.constraintMap = NewConstraintMap()
			p.planningExpressionRules = []ExpressionRule{NewMatchLeafRule(), NewMatchIntermediateRule()}
			p.implementationRules = []ImplementationRule{rule}
			(&ExploreExprTask{Phase: PhasePlanning, Ref: expressions.FinalOf(parent), Expr: parent}).Run(t.Context(), p)
			queued := 0
			for _, task := range p.stack {
				if transform, ok := task.(*TransformImplTask); ok && transform.Rule == rule {
					queued++
				}
			}
			if (queued != 0) != want {
				t.Fatalf("queued=%d, want admitted=%t", queued, want)
			}
		})
	}
}

type growInputRule struct {
	*indexProbeRule
	grow     func()
	preorder bool
}

func (r *growInputRule) IsPreOrder() bool                { return r.preorder }
func (r *growInputRule) OnMatch(*ImplementationRuleCall) { r.grow() }

func TestInputPatternSchedulingRetainsFutureChildMembers(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"unexplored", "canonical", "preorder", "expression_before", "implementation_before", "pending_group", "pending_consumption"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			scan := pushFetchScan()
			child := expressions.FinalOf(scan)
			child.ConstraintsMap().SetExplored()
			if mode == "unexplored" {
				child = expressions.FinalOf(scan)
			} else if mode == "canonical" {
				child = expressions.FinalOfAtStage(scan, expressions.StageCanonical)
				child.ConstraintsMap().SetExplored()
			}
			parent := mustPushFetchConstruct(plans.NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(
				expressions.NewPhysicalQuantifier(child), nil, pushFetchRowType(), plans.FetchIndexRecordsPrimaryKey))
			ref := expressions.FinalOf(parent)
			index := pushFetchIndex("future_input")
			covering := mustPushFetchConstruct(plans.NewRecordQueryCoveringIndexPlan(index))
			grow := func() { child.InsertFinal(covering) }
			p := NewPlanner(nil, EmptyPlanContext())
			p.constraintMap = NewConstraintMap()
			rule := NewMergeFetchIntoCoveringIndexRule()
			p.planningExpressionRules = []ExpressionRule{NewMatchLeafRule(), NewMatchIntermediateRule()}
			p.implementationRules = []ImplementationRule{rule}
			probe := probeRule(NewExpressionMatcher[*plans.RecordQueryFetchFromPartialRecordPlan]("parent"))
			switch mode {
			case "unexplored":
				p.implementationRules = append(p.implementationRules, &growInputRule{
					indexProbeRule: probeRule(NewExpressionMatcher[*plans.RecordQueryScanPlan]("child")), grow: grow,
				})
			case "preorder", "implementation_before":
				p.implementationRules = append([]ImplementationRule{&growInputRule{
					indexProbeRule: probe, grow: grow, preorder: mode == "preorder",
				}}, p.implementationRules...)
			case "expression_before":
				p.planningExpressionRules = append([]ExpressionRule{&seedChildMatchExpressionRule{indexProbeRule: probe, seed: grow}}, p.planningExpressionRules...)
			case "pending_group":
				p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: child})
			case "pending_consumption":
				p.queueDataAccessTask(child)
			}
			(&ExploreExprTask{Phase: PhasePlanning, Ref: ref, Expr: parent}).Run(t.Context(), p)
			var transform *TransformImplTask
			for _, task := range p.stack {
				if candidate, ok := task.(*TransformImplTask); ok && candidate.Rule == rule {
					if transform != nil {
						t.Fatal("fetch rule queued twice")
					}
					transform = candidate
				}
			}
			if transform == nil {
				t.Fatal("fetch rule discarded before an earlier task could grow its child")
			}
			if mode == "unexplored" || mode == "preorder" || mode == "expression_before" || mode == "implementation_before" {
				for len(p.stack) != 0 {
					task := p.pop()
					if task == transform {
						break
					}
					task.Run(t.Context(), p)
					if p.capErr != nil {
						t.Fatal(p.capErr)
					}
				}
			} else {
				// Model the completion of the retained child batch.
				grow()
			}
			transform.Run(t.Context(), p)
			if p.capErr != nil {
				t.Fatal(p.capErr)
			}
			if !ref.ContainsExactly(index) {
				t.Fatal("retained fetch rule did not merge the new covering input")
			}
		})
	}
}

func TestUnorderedUnionFetchSchedulingRetainsFutureFetchLeg(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"child_rule", "preorder", "pending_child"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			left := expressions.FinalOf(pushFetchFetch(pushFetchIndex("left"), nil))
			scan := pushFetchScan()
			right := expressions.FinalOf(scan)
			left.ConstraintsMap().SetExplored()
			if mode != "child_rule" {
				right.ConstraintsMap().SetExplored()
			}
			parent := mustPushFetchConstruct(plans.NewRecordQueryUnorderedUnionPlanFromQuantifiers([]expressions.Quantifier{
				expressions.NewPhysicalQuantifier(left), expressions.NewPhysicalQuantifier(right),
			}))
			ref := expressions.FinalOf(parent)
			fetch := pushFetchFetch(pushFetchIndex("right"), nil)
			grow := func() { right.InsertFinal(fetch) }
			p := NewPlanner(nil, EmptyPlanContext())
			p.constraintMap = NewConstraintMap()
			rule := NewPushUnorderedUnionThroughFetchRule()
			p.implementationRules = []ImplementationRule{rule}
			switch mode {
			case "child_rule", "pending_child":
				p.implementationRules = append(p.implementationRules, &growInputRule{
					indexProbeRule: probeRule(NewExpressionMatcher[*plans.RecordQueryScanPlan]("child")), grow: grow,
				})
			case "preorder":
				p.implementationRules = append(p.implementationRules, &growInputRule{
					indexProbeRule: probeRule(NewExpressionMatcher[*plans.RecordQueryUnorderedUnionPlan]("parent")), grow: grow, preorder: true,
				})
			}
			if mode == "pending_child" {
				right.MarkForcedExploration(scan)
				right.ConstraintsMap().ReArm()
				p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: right})
			}
			(&ExploreExprTask{Phase: PhasePlanning, Ref: ref, Expr: parent}).Run(t.Context(), p)
			var transform *TransformImplTask
			for _, task := range p.stack {
				if candidate, ok := task.(*TransformImplTask); ok && candidate.Rule == rule {
					transform = candidate
				}
			}
			if transform == nil {
				t.Fatal("fetch rule discarded before the second fetch could be produced")
			}
			for len(p.stack) > 0 {
				task := p.pop()
				task.Run(t.Context(), p)
				if p.capErr != nil {
					t.Fatal(p.capErr)
				}
				if task == transform {
					break
				}
			}
			if !referenceHasMemberOfType[*plans.RecordQueryFetchFromPartialRecordPlan](ref) {
				t.Fatal("retained union transformation did not lift the two fetches")
			}
		})
	}
}

func TestRuleSchedulingChecksImmutableQuantifierAndPredicateKinds(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"ordinary", "null_on_empty", "existential", "left_outer", "in", "nested_in"} {
		t.Run(shape, func(t *testing.T) {
			t.Parallel()
			child := expressions.InitialOf(mustOrderedScanFull(t, []string{"T"}))
			qs := []expressions.Quantifier{expressions.ForEachQuantifier(child), expressions.ForEachQuantifier(child)}
			join := expressions.JoinInner
			switch shape {
			case "null_on_empty":
				qs[1] = expressions.ForEachNullOnEmptyQuantifier(child)
			case "existential":
				qs[1] = expressions.ExistentialQuantifier(child)
			case "left_outer":
				join = expressions.JoinLeftOuter
			}
			var preds []predicates.QueryPredicate
			if shape == "in" || shape == "nested_in" {
				in := predicates.NewComparisonPredicate(&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong},
					predicates.Comparison{Type: predicates.ComparisonIn, Operand: inExplodeList([]any{int64(1)}, values.NotNullLong)})
				preds = []predicates.QueryPredicate{in}
				if shape == "nested_in" {
					preds = []predicates.QueryPredicate{predicates.NewNot(in)}
				}
			}
			sel, err := expressions.NewSelectExpressionWithJoinType(mustOrderedScanFlowed(t, qs[0]), qs, preds, nil, join)
			if err != nil {
				t.Fatal(err)
			}
			filter := mustOrderedScanFilter(t, preds, qs[0])
			for _, tc := range []struct {
				rule matcherHaver
				expr expressions.RelationalExpression
				want bool
			}{
				{NewEliminateNullOnEmptyRule(), sel, shape == "null_on_empty"},
				{NewRewriteOuterJoinRule(), sel, shape == "left_outer"},
				{NewPushRequestedOrderingThroughSelectExistentialRule(), sel, shape == "existential"},
				{NewInComparisonToExplodeRule(), filter, shape == "in"},
			} {
				if got := len(tc.rule.Matcher().BindMatches(matching.NewBindings(), tc.expr)) != 0; got != tc.want {
					t.Errorf("%T admitted %s: got %t, want %t", tc.rule, shape, got, tc.want)
				}
				p := NewPlanner(nil, nil)
				switch rule := tc.rule.(type) {
				case ExpressionRule:
					p.planningExpressionRules = []ExpressionRule{rule}
				case ImplementationRule:
					p.implementationRules = []ImplementationRule{rule}
				}
				(&ExploreExprTask{Phase: PhasePlanning, Ref: expressions.InitialOf(tc.expr), Expr: tc.expr}).Run(t.Context(), p)
				queued := 0
				for _, task := range p.stack {
					switch task.(type) {
					case *TransformExprTask, *TransformImplTask:
						queued++
					}
				}
				if (tc.want && queued != 1) || (!tc.want && queued != 0) {
					t.Errorf("%T queued %d tasks on %s, want admitted=%t", tc.rule, queued, shape, tc.want)
				}
			}
		})
	}
}

func TestExploreExpressionRejectsImpossibleQuantifierArity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		rule     matcherHaver
		min, max int
	}{
		{NewPartitionBinarySelectRule(), 2, 2},
		{NewPartitionSelectRule(), 3, 3},
		{NewImplementNestedLoopJoinRule(), 2, 2},
		{NewImplementInJoinRule(), 2, 3},
		{NewImplementInUnionRule(), 2, 3},
		{NewSplitSelectExtractIndependentQuantifiersRule(), 2, 3},
		{NewPushRequestedOrderingThroughInLikeSelectRule(), 2, 3},
	} {
		for arity := 1; arity <= 3; arity++ {
			t.Run(fmt.Sprintf("%T/%d", tc.rule, arity), func(t *testing.T) {
				t.Parallel()
				var quantifiers []expressions.Quantifier
				for range arity {
					quantifiers = append(quantifiers, expressions.ForEachQuantifier(
						expressions.InitialOf(mustOrderedScanFull(t, []string{"T"})),
					))
				}
				selectExpr, err := expressions.NewSelectExpression(
					mustOrderedScanFlowed(t, quantifiers[0]), quantifiers, nil,
				)
				if err != nil {
					t.Fatal(err)
				}
				want := arity >= tc.min && arity <= tc.max
				if got := len(tc.rule.Matcher().BindMatches(matching.NewBindings(), selectExpr)) > 0; got != want {
					t.Errorf("root matcher admitted arity %d: got %t, want %t", arity, got, want)
				}
				planner := NewPlanner(nil, nil)
				switch rule := tc.rule.(type) {
				case ExpressionRule:
					planner.WithPlanningExpressionRules([]ExpressionRule{rule})
				case ImplementationRule:
					planner.WithImplementationRules([]ImplementationRule{rule})
				default:
					t.Fatalf("not a planner rule: %T", rule)
				}
				ref := expressions.InitialOf(selectExpr)
				(&ExploreExprTask{Phase: PhasePlanning, Ref: ref, Expr: selectExpr}).Run(t.Context(), planner)
				queued := 0
				for _, task := range planner.stack {
					switch task := task.(type) {
					case *TransformExprTask:
						if task.Rule == tc.rule {
							queued++
						}
					case *TransformImplTask:
						if task.Rule == tc.rule {
							queued++
						}
					}
				}
				if (queued == 1) != want || queued > 1 {
					t.Fatalf("queued %d transform tasks, want applicable=%t", queued, want)
				}
			})
		}
	}
}

// TestRuleIndex_ProductionRootsAreConcrete sweeps every production rule
// set: a matcher that declares a root operator must declare a CONCRETE
// type an expression can actually have — never an interface, which would
// index the rule where no lookup ever hits.
func TestRuleIndex_ProductionRootsAreConcrete(t *testing.T) {
	t.Parallel()

	checkMatcher := func(name string, m matching.BindingMatcher) {
		rm, ok := m.(matching.RootOperatorMatcher)
		if !ok {
			return // always rule — fine
		}
		root := rm.RootOperator()
		if root == nil {
			return // explicit always — fine
		}
		if root.Kind() == reflect.Interface {
			t.Errorf("%s: RootOperator returned interface %v — the rule would never be offered to any expression", name, root)
		}
	}
	for _, r := range DefaultExpressionRules() {
		checkMatcher(reflect.TypeOf(r).String(), r.Matcher())
	}
	for _, r := range BatchAExpressionRules() {
		checkMatcher(reflect.TypeOf(r).String(), r.Matcher())
	}
	for _, r := range PlanningExplorationRules() {
		checkMatcher(reflect.TypeOf(r).String(), r.Matcher())
	}
	for _, r := range DefaultImplementationRules() {
		checkMatcher(reflect.TypeOf(r).String(), r.Matcher())
	}
	// The REWRITING implementation rule set (FinalizeExpressionsRule et al)
	// is registered in NewPlanner, not a exported list — sweep it too.
	for _, r := range NewPlanner(nil, nil).rewritingImplRules {
		checkMatcher(reflect.TypeOf(r).String(), r.Matcher())
	}
}
