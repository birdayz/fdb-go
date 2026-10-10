// Portions derived from FoundationDB Record Layer (RewriteOuterJoinRule.java,
// OuterJoinExpression.java, Expression.java, SelectExpression.java,
// and others),
// Copyright 2015-2020 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2024 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RewriteOuterJoinRule canonicalizes a LEFT OUTER join SelectExpression into the
// nested form Java uses (rules/RewriteOuterJoinRule.java + expressions/OuterJoin
// Expression.java): the ON-predicates are pushed BELOW the null-extension boundary
// into a correlated null-supplying inner SELECT, and the join becomes an INNER
// SelectExpression over the preserved leg plus a NULL-on-empty quantifier carrying
// the outer-join semantics.
//
// Go encodes an outer join as a flat 2-quantifier SelectExpression with
// joinType=JoinLeftOuter and the ON-predicates in the top-level predicate list
// (the translator keeps WHERE *above* the join for LEFT OUTER, so every predicate
// here is an ON-predicate). Java's RewriteOuterJoinRule matches an
// OuterJoinExpression; Go matches the LEFT-OUTER SelectExpression directly, since
// Go carries outer-join on the flag rather than a dedicated logical box. That is
// the only deviation from a 1:1 port, forced by Go's representation.
//
// Why this is needed (RFC-150 Phase-2b Piece 2): without it, neither
// PartitionBinarySelectRule nor PushFilterBelowJoinRule fires (both guard on
// JoinInner), so no leg becomes correlated, and the data-access FlatMap path
// (yieldGeneralFlatMap) never produces a correlated LEFT-OUTER FlatMap. This rule
// creates the correlation Java's single path relies on — it is what let the former
// Go-only tryFlatMapPlan (a hand-rolled inner-pushed-residual shortcut) be retired:
// the correlated LEFT-OUTER FlatMap now emerges from the standard data-access path.
//
// Correctness (the LEFT-OUTER axis): the ON-predicates MUST filter the inner stream
// BEFORE the empty→NULL extension. Folding them into innerSelect (below the
// NULL-on-empty quantifier) does exactly that — applying an ON-predicate above the
// FlatMap, after the NULL row is injected, would drop that row and silently degrade
// LEFT OUTER into INNER. The outer SelectExpression carries NO predicates, so there
// is nothing left to apply above the join. Mirrors Java RewriteOuterJoinRule.
type RewriteOuterJoinRule struct {
	matcher matching.BindingMatcher
}

// NewRewriteOuterJoinRule constructs the rule.
func NewRewriteOuterJoinRule() *RewriteOuterJoinRule {
	return &RewriteOuterJoinRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("outer_join_select").WithRootPredicate(
			func(sel *expressions.SelectExpression) bool { return sel.GetJoinType() == expressions.JoinLeftOuter },
		),
	}
}

// Matcher returns the pattern.
var _ ExpressionRule = (*RewriteOuterJoinRule)(nil)

func (r *RewriteOuterJoinRule) ConstraintDependencies() []any { return nil }

