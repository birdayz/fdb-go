package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestOptimizeInputsCompletesOnlySettledSingletonBatches(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"settled", "fresh", "exploring", "rearmed", "canonical", "exploratory_member", "second_final", "rewriting", "pending_group", "pending_consumption", "pending_transform", "pending_inputs", "pending_optimizer", "pending_expression", "unfinished_sibling"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			scan := pushFetchScan()
			child := expressions.FinalOf(scan)
			child.ConstraintsMap().SetExplored()
			p := NewPlanner(nil, EmptyPlanContext())
			p.constraintMap = NewConstraintMap()
			switch mode {
			case "fresh":
				child = expressions.FinalOf(scan)
			case "exploring":
				child = expressions.FinalOf(scan)
				child.StartExploration()
			case "rearmed":
				child.ConstraintsMap().ReArm()
			case "canonical":
				child = expressions.FinalOfAtStage(scan, expressions.StageCanonical)
				child.ConstraintsMap().SetExplored()
			case "exploratory_member":
				child.Insert(scan)
			case "second_final":
				child.InsertFinal(pushFetchIndex("alternative"))
			case "pending_group":
				p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: child})
			case "pending_consumption":
				p.queueDataAccessTask(child)
			case "pending_transform":
				p.push(&TransformExprTask{
					Phase: PhasePlanning, Ref: child, Expr: scan,
					Rule: probeRule(NewExpressionMatcher[*plans.RecordQueryScanPlan]("scan")),
				})
			case "pending_inputs":
				// A queued sibling prepass is input work even for this leaf.
				p.stack = append(p.stack, &OptimizeInputsTask{Phase: PhasePlanning, Ref: child, Expr: scan})
			case "pending_optimizer":
				p.push(&OptimizeGroupTask{Phase: PhasePlanning, Ref: child})
			case "pending_expression":
				p.planningExpressionRules = []ExpressionRule{probeRule(NewExpressionMatcher[*plans.RecordQueryScanPlan]("scan"))}
				p.push(&ExploreExprTask{Phase: PhasePlanning, Ref: child, Expr: scan})
			}
			var parent expressions.RelationalExpression = mustPushFetchConstruct(plans.NewRecordQueryLimitPlanFromQuantifier(
				expressions.NewPhysicalQuantifier(child), 1, 0, nil))
			if mode == "unfinished_sibling" {
				parent = mustPushFetchConstruct(plans.NewRecordQueryUnorderedUnionPlanFromQuantifiers([]expressions.Quantifier{
					expressions.NewPhysicalQuantifier(child),
					expressions.NewPhysicalQuantifier(expressions.FinalOf(pushFetchScan())),
				}))
			}
			ref := expressions.FinalOf(parent)
			phase := PhasePlanning
			if mode == "rewriting" {
				phase = PhaseRewriting
			}
			for range 2 {
				(&OptimizeInputsTask{Phase: phase, Ref: ref, Expr: parent}).Run(t.Context(), p)
				if p.capErr != nil {
					t.Fatal(p.capErr)
				}
				optimizers := 0
				for _, task := range p.stack {
					if optimize, ok := task.(*OptimizeGroupTask); ok && optimize.Ref.Canonical() == child.Canonical() {
						optimizers++
					}
				}
				if mode != "settled" {
					if optimizers != 1 {
						t.Fatalf("unfinished child retained %d optimizers, want 1", optimizers)
					}
					return
				}
				if optimizers != 0 || len(p.stack) != 0 {
					t.Fatalf("settled singleton queued %d tasks (%d optimizers)", len(p.stack), optimizers)
				}
				if child.Winner() != scan || child.IsPinnedFinal() {
					t.Fatal("singleton completion lost the winner or permanently pinned an ordinary group")
				}
				props := GetRefPlanPropertiesMap(child)
				if props == nil || props.GetProperties(scan) == nil {
					t.Fatal("singleton completion did not publish plan properties")
				}
			}
		})
	}
}

