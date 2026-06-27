package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// PartitionBinarySelectRule handles the special case of a
// SelectExpression with exactly 2 quantifiers. It rearranges
// predicates so that each predicate is evaluated at its leftmost
// possible position, creating sub-SelectExpressions that absorb
// predicates that can be pushed down.
//
// For each ordering (left, right) and (right, left), the rule
// attempts to push predicates toward the "left" side (which will
// become the outer of a nested loop join). Join predicates
// (referencing both sides) go to the right side (inner), since
// the left must be planned first. This creates a correlation from
// right to left, forcing the right side to be planned as the inner
// of a nested-loop join.
//
// Three cases:
//  1. One leg is correlated to the other — the correlated-to leg must
//     be planned as the outer. The rule only produces the valid ordering.
//  2. No correlation but join predicates — predicates are pushed to one
//     side, creating a new correlation. Both orderings are explored.
//  3. Completely independent — predicates go to whichever side they
//     correlate to. Both orderings are explored (producing equivalent results).
//
// The rule fires twice per pair of quantifiers (once for each assignment
// of left/right), since it matches quantifiers via exactlyInAnyOrder.
// Go implements this by iterating over both orderings explicitly.
//
// Ports Java's PartitionBinarySelectRule (ExplorationCascadesRule).
type PartitionBinarySelectRule struct {
	matcher matching.BindingMatcher
}

func NewPartitionBinarySelectRule() *PartitionBinarySelectRule {
	return &PartitionBinarySelectRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("partition_binary_select"),
	}
}

func (r *PartitionBinarySelectRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PartitionBinarySelectRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)
	quantifiers := sel.GetQuantifiers()

	if len(quantifiers) != 2 {
		return
	}

	// Only fire on inner joins. Java's Cascades SelectExpression models
	// inner-join semantics — outer joins are a Go extension carried on the
	// joinType field. Partitioning a LEFT/RIGHT OUTER join into correlated
	// sub-Selects pushes WHERE/ON predicates below the join, which is wrong:
	// a predicate on the nullable side (e.g. p.id IS NULL) must run AFTER
	// the null-fill, not as a pre-join filter. Keep all predicates at the
	// join level so ImplementNestedLoopJoinRule plans a non-correlated NLJ
	// with post-join predicate evaluation. Mirrors PushFilterBelowJoinRule,
	// which also guards on JoinInner.
	if sel.GetJoinType() != expressions.JoinInner {
		return
	}

	// Only partition binary joins (both ForEach). Existential quantifiers
	// (EXISTS subqueries) have special alias semantics that break when
	// wrapped in sub-SelectExpressions — the ExistentialValuePredicate references
	// the existential's alias, and wrapping would change the structure
	// downstream rules expect. Java handles this in the exploration
	// phase where Memo-level dedup prevents interference; Go's fixpoint
	// architecture requires an explicit guard.
	for _, q := range quantifiers {
		if q.Kind() != expressions.QuantifierForEach {
			return
		}
	}

	// An UNCORRELATED Explode leg is the IN-list shape (`col IN (v1,v2,…)` → a
	// SelectExpression with an Explode over the constant list) — owned by
	// ImplementInJoinRule, not the binary-join partition path. Partitioning it
	// pushes the IN-match predicate (`col = <explode binding>`) into the Explode
	// leg: that predicate's table column is a FLAT FieldValue, so its correlation
	// reports only the explode binding (not the table), and the predicate is
	// routed into the Explode leg where the table column is unbound — the
	// materialized NLJ then embeds it → 0 rows (TestFDB_GroupByWithWherePush /
	// the IN-with-GROUP-BY cases). A CORRELATED Explode (a lateral array UNNEST,
	// `FROM t, t.arr AS x`) genuinely partitions and is left alone. Mirrors the
	// same uncorrelated-Explode guard in ImplementNestedLoopJoinRule.
	for i, q := range quantifiers {
		other := quantifiers[1-i]
		if getExplodeExpression(q.GetRangesOver()) != nil &&
			!referenceIsCorrelatedTo(q.GetRangesOver(), other.GetAlias()) {
			return
		}
	}

	// Idempotency guard: if the Reference already contains the predicate-less
	// partition of THIS select — a 2-quantifier SelectExpression with no
	// predicates over the SAME quantifier alias set — then this rule (or a
	// previous iteration) has already pushed THIS select's predicates into
	// sub-SelectExpressions. Re-firing would re-create it, and the cycle
	// PartitionBinary → SelectMerge → PartitionBinary would grow the memo
	// without bound (MaxTasks cap hit). Java has no such guard — it relies on
	// memo interning to dedup the identical re-partition; Go's fixpoint needs
	// the explicit check.
	//
	// The match is on the QUANTIFIER ALIAS SET, not "any predicate-less binary
	// in the group": a join group holds several DISTINCT bipartitions of the
	// same N-way join (e.g. {$m(t1⋈t2), t3} and {$m(t2⋈t3), t1}), each with a
	// different alias set. Blocking on "any predicate-less binary" stopped every
	// merge-quantifier upper from ever being partitioned, so the correlated
	// index-probe FlatMap chain for ≥3-way joins was never enumerated (the inner
	// materialized as a full-scan NLJ instead). Scoping the guard to THIS
	// select's own alias set breaks the cycle (the same partition can't be
	// re-created) while leaving the sibling bipartitions free to partition.
	selAliases := quantifierAliasSet(sel)
	for _, m := range call.Reference.Members() {
		if other, ok := m.(*expressions.SelectExpression); ok && other != sel {
			if len(other.GetQuantifiers()) == 2 && !other.HasPredicates() &&
				aliasSetsEqual(selAliases, quantifierAliasSet(other)) {
				return
			}
		}
	}

	// Try both orderings: (q0 as left, q1 as right) and (q1 as left, q0 as right).
	// Java achieves this via the exactlyInAnyOrder matcher which binds both permutations.
	for _, ordering := range [][2]int{{0, 1}, {1, 0}} {
		leftQ := quantifiers[ordering[0]]
		rightQ := quantifiers[ordering[1]]
		r.tryPartition(call, sel, leftQ, rightQ)
	}
}

