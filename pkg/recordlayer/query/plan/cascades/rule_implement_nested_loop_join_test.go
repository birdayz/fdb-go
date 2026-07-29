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

	// Both legs flow their declared row type, and both comparands are baked
	// against it. The index shortcut resolves the candidate's first column name
	// against the inner leg's layout once and then compares ordinals, so an
	// untyped leg has no layout to resolve in and declines before candidate
	// selection is ever reached — which would make this test pass for the wrong
	// reason (nothing selected because nothing was tried).
	outerRowType := values.Type(nljTestLayouts["OUTER"])
	innerRowType := values.Type(nljTestLayouts["INNERFK"])

	outerScan := expressions.NewFullUnorderedScanExpression([]string{"OUTER"}, outerRowType)
	outerRef := expressions.InitialOf(outerScan)
	outerRef.InsertFinal(plans.NewRecordQueryScanPlan([]string{"OUTER"}, outerRowType, false))

	innerScan := expressions.NewFullUnorderedScanExpression([]string{"INNER"}, innerRowType)
	innerRef := expressions.InitialOf(innerScan)
	innerRef.InsertFinal(plans.NewRecordQueryScanPlan([]string{"INNER"}, innerRowType, false))

	outerQ := expressions.NamedForEachQuantifier(outerAlias, outerRef)
	innerQ := expressions.NamedExistentialQuantifier(innerAlias, innerRef)
	outerID := nljBakedRef(t, "OUTER", outerAlias, "ID")
	innerFK := nljBakedRef(t, "INNERFK", innerAlias, "FK")
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

// fusedNestedFieldValue builds a FUSED baked nested reference (Field=leaf,
// Child=the bare source QOV directly, Resolved carrying a TWO-accessor
// [parent, leaf] path) — the shape a baked `alias.parent.leaf` reference
// takes, as opposed to a flat `alias.leaf` top-level column. Mirrors
// fkChainCorrelatedNestedEq (fk_chain_cardinality_test.go), which pins the
// identical hole in the sibling fk-chain cardinality cap.
func fusedNestedFieldValue(alias values.CorrelationIdentifier, parent, leaf string) *values.FieldValue {
	return &values.FieldValue{
		Field: leaf, Typ: values.UnknownType,
		Child: values.NewQuantifiedObjectValue(alias),
		Resolved: &values.FieldPath{Accessors: []values.ResolvedAccessor{
			{Field: parent, Ordinal: 0},
			{Field: leaf, Ordinal: 1},
		}},
	}
}

// TestLegCorrelationOf_DeclinesFusedNestedSameLeafName pins the wrong-rows
// hole the old fieldValueAliasAndCol's bare-column allowlist guarded: a fused
// multi-accessor bake (Child=QOV directly, Resolved=[ADDRESS, ID],
// Field="ID") passes the Child==QOV check while still reading a NESTED
// record's column, so the quantifier's row is not the layout its leaf names.
// Reporting the root quantifier would let `i.address.id` stand in for a bare
// top-level `I.ID` reference in matchJoinPKPredicate.
func TestLegCorrelationOf_DeclinesFusedNestedSameLeafName(t *testing.T) {
	t.Parallel()

	fused := fusedNestedFieldValue(values.NamedCorrelationIdentifier("I"), "ADDRESS", "ID")
	if leg, ok := legCorrelationOf(fused); ok {
		t.Fatalf("legCorrelationOf(fused I.ADDRESS.ID) = (%v, true), want ok=false — "+
			"a nested reference must never report the root quantifier as the leg it reads", leg)
	}
}

// TestLegCorrelationOf_AcceptsBareTopLevelColumn is the accept-direction
// companion: a genuine bare top-level reference must still report its leg, so
// the decline above is specific to the fused shape rather than a blanket
// refusal that would silently disable the fast path.
func TestLegCorrelationOf_AcceptsBareTopLevelColumn(t *testing.T) {
	t.Parallel()

	bare := values.NewFieldValue(
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("I")), "ID", values.UnknownType)
	leg, ok := legCorrelationOf(bare)
	if !ok || leg.String() != "I" {
		t.Fatalf("legCorrelationOf(bare I.ID) = (%v, %v), want (I, true) — "+
			"the decline must not over-reach to ordinary references", leg, ok)
	}
}