func TestPlannerCompletesRulelessInputClosureBeforeRuleAdmission(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"leaf", "nested", "covering", "candidate", "custom_rule", "preorder", "pending_child", "pending_descendant", "exploring", "canonical", "two_finals", "exploratory_member", "cycle", "unknown_task"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			var leaf plans.RecordQueryPlan = pushFetchScan()
			if mode == "covering" {
				leaf = mustPushFetchConstruct(plans.NewRecordQueryCoveringIndexPlan(pushFetchIndex("covering")))
			}
			leafRef := expressions.FinalOf(leaf)
			child := leafRef
			if mode == "nested" || mode == "pending_descendant" {
				child = expressions.FinalOf(mustPushFetchConstruct(plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(
					expressions.NewPhysicalQuantifier(leafRef), nil)))
			}
			parent := mustPushFetchConstruct(plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(
				expressions.NewPhysicalQuantifier(child), nil))
			ref := expressions.FinalOf(parent)
			var candidates []MatchCandidate
			if mode == "candidate" {
				candidates = []MatchCandidate{&testMatchCandidate{name: "physical", traversal: NewTraversal(expressions.FinalOf(pushFetchScan()))}}
			}
			p := NewPlanner(nil, testPlanContextForMatching{candidates: candidates})
			p.constraintMap = NewConstraintMap()
			p.planningExpressionRules = []ExpressionRule{NewMatchLeafRule(), NewMatchIntermediateRule()}
			p.implementationRules = []ImplementationRule{NewPushFilterThroughFetchRule(), NewMergeFetchIntoCoveringIndexRule()}
			switch mode {
			case "custom_rule":
				p.planningExpressionRules = append(p.planningExpressionRules, probeRule(NewExpressionMatcher[*plans.RecordQueryScanPlan]("scan")))
			case "preorder":
				p.implementationRules = append(p.implementationRules, &growInputRule{
					indexProbeRule: probeRule(NewExpressionMatcher[*plans.RecordQueryPredicatesFilterPlan]("filter")),
					grow:           func() {}, preorder: true,
				})
			case "pending_child":
				p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: child})
			case "pending_descendant":
				p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: leafRef})
			case "exploring":
				child.StartExploration()
			case "canonical":
				child.AdvanceStagePreservingMembers(expressions.StageCanonical)
			case "two_finals":
				child.InsertFinal(pushFetchIndex("other"))
			case "exploratory_member":
				child.Insert(leaf)
			case "cycle":
				child.PruneWith(parent)
			case "unknown_task":
				p.push(&InitiatePlannerPhaseTask{Phase: PhasePlanning, RootRef: ref})
			}
			p.push(&OptimizeInputsTask{Phase: PhasePlanning, Ref: ref, Expr: parent})
			(&ExploreExprTask{Phase: PhasePlanning, Ref: ref, Expr: parent}).Run(t.Context(), p)
			if p.capErr != nil {
				t.Fatal(p.capErr)
			}
			complete := mode == "leaf" || mode == "nested" || mode == "covering"
			if !complete {
				if child.ConstraintsMap().IsExplored() && !child.ConstraintsMap().IsExploring() {
					t.Fatal("unfinished or matchable child was completed prematurely")
				}
				if len(p.stack) <= 1 {
					t.Fatal("unfinished child lost its exploration")
				}
				return
			}
			if len(p.stack) != 1 {
				t.Fatalf("ruleless closure left %d tasks, want only parent input validation", len(p.stack))
			}
			for _, completed := range []*expressions.Reference{leafRef, child} {
				if !completed.ConstraintsMap().IsExplored() || completed.IsPinnedFinal() {
					t.Fatal("ruleless closure remained unexplored or was permanently pinned")
				}
				if props := GetRefPlanPropertiesMap(completed); props == nil || props.GetProperties(completed.FinalMembers()[0]) == nil {
					t.Fatal("ruleless closure did not publish physical properties")
				}
			}
			if child.Winner() != nil {
				t.Fatal("input closure completion selected the outer winner before input optimization")
			}
			p.pop().Run(t.Context(), p)
			if p.capErr != nil || len(p.stack) != 0 || child.Winner() != child.FinalMembers()[0] {
				t.Fatalf("parent input validation did not finish: tasks=%d error=%v", len(p.stack), p.capErr)
			}
		})
	}
}

