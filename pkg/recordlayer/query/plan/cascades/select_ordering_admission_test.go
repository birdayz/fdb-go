package cascades

import (
	"fmt"
	"maps"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

type selectOrderingWithoutAdmission struct {
	ImplementationRule
	preOrderMarker
}

func TestSelectOrderingAdmissionPreservesJoinExploration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		build   func() expressions.RelationalExpression
		savings int
	}{
		// The SELECT ordering rule declares its constraint (WS-F D2), so the
		// unadmitted baseline re-fires it less on re-exploration: 21→15,
		// 124→85, 145→93.
		{"chain3", func() expressions.RelationalExpression { return buildOrdinalChainSelect(t, 3) }, 15},
		{"chain4", func() expressions.RelationalExpression { return buildOrdinalChainSelect(t, 4) }, 85},
		{"star3", func() expressions.RelationalExpression { return buildOrdinalStar(t, 3) }, 93},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			type census struct {
				tasks, propagation, groups int
				otherTasks, members        map[string]int
				planHash                   uint64
			}
			run := func(admission bool) census {
				p := fullChainPlanner()
				if !admission {
					rules := append([]ImplementationRule(nil), p.implementationRules...)
					wrapped := false
					for i, rule := range rules {
						if _, ok := rule.(*PushRequestedOrderingThroughSelectRule); ok {
							rules[i] = &selectOrderingWithoutAdmission{ImplementationRule: rule}
							wrapped = true
						}
					}
					if !wrapped {
						t.Fatal("control did not disable SELECT ordering admission")
					}
					p.WithImplementationRules(rules)
				}
				got := census{otherTasks: make(map[string]int), members: make(map[string]int)}
				p.WithTaskObserver(func(task Task) {
					key := fmt.Sprintf("%T", task)
					switch task := task.(type) {
					case *TransformImplTask:
						switch task.Rule.(type) {
						case *PushRequestedOrderingThroughSelectRule, *selectOrderingWithoutAdmission:
							got.propagation++
							return
						}
						key += fmt.Sprintf("/%T", task.Rule)
					case *TransformExprTask:
						key += fmt.Sprintf("/%T", task.Rule)
					}
					got.otherTasks[key]++
				})
				expr, tasks, err := p.Plan(expressions.InitialOf(tc.build()))
				if err != nil {
					t.Fatal(err)
				}
				got.tasks = tasks
				pg, ok := expr.(interface{ GetRecordQueryPlan() plans.RecordQueryPlan })
				if !ok {
					t.Fatalf("planned expression %T has no physical plan", expr)
				}
				got.planHash = plans.PlanHash(pg.GetRecordQueryPlan())
				for ref := range p.Memo().References() {
					got.groups++
					for _, member := range ref.Members() {
						got.members[fmt.Sprintf("%d/exploratory/%T", ref.Stage(), member)]++
					}
					for _, member := range ref.FinalMembers() {
						got.members[fmt.Sprintf("%d/final/%T", ref.Stage(), member)]++
					}
				}
				return got
			}
			before, after := run(false), run(true)
			if before.tasks-after.tasks != tc.savings || before.propagation-after.propagation != tc.savings {
				t.Fatalf("task savings=%d, SELECT propagation savings=%d, want %d", before.tasks-after.tasks, before.propagation-after.propagation, tc.savings)
			}
			if !maps.Equal(before.otherTasks, after.otherTasks) {
				t.Fatalf("non-SELECT tasks changed: before=%v after=%v", before.otherTasks, after.otherTasks)
			}
			if before.groups == 0 || len(before.members) == 0 || before.groups != after.groups || !maps.Equal(before.members, after.members) {
				t.Fatalf("memo population changed: groups=%d/%d members=%v/%v", before.groups, after.groups, before.members, after.members)
			}
			if before.planHash != after.planHash {
				t.Fatalf("selected plan changed: hash=%d/%d", before.planHash, after.planHash)
			}
		})
	}
}