// TestReadsKeyColumn_Dimensions is the identity comparison that replaced the
// leaf-name match, probed on each element of RFC-197's triple SEPARATELY.
//
// Each case differs from the accepted one in exactly ONE element, so a
// mutation that drops that element is caught here and nowhere else. That
// separation is the point: on the explaindiff corpus the domain check and the
// correlation check reject the same predicates, so a corpus-only check cannot
// tell which of the two is doing the work, and a fix satisfying only one of
// them would measure as complete.
func TestReadsKeyColumn_Dimensions(t *testing.T) {
	t.Parallel()

	innerLayout := nljTestLayouts["INNER"]
	innerFrontier := values.OrdinalDomainOfType(innerLayout)
	innerCorr := values.NamedCorrelationIdentifier("I")

	// The inner table's primary key, resolved once against the inner leg's own
	// layout — the boundary rule, and the only place the name is legitimate.
	keyIdent, ok := values.OrdinalOfNameIn(innerLayout, "ID")
	if !ok {
		t.Fatal("setup: INNER.ID must resolve against INNER's own layout")
	}

	t.Run("accepts the real key column", func(t *testing.T) {
		t.Parallel()
		ref := nljBakedRef(t, "INNER", innerCorr, "ID")
		if !readsKeyColumn(ref, innerCorr, keyIdent, innerFrontier) {
			t.Fatal("I.ID must match INNER's primary key ID — over-declining silently disables the probe")
		}
	})

	t.Run("declines a same-named column of another quantifier", func(t *testing.T) {
		t.Parallel()
		// O.ID: same leaf name, same ordinal (0), DIFFERENT correlation and
		// domain. This is the shape the name-keyed proof could not see at all.
		ref := nljBakedRef(t, "OUTER", values.NamedCorrelationIdentifier("O"), "ID")
		if readsKeyColumn(ref, innerCorr, keyIdent, innerFrontier) {
			t.Fatal("O.ID matched INNER's primary key — two columns sharing a leaf name " +
				"were treated as one, which is the whole bug class RFC-197 exists to end")
		}
	})

	t.Run("declines a same-DOMAIN same-ordinal column of another quantifier", func(t *testing.T) {
		t.Parallel()
		// A self-join: baked against INNER's OWN layout, so the domain and the
		// ordinal both agree with the key, and only the correlation says this
		// reads a DIFFERENT INNER row. The `O.ID` case above cannot isolate the
		// correlation because OUTER's domain differs too and the frontier gate
		// rejects it first — measured: dropping the correlation check alone
		// left every other case in this suite green.
		otherLeg := nljBakedRef(t, "INNER", values.NamedCorrelationIdentifier("I2"), "ID")
		if readsKeyColumn(otherLeg, innerCorr, keyIdent, innerFrontier) {
			t.Fatal("a reference to ANOTHER INNER row matched this leg's primary key — " +
				"ordinal 0 of two quantifiers are different columns, which is the " +
				"element a pair of (name, ordinal) can never carry")
		}
	})

	t.Run("declines a same-ordinal column of another DOMAIN, correlation held equal", func(t *testing.T) {
		t.Parallel()
		// Baked against SHADOW, whose "ID" is also ordinal 0, but stamped with
		// the INNER correlation. Correlation and ordinal both AGREE with the
		// key; only the domain differs. Dropping the domain check accepts this
		// and nothing else in the suite notices.
		ref := nljBakedRef(t, "SHADOW", innerCorr, "ID")
		if readsKeyColumn(ref, innerCorr, keyIdent, innerFrontier) {
			t.Fatal("a SHADOW-domain ordinal 0 matched INNER's ordinal-0 primary key — " +
				"an ordinal compared across layouts is the same conflation as a name, " +
				"wearing a type that reads as authoritative")
		}
	})

	t.Run("declines a same-domain non-key column", func(t *testing.T) {
		t.Parallel()
		// I.OUTER_ID: right leg, right layout, WRONG column. Pins that the
		// comparison is to the key's ordinal and not merely to "some resolved
		// column of the inner leg".
		ref := nljBakedRef(t, "INNER", innerCorr, "OUTER_ID")
		if readsKeyColumn(ref, innerCorr, keyIdent, innerFrontier) {
			t.Fatal("I.OUTER_ID matched INNER's primary key ID — the probe would be built " +
				"on a non-key column and narrow the scan to the wrong records")
		}
	})

	t.Run("declines when the key was resolved in a DIFFERENT layout than the frontier", func(t *testing.T) {
		t.Parallel()
		// readsKeyColumn takes the frontier and the resolved key as two
		// INDEPENDENT arguments, and they are only meaningful together: a key
		// ordinal resolved in SHADOW says nothing about a comparand's ordinal
		// in INNER. The frontier gate inside CorrelatedIdentityIn cannot catch
		// this one — the comparand is a perfectly good INNER reference — so the
		// agreement between the two arguments is checked explicitly.
		//
		// This is the guard that keeps the per-site proof a predicate the
		// function checks rather than a comment the next caller does not read.
		shadowLayout := nljTestLayouts["SHADOW"]
		shadowKey, ok := values.OrdinalOfNameIn(shadowLayout, "ID")
		if !ok {
			t.Fatal("setup: SHADOW.ID must resolve against SHADOW's layout")
		}
		ref := nljBakedRef(t, "INNER", innerCorr, "ID")
		if readsKeyColumn(ref, innerCorr, shadowKey, innerFrontier) {
			t.Fatal("a key resolved in SHADOW's layout was matched against a comparand's " +
				"ordinal in INNER's — two ordinals from different layouts are not comparable, " +
				"and agreeing by accident is what the domain element exists to prevent")
		}
	})

	t.Run("declines a LAZY reference to the key column", func(t *testing.T) {
		t.Parallel()
		// Right name, right leg, no resolved ordinal at all. There is nothing
		// to compare, so it fails closed rather than falling back to the name
		// it still carries.
		ref := values.NewFieldValue(values.NewQuantifiedObjectValue(innerCorr), "ID", values.UnknownType)
		if readsKeyColumn(ref, innerCorr, keyIdent, innerFrontier) {
			t.Fatal("a lazy I.ID matched the primary key — the only thing it could have " +
				"matched on is its display name")
		}
	})
}