func TestRulelessInputClosureRequiresWholeSiblingBatch(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"siblings", "shared_descendant", "repeated_input", "settled_sibling", "active_sibling", "pending_sibling", "preorder", "rewriting"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			leaf := expressions.FinalOf(pushFetchScan())
			left := expressions.FinalOf(mustPushFetchConstruct(plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(
				expressions.NewPhysicalQuantifier(leaf), nil)))
			rightLeaf := expressions.FinalOf(pushFetchIndex("right"))
			if mode == "shared_descendant" {
				rightLeaf = leaf
			}
			right := expressions.FinalOf(mustPushFetchConstruct(plans.NewRecordQueryLimitPlanFromQuantifier(
				expressions.NewPhysicalQuantifier(rightLeaf), 1, 0, nil)))
			if mode == "settled_sibling" {
				rightLeaf.ConstraintsMap().SetExplored()
				right.ConstraintsMap().SetExplored()
			}
			qs := []expressions.Quantifier{expressions.NewPhysicalQuantifier(left), expressions.NewPhysicalQuantifier(right)}
			if mode == "repeated_input" {
				qs = append(qs, expressions.NewPhysicalQuantifier(left))
			}
			parent := mustPushFetchConstruct(plans.NewRecordQueryUnorderedUnionPlanFromQuantifiers(qs))
			ref := expressions.FinalOf(parent)
			p := NewPlanner(nil, EmptyPlanContext())
			p.constraintMap = NewConstraintMap()
			p.planningExpressionRules = []ExpressionRule{NewMatchLeafRule(), NewMatchIntermediateRule()}
			p.implementationRules = []ImplementationRule{NewPushFilterThroughFetchRule(), NewPushUnorderedUnionThroughFetchRule()}
			phase := PhasePlanning
			switch mode {
			case "active_sibling":
				p.planningExpressionRules = append(p.planningExpressionRules, probeRule(NewExpressionMatcher[*plans.RecordQueryLimitPlan]("right")))
			case "pending_sibling":
				p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: rightLeaf})
			case "preorder":
				p.implementationRules = append(p.implementationRules, &growInputRule{
					indexProbeRule: probeRule(NewExpressionMatcher[*plans.RecordQueryUnorderedUnionPlan]("union")), grow: func() {}, preorder: true,
				})
			case "rewriting":
				phase = PhaseRewriting
			}
			p.push(&OptimizeInputsTask{Phase: phase, Ref: ref, Expr: parent})
			(&ExploreExprTask{Phase: phase, Ref: ref, Expr: parent}).Run(t.Context(), p)
			if p.capErr != nil {
				t.Fatal(p.capErr)
			}
			complete := mode == "siblings" || mode == "shared_descendant" || mode == "repeated_input" || mode == "settled_sibling"
			if !complete {
				if left.ConstraintsMap().IsExplored() || leaf.ConstraintsMap().IsExplored() {
					t.Fatal("partial proof completed a sibling before the whole batch was known inert")
				}
				return
			}
			if len(p.stack) != 1 {
				t.Fatalf("ruleless sibling closure left %d tasks, want only parent input validation", len(p.stack))
			}
			for _, child := range []*expressions.Reference{left, right} {
				if !child.ConstraintsMap().IsExplored() || child.IsPinnedFinal() || child.Winner() != nil {
					t.Fatal("input closure did not complete exploration without selecting or pinning its inputs")
				}
			}
			p.pop().Run(t.Context(), p)
			if p.capErr != nil || len(p.stack) != 0 {
				t.Fatalf("parent input validation did not finish: tasks=%d error=%v", len(p.stack), p.capErr)
			}
			for _, child := range []*expressions.Reference{left, right} {
				if child.Winner() != child.FinalMembers()[0] {
					t.Fatal("parent optimization did not select the sole final")
				}
			}
		})
	}
}

func TestPlannerCompletesRulelessGroupsOverSettledInputs(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"ordinary", "pinned", "siblings", "fresh", "exploring", "rearmed", "canonical", "two_finals", "input_match", "custom_rule", "preorder", "pending_group", "pending_transform", "pending_consumption", "unfinished_sibling", "rewriting"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			var input plans.RecordQueryPlan = pushFetchScan()
			if mode == "input_match" {
				input = pushFetchFetch(pushFetchIndex("fetch"), nil)
			}
			child := expressions.FinalOf(input)
			if mode == "pinned" {
				child = expressions.PinnedFinalOf(input)
			}
			child.ConstraintsMap().SetExplored()
			p := NewPlanner(nil, EmptyPlanContext())
			p.constraintMap = NewConstraintMap()
			p.planningExpressionRules = []ExpressionRule{NewMatchLeafRule(), NewMatchIntermediateRule()}
			p.implementationRules = []ImplementationRule{NewPushFilterThroughFetchRule()}
			probe := probeRule(NewExpressionMatcher[*plans.RecordQueryPredicatesFilterPlan]("filter"))
			switch mode {
			case "fresh":
				child = expressions.FinalOf(input)
			case "exploring":
				child = expressions.FinalOf(input)
				child.StartExploration()
			case "rearmed":
				child.ConstraintsMap().ReArm()
			case "canonical":
				child = expressions.FinalOfAtStage(input, expressions.StageCanonical)
				child.ConstraintsMap().SetExplored()
			case "two_finals":
				child.InsertFinal(pushFetchIndex("alternative"))
			case "custom_rule":
				p.planningExpressionRules = append(p.planningExpressionRules, probe)
			case "preorder":
				p.implementationRules = append(p.implementationRules, &growInputRule{indexProbeRule: probe, grow: func() {}, preorder: true})
			case "pending_group":
				p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: child})
			case "pending_transform":
				p.push(&TransformExprTask{Phase: PhasePlanning, Ref: child, Expr: input, Rule: probe})
			case "pending_consumption":
				p.queueDataAccessTask(child)
			}
			parent := mustPushFetchConstruct(plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(
				expressions.NewPhysicalQuantifier(child), nil))
			ref := expressions.FinalOf(parent)
			if mode == "siblings" || mode == "unfinished_sibling" {
				other := child
				if mode == "unfinished_sibling" {
					other = expressions.FinalOf(pushFetchScan())
				}
				ref.InsertFinal(mustPushFetchConstruct(plans.NewRecordQueryLimitPlanFromQuantifier(
					expressions.NewPhysicalQuantifier(other), 1, 0, nil)))
			}
			phase := PhasePlanning
			if mode == "rewriting" {
				phase = PhaseRewriting
				ref = expressions.FinalOfAtStage(parent, expressions.StageCanonical)
			}
			(&ExploreGroupTask{Phase: phase, Ref: ref}).Run(t.Context(), p)
			if p.capErr != nil {
				t.Fatal(p.capErr)
			}
			idle := mode == "ordinary" || mode == "pinned" || mode == "siblings"
			if !idle {
				if len(p.stack) == 0 {
					t.Fatal("unfinished group lost its exploration or input validation")
				}
				return
			}
			if len(p.stack) != 0 {
				t.Fatalf("ruleless settled group scheduled %d tasks", len(p.stack))
			}
			if !ref.ConstraintsMap().IsExplored() || ref.ConstraintsMap().IsExploring() || child.Winner() != input {
				t.Fatal("ruleless group did not complete exploration and input selection")
			}
			if child.IsPinnedFinal() != (mode == "pinned") {
				t.Fatal("group completion changed the child's pinning policy")
			}
			for _, member := range ref.FinalMembers() {
				props := GetRefPlanPropertiesMap(ref)
				if props == nil || props.GetProperties(member) == nil {
					t.Fatalf("completed parent %T has no properties", member)
				}
			}
			if requirements, ok := Get(p.constraintMap, child, OrdinalLayoutConstraintKey); !ok || len(requirements) == 0 {
				t.Fatal("completed parent did not push its ordinal input requirements")
			}
		})
	}
}