func TestSelectOrderingAdmissionChecksEveryDestination(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"all_subsumed", "second_missing", "same_ref", "noncontributing", "existential_first", "only_existential", "missing_source", "nil_first"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			left := requestedOrderingQuantifier("L", "left")
			right := requestedOrderingQuantifier("R", "right")
			if mode == "same_ref" {
				right = expressions.RebuildQuantifier(right, left.GetRangesOver())
			}
			exists := expressions.ExistentialQuantifier(expressions.InitialOf(requestedOrderingScan("E")))
			result := values.NewRecordConstructorValue(
				values.RecordConstructorField{Name: "L_ID", Value: requestedOrderingField(left, "ID")},
				values.RecordConstructorField{Name: "R_ID", Value: requestedOrderingField(right, "ID")})
			quantifiers := []expressions.Quantifier{left, right}
			if mode == "existential_first" {
				quantifiers = append([]expressions.Quantifier{exists}, quantifiers...)
			} else if mode == "only_existential" {
				quantifiers = []expressions.Quantifier{exists}
			} else if mode == "nil_first" {
				quantifiers[0] = expressions.RebuildQuantifier(left, nil)
			}
			sel := mustRequestedOrderingConstruct(expressions.NewSelectExpression(result, quantifiers, nil))
			ref := expressions.InitialOf(sel)
			constraints := NewConstraintMap()
			childOrdering := func(q expressions.Quantifier, direction properties.RequestedSortOrder) []*properties.RequestedOrdering {
				return []*properties.RequestedOrdering{properties.NewRequestedOrdering([]properties.RequestedOrderingPart{{
					Value: requestedOrderingCurrentField(q, "ID"), SortOrder: direction,
				}}, properties.DistinctnessPreserveDistinctness, false)}
			}
			leftOrdering := childOrdering(left, properties.RequestedSortOrderAscending)
			rightOrdering := childOrdering(right, properties.RequestedSortOrderDescending)
			if mode != "missing_source" {
				Set(constraints, left.GetRangesOver(), RequestedOrderingConstraintKey, leftOrdering)
				parts := []properties.RequestedOrderingPart{{Value: requestedOrderingCurrentOutputField(result, 0), SortOrder: properties.RequestedSortOrderAscending}}
				if mode != "noncontributing" && mode != "nil_first" {
					parts = append(parts, properties.RequestedOrderingPart{Value: requestedOrderingCurrentOutputField(result, 1), SortOrder: properties.RequestedSortOrderDescending})
				}
				Set(constraints, ref, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{
					properties.NewRequestedOrdering(parts, properties.DistinctnessNotDistinct, false),
				})
			}
			if mode == "all_subsumed" {
				Set(constraints, right.GetRangesOver(), RequestedOrderingConstraintKey, rightOrdering)
			}
			rule := NewPushRequestedOrderingThroughSelectRule()
			wantEffect := mode == "second_missing" || mode == "same_ref" || mode == "existential_first" || mode == "missing_source"
			leftTick, rightTick := left.GetRangesOver().ConstraintsMap().CurrentTick(), right.GetRangesOver().ConstraintsMap().CurrentTick()
			if effect := rule.hasConstraintEffect(constraints, ref, sel); effect != wantEffect {
				t.Fatalf("constraint effect=%t, want %t", effect, wantEffect)
			}
			if left.GetRangesOver().ConstraintsMap().CurrentTick() != leftTick || right.GetRangesOver().ConstraintsMap().CurrentTick() != rightTick {
				t.Fatal("admission changed child constraints")
			}
			fireConstraintOnlyRule(t, rule, sel, ref, constraints)
			if _, present := Get(constraints, exists.GetRangesOver(), RequestedOrderingConstraintKey); present {
				t.Fatal("SELECT ForEach ordering rule pushed to existential child")
			}
			if mode == "missing_source" {
				pushed, present := Get(constraints, left.GetRangesOver(), RequestedOrderingConstraintKey)
				if !present || len(pushed) != 0 {
					t.Fatal("missing source must still install an empty constraint on the first ForEach")
				}
			}
			pushed, present := Get(constraints, right.GetRangesOver(), RequestedOrderingConstraintKey)
			wantRight := mode == "all_subsumed" || mode == "second_missing" || mode == "same_ref" || mode == "existential_first"
			if present != wantRight {
				t.Fatalf("right child constraint present=%t, want %t", present, wantRight)
			}
			if wantRight {
				index := 0
				if mode == "same_ref" {
					index = 1
				}
				if len(pushed) != index+1 || pushed[index].IsPreserve() || len(pushed[index].GetParts()) != 1 || pushed[index].GetParts()[0].SortOrder != properties.RequestedSortOrderDescending {
					t.Fatalf("right child lost its concrete descending request: %v", pushed)
				}
				assertRequestedOrderingField(t, pushed[index].GetParts()[0].Value, requestedOrderingCurrentField(right, "ID"))
			}
		})
	}
}

