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

// TestImplementNestedLoopJoin_StrictSingleForcesCompensatedFlatMap pins the
// semantic carrier used by correlated scalar subqueries. A later simplification
// can remove the inner plan's last syntactic reference to the outer row (for
// example, `inner.fk = outer.id OR TRUE`), but that must not make the
// strict-single edge eligible for the ordinary materialized NLJ path: that path
// has no at-most-one-row check and would silently fan out the outer row.
//
// The edge flag is the durable contract. Even with two completely independent
// scan references, it must force the existing FlatMap + strict
// FirstOrDefault compensation and must produce no unwrapped NLJ alternative.
func TestImplementNestedLoopJoin_StrictSingleForcesCompensatedFlatMap(t *testing.T) {
	t.Parallel()

	outerAlias := values.NamedCorrelationIdentifier("O")
	innerAlias := values.NamedCorrelationIdentifier("I")

	outerLogical := expressions.NewFullUnorderedScanExpression([]string{"OUTER"}, values.UnknownType)
	outerRef := expressions.InitialOf(outerLogical)
	outerRef.InsertFinal(plans.NewRecordQueryScanPlan([]string{"OUTER"}, values.UnknownType, false))

	innerLogical := expressions.NewFullUnorderedScanExpression([]string{"INNER"}, values.UnknownType)
	innerRef := expressions.InitialOf(innerLogical)
	innerRef.InsertFinal(plans.NewRecordQueryScanPlan([]string{"INNER"}, values.UnknownType, false))

	outerQ := expressions.NamedForEachQuantifier(outerAlias, outerRef)
	innerQ := expressions.NamedForEachStrictSingleQuantifier(innerAlias, innerRef)
	sel := expressions.NewSelectExpressionWithJoinType(
		outerQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{outerQ, innerQ},
		nil,
		[]string{"O", "I"},
		expressions.JoinLeftOuter,
	)

	results := FireExpressionRule(NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel))
	if len(results) == 0 {
		t.Fatal("strict-single select yielded no physical implementation")
	}

	foundStrictFlatMap := false
	for _, result := range results {
		if _, ok := result.(*plans.RecordQueryNestedLoopJoinPlan); ok {
			t.Fatalf("strict-single select yielded an unwrapped materialized NLJ: %T", result)
		}
		flatMap, ok := result.(*plans.RecordQueryFlatMapPlan)
		if !ok {
			continue
		}
		hasStrictFirstOrDefault := false
		plans.Walk(flatMap, func(plan plans.RecordQueryPlan) bool {
			if first, ok := plan.(*plans.RecordQueryFirstOrDefaultPlan); ok && first.IsStrict() {
				hasStrictFirstOrDefault = true
			}
			return true
		})
		if hasStrictFirstOrDefault {
			foundStrictFlatMap = true
		}
	}
	if !foundStrictFlatMap {
		t.Fatalf("strict-single select yielded no FlatMap with strict FirstOrDefault; results: %d", len(results))
	}
}

// TestImplementNestedLoopJoin_DualStrictSingleFailsClosed covers a malformed
// shape the SQL translator does not emit: both legs claim scalar cardinality.
// The current FlatMap compensation can enforce one inner leg per outer, not two
// mutually inner legs. The rule must therefore decline the shape rather than
// fall back to an ordinary materialized NLJ that enforces neither contract.
func TestImplementNestedLoopJoin_DualStrictSingleFailsClosed(t *testing.T) {
	t.Parallel()

	leftAlias := values.NamedCorrelationIdentifier("L")
	rightAlias := values.NamedCorrelationIdentifier("R")

	leftRef := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"LEFT"}, values.UnknownType))
	leftRef.InsertFinal(plans.NewRecordQueryScanPlan([]string{"LEFT"}, values.UnknownType, false))
	rightRef := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"RIGHT"}, values.UnknownType))
	rightRef.InsertFinal(plans.NewRecordQueryScanPlan([]string{"RIGHT"}, values.UnknownType, false))

	leftQ := expressions.NamedForEachStrictSingleQuantifier(leftAlias, leftRef)
	rightQ := expressions.NamedForEachStrictSingleQuantifier(rightAlias, rightRef)
	sel := expressions.NewSelectExpressionWithJoinType(
		leftQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{leftQ, rightQ},
		nil,
		[]string{"L", "R"},
		expressions.JoinInner,
	)

	results := FireExpressionRule(NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel))
	if len(results) != 0 {
		for _, result := range results {
			if _, ok := result.(*plans.RecordQueryNestedLoopJoinPlan); ok {
				t.Fatalf("dual strict-single select yielded an unwrapped materialized NLJ: %T", result)
			}
		}
		t.Fatalf("dual strict-single select must fail closed, got %d implementation(s)", len(results))
	}
}