func TestRulelessGroupCompletionRetainsIntermediateMatches(t *testing.T) {
	t.Parallel()
	leaf, candidateLeaf := pushFetchScan(), pushFetchScan()
	child, candidateChild := expressions.FinalOf(leaf), expressions.FinalOf(candidateLeaf)
	child.ConstraintsMap().SetExplored()
	parent := mustPushFetchConstruct(plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(
		expressions.NewPhysicalQuantifier(child), nil))
	candidateParent := mustPushFetchConstruct(plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(
		expressions.NewPhysicalQuantifier(candidateChild), nil))
	candidate := &testMatchCandidate{name: "external", traversal: NewTraversal(expressions.FinalOf(candidateParent))}
	matches := matchLeafWithCandidate(leaf, candidateLeaf)
	if len(matches) != 1 {
		t.Fatalf("child fixture yielded %d matches", len(matches))
	}
	match := matches[0]
	AddPartialMatchForCandidate(child, candidate, NewPartialMatch(match.boundAliasMap, candidate, child, leaf, candidateChild, match.matchInfo))
	ref := expressions.FinalOf(parent)
	p := NewPlanner(nil, EmptyPlanContext())
	p.constraintMap = NewConstraintMap()
	p.planningExpressionRules = []ExpressionRule{NewMatchLeafRule(), NewMatchIntermediateRule()}
	p.implementationRules = []ImplementationRule{NewPushFilterThroughFetchRule()}
	(&ExploreGroupTask{Phase: PhasePlanning, Ref: ref}).Run(t.Context(), p)
	for len(p.stack) > 0 && len(ref.GetAllPartialMatches()) == 0 {
		p.pop().Run(t.Context(), p)
		if p.capErr != nil {
			t.Fatal(p.capErr)
		}
	}
	if got := len(GetPartialMatchesForCandidate(ref, candidate)); got != 1 {
		t.Fatalf("ruleless completion lost the real parent match: got %d", got)
	}
}

func TestPlannerSkipsRulelessExplorationOverCompletedPins(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"ruleless", "ordinary", "fresh_pin", "preorder", "input_match", "custom_rule", "pending_group", "rewriting"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			var input plans.RecordQueryPlan = pushFetchScan()
			if mode == "input_match" {
				input = pushFetchFetch(pushFetchIndex("fetch"), nil)
			}
			child := expressions.PinnedFinalOf(input)
			if mode == "ordinary" {
				child = expressions.FinalOf(input)
			}
			if mode != "fresh_pin" {
				child.ConstraintsMap().SetExplored()
			}
			parent := mustPushFetchConstruct(plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(
				expressions.NewPhysicalQuantifier(child), nil))
			ref := expressions.FinalOf(parent)
			p := NewPlanner(nil, EmptyPlanContext())
			p.planningExpressionRules = []ExpressionRule{NewMatchLeafRule(), NewMatchIntermediateRule()}
			p.implementationRules = []ImplementationRule{NewPushFilterThroughFetchRule()}
			probe := probeRule(NewExpressionMatcher[*plans.RecordQueryPredicatesFilterPlan]("filter"))
			switch mode {
			case "preorder":
				p.implementationRules = append(p.implementationRules, &growInputRule{indexProbeRule: probe, grow: func() {}, preorder: true})
			case "custom_rule":
				p.planningExpressionRules = append(p.planningExpressionRules, probe)
			case "pending_group":
				p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: child})
			}
			phase := PhasePlanning
			if mode == "rewriting" {
				phase = PhaseRewriting
			}
			p.push(&ExploreExprTask{Phase: phase, Ref: ref, Expr: parent})
			queued := 0
			for _, task := range p.stack {
				if _, ok := task.(*ExploreExprTask); ok {
					queued++
				}
			}
			want := 1
			if mode == "ruleless" {
				want = 0
			}
			if queued != want {
				t.Fatalf("queued %d explorations, want %d", queued, want)
			}
		})
	}
}

