package cascades

import (
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"google.golang.org/protobuf/proto"
)

func TestImplementNestedLoopJoin_Fires(t *testing.T) {
	t.Parallel()

	// Build: Select([a.id = b.id], [Scan(A), Scan(B)])
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanARef := expressions.InitialOf(scanA)
	scanAQ := expressions.ForEachQuantifier(scanARef)

	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	scanBRef := expressions.InitialOf(scanB)
	scanBQ := expressions.ForEachQuantifier(scanBRef)

	joinPred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "a_id", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "b_id"),
	)

	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()),
		[]expressions.Quantifier{scanAQ, scanBQ},
		[]predicates.QueryPredicate{joinPred},
	)
	selRef := expressions.InitialOf(sel)

	// NLJ fires during PLANNING phase (ImplementationRule). Run Plan() to
	// trigger both EXPLORE and PLANNING; physical wrappers land in Members.
	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(selRef); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// After planning, the Select should have a physical NLJ member.
	foundNLJ := false
	for _, m := range selRef.AllMembers() {
		if IsPhysicalNestedLoopJoin(m) {
			foundNLJ = true
			break
		}
	}
	if !foundNLJ {
		t.Fatal("ImplementNestedLoopJoinRule didn't produce a physical NLJ member")
	}
}

func TestImplementNestedLoopJoin_DoesNotFireOnSingleQuantifier(t *testing.T) {
	t.Parallel()

	// Select with only 1 quantifier (not a join).
	scan := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()),
		[]expressions.Quantifier{scanQ},
		nil,
	)
	selRef := expressions.InitialOf(sel)

	results := FireExpressionRule(NewImplementNestedLoopJoinRule(), selRef)
	if len(results) != 0 {
		t.Fatal("ImplementNestedLoopJoinRule should NOT fire on single-quantifier Select")
	}
}

func TestImplementNestedLoopJoin_PlanOutput(t *testing.T) {
	t.Parallel()

	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanARef := expressions.InitialOf(scanA)
	scanAQ := expressions.ForEachQuantifier(scanARef)

	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	scanBRef := expressions.InitialOf(scanB)
	scanBQ := expressions.ForEachQuantifier(scanBRef)

	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()),
		[]expressions.Quantifier{scanAQ, scanBQ},
		nil,
	)
	selRef := expressions.InitialOf(sel)

	// Plan the join.
	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(selRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if !IsPhysicalNestedLoopJoin(plan) && !IsPhysicalFlatMap(plan) {
		t.Fatalf("expected NLJ or FlatMap plan, got %T", plan)
	}

	// Verify explain output.
	explain := ExplainPhysicalPlan(plan)
	if explain == "" {
		t.Fatal("ExplainPhysicalPlan returned empty")
	}
	t.Logf("NLJ Explain: %s", explain)
}

// TestImplementNestedLoopJoin_ExistsShortcutRejectsFanOutCandidate pins the
// raw correlated-index shortcut used by nested EXISTS. A composite
// (FK, TAGS FAN_OUT) index has no entry when TAGS is empty, even when FK
// matches, so using its flat first-column metadata as a correlated FK probe
// would turn a true EXISTS into false. The ordinary (FK, STATUS) index remains
// eligible and is selected after the fan-out candidate is rejected.
func TestImplementNestedLoopJoin_ExistsShortcutRejectsFanOutCandidate(t *testing.T) {
	t.Parallel()

	outerAlias := values.NamedCorrelationIdentifier("O")
	innerAlias := values.NamedCorrelationIdentifier("I")

	outerScan := expressions.NewFullUnorderedScanExpression([]string{"OUTER"}, values.UnknownType)
	outerRef := expressions.InitialOf(outerScan)
	outerRef.InsertFinal(plans.NewRecordQueryScanPlan([]string{"OUTER"}, values.UnknownType, false))

	innerScan := expressions.NewFullUnorderedScanExpression([]string{"INNER"}, values.UnknownType)
	innerRef := expressions.InitialOf(innerScan)
	innerRef.InsertFinal(plans.NewRecordQueryScanPlan([]string{"INNER"}, values.UnknownType, false))

	outerQ := expressions.NamedForEachQuantifier(outerAlias, outerRef)
	innerQ := expressions.NamedExistentialQuantifier(innerAlias, innerRef)
	outerID := values.NewFieldValue(values.NewQuantifiedObjectValue(outerAlias), "ID", values.UnknownType)
	innerFK := values.NewFieldValue(values.NewQuantifiedObjectValue(innerAlias), "FK", values.UnknownType)
	joinPredicate := predicates.NewComparisonPredicate(
		innerFK,
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: outerID},
	)
	selectExpr := expressions.NewSelectExpressionWithAliases(
		values.NewQuantifiedObjectValue(outerAlias),
		[]expressions.Quantifier{outerQ, innerQ},
		[]predicates.QueryPredicate{
			joinPredicate,
			predicates.NewExistentialAlias(innerAlias),
		},
		[]string{"O", "I"},
	)

	fanOut := true
	scalar := false
	scalarFanType := gen.Field_SCALAR
	newCandidate := func(name string, columns []string, createsDuplicates *bool) MatchCandidate {
		aliases := make([]values.CorrelationIdentifier, len(columns))
		for i := range aliases {
			aliases[i] = values.UniqueCorrelationIdentifier()
		}
		return NewValueIndexScanMatchCandidateWithFunctions(
			name,
			[]string{"INNER"},
			columns,
			nil,
			aliases,
			values.UnknownType,
			false,
			nil,
			createsDuplicates,
		)
	}
	functionCandidate := NewValueIndexScanMatchCandidateWithFunctions(
		"INNER$cardinality_fk",
		[]string{"INNER"},
		[]string{"FK"},
		[]string{FunctionKindCardinality},
		[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
		values.UnknownType,
		false,
		nil,
		&scalar,
	)
	nestedCandidate := NewValueIndexScanMatchCandidateWithFunctions(
		"INNER$addr_fk",
		[]string{"INNER"},
		[]string{"FK"},
		nil,
		[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
		values.UnknownType,
		false,
		nil,
		&scalar,
	).WithRootKeyExpression(&gen.KeyExpression{Nesting: &gen.Nesting{
		Parent: &gen.Field{
			FieldName: proto.String("ADDR"),
			FanType:   &scalarFanType,
		},
		Child: candidateTestKeyField("FK", gen.Field_SCALAR),
	}})
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{
		newCandidate("INNER$fk_tags_fanout", []string{"FK", "TAGS"}, &fanOut),
		functionCandidate,
		nestedCandidate,
		newCandidate("INNER$fk_status", []string{"FK", "STATUS"}, &scalar),
	}}

	results := FireExpressionRuleWithMemo(
		NewImplementNestedLoopJoinRule(),
		expressions.InitialOf(selectExpr),
		ctx,
		nil,
	)
	if len(results) != 1 {
		t.Fatalf("expected one EXISTS FlatMap alternative, got %d", len(results))
	}
	flatMap, ok := results[0].(*plans.RecordQueryFlatMapPlan)
	if !ok {
		t.Fatalf("expected *plans.RecordQueryFlatMapPlan, got %T", results[0])
	}

	var selected []string
	plans.Walk(flatMap, func(plan plans.RecordQueryPlan) bool {
		if indexPlan, ok := plan.(*plans.RecordQueryIndexPlan); ok {
			selected = append(selected, indexPlan.GetIndexName())
		}
		return true
	})
	if len(selected) != 1 || selected[0] != "INNER$fk_status" {
		t.Fatalf("EXISTS shortcut selected indexes %v, want only the non-fan-out FK index", selected)
	}
}