func TestSelectOrderingAdmissionSkipsSubsumedTranslatedPush(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"subsumed", "missing", "additional", "exhaustive", "empty_missing", "empty_present", "earlier_growth", "later_growth", "pending_child", "translation_error"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			q := requestedOrderingQuantifier("T", "select_child")
			child := q.GetRangesOver()
			rv := values.NewRecordConstructorValue(values.RecordConstructorField{Name: "RENAMED", Value: requestedOrderingField(q, "ID")})
			first := mustRequestedOrderingConstruct(expressions.NewSelectExpression(rv, []expressions.Quantifier{q}, nil))
			firstRef := expressions.InitialOf(first)
			if mode == "translation_error" {
				child = &expressions.Reference{}
				q = expressions.RebuildQuantifier(q, child)
			}
			second := mustRequestedOrderingConstruct(expressions.NewSelectExpression(rv, []expressions.Quantifier{q}, []predicates.QueryPredicate{
				predicates.NewComparisonPredicate(rv.Fields[0].Value, predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(0))),
			}))
			secondRef := expressions.InitialOf(second)
			request := func(reverse, exhaustive bool) []*properties.RequestedOrdering {
				direction := properties.RequestedSortOrderAscending
				if reverse {
					direction = properties.RequestedSortOrderDescending
				}
				return []*properties.RequestedOrdering{properties.NewRequestedOrdering([]properties.RequestedOrderingPart{{
					Value: requestedOrderingCurrentOutputField(rv, 0), SortOrder: direction,
				}}, properties.DistinctnessNotDistinct, exhaustive)}
			}
			p := NewPlanner(nil, EmptyPlanContext())
			p.constraintMap = NewConstraintMap()
			rule := NewPushRequestedOrderingThroughSelectRule()
			p.implementationRules = []ImplementationRule{rule}
			if mode != "missing" && mode != "empty_missing" && mode != "translation_error" {
				Set(p.constraintMap, firstRef, RequestedOrderingConstraintKey, request(false, false))
				if mode == "empty_present" {
					Set(p.constraintMap, child, RequestedOrderingConstraintKey, []*properties.RequestedOrdering(nil))
				} else {
					fireConstraintOnlyRule(t, rule, first, firstRef, p.constraintMap)
					pushed := requirePushedOrdering(t, p.constraintMap, child)
					if len(pushed.GetParts()) != 1 {
						t.Fatal("first SELECT did not install a concrete ordering")
					}
					assertRequestedOrderingField(t, pushed.GetParts()[0].Value, requestedOrderingCurrentField(q, "ID"))
				}
			}
			var source []*properties.RequestedOrdering
			if mode != "empty_missing" && mode != "empty_present" {
				source = request(mode == "additional", mode == "exhaustive")
			}
			Set(p.constraintMap, secondRef, RequestedOrderingConstraintKey, source)
			if mode == "earlier_growth" || mode == "later_growth" {
				growth := &growInputRule{indexProbeRule: probeRule(rule.Matcher()), preorder: true, grow: func() {
					Set(p.constraintMap, secondRef, RequestedOrderingConstraintKey, request(true, false))
				}}
				if mode == "earlier_growth" {
					p.implementationRules = []ImplementationRule{growth, rule}
				} else {
					p.implementationRules = []ImplementationRule{rule, growth}
				}
			}
			var pending *ExploreGroupTask
			if mode == "pending_child" {
				child.ConstraintsMap().SetExplored()
				pending = &ExploreGroupTask{Phase: PhasePlanning, Ref: child}
				p.push(pending)
			}
			tick := child.ConstraintsMap().CurrentTick()
			(&ExploreExprTask{Phase: PhasePlanning, Ref: secondRef, Expr: second}).Run(t.Context(), p)
			if child.ConstraintsMap().CurrentTick() != tick {
				t.Fatal("admission changed the destination constraint")
			}
			var transform *TransformImplTask
			pendingFound := false
			for _, task := range p.stack {
				pendingFound = pendingFound || task == pending
				if candidate, ok := task.(*TransformImplTask); ok && candidate.Rule == rule {
					if transform != nil {
						t.Fatal("SELECT propagation was scheduled twice")
					}
					transform = candidate
				}
			}
			want := mode == "missing" || mode == "additional" || mode == "exhaustive" || mode == "empty_missing" || mode == "earlier_growth" || mode == "translation_error"
			if (transform != nil) != want {
				t.Fatalf("SELECT propagation queued=%v, want %v", transform != nil, want)
			}
			if pending != nil && !pendingFound {
				t.Fatal("omitting an inert propagation dropped pending child exploration")
			}
			if len(firstRef.AllMembers()) != 1 || firstRef.Get() != first || len(secondRef.AllMembers()) != 1 || secondRef.Get() != second {
				t.Fatal("admission changed SELECT alternatives")
			}
			if !want {
				return
			}
			for len(p.stack) > 0 {
				task := p.pop()
				task.Run(t.Context(), p)
				if task == transform {
					break
				}
			}
			if mode == "translation_error" {
				if p.capErr == nil {
					t.Fatal("translation failure was hidden by admission")
				}
				return
			}
			if p.capErr != nil {
				t.Fatal(p.capErr)
			}
			pushed, present := Get(p.constraintMap, child, RequestedOrderingConstraintKey)
			if !present || child.ConstraintsMap().CurrentTick() == tick {
				t.Fatal("effectful propagation did not grow the destination")
			}
			if mode == "exhaustive" && (len(pushed) != 2 || pushed[0].IsExhaustive() || !pushed[1].IsExhaustive()) {
				t.Fatal("propagation lost the exhaustive upgrade")
			}
		})
	}
}