func TestPlannerSkipsRulelessLeafExploration(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"ruleless", "typed_rule", "unknown_rule", "candidate", "partial_match", "nonleaf", "rewriting"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			leaf := mustOrderedScanPlan(t, []string{"T"}, false)
			var expr expressions.RelationalExpression = leaf
			if mode == "nonleaf" {
				parent, err := plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(leaf)
				expr = mustConstruct(t, parent, err)
			}
			ref := expressions.FinalOf(expr)
			candidateLeaf := mustOrderedScanPlan(t, []string{"T"}, false)
			candidateRef := expressions.FinalOf(candidateLeaf)
			candidate := &testMatchCandidate{name: "physical", traversal: NewTraversal(candidateRef)}
			var candidates []MatchCandidate
			if mode == "candidate" {
				candidates = []MatchCandidate{candidate}
			}
			if mode == "partial_match" {
				matches := matchLeafWithCandidate(leaf, candidateLeaf)
				if len(matches) != 1 {
					t.Fatalf("partial match fixture produced %d matches", len(matches))
				}
				match := matches[0]
				AddPartialMatchForCandidate(ref, candidate, NewPartialMatch(match.boundAliasMap, candidate, ref, leaf, candidateRef, match.matchInfo))
			}
			p := NewPlanner(nil, testPlanContextForMatching{candidates: candidates})
			p.planningExpressionRules = []ExpressionRule{NewMatchLeafRule()}
			switch mode {
			case "typed_rule":
				p.planningExpressionRules = append(p.planningExpressionRules, probeRule(NewExpressionMatcher[*plans.RecordQueryScanPlan]("scan")))
			case "unknown_rule":
				p.planningExpressionRules = append(p.planningExpressionRules, probeRule(NewExpressionMatcher[expressions.RelationalExpression]("any")))
			}
			phase := PhasePlanning
			if mode == "rewriting" {
				phase = PhaseRewriting
			}
			p.push(&ExploreExprTask{Phase: phase, Ref: ref, Expr: expr})
			want := 1
			if mode == "ruleless" {
				want = 0
			}
			if len(p.stack) != want {
				t.Fatalf("queued %d expression explorations, want %d", len(p.stack), want)
			}
			if mode == "partial_match" {
				p.pop().Run(t.Context(), p)
				consumption := 0
				for _, task := range p.stack {
					if _, ok := task.(*ConsumeMatchPartitionTask); ok {
						consumption++
					}
				}
				if consumption != 1 {
					t.Fatalf("retained partial match scheduled %d consumption tasks, want 1", consumption)
				}
			}
		})
	}
}

func TestPlannerCompletesRulelessLeafGroup(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"leaf", "two_leaves", "candidate", "nonleaf", "invalid_leaf"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			leaf := mustOrderedScanPlan(t, []string{"T"}, false)
			ref := expressions.FinalOf(leaf)
			if mode == "two_leaves" {
				ref.InsertFinal(mustOrderedScanPlan(t, []string{"U"}, false))
			}
			if mode == "nonleaf" || mode == "invalid_leaf" {
				limit, err := plans.NewRecordQueryLimitPlan(leaf, 1, 0)
				limit = mustConstruct(t, limit, err)
				if mode == "nonleaf" {
					ref.InsertFinal(limit)
				} else {
					ref.InsertFinal(&inputlessOrdinalExpression{limit})
				}
			}
			var candidates []MatchCandidate
			if mode == "candidate" {
				candidates = []MatchCandidate{&testMatchCandidate{name: "physical", traversal: NewTraversal(expressions.FinalOf(leaf))}}
			}
			p := NewPlanner(nil, testPlanContextForMatching{candidates: candidates})
			p.constraintMap = NewConstraintMap()
			if mode != "invalid_leaf" {
				p.planningExpressionRules = []ExpressionRule{NewMatchLeafRule()}
			}
			(&ExploreGroupTask{Phase: PhasePlanning, Ref: ref}).Run(t.Context(), p)
			idle := mode == "leaf" || mode == "two_leaves"
			if idle {
				if len(p.stack) != 0 {
					t.Fatalf("ruleless leaf group queued %d continuation tasks", len(p.stack))
				}
				if ref.NeedsExploration() || !ref.ConstraintsMap().IsExplored() || ref.ConstraintsMap().IsExploring() {
					t.Fatal("ruleless leaf group did not finish its exploration epoch")
				}
				props := GetRefPlanPropertiesMap(ref)
				for _, member := range ref.FinalMembers() {
					if props == nil || props.GetProperties(member) == nil {
						t.Fatalf("completed leaf %T has no physical properties", member)
					}
				}
			} else if len(p.stack) == 0 {
				t.Fatal("unfinished matching, child exploration, or ordinal validation was discarded")
			}
			if mode == "invalid_leaf" {
				for len(p.stack) > 0 && p.capErr == nil {
					p.pop().Run(t.Context(), p)
				}
				if p.capErr == nil {
					t.Fatal("malformed leaf requirements were not rejected")
				}
			}
		})
	}
}