// TestOrdinalOfNameIn_KeyResolutionIsCaseInsensitiveAndFailsClosed pins the
// boundary itself. The metadata layer names its columns and that name is
// resolved ONCE, here; if this resolution silently declined, every caller
// downstream would decline too and the fast path would vanish without a
// failing test — the quiet-regression shape RFC-197 warns about.
func TestOrdinalOfNameIn_KeyResolutionIsCaseInsensitiveAndFailsClosed(t *testing.T) {
	t.Parallel()

	innerLayout := nljTestLayouts["INNER"]

	if id, ok := values.OrdinalOfNameIn(innerLayout, "id"); !ok || id.Ordinal != 0 {
		t.Fatalf(`OrdinalOfNameIn(INNER, "id") = (%v, %v), want ordinal 0 — `+
			"metadata column lists do not agree with a record type's spelling on case", id, ok)
	}
	if id, ok := values.OrdinalOfNameIn(innerLayout, "NO_SUCH_COLUMN"); ok {
		t.Fatalf("OrdinalOfNameIn resolved a column INNER does not declare: %v", id)
	}
	if id, ok := values.OrdinalOfNameIn(values.UnknownType, "ID"); ok {
		t.Fatalf("OrdinalOfNameIn resolved against a layout with no declared column order: %v", id)
	}
}