func (r *RewriteOuterJoinRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch rewrites a LEFT OUTER SelectExpression into the INNER + null-on-empty form.
func (r *RewriteOuterJoinRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)

	// LEFT OUTER only. RIGHT OUTER is normalized to LEFT-with-swapped-children in the
	// translator; FULL OUTER stays on the materialized NestedLoopJoin (Java has no FULL
	// in Cascades — it tracks global inner-match state for the drain phase, which a
	// FlatMap cannot do).
	if sel.GetJoinType() != expressions.JoinLeftOuter {
		return
	}
	quantifiers := sel.GetQuantifiers()

	// The outer join is a PAIR, and the pair is what this rule rewrites. A select
	// may carry additional EXISTENTIAL quantifiers beside it — `dept d LEFT JOIN
	// emp e ON … WHERE EXISTS (…)`, or the projected `SELECT …, EXISTS (…) AS f` —
	// and those are semi-join filters riding ABOVE the null-extension, not sides
	// of the join. They are carried to the rewritten outer select untouched.
	//
	// This used to require EXACTLY two quantifiers, which left a three-quantifier
	// LEFT OUTER select owned by nobody: this rule refused it on arity,
	// PartitionSelectRule refuses any non-INNER join type, and the NLJ rule is
	// binary. It was reachable only through that rule's three-quantifier arm, and
	// retiring the arm (RFC-235) turned the shape into a planner decline that Java
	// answers — measured in
	// conformance/projected_exists_left_join_java_probe_test.go.
	var forEachQuants []expressions.Quantifier
	var existentialQuants []expressions.Quantifier
	for _, q := range quantifiers {
		switch q.Kind() {
		case expressions.QuantifierForEach:
			forEachQuants = append(forEachQuants, q)
		case expressions.QuantifierExistential:
			existentialQuants = append(existentialQuants, q)
		default:
			// Any other quantifier kind is outside this rewrite's contract.
			return
		}
	}
	if len(forEachQuants) != 2 {
		return
	}
	preserved := forEachQuants[0]     // left leg = preserved side
	nullSupplying := forEachQuants[1] // right leg = null-supplying side
	// StrictSingle is a semantic edge contract, and this rewrite replaces the
	// null-supplying edge with a newly constructed null-on-empty quantifier.
	// Neither that replacement nor the preserved-edge outer-join rewrite has a
	// strict cardinality authority. Treat the flag as a rewrite barrier rather
	// than relying on today's scalar seed being predicate-less.
	if hasStrictSingleQuantifier(quantifiers) {
		return
	}

	// Split the predicates by which side of the null-extension they belong on.
	//
	// An ON-predicate must filter the inner stream BEFORE the empty→NULL extension,
	// so it goes below. A predicate touching an EXISTENTIAL is a WHERE — it is
	// applied to the joined row, after the extension — so it stays above. Applying
	// an ON-predicate above would drop the null-extended row and silently degrade
	// LEFT OUTER to INNER; pushing a WHERE-EXISTS below would evaluate the
	// semi-join against the unextended stream.
	// Every alias this select declares. Used twice: to keep the buried-alias map
	// below from folding a SELECT alias into an existential, and to fail closed on
	// an existential correlating to a sibling the box does not contain.
	selQuantifierAliases := make(map[values.CorrelationIdentifier]struct{}, len(quantifiers))
	for _, q := range quantifiers {
		selQuantifierAliases[q.GetAlias()] = struct{}{}
	}
	existentialAliases := make(map[values.CorrelationIdentifier]struct{}, len(existentialQuants))
	for _, q := range existentialQuants {
		existentialAliases[q.GetAlias()] = struct{}{}
	}
	// A legacy programmatic predicate may name a source inside an existential.
	// Its owner must stay above null-extension, not become an ON predicate.
	buriedToExistential := make(map[values.CorrelationIdentifier]values.CorrelationIdentifier)
	for _, q := range existentialQuants {
		for buried := range boundAliasesOfReference(q.GetRangesOver()) {
			if _, isSelectAlias := selQuantifierAliases[buried]; isSelectAlias {
				continue
			}
			buriedToExistential[buried] = q.GetAlias()
		}
	}
	var preds []predicates.QueryPredicate
	var abovePreds []predicates.QueryPredicate
	for _, p := range sel.GetPredicates() {
		above := false
		for alias := range predicates.GetCorrelatedToOfPredicate(p) {
			if _, isExistential := existentialAliases[alias]; isExistential {
				above = true
				break
			}
			if _, buried := buriedToExistential[alias]; buried {
				above = true
				break
			}
		}
		if above {
			abovePreds = append(abovePreds, p)
			continue
		}
		preds = append(preds, p)
	}

	// Every outer join is rewritten, whatever its ON reads and whether it has an
	// ON at all, as Java's rule rewrites every OuterJoinExpression
	// (RewriteOuterJoinRule.java:80-113). The rewritten inner of an ON that does
	// not read the preserved leg is uncorrelated; the materialized NLJ declines a
	// null-on-empty leg, so that inner is never joined as INNER, and the
	// LEFT-OUTER-typed select stays a member beside it, whose own lowering places
	// the ON conjuncts below the null-extension as the rewrite does. A leg whose
	// correlation is buried in a merged preserved side is rebased onto the merge
	// by ImplementNestedLoopJoinRule.yieldGeneralFlatMap, which declines the probe
	// when it cannot guarantee that rewire (RFC-153), leaving the materialized NLJ.

	// Idempotency: if this Reference already holds the rewritten form, don't
	// re-fire. Re-exploration fires it on the same Reference again, and every
	// firing mints a fresh
	// UniqueCorrelationIdentifier — so a rewritten form the guard cannot
	// recognize is a structurally distinct NEW member on every pass, which is
	// unbounded memo growth for exactly the shape it failed to see.
	//
	// TWO forms are recognized, because the rewrite emits two shapes:
	//
	//   FLAT (no existentials): the null-on-empty quantifier sits at TOP LEVEL
	//   and the arity equals the original select's.
	//
	//   BOXED (existentials present): the outer pair is wrapped into ONE box
	//   quantifier, so the arity is 1+E and the null-on-empty lives INSIDE the
	//   box. A top-level arity-and-flag test can see neither, so testing only the
	//   flat shape left the boxed one re-firing on every pass.
	for _, m := range call.Reference.Members() {
		if other, isSelect := m.(*expressions.SelectExpression); isSelect && other != sel {
			if isRewrittenOuterJoinForm(other, len(quantifiers), len(existentialQuants)) {
				return
			}
		}
	}

	// buildInnerSelect: wrap the null-supplying leg in a SUBSEL carrying ALL the
	// ON-predicates, then expose it through a NULL-on-empty quantifier that REUSES the
	// null-supplying alias (so the outer result value, which references that alias,
	// stays correctly correlated). The ON-predicate's reference to the preserved alias
	// makes innerSelect correlated to the preserved leg — exactly the rightDepsLeft
	// shape the data-access FlatMap path consumes.
	builder := NewGraphExpansionBuilder()
	builder.AddQuantifier(nullSupplying)
	for _, p := range preds {
		builder.AddPredicate(p)
	}
	flowed, err := nullSupplying.RequireFlowedObjectValue()
	if err != nil {
		call.Fail(err)
		return
	}
	innerSelect, err := builder.Build().Seal().BuildSelectWithResultValue(flowed)
	if err != nil {
		call.Fail(err)
		return
	}
	nullOnEmptyQun := expressions.NamedForEachNullOnEmptyQuantifier(
		nullSupplying.GetAlias(),
		call.MemoizeExpression(innerSelect),
	)

	// Source aliases are POSITIONAL against a quantifier list, so each rewritten
	// select gets its OWN slice: the box's legs and the outer select's
	// quantifiers are different lists, and sharing one slice between them is how
	// an entry ends up naming the wrong quantifier.
	//
	// legAliases names the box's two legs, in box order. It is also the flat
	// (no-existential) form's slice, where those two ARE the whole select.
	la := preserved.GetAlias().Name()
	ra := nullSupplying.GetAlias().Name()
	var legAliases []string
	if la != "" && ra != "" {
		legAliases = []string{la, ra}
	}

	// Ordinalization of a LEFT/RIGHT box happens at TRANSLATION: for a
	// merge of ≥2 legs, translation builds the declaration-order ordinal
	// seed with the null-supplying leg marked RECORD-nullable. The box's
	// result value flows through this rewrite UNCHANGED — a raw positional
	// RC, null-extended by the executor when the null-supplying leg is
	// empty. No rewrite-time reconversion: rebuilding a seed here would
	// stamp per-column nullability, contradicting the record-level
	// nullable wrap the seed carries.
	resultValue := sel.GetResultValue()

	// With NO existentials this is the flat two-quantifier form: the outer select
	// is INNER (outer-join semantics live entirely in the null-on-empty
	// quantifier) and carries no predicates.
	if len(existentialQuants) == 0 {
		outerSelect, err := expressions.NewSelectExpressionWithJoinType(
			resultValue,
			[]expressions.Quantifier{preserved, nullOnEmptyQun},
			nil,
			legAliases,
			expressions.JoinInner,
		)
		if err != nil {
			call.Fail(err)
			return
		}
		call.Yield(outerSelect)
		return
	}

	// WITH existentials the join is BOXED into a single quantifier, which is
	// Java's shape rather than a Go invention: Java's outer join is an
	// OuterJoinExpression holding both sides internally, so an enclosing select
	// sees ONE quantifier and `dept LEFT JOIN emp … EXISTS (…)` is BINARY —
	// exactly what ImplementNestedLoopJoinRule matches
	// (`exactlyInAnyOrder`, ImplementNestedLoopJoinRule.java:98).
	//
	// Go's flat encoding has to reach that shape by construction, because it
	// cannot reach it by bipartition. Every route was measured and every one is
	// closed: PartitionSelectRule's usefulness check kills every single-quantifier
	// lower (a PROJECTED existential contributes no predicate to push down), the
	// {preserved, nullSupplying} lower dies on Java's own
	// `lowersCorrelatedToByUppers != lowerAliasCorrelatedToByUpperAliases` guard
	// (the result needs the preserved leg, the existential depends on the
	// null-supplying one, and Case 2 flows only ONE lower's row), and the ≥2
	// positional-merge arm DECLINES a null-on-empty leg outright by design
	// (positional_merge.go:36-53 — the null-extension is per-outer-row and must
	// not be collapsed). Java never meets any of this because Java never
	// partitions an outer join at all.
	//
	// The box's own row is the positional merge of the two sides, and every
	// reference the enclosing select made to either side is re-anchored onto it
	// by ordinal — the same TranslationMap shape PartitionSelectRule.java:296-303
	// builds, and the same one positionalMergeCase builds for the flat case.
	boxLegs := []expressions.Quantifier{preserved, nullOnEmptyQun}
	// The box IS a positional merge, so it gets positionalMergeCase's slot-typing
	// discipline rather than a bare RequireFlowedObjectValue. The three parts are
	// the same three, for the same reasons stated at that site:
	//
	//   SCAVENGE — a slot that enters the record constructor stating no
	//   *RecordType strips the leg type the executor's span recovery resolves
	//   through, and it does so SILENTLY: the reference stays source-relative, a
	//   source-relative operand pushed into a leg's scan as a SARG evaluates to
	//   NULL against the build-bound row, and the join returns zero rows with no
	//   error. legRowTypes scavenges the select's own value surfaces for the one
	//   planner-constructed typed leg QOV.
	//
	//   CENSUS — the same instrument, so the box's slots are measured too. Before
	//   this, recordMergeSlotTyping had exactly two call sites, both in
	//   positional_merge.go, which made "the census measures the positional merge"
	//   a claim about ONE of the two positional merges in the tree.
	//
	//   DECLINE, NOT FAIL — a quantifier whose reference has members flowing
	//   different row shapes is reachable without a real defect (the untyped-member
	//   reporting gap), so it is counted and witnessed and the rule declines.
	//   call.Fail aborts the whole planner run, which turns a shape this rule
	//   simply cannot rewrite into a failed query.
	boxLegTypes := legRowTypes(resultValue, sel.GetPredicates())
	boxFields := make([]values.RecordConstructorField, len(boxLegs))
	boxTypeFields := make([]values.Field, len(boxLegs))
	for i, leg := range boxLegs {
		fov, fovErr := leg.RequireFlowedObjectValue()
		if fovErr != nil {
			recordMergeSlotTypeDisagreement(fovErr)
			return
		}
		scavenged := false
		if _, typed := fov.Type().(*values.RecordType); !typed {
			if rt := boxLegTypes[leg.GetAlias()]; rt != nil {
				scavengedFOV, scavengeErr := values.NewQuantifiedObjectValue(leg.GetAlias(), rt)
				if scavengeErr != nil {
					call.Fail(scavengeErr)
					return
				}
				fov = scavengedFOV
				scavenged = true
			}
		}
		if values.LegIdentityCensusEnabled() {
			recordMergeSlotTyping(leg.GetAlias(), fov.Type(), scavenged)
		}
		boxFields[i] = values.RecordConstructorField{Name: values.OrdinalFieldName(i), Value: fov}
		boxTypeFields[i] = values.Field{Name: values.OrdinalFieldName(i), FieldType: fov.Type(), Ordinal: i}
	}
	// Source aliases are POSITIONAL against the quantifier list, so the box gets
	// exactly its two legs' names. Handing it the full slice — which is sized for
	// the ENCLOSING select — over-declares, and rule_implement_nested_loop_join.go
	// harvests GetSourceAliases() into a DECLARED set.
	var boxAliases []string
	if len(legAliases) >= len(boxLegs) {
		boxAliases = legAliases[:len(boxLegs)]
	}
	boxSelect, err := expressions.NewSelectExpressionWithJoinType(
		values.NewRawRecordConstructorValue(boxFields...),
		boxLegs,
		nil,
		boxAliases,
		expressions.JoinInner,
	)
	if err != nil {
		call.Fail(err)
		return
	}
	boxAlias := values.UniqueCorrelationIdentifier()
	boxQ := expressions.NamedForEachQuantifier(boxAlias, call.MemoizeExpression(boxSelect))

	// Carry explicit runtime source bindings into the outer select: programmatic
	// selects can declare them independently of the existential quantifier alias.
	//
	// All-or-nothing: an unnamed entry makes every LATER position name the wrong
	// quantifier, so the slice is dropped rather than truncated. Dropping it
	// cannot reach legAliases — separating the two slices is what stops an
	// unnamed existential from stripping the box's own declared sources.
	srcAliases := sel.GetSourceAliases()
	quantAliasToSource := make(map[values.CorrelationIdentifier]string, len(quantifiers))
	for i, q := range quantifiers {
		if i < len(srcAliases) {
			quantAliasToSource[q.GetAlias()] = srcAliases[i]
		}
	}
	outerAliases := []string{boxAlias.Name()}
	for _, q := range existentialQuants {
		n, carried := quantAliasToSource[q.GetAlias()]
		if !carried || n == "" {
			n = q.GetAlias().Name()
		}
		if n == "" {
			outerAliases = nil
			break
		}
		outerAliases = append(outerAliases, n)
	}

	boxQOV, err := values.NewQuantifiedObjectValue(boxAlias, &values.RecordType{Fields: boxTypeFields})
	if err != nil {
		call.Fail(err)
		return
	}
	tb := values.NewTranslationMapBuilder()
	for i, leg := range boxLegs {
		slot, slotErr := values.ResolveFieldOrdinals(boxQOV, []int{i})
		if slotErr != nil {
			call.Fail(slotErr)
			return
		}
		tb = tb.When(leg.GetAlias()).Then(func(_ values.CorrelationIdentifier, _ values.Value) values.Value {
			return slot
		})
	}
	boxMap := tb.Build()

	boxedResult, err := values.TranslateCorrelationsChecked(resultValue, boxMap)
	if err != nil {
		call.Fail(err)
		return
	}
	boxedPreds := make([]predicates.QueryPredicate, len(abovePreds))
	for i, p := range abovePreds {
		translated, translateErr := predicates.TranslateLeafPredicatesChecked(p, boxMap)
		if translateErr != nil {
			call.Fail(translateErr)
			return
		}
		boxedPreds[i] = translated
	}

	// THE EXISTENTIALS ARE APPENDED UNTRANSLATED, AND THAT IS A CLAIM, SO IT IS
	// CHECKED HERE.
	//
	// resultValue and abovePreds are re-anchored through boxMap above. An
	// existential quantifier's ranged-over subgraph is NOT: there is no
	// Reference-level correlation translation in this tree, and its body still
	// names the null-supplying leg that is now buried inside the box (`r.id =
	// q.qid`, where q is boxed).
	//
	// It resolves at runtime through a DECLARED channel, not a widened namespace.
	// The box's physical plan publishes an output layout whose WindowSources are
	// its two legs, and executor.bindOuterLayoutSources binds every declared
	// source by alias before the inner runs — failing LOUD ("outer layout omitted
	// declared source %q") if one is missing. That is categorically unlike the
	// retired bindMergedOuterLegs, which bound sibling aliases nobody declared.
	//
	// What that channel cannot serve is a correlation naming something the box
	// does not contain, so the rule fails closed on it rather than emitting a
	// select whose Reference leaks a correlation nobody provides. Without this
	// check the leak would surface only as wrong rows, and only for shapes the
	// corpus does not have.
	boxedLegAliases := map[values.CorrelationIdentifier]struct{}{
		preserved.GetAlias():     {},
		nullSupplying.GetAlias(): {},
	}
	for _, ex := range existentialQuants {
		for corr := range ex.GetCorrelatedTo() {
			if _, boxed := boxedLegAliases[corr]; boxed {
				continue // served by the box's declared window sources
			}
			if _, sibling := selQuantifierAliases[corr]; !sibling {
				continue // external to this select; the enclosing context binds it
			}
			// A correlation to a SIBLING of this select that the box does not
			// contain: nothing in the rewritten shape binds it.
			return
		}
	}

	outerSelect, err := expressions.NewSelectExpressionWithJoinType(
		boxedResult,
		append([]expressions.Quantifier{boxQ}, existentialQuants...),
		boxedPreds,
		outerAliases,
		expressions.JoinInner,
	)
	if err != nil {
		call.Fail(err)
		return
	}
	call.Yield(outerSelect)
}