// TestImplementNestedLoopJoin_StrictSingleUnsupportedShapesFailClosed pins the
// rule's global carrier invariant. StrictSingle has exactly one implementation:
// LEFT OUTER [plain outer, strict right]. Other join kinds, orientations, and
// special arms must not consume or materialize a flagged edge without the exact
// scalar-subquery semantics owned by that path.
func TestImplementNestedLoopJoin_StrictSingleUnsupportedShapesFailClosed(t *testing.T) {
	t.Parallel()

	newScanRef := func(name string) *expressions.Reference {
		ref := expressions.InitialOf(
			expressions.NewFullUnorderedScanExpression([]string{name}, values.UnknownType))
		ref.InsertFinal(plans.NewRecordQueryScanPlan([]string{name}, values.UnknownType, false))
		return ref
	}
	assertNoImplementation := func(t *testing.T, sel *expressions.SelectExpression) {
		t.Helper()
		results := FireExpressionRule(NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel))
		if len(results) != 0 {
			t.Fatalf("strict-single unsupported shape yielded %d implementation(s), including %T",
				len(results), results[0])
		}
	}

	t.Run("full_outer", func(t *testing.T) {
		leftQ := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("L"), newScanRef("LEFT"))
		rightQ := expressions.NamedForEachStrictSingleQuantifier(
			values.NamedCorrelationIdentifier("R"), newScanRef("RIGHT"))
		assertNoImplementation(t, expressions.NewSelectExpressionWithJoinType(
			leftQ.GetFlowedObjectValue(),
			[]expressions.Quantifier{leftQ, rightQ},
			nil,
			[]string{"L", "R"},
			expressions.JoinFullOuter,
		))
	})

	t.Run("inner", func(t *testing.T) {
		leftQ := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("L"), newScanRef("LEFT"))
		rightQ := expressions.NamedForEachStrictSingleQuantifier(
			values.NamedCorrelationIdentifier("R"), newScanRef("RIGHT"))
		assertNoImplementation(t, expressions.NewSelectExpressionWithJoinType(
			leftQ.GetFlowedObjectValue(),
			[]expressions.Quantifier{leftQ, rightQ},
			nil,
			[]string{"L", "R"},
			expressions.JoinInner,
		))
	})

	t.Run("cross", func(t *testing.T) {
		leftQ := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("L"), newScanRef("LEFT"))
		rightQ := expressions.NamedForEachStrictSingleQuantifier(
			values.NamedCorrelationIdentifier("R"), newScanRef("RIGHT"))
		assertNoImplementation(t, expressions.NewSelectExpressionWithJoinType(
			leftQ.GetFlowedObjectValue(),
			[]expressions.Quantifier{leftQ, rightQ},
			nil,
			[]string{"L", "R"},
			expressions.JoinCross,
		))
	})

	t.Run("strict_left", func(t *testing.T) {
		leftQ := expressions.NamedForEachStrictSingleQuantifier(
			values.NamedCorrelationIdentifier("L"), newScanRef("LEFT"))
		rightQ := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("R"), newScanRef("RIGHT"))
		assertNoImplementation(t, expressions.NewSelectExpressionWithJoinType(
			leftQ.GetFlowedObjectValue(),
			[]expressions.Quantifier{leftQ, rightQ},
			nil,
			[]string{"L", "R"},
			expressions.JoinLeftOuter,
		))
	})

	t.Run("null_on_empty_left", func(t *testing.T) {
		leftQ := expressions.NamedForEachNullOnEmptyQuantifier(
			values.NamedCorrelationIdentifier("L"), newScanRef("LEFT"))
		rightQ := expressions.NamedForEachStrictSingleQuantifier(
			values.NamedCorrelationIdentifier("R"), newScanRef("RIGHT"))
		assertNoImplementation(t, expressions.NewSelectExpressionWithJoinType(
			leftQ.GetFlowedObjectValue(),
			[]expressions.Quantifier{leftQ, rightQ},
			nil,
			[]string{"L", "R"},
			expressions.JoinLeftOuter,
		))
	})

	t.Run("three_quantifier_existential", func(t *testing.T) {
		leftQ := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("L"), newScanRef("LEFT"))
		rightQ := expressions.NamedForEachStrictSingleQuantifier(
			values.NamedCorrelationIdentifier("R"), newScanRef("RIGHT"))
		existAlias := values.NamedCorrelationIdentifier("E")
		existQ := expressions.NamedExistentialQuantifier(existAlias, newScanRef("EXISTS"))
		assertNoImplementation(t, expressions.NewSelectExpressionWithAliases(
			leftQ.GetFlowedObjectValue(),
			[]expressions.Quantifier{leftQ, rightQ, existQ},
			[]predicates.QueryPredicate{predicates.NewExistentialAlias(existAlias)},
			[]string{"L", "R", "E"},
		))
	})

	t.Run("two_quantifier_existential", func(t *testing.T) {
		leftQ := expressions.NamedForEachStrictSingleQuantifier(
			values.NamedCorrelationIdentifier("L"), newScanRef("LEFT"))
		existAlias := values.NamedCorrelationIdentifier("E")
		existQ := expressions.NamedExistentialQuantifier(existAlias, newScanRef("EXISTS"))
		assertNoImplementation(t, expressions.NewSelectExpressionWithAliases(
			leftQ.GetFlowedObjectValue(),
			[]expressions.Quantifier{leftQ, existQ},
			[]predicates.QueryPredicate{predicates.NewExistentialAlias(existAlias)},
			[]string{"L", "E"},
		))
	})
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