// TestCorrelatedFastPathOperand_DeclinesLazyOuterRef pins a NEGATIVE result,
// and states what re-arms if it changes.
//
// The lazy arm used to rebuild the outer comparand as a bare
// `QOV(outer).<name>`. That is not a weaker operand, it is an UNEVALUABLE one:
// FieldValue.evaluateOrdinal has no runtime name-resolution fallback and
// returns OrdinalResolutionError{Ordinal: -1} for any unbaked reference
// (values.go:789-793). So the arm could only ever have produced a plan that
// fails loud at execution, which is why nothing in the corpus reaches it —
// not because the shape cannot occur, but because a query that took it would
// not have survived.
//
// If a runtime name read is ever introduced, this test keeps failing until
// someone decides deliberately whether the fast path may build one. It must
// not be "fixed" by re-adding the lazy construction.
func TestCorrelatedFastPathOperand_DeclinesLazyOuterRef(t *testing.T) {
	t.Parallel()

	outerCorr := values.NamedCorrelationIdentifier("O")
	lazy := values.NewFieldValue(values.NewQuantifiedObjectValue(outerCorr), "ID", values.UnknownType)
	if operand, ok := correlatedFastPathOperand(lazy, outerCorr); ok {
		t.Fatalf("correlatedFastPathOperand built %v from a LAZY outer reference — "+
			"an unbaked reference has no ordinal, and the executor has no name fallback "+
			"to give it one; the fast path must decline and let the general "+
			"correlated path rebase the reference", operand)
	}

	// Accept direction: a source-relative bake still transfers, carrying its
	// ordinal. Without this the decline above could be satisfied by refusing
	// everything.
	baked := nljBakedRef(t, "OUTER", outerCorr, "ID")
	operand, ok := correlatedFastPathOperand(baked, outerCorr)
	if !ok {
		t.Fatal("correlatedFastPathOperand declined a SOURCE-RELATIVE baked outer reference — " +
			"that is the arm the whole fast path runs on")
	}
	built, isFV := operand.(*values.FieldValue)
	if !isFV || built.Resolved == nil || built.Resolved.Root().Ordinal != 0 {
		t.Fatalf("rebuilt operand %#v lost its ordinal — the ordinal IS the identity here; "+
			"the display name beside it decides nothing", operand)
	}
}

// TestMatchJoinPKPredicate_BuriedLegHasNoBareNameBackDoor pins the decline
// that made matchJoinPKPredicate's removed "deep-flowed" arms dead.
//
// Those arms accepted an outer comparand on ANY leg other than the inner one,
// so a re-enumerated multi-way chain could probe through a table buried inside
// the outer sub-join by reading it off the merged row under its bare name.
// They could never do that: both call sites hand every accepted comparand to
// outerValRefsBuriedLeg, which declines exactly the legs those arms alone
// admitted, so no rewrite they accepted could be yielded.
//
// What re-arms if this changes: relaxing outerValRefsBuriedLeg would make a
// bare-name read of a merged row reachable again, and on a colliding column
// name that is last-leg-wins — wrong rows, not a worse plan. The capability
// belongs to the below-FOD rebase, which rewrites a buried reference to a
// qualified key or a baked ordinal; it does not belong to a name read here.
func TestMatchJoinPKPredicate_BuriedLegHasNoBareNameBackDoor(t *testing.T) {
	t.Parallel()

	outerCorr := values.NamedCorrelationIdentifier("O")
	buriedCorr := values.NamedCorrelationIdentifier("BURIED")

	buried := nljBakedRef(t, "OUTER", buriedCorr, "ID")
	if !outerValRefsBuriedLeg(buried, outerCorr) {
		t.Fatal("outerValRefsBuriedLeg accepted a comparand on a leg that is neither the " +
			"outer nor the inner one — the fast path rebuilds the probe as " +
			"QOV(outerAlias).<col>, which reads the merged row's rightmost leg, " +
			"so accepting a buried leg is a wrong-rows rewrite")
	}
	// Fails CLOSED: a comparand whose leg cannot even be stated is treated as
	// buried, because "which leg does this read" is the question that has to be
	// answered before the probe may be rebuilt.
	unstateable := &values.FieldValue{Field: "ID", Typ: values.UnknownType}
	if !outerValRefsBuriedLeg(unstateable, outerCorr) {
		t.Fatal("outerValRefsBuriedLeg accepted a comparand with no stateable leg")
	}
	// Accept direction, so the decline cannot be satisfied by refusing all.
	own := nljBakedRef(t, "OUTER", outerCorr, "ID")
	if outerValRefsBuriedLeg(own, outerCorr) {
		t.Fatal("outerValRefsBuriedLeg declined a reference to the outer leg itself")
	}
}