// isRewrittenOuterJoinForm reports whether other is already this rule's output,
// so a re-firing can decline instead of minting a structurally distinct twin.
//
// It is a named function rather than a closure because it IS the idempotency
// decision, and the decision needs a test that drives every arm. A corpus run
// walks only the arms the corpus happens to reach, and the arm that went
// unrecognized here was the one a pending change had just made reachable.
//
// originalArity is the firing select's quantifier count and existentialCount how
// many of those are existential — together they name the two shapes the rewrite
// emits:
//
//   - FLAT (existentialCount == 0): the null-on-empty quantifier is at TOP
//     LEVEL and the arity equals the original's.
//   - BOXED (existentialCount > 0): the outer pair is wrapped into ONE box
//     quantifier, so the arity is 1+E and the null-on-empty lives INSIDE the
//     box, where a top-level flag test cannot reach it.
//
// NOT covered, and each of these has a committed arm rather than an argument:
// a rewritten form whose join type is not INNER (the rewrite always emits
// INNER); a box nested more than one level down (the rewrite prepends its box
// directly, so a deeper one is somebody else's expression); and a box sitting
// anywhere but first (the rewrite prepends).
//
// A form outside that list is not recognized, so the rule re-fires — and because
// this rule is registered in two phases and mints a fresh
// UniqueCorrelationIdentifier per firing, that is UNBOUNDED memo growth, not one
// wasted member. It is still the direction to fail in: over-recognizing
// SUPPRESSES a legitimate rewrite, and a shape that never gets its plan is worse
// than a memo that grows. The remedy for an unrecognized shape is to add it here
// with an arm, not to loosen the test.
func isRewrittenOuterJoinForm(other *expressions.SelectExpression, originalArity, existentialCount int) bool {
	if other == nil || other.GetJoinType() != expressions.JoinInner {
		return false
	}
	quants := other.GetQuantifiers()
	if len(quants) == originalArity {
		for _, q := range quants {
			if q.IsNullOnEmpty() {
				return true
			}
		}
	}
	if len(quants) != 1+existentialCount || len(quants) == 0 {
		return false
	}
	ref := quants[0].GetRangesOver()
	if ref == nil {
		return false
	}
	for _, m := range ref.Members() {
		inner, isSelect := m.(*expressions.SelectExpression)
		if !isSelect {
			continue
		}
		for _, iq := range inner.GetQuantifiers() {
			if iq.IsNullOnEmpty() {
				return true
			}
		}
	}
	return false
}