// quantifierAliasSet returns the set of quantifier aliases of a SelectExpression.
func quantifierAliasSet(sel *expressions.SelectExpression) map[values.CorrelationIdentifier]struct{} {
	out := make(map[values.CorrelationIdentifier]struct{}, len(sel.GetQuantifiers()))
	for _, q := range sel.GetQuantifiers() {
		out[q.GetAlias()] = struct{}{}
	}
	return out
}

// aliasSetsEqual reports whether two alias sets contain exactly the same aliases.
func aliasSetsEqual(a, b map[values.CorrelationIdentifier]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func (r *PartitionBinarySelectRule) tryPartition(
	call *ExpressionRuleCall,
	sel *expressions.SelectExpression,
	leftQuantifier, rightQuantifier expressions.Quantifier,
) {
	leftAlias := leftQuantifier.GetAlias()
	rightAlias := rightQuantifier.GetAlias()

	fullCorrelationOrder := computeTransitiveCorrelationOrder(sel.GetQuantifiers())

	leftDeps := fullCorrelationOrder[leftAlias]
	if _, leftDependsOnRight := leftDeps[rightAlias]; leftDependsOnRight {
		return
	}

	var leftPredicates []predicates.QueryPredicate
	var rightPredicates []predicates.QueryPredicate

	// Route each CONJUNCT independently, not the predicate list opaquely. Go
	// stores a multi-conjunct WHERE as a single AndPredicate; Java's
	// SelectExpression keeps a FLAT conjunct list, so Java's identical routing
	// loop splits `o.fk = c.id AND o.id = 5` into a join conjunct (→ the inner
	// leg, creating the correlation) and a selective conjunct (→ the outer leg,
	// where it SARGs the driver's scan). Without flattening, the whole And —
	// correlated to BOTH sides — lands on one leg, so the selective conjunct
	// never reaches the outer leg as a separate SARGable predicate and the driver
	// stays a full scan (the index-nested-loop's selective driver is lost; this is
	// the selective-driver SARG the retired tryFlatMapPlan used to hand-roll).
	// PartitionSelectRule (the ≥3-way twin) already flattens the same way.
	for _, pred := range flattenConjuncts(sel.GetPredicates()) {
		correlatedTo := predicates.GetCorrelatedToOfPredicate(pred)
		// Re-expose the buried leg aliases of any source-anchored join RC the
		// predicate reads through (GetCorrelatedToOfPredicate HIDES them for
		// exploration-budget reasons). A spanning predicate like `c.c_bid = b.bid`
		// reads B's column through a (B⋈A) merge RC; without un-hiding B, its
		// correlation reports only the merge alias, so it is routed into the merge
		// leg instead of creating the correlation to the OTHER leg — the materialized
		// NLJ then embeds it with B unbound → 0 rows
		// (TestFDB_JoinMerge_OuterColumn_NotDropped). PartitionSelectRule (the ≥3-way
		// twin) layers the same re-exposure on its classification.
		predicates.AddMergeSeedAliases(pred, correlatedTo)
		if _, ok := correlatedTo[rightAlias]; ok {
			rightPredicates = append(rightPredicates, pred)
		} else {
			leftPredicates = append(leftPredicates, pred)
		}
	}

	// Only proceed if the partitioning is useful.
	if len(leftPredicates) == 0 && len(rightPredicates) == 0 {
		return
	}

	// Build the new left quantifier (possibly wrapped in a new Select
	// with absorbed predicates).
	var newLeftQuantifier expressions.Quantifier
	if len(leftPredicates) == 0 {
		newLeftQuantifier = leftQuantifier
	} else {
		leftBuilder := NewGraphExpansionBuilder()
		leftBuilder.AddQuantifier(leftQuantifier)
		for _, p := range leftPredicates {
			leftBuilder.AddPredicate(p)
		}

		var leftSelectExpr *expressions.SelectExpression
		if leftQuantifier.Kind() == expressions.QuantifierForEach {
			// buildSimpleSelectOverQuantifier: result value = quantifier's
			// flowed object value.
			leftSelectExpr = leftBuilder.Build().Seal().BuildSelectWithResultValue(
				leftQuantifier.GetFlowedObjectValue(),
			)
		} else {
			leftBuilder.AddColumn("", values.LiteralValue(int64(1)))
			leftSelectExpr = leftBuilder.Build().Seal().BuildSelect()
		}
		newLeftQuantifier = expressions.NamedForEachQuantifier(
			leftQuantifier.GetAlias(),
			call.MemoizeExpression(leftSelectExpr),
		)
	}

	// Build the new right quantifier (possibly wrapped in a new Select
	// with absorbed predicates).
	var newRightQuantifier expressions.Quantifier
	if len(rightPredicates) == 0 {
		newRightQuantifier = rightQuantifier
	} else {
		rightBuilder := NewGraphExpansionBuilder()
		rightBuilder.AddQuantifier(rightQuantifier)
		for _, p := range rightPredicates {
			rightBuilder.AddPredicate(p)
		}

		var rightSelectExpr *expressions.SelectExpression
		if rightQuantifier.Kind() == expressions.QuantifierForEach {
			rightSelectExpr = rightBuilder.Build().Seal().BuildSelectWithResultValue(
				rightQuantifier.GetFlowedObjectValue(),
			)
		} else {
			rightBuilder.AddColumn("", values.LiteralValue(int64(1)))
			rightSelectExpr = rightBuilder.Build().Seal().BuildSelect()
		}
		newRightQuantifier = expressions.NamedForEachQuantifier(
			rightQuantifier.GetAlias(),
			call.MemoizeExpression(rightSelectExpr),
		)
	}

	// Propagate sourceAliases in the new quantifier order. After alias
	// unification, quantifier aliases ARE the source aliases.
	la := leftQuantifier.GetAlias().Name()
	ra := rightQuantifier.GetAlias().Name()
	var newAliases []string
	if la != "" && ra != "" {
		newAliases = []string{la, ra}
	}

	newSelectExpr := expressions.NewSelectExpressionWithJoinType(
		sel.GetResultValue(),
		[]expressions.Quantifier{newLeftQuantifier, newRightQuantifier},
		nil,
		newAliases,
		sel.GetJoinType(),
	)

	call.Yield(newSelectExpr)
}

var _ ExpressionRule = (*PartitionBinarySelectRule)(nil)