// nljTestLayouts are the declared column orders the EXISTS-shortcut scenarios
// resolve against. Stating them is the point of RFC-197: an ordinal means
// nothing without the layout it indexes, and the inner leg's layout is where
// the inner table's primary-key NAME is resolved exactly once.
//
// OUTER and INNER deliberately BOTH declare a column named "ID" at ordinal 0.
// That is the dimension every name-keyed proof in this file was blind to: with
// the leaf name as the key the two are one column, and with the ordinal alone
// (no domain, no correlation) they are still one column. Only the full triple
// tells them apart.
var nljTestLayouts = map[string]*values.RecordType{
	"OUTER": nljTestLayout("OUTER", "ID", "CATEGORY"),
	"INNER": nljTestLayout("INNER", "ID", "OUTER_ID"),
	// SHADOW has "ID" at ordinal 0 exactly as INNER does, so an operand baked
	// against it is ordinal-equal and correlation-equal to a real INNER.ID
	// reference and differs ONLY in the domain. It is what isolates the domain
	// check from the correlation check under mutation.
	"SHADOW": nljTestLayout("SHADOW", "ID", "NOTE"),
	// The inner leg of the secondary-index shortcut scenario, whose join key
	// is a foreign key rather than the primary key.
	"INNERFK": nljTestLayout("INNER", "FK", "STATUS", "TAGS"),
}

func nljTestLayout(name string, cols ...string) *values.RecordType {
	fields := make([]values.Field, len(cols))
	for i, c := range cols {
		fields[i] = values.Field{Name: c, FieldType: values.UnknownType, Ordinal: i}
	}
	return values.NewRecordType(name, false, fields)
}

// nljBakedRef builds the production shape of a resolved column reference:
// correlated to alias, carrying the ordinal the column has in rt's declared
// column order, and stamped with rt's domain (the SQL resolver's
// sourceColumnOrdinal derives ordinal and domain in one breath). A column rt
// does not declare stays LAZY, which is what a reference outside that row
// really looks like and what the identity proofs decline.
func nljBakedRef(t *testing.T, rt string, alias values.CorrelationIdentifier, field string) *values.FieldValue {
	t.Helper()
	layout, known := nljTestLayouts[rt]
	if !known {
		t.Fatalf("setup: no layout registered for %q", rt)
	}
	ord, found := layout.FieldIndex(field)
	if !found {
		return &values.FieldValue{
			Field: field, Typ: values.UnknownType,
			Child: values.NewQuantifiedObjectValue(alias),
		}
	}
	return values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		values.NewQuantifiedObjectValue(alias), field, ord, values.UnknownType,
		values.OrdinalDomainOfType(layout),
	)
}

// buildExistsPKShortcutScenario assembles `SELECT * FROM OUTER O WHERE
// EXISTS (SELECT 1 FROM INNER I WHERE <innerOperand> = O.ID)` — the shape
// tryExistsFlatMap's PK-shortcut branch tries to rewrite into a correlated
// PK-narrowed scan. The inner table's declared primary key is the flat
// top-level column "ID" (via pkGateTestCtx).
//
// Both leaf scans flow their table's declared row type. A leaf that flowed
// UnknownType would have no domain, and the whole fast path fails closed on
// that — so an untyped scenario could not distinguish "declined because the
// identity says no" from "declined because there was no layout to ask", which
// is precisely the confusion the mutation checks have to avoid.
func buildExistsPKShortcutScenario(t *testing.T, innerOperand *values.FieldValue) []expressions.RelationalExpression {
	t.Helper()
	return buildExistsPKShortcutScenarioWithOuter(t, innerOperand, nil)
}