func TestPlannerCoalescesOnlyPendingExplorationTasks(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("T", false, nil)
	first, err := expressions.NewFullUnorderedScanExpression([]string{"T"}, rowType)
	if err != nil {
		t.Fatalf("construct first scan: %v", err)
	}
	second, err := expressions.NewFullUnorderedScanExpression([]string{"U"}, rowType)
	if err != nil {
		t.Fatalf("construct second scan: %v", err)
	}
	ref := expressions.InitialOf(first)
	candidate := &testMatchCandidate{name: "scan", traversal: NewTraversal(expressions.InitialOf(first))}
	p := NewPlanner(nil, testPlanContextForMatching{candidates: []MatchCandidate{candidate}})
	p.planningExpressionRules = []ExpressionRule{NewMatchLeafRule()}

	p.push(&ExploreGroupTask{Phase: PhaseRewriting, Ref: ref})
	p.push(&ExploreGroupTask{Phase: PhaseRewriting, Ref: ref})
	if got := len(p.stack); got != 1 {
		t.Fatalf("duplicate pending group task grew the stack to %d", got)
	}

	// Phase is part of the work identity: planning must not hide behind a
	// pending rewriting task for the same memo group.
	p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: ref})
	if got := len(p.stack); got != 2 {
		t.Fatalf("distinct-phase group task was coalesced: stack=%d", got)
	}
	if got := p.pop(); got.(*ExploreGroupTask).Phase != PhasePlanning {
		t.Fatalf("planner lost LIFO order: popped %#v", got)
	}

	// A task stops being pending when popped. Scheduling the same work again
	// is therefore legal: it may observe members or constraints produced by
	// the task that just ran.
	p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: ref})
	if got := len(p.stack); got != 2 {
		t.Fatalf("popped group task remained permanently suppressed: stack=%d", got)
	}
	for len(p.stack) > 0 {
		p.pop()
	}

	p.push(&OptimizeGroupTask{Phase: PhaseRewriting, Ref: ref})
	p.push(&OptimizeGroupTask{Phase: PhaseRewriting, Ref: ref})
	p.push(&OptimizeGroupTask{Phase: PhasePlanning, Ref: ref})
	if got := len(p.stack); got != 2 {
		t.Fatalf("pending optimizer identity lost phase/group semantics: stack=%d", got)
	}
	p.pop()
	p.push(&OptimizeGroupTask{Phase: PhasePlanning, Ref: ref})
	if got := len(p.stack); got != 2 {
		t.Fatalf("popped optimizer remained permanently suppressed: stack=%d", got)
	}
	for len(p.stack) > 0 {
		p.pop()
	}

	p.push(&ExploreExprTask{Phase: PhaseRewriting, Ref: ref, Expr: first})
	p.push(&ExploreExprTask{Phase: PhaseRewriting, Ref: ref, Expr: first})
	if got := len(p.stack); got != 1 {
		t.Fatalf("duplicate pending expression task grew the stack to %d", got)
	}

	p.push(&ExploreExprTask{Phase: PhaseRewriting, Ref: ref, Expr: second})
	p.push(&ExploreExprTask{Phase: PhasePlanning, Ref: ref, Expr: first})
	if got := len(p.stack); got != 3 {
		t.Fatalf("expression or phase discriminator was lost: stack=%d", got)
	}

	for len(p.stack) > 0 {
		p.pop()
	}

	// Memo integration can forward a group while its exploration task waits
	// on the stack. The survivor must still coalesce with that pending work,
	// and popping the loser task must release the original pending key.
	loser := expressions.InitialOf(first)
	survivor := expressions.InitialOf(first)
	p.push(&ExploreGroupTask{Phase: PhaseRewriting, Ref: loser})
	survivor.Absorb(loser)
	p.push(&ExploreGroupTask{Phase: PhaseRewriting, Ref: survivor})
	if got := len(p.stack); got != 1 {
		t.Fatalf("forwarded group gained duplicate pending work: stack=%d", got)
	}
	p.pop()
	p.push(&ExploreGroupTask{Phase: PhaseRewriting, Ref: survivor})
	if got := len(p.stack); got != 1 {
		t.Fatalf("forwarded task left its captured pending key behind: stack=%d", got)
	}
	p.pop()

	optLoser := expressions.InitialOf(first)
	optSurvivor := expressions.InitialOf(first)
	p.push(&OptimizeGroupTask{Phase: PhasePlanning, Ref: optLoser})
	optSurvivor.Absorb(optLoser)
	p.push(&OptimizeGroupTask{Phase: PhasePlanning, Ref: optSurvivor})
	if got := len(p.stack); got != 1 {
		t.Fatalf("forwarded group gained duplicate pending optimization: stack=%d", got)
	}
	p.pop()
	p.push(&OptimizeGroupTask{Phase: PhasePlanning, Ref: optSurvivor})
	if got := len(p.stack); got != 1 {
		t.Fatalf("forwarded optimizer left its captured pending key behind: stack=%d", got)
	}
	p.pop()

	exprLoser := expressions.InitialOf(first)
	exprSurvivor := expressions.InitialOf(first)
	p.push(&ExploreExprTask{Phase: PhaseRewriting, Ref: exprLoser, Expr: first})
	exprSurvivor.Absorb(exprLoser)
	p.push(&ExploreExprTask{Phase: PhaseRewriting, Ref: exprSurvivor, Expr: first})
	if got := len(p.stack); got != 1 {
		t.Fatalf("forwarded expression gained duplicate pending work: stack=%d", got)
	}
	p.pop()
	p.push(&ExploreExprTask{Phase: PhaseRewriting, Ref: exprSurvivor, Expr: first})
	if got := len(p.stack); got != 1 {
		t.Fatalf("forwarded expression task left its captured key behind: stack=%d", got)
	}
	p.pop()
	if len(p.pendingExploreGroups) != 0 || len(p.pendingExploreExprs) != 0 || len(p.pendingOptimizeGroups) != 0 {
		t.Fatalf("drained stack retained pending work: groups=%d expressions=%d optimizers=%d",
			len(p.pendingExploreGroups), len(p.pendingExploreExprs), len(p.pendingOptimizeGroups))
	}
}