// TestMaterializedNLJOrdinalLayoutMatches pins a real production defect: a
// ChildrenAsSet-swapped firing of ImplementNestedLoopJoinRule (the
// join-commutativity exploration in fireExprRuleOnMember,
// expression_matcher.go) reuses the logical select's resultValue UNCHANGED
// under the swapped (outer, inner) assignment
// (expressions.SelectExpression.WithSwappedQuantifiers). That is sound for
// an ordinary correlation-addressed resultValue (order-independent by
// design) but NOT for a pristine ORDINAL SEED — a RecordConstructorValue of
// baked FrontierPinned FieldValues whose ordinals assume ONE FIXED physical
// field order, baked when the seed was first built. The materialized NLJ's
// own executor (executor.go's concatLegPositionals/mergeRows) ALWAYS
// concatenates outer-then-inner, so a swapped orientation whose seed still
// assumes the OTHER leg first silently (or loudly) misreads the runtime
// merged row.
//
// This was found via TestFDB_LeftJoinExistsResidual/I1 going from a correct
// empty result to a hard "correlated FieldValue ID (correlation E) evaluated
// against an unbound/unrecognized context" runtime error once a cost-model
// fix (RFC-192 follow-up) made the previously-never-cheapest swapped
// orientation the memo's winner for the first time — the bug was always
// latent, just unreachable before. A first fix attempt compared alias
// STRINGS (sel.GetSourceAliases() falling back to the quantifier's own
// GetAlias()) and regressed two OTHER real things: several RIGHT-OUTER-JOIN
// queries (whose quantifier identity is the synthetic one while
// sourceAliases carries the real leg name — the opposite divergence
// direction) and, worse, TestFDB_QuotedMachineShapedAliases/join_legs — a
// user alias quoted as "q$2" is byte-identical to a synthetic
// UniqueCorrelationIdentifier that happens to reach counter 2
// (values.CorrelationIdentifier is a bare wrapped string), so alias-string
// guessing is fundamentally unsafe. The final fix compares leg ROW SHAPE
// (RecordName + fields) instead — never a name.
func TestMaterializedNLJOrdinalLayoutMatches(t *testing.T) {
	t.Parallel()

	t1 := plans.NewRecordQueryScanPlan([]string{"T1"}, commit2RecType("T1", "ID", "V"), false)
	t2 := plans.NewRecordQueryScanPlan([]string{"T2"}, commit2RecType("T2", "ID", "T1_ID"), false)
	seed := reconstructFoldStep1Seed(t1, t2, "T1", "T2")
	if seed == nil {
		t.Fatal("setup: expected reconstructFoldStep1Seed to build a seed for two scan legs")
	}
	// Sanity-check the setup matches TestReconstructFoldStep1Seed's own pin:
	// T1 at offset 0 (2 columns), T2 at offset 2.
	if w, _ := ordinalSeedLegWindowsOf(seed); w == nil || w["T1"].Offset != 0 || w["T2"].Offset != 2 {
		t.Fatalf("setup: unexpected seed layout %+v", w)
	}

	t.Run("matching orientation passes", func(t *testing.T) {
		t.Parallel()
		if !materializedNLJOrdinalLayoutMatches(seed, t1, t2) {
			t.Fatal("T1-outer/T2-inner matches the seed's own T1@0,T2@2 layout — must pass")
		}
	})

	t.Run("swapped orientation is rejected", func(t *testing.T) {
		t.Parallel()
		if materializedNLJOrdinalLayoutMatches(seed, t2, t1) {
			t.Fatal("T2-outer/T1-inner contradicts the seed's T1-first layout — must be rejected")
		}
	})

	t.Run("non-ordinal-seed resultValue is always order-independent", func(t *testing.T) {
		t.Parallel()
		lazy := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("X"))
		if !materializedNLJOrdinalLayoutMatches(lazy, t2, t1) {
			t.Fatal("a non-ordinal-seed (correlation-addressed) resultValue must match regardless of orientation")
		}
	})

	t.Run("adversarial quoted alias shaped like the machine namespace never matters", func(t *testing.T) {
		t.Parallel()
		// The real production regression: a user alias quoted as "q$2" is
		// byte-identical, as a values.CorrelationIdentifier, to a synthetic
		// UniqueCorrelationIdentifier that happens to reach counter 2 — an
		// alias-string-based check cannot tell them apart. Naming the legs
		// "Q$1"/"Q$2" here must not change the verdict at all, because this
		// check never looks at a name.
		q1 := plans.NewRecordQueryScanPlan([]string{"T1"}, commit2RecType("T1", "ID", "V"), false)
		q2 := plans.NewRecordQueryScanPlan([]string{"T2"}, commit2RecType("T2", "ID", "T1_ID"), false)
		adversarialSeed := reconstructFoldStep1Seed(q1, q2, "Q$1", "Q$2")
		if adversarialSeed == nil {
			t.Fatal("setup: expected a seed for the adversarially-named legs")
		}
		if !materializedNLJOrdinalLayoutMatches(adversarialSeed, q1, q2) {
			t.Fatal("matching orientation must pass regardless of the legs' alias spelling")
		}
		if materializedNLJOrdinalLayoutMatches(adversarialSeed, q2, q1) {
			t.Fatal("swapped orientation must still be rejected regardless of the legs' alias spelling")
		}
	})

	t.Run("cannot verify structurally defaults to true", func(t *testing.T) {
		t.Parallel()
		// A plan whose own GetResultType() isn't a (Relation-of-)RecordType
		// (here: nil, standing in for an opaque/erased result) can't be
		// compared against the seed's leg shapes at all — under-detecting is
		// the documented safe default.
		if !materializedNLJOrdinalLayoutMatches(seed, nil, t2) {
			t.Fatal("an unverifiable outer plan must default to true (under-detection is safe)")
		}
	})

	t.Run("self-join (identical leg shapes) is always safe", func(t *testing.T) {
		t.Parallel()
		s1 := plans.NewRecordQueryScanPlan([]string{"T1"}, commit2RecType("T1", "ID", "V"), false)
		s2 := plans.NewRecordQueryScanPlan([]string{"T1"}, commit2RecType("T1", "ID", "V"), false)
		selfSeed := reconstructFoldStep1Seed(s1, s2, "A", "B")
		if selfSeed == nil {
			t.Fatal("setup: expected a seed for two same-shaped legs")
		}
		if !materializedNLJOrdinalLayoutMatches(selfSeed, s1, s2) || !materializedNLJOrdinalLayoutMatches(selfSeed, s2, s1) {
			t.Fatal("identical leg shapes are structurally indistinguishable — both orientations must pass")
		}
	})
}