// buildExistsPKShortcutScenarioWithOuter is buildExistsPKShortcutScenario with
// the OUTER-side comparand supplied too; nil means the ordinary baked O.ID.
func buildExistsPKShortcutScenarioWithOuter(
	t *testing.T,
	innerOperand *values.FieldValue,
	outerOperand *values.FieldValue,
) []expressions.RelationalExpression {
	t.Helper()

	outerAlias := values.NamedCorrelationIdentifier("O")
	innerAlias := values.NamedCorrelationIdentifier("I")

	outerRowType := values.Type(nljTestLayouts["OUTER"])
	innerRowType := values.Type(nljTestLayouts["INNER"])

	outerScan := expressions.NewFullUnorderedScanExpression([]string{"OUTER"}, outerRowType)
	outerRef := expressions.InitialOf(outerScan)
	outerRef.InsertFinal(plans.NewRecordQueryScanPlan([]string{"OUTER"}, outerRowType, false))

	innerScan := expressions.NewFullUnorderedScanExpression([]string{"INNER"}, innerRowType)
	innerRef := expressions.InitialOf(innerScan)
	innerRef.InsertFinal(plans.NewRecordQueryScanPlan([]string{"INNER"}, innerRowType, false))

	outerQ := expressions.NamedForEachQuantifier(outerAlias, outerRef)
	innerQ := expressions.NamedExistentialQuantifier(innerAlias, innerRef)

	if outerOperand == nil {
		outerOperand = nljBakedRef(t, "OUTER", outerAlias, "ID")
	}
	joinPredicate := predicates.NewComparisonPredicate(
		innerOperand,
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: outerOperand},
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

	ctx := &pkGateTestCtx{pk: []string{"ID"}}
	return FireExpressionRuleWithMemo(
		NewImplementNestedLoopJoinRule(),
		expressions.InitialOf(selectExpr),
		ctx,
		nil,
	)
}

// anyScanCarriesComparisons reports whether any RecordQueryScanPlan reachable
// from any produced alternative carries non-empty ScanComparisons — the
// marker that the EXISTS PK shortcut fired and narrowed the inner scan to a
// correlated PK probe (WithScanComparisons is the shortcut's only producer of
// scan comparisons in this scenario: both leaf scans start comparison-free).
func anyScanCarriesComparisons(t *testing.T, results []expressions.RelationalExpression) bool {
	t.Helper()
	found := false
	for _, r := range results {
		rp, ok := r.(plans.RecordQueryPlan)
		if !ok {
			continue
		}
		plans.Walk(rp, func(p plans.RecordQueryPlan) bool {
			if sp, ok := p.(*plans.RecordQueryScanPlan); ok && len(sp.GetScanComparisons()) > 0 {
				found = true
			}
			return true
		})
	}
	return found
}