func TestPlannerDoesNotCoalescePostTransitionContinuationWithForwardedPendingTask(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("T", false, nil)
	scan, err := expressions.NewFullUnorderedScanExpression([]string{"T"}, rowType)
	if err != nil {
		t.Fatalf("construct scan: %v", err)
	}
	loser := expressions.InitialOf(scan)
	survivor := expressions.InitialOf(scan)
	p := NewPlanner(nil, EmptyPlanContext())

	// These are distinct groups when queued, so both tasks are pending. Memo
	// integration can forward them while they wait on the LIFO stack.
	p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: loser})
	p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: survivor})
	if got := len(p.stack); got != 2 {
		t.Fatalf("distinct groups did not queue distinct tasks: stack=%d", got)
	}
	survivor.Absorb(loser)

	// The top task starts the canonical group's planning-stage round. Its
	// continuation must sit above the stale pre-transition task; otherwise a
	// parent optimizer between them can run before this round converges.
	p.pop()
	survivor.AdvanceStagePreservingMembers(expressions.StagePlanned)
	p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: survivor})
	if got := len(p.stack); got != 2 {
		t.Fatalf("post-transition continuation coalesced with buried pre-transition task: stack=%d", got)
	}
}

func TestPlannerExploresPinnedChildOnlyWhenNeeded(t *testing.T) {
	t.Parallel()

	for _, state := range []string{"fresh", "retained", "constraint growth"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			scan := mkEnumIndexPlan("idx_a")
			child := expressions.PinnedFinalOf(scan)
			planner := NewPlanner(nil, EmptyPlanContext())
			planner.constraintMap = NewConstraintMap()
			wantExplores := 1
			if state != "fresh" {
				source := expressions.FinalOf(scan)
				source.ConstraintsMap().SetExplored()
				child = newRestrictedFinalReference("test", source,
					[]expressions.RelationalExpression{scan}, expressions.StagePlanned)
				wantExplores = 0
				if props := GetRefPlanPropertiesMap(child); props == nil || props.GetProperties(scan) == nil {
					t.Fatal("retained final has no properties before parent exploration")
				}
			}
			if state == "constraint growth" {
				Set(planner.constraintMap, child, &PlannerConstraint[int]{name: "ordinary"}, 1)
				wantExplores = 1
			}
			filter, err := plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(
				expressions.NewPhysicalQuantifier(child),
				[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)})
			if err != nil {
				t.Fatal(err)
			}
			parent := expressions.FinalOf(filter)
			planner.push(&OptimizeInputsTask{Phase: PhasePlanning, Ref: parent, Expr: filter})
			planner.push(&ExploreExprTask{Phase: PhasePlanning, Ref: parent, Expr: filter})
			explores, optimizations := 0, 0
			for len(planner.stack) > 0 {
				task := planner.pop()
				switch typed := task.(type) {
				case *ExploreGroupTask:
					if typed.Ref == child {
						explores++
					}
				case *OptimizeGroupTask:
					if typed.Ref == child {
						optimizations++
					}
				}
				task.Run(t.Context(), planner)
				if planner.capErr != nil {
					t.Fatal(planner.capErr)
				}
			}
			if explores != wantExplores || optimizations != 0 {
				t.Fatalf("child tasks: explores=%d optimizations=%d, want %d and 0",
					explores, optimizations, wantExplores)
			}
			if child.Winner() != scan || len(child.FinalMembers()) != 1 || child.NeedsExploration() {
				t.Fatal("pinned child lost its selected final or remained unexplored")
			}
		})
	}
}