// TestImplementNestedLoopJoin_ExistsPKShortcutDimensions carries the identity
// dimensions up to the RULE level, where the consequence is a plan rather than
// a boolean: the inner scan either gets narrowed to a correlated probe or it
// does not.
func TestImplementNestedLoopJoin_ExistsPKShortcutDimensions(t *testing.T) {
	t.Parallel()

	innerCorr := values.NamedCorrelationIdentifier("I")
	outerCorr := values.NamedCorrelationIdentifier("O")

	t.Run("declines a same-named column of the OUTER leg on the inner side", func(t *testing.T) {
		t.Parallel()
		// `O.ID = O.ID`: the "inner" side is really an OUTER reference that
		// merely spells its column the way INNER's primary key is spelled.
		// Under a leaf-name match this is a PK equi-join and the rule builds a
		// correlated probe of INNER on a value that never reads INNER at all.
		results := buildExistsPKShortcutScenarioWithOuter(t,
			nljBakedRef(t, "OUTER", outerCorr, "ID"),
			nljBakedRef(t, "OUTER", outerCorr, "ID"))
		if anyScanCarriesComparisons(t, results) {
			t.Fatal("EXISTS PK shortcut fired on an OUTER reference sharing the inner PK's " +
				"leaf name — the probe narrows INNER by a column of a different table")
		}
	})

	t.Run("declines a same-named column of another DOMAIN under the inner alias", func(t *testing.T) {
		t.Parallel()
		// Correlated to I and ordinal 0, exactly like a real I.ID, but baked
		// against SHADOW's layout. Only the domain separates it from the
		// accepted case, so this is what a domain-dropped mutation reaches.
		results := buildExistsPKShortcutScenario(t, nljBakedRef(t, "SHADOW", innerCorr, "ID"))
		if anyScanCarriesComparisons(t, results) {
			t.Fatal("EXISTS PK shortcut fired on an ordinal baked against a different layout — " +
				"the probe's slot was chosen in a row the inner leg does not flow")
		}
	})

	t.Run("declines a same-domain NON-key column", func(t *testing.T) {
		t.Parallel()
		// I.OUTER_ID is a genuine, correctly-baked inner column — it is simply
		// not the primary key. The shortcut may only narrow a scan by the key
		// it claims to be probing.
		results := buildExistsPKShortcutScenario(t, nljBakedRef(t, "INNER", innerCorr, "OUTER_ID"))
		if anyScanCarriesComparisons(t, results) {
			t.Fatal("EXISTS PK shortcut fired on a non-key inner column — the correlated " +
				"scan would be narrowed by the wrong column")
		}
	})

	t.Run("fires on the real key column", func(t *testing.T) {
		t.Parallel()
		results := buildExistsPKShortcutScenario(t, nljBakedRef(t, "INNER", innerCorr, "ID"))
		if len(results) == 0 {
			t.Fatal("setup: expected at least one physical alternative")
		}
		if !anyScanCarriesComparisons(t, results) {
			t.Fatal("EXISTS PK shortcut did not fire on a genuine baked PK equi-join — " +
				"the identity conversion must not over-decline the case the shortcut exists for")
		}
	})
}

// TestImplementNestedLoopJoin_ExistsPKShortcutDeclinesFusedNestedSameLeafName
// pins the wrong-rows hazard the bare-column allowlist guards against at the
// RULE level, not just the matcher unit: an EXISTS join
// predicate whose INNER operand is a FUSED nested reference (i.address.id)
// sharing its LEAF name with the inner table's own top-level primary key
// ("ID") must NOT be mistaken for a PK equi-join and used to build a
// correlated PK-narrowed scan — that scan would filter on the top-level ID
// column while the query actually asked about a nested field, silently
// changing which rows the EXISTS reports as present.
func TestImplementNestedLoopJoin_ExistsPKShortcutDeclinesFusedNestedSameLeafName(t *testing.T) {
	t.Parallel()

	innerNestedID := fusedNestedFieldValue(values.NamedCorrelationIdentifier("I"), "ADDRESS", "ID")
	results := buildExistsPKShortcutScenario(t, innerNestedID)
	if len(results) == 0 {
		t.Fatal("setup: expected at least one physical alternative from ImplementNestedLoopJoinRule")
	}
	if anyScanCarriesComparisons(t, results) {
		t.Fatal("EXISTS PK shortcut fired on a fused nested reference sharing the PK's leaf name — " +
			"built a correlated PK-narrowed scan that filters the WRONG column (wrong EXISTS rows)")
	}
}

// TestImplementNestedLoopJoin_ExistsPKShortcutFiresOnBareTopLevelColumn is the
// accept-direction companion at the rule level: a genuine bare top-level PK
// join (i.id = o.id) must still take the correlated PK-shortcut fast path —
// proving the fix declines ONLY the fused nested shape, not the ordinary case
// the shortcut exists to serve.
func TestImplementNestedLoopJoin_ExistsPKShortcutFiresOnBareTopLevelColumn(t *testing.T) {
	t.Parallel()

	innerID := nljBakedRef(t, "INNER", values.NamedCorrelationIdentifier("I"), "ID")
	results := buildExistsPKShortcutScenario(t, innerID)
	if len(results) == 0 {
		t.Fatal("setup: expected at least one physical alternative from ImplementNestedLoopJoinRule")
	}
	if !anyScanCarriesComparisons(t, results) {
		t.Fatal("EXISTS PK shortcut did not fire on a genuine bare top-level PK join — the fix must not over-decline")
	}
}

// TestBuriedLegOrdinalLayout_SkipsFusedNestedSameLeafName pins the same
// bare-column-allowlist hole in buriedLegOrdinalLayout's own leaf-name keying:
// slot 0 is a FUSED nested reference (leg.address.id, leaf "ID"); slot 1 is
// the leg's GENUINE bare "leg.id" column. Both would key to "LEG.ID" under a
// leaf-name-only read, and "first occurrence wins" would let the fused slot's
// ordinal (0) silently answer for the real bare column, misdirecting any
// buried-leg rebase that looks up "LEG.ID" to the wrong RC slot. The fused
// slot must be skipped so "LEG.ID" maps to its one genuine owner, slot 1.
func TestBuriedLegOrdinalLayout_SkipsFusedNestedSameLeafName(t *testing.T) {
	t.Parallel()

	leg := values.NamedCorrelationIdentifier("LEG")
	fused := fusedNestedFieldValue(leg, "ADDRESS", "ID")
	bare := values.NewFieldValue(values.NewQuantifiedObjectValue(leg), "ID", values.UnknownType)

	rc := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "F0", Value: fused},
		values.RecordConstructorField{Name: "F1", Value: bare},
	)

	outer := plans.NewRecordQueryScanPlan([]string{"OUTER"}, values.UnknownType, false)
	inner := plans.NewRecordQueryScanPlan([]string{"INNER"}, values.UnknownType, false)
	fm := plans.NewRecordQueryFlatMapPlan(outer, inner, leg, values.NamedCorrelationIdentifier("i"), rc, false)

	layout := buriedLegOrdinalLayout(fm)
	if layout == nil {
		t.Fatal("setup: expected a non-nil layout")
	}
	ord, ok := layout["LEG.ID"]
	if !ok {
		t.Fatal(`expected key "LEG.ID" to be present from the genuine bare field, got missing`)
	}
	if ord != 1 {
		t.Fatalf(`layout["LEG.ID"] = %d, want 1 (the genuine bare "leg.id" field at slot 1) — `+
			"the fused nested field at slot 0 must not have claimed this key first", ord)
	}
}

// TestRebaseOuterLegValue_DeclinesFusedNestedSameLeafName pins the same hole
// in rebaseOuterLegValue's own leg-match arm: a fused nested outer reference
// (leg.address.id) whose leaf name ("ID") collides with a plain column must
// not be rewritten into the qualified key "LEG.ID" — that would silently
// discard the ADDRESS accessor and re-anchor the predicate onto the WRONG
// column of the merged row. The node must come back unchanged (declined),
// exactly like any other shape this rebase does not recognize.
func TestRebaseOuterLegValue_DeclinesFusedNestedSameLeafName(t *testing.T) {
	t.Parallel()

	leg := "LEG"
	fused := fusedNestedFieldValue(values.NamedCorrelationIdentifier(leg), "ADDRESS", "ID")
	mergedCorr := values.NamedCorrelationIdentifier("MERGED")

	got := rebaseOuterLegValue(fused, []string{leg}, mergedCorr, nil)
	gotFV, ok := got.(*values.FieldValue)
	if !ok || gotFV != fused {
		t.Fatalf("rebaseOuterLegValue(fused LEG.ADDRESS.ID) = %#v, want the ORIGINAL unrewritten node — "+
			"a nested reference must never be re-anchored onto a colliding leaf-name qualified key", got)
	}
}