func TestOptimizeInputsPinnedCompletionValidatesAllRequirements(t *testing.T) {
	t.Parallel()
	for _, from := range []string{"constraint", "sibling"} {
		t.Run(from, func(t *testing.T) {
			t.Parallel()
			_, source, layoutA, _ := ordinalLayoutSelectionFixture(t)
			child := expressions.PinnedFinalOf(compatiblePhysicalMember(t, source, layoutA))
			parentA, err := plans.NewRecordQueryLimitPlanFromQuantifier(expressions.NewPhysicalQuantifier(child), 1, 0, nil)
			parentA = mustConstruct(t, parentA, err)
			child.PruneWith(source.Winner())
			child.ConstraintsMap().SetExplored()
			parentB, err := plans.NewRecordQueryLimitPlanFromQuantifier(expressions.NewPhysicalQuantifier(child), 2, 0, nil)
			parentB = mustConstruct(t, parentB, err)
			parentRef := expressions.FinalOf(parentB)
			p := NewPlanner(nil, nil)
			p.constraintMap = NewConstraintMap()
			if from == "sibling" {
				if !parentRef.InsertFinal(parentA) {
					t.Fatal("fixture lost the sibling parent")
				}
			} else {
				requirements, err := ordinalInputRequirementsOf(parentA)
				if err != nil || len(requirements) != 1 {
					t.Fatalf("requirements: %v, %v", requirements, err)
				}
				Set(p.constraintMap, child, OrdinalLayoutConstraintKey, requirements)
			}
			(&OptimizeInputsTask{Phase: PhasePlanning, Ref: parentRef, Expr: parentB}).Run(t.Context(), p)
			if p.capErr == nil {
				t.Fatal("pinned completion did not reject the incompatible retained layout")
			}
			if len(p.stack) != 0 {
				t.Fatal("invalid pin scheduled work after failure")
			}
		})
	}
}

func TestPlannerExploredPinStillValidatesNewOrdinalRequirements(t *testing.T) {
	t.Parallel()
	parent, source, _, _ := ordinalLayoutSelectionFixture(t)
	requirements, err := ordinalInputRequirementsOf(parent)
	if err != nil {
		t.Fatal(err)
	}
	child := newRestrictedFinalReference("test", source,
		[]expressions.RelationalExpression{source.Winner()}, expressions.StagePlanned)
	planner := NewPlanner(nil, EmptyPlanContext())
	planner.constraintMap = NewConstraintMap()
	Set(planner.constraintMap, child, OrdinalLayoutConstraintKey, requirements)
	if child.NeedsExploration() {
		t.Fatal("optimizer-only layout requirement restarted exploration")
	}
	planner.push(&OptimizeGroupTask{Phase: PhasePlanning, Ref: child})
	planner.scheduleExploreGroupsBeforeBatch(PhasePlanning, []*expressions.Reference{child}, 0)
	if len(planner.stack) != 1 {
		t.Fatalf("explored pin queued %d tasks, want only optimization", len(planner.stack))
	}
	planner.pop().Run(t.Context(), planner)
	if planner.capErr == nil {
		t.Fatal("optimizer accepted an incompatible retained final")
	}
}

func TestPlannerMovesPendingExploreGroupAboveLIFOBarrier(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("T", false, nil)
	scan, err := expressions.NewFullUnorderedScanExpression([]string{"T"}, rowType)
	if err != nil {
		t.Fatalf("construct scan: %v", err)
	}
	child := expressions.InitialOf(scan)
	parent := expressions.InitialOf(scan)
	p := NewPlanner(nil, EmptyPlanContext())

	// The child exploration is already pending below a newly pushed parent
	// optimizer. Reusing it is safe only if the dependent batch moves behind it;
	// otherwise the parent observes an unimplemented child (the nested-UNION
	// fuzz regression).
	p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: child})
	dependentFloor := len(p.stack)
	p.push(&OptimizeGroupTask{Phase: PhasePlanning, Ref: parent})
	p.scheduleExploreGroupsBeforeBatch(PhasePlanning, []*expressions.Reference{child}, dependentFloor)
	if got := len(p.stack); got != 2 {
		t.Fatalf("dependency scheduling duplicated work: stack=%d, want 2", got)
	}
	if top, ok := p.stack[len(p.stack)-1].(*ExploreGroupTask); !ok || top.Ref.Canonical() != child.Canonical() {
		t.Fatalf("pending child exploration is not next to run: top=%T", p.stack[len(p.stack)-1])
	}
}
