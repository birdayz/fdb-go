package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// prefixTestCandidate records the bindings it is handed and returns a prefix
// that NO generic sargable-alias walk would ever produce: it skips the first
// parameter and keeps a later one across the gap. That is deliberately
// unrealistic for a value index but structurally exactly what
// VectorIndexScanMatchCandidate does — it retains its DistanceRank binding
// (last in the parameter list) even when the partition prefix is partial.
type prefixTestCandidate struct {
	stubMatchCandidate
	sargables []values.CorrelationIdentifier
	// keep is the candidate's own answer, independent of order or contiguity.
	keep []values.CorrelationIdentifier
	// sawBindings captures what the candidate was actually called with.
	sawBindings *map[values.CorrelationIdentifier]*predicates.ComparisonRange
	calls       *int
}

func (c prefixTestCandidate) GetSargableAliases() []values.CorrelationIdentifier {
	return c.sargables
}

func (c prefixTestCandidate) ComputeBoundParameterPrefixMap(
	bindings map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) map[values.CorrelationIdentifier]*predicates.ComparisonRange {
	*c.calls++
	*c.sawBindings = bindings
	out := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange)
	for _, k := range c.keep {
		if cr, ok := bindings[k]; ok {
			out[k] = cr
		}
	}
	return out
}

func hazardEqRange(t *testing.T) *predicates.ComparisonRange {
	t.Helper()
	c := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(5))
	return predicates.EmptyComparisonRange().Merge(&c).Range
}

// TestGetBoundParameterPrefixMap_DelegatesToCandidate pins RFC-190 190.4c:
// what counts as a usable scan prefix is the CANDIDATE's decision, so
// PartialMatch must delegate to MatchCandidate.ComputeBoundParameterPrefixMap
// and hand it the full parameter binding map.
//
// This is not a style preference. The pre-fix implementation returned the
// whole binding map, over-claiming bindings no scan performs; an earlier
// attempt at this fix replaced that with a generic in-order sargable walk,
// which under-claims for candidates whose semantics are not "contiguous
// equality run" — it would drop VectorIndexScanMatchCandidate's DistanceRank
// binding whenever the partition prefix is partial, and ToScanPlan then emits
// a plan with no query vector. The candidate below keeps a binding across a
// gap for exactly that reason: only real delegation can produce this answer.
func TestGetBoundParameterPrefixMap_DelegatesToCandidate(t *testing.T) {
	t.Parallel()

	p0 := values.NamedCorrelationIdentifier("p0")
	p1 := values.NamedCorrelationIdentifier("p1")
	p2 := values.NamedCorrelationIdentifier("p2")
	eq := hazardEqRange(t)

	bindings := map[values.CorrelationIdentifier]*predicates.ComparisonRange{p0: eq, p1: eq, p2: eq}

	var saw map[values.CorrelationIdentifier]*predicates.ComparisonRange
	calls := 0
	scanExpr := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	mi := NewRegularMatchInfo(bindings, nil, nil, nil, nil, nil, nil, nil)
	pm := NewPartialMatch(
		EmptyAliasMap(),
		prefixTestCandidate{
			stubMatchCandidate: stubMatchCandidate{name: "idx"},
			sargables:          []values.CorrelationIdentifier{p0, p1, p2},
			keep:               []values.CorrelationIdentifier{p2}, // across the gap
			sawBindings:        &saw,
			calls:              &calls,
		},
		expressions.InitialOf(scanExpr), scanExpr,
		expressions.InitialOf(scanExpr), mi,
	)

	got := pm.GetBoundParameterPrefixMap()

	if calls != 1 {
		t.Fatalf("candidate.ComputeBoundParameterPrefixMap called %d times, want 1 (must delegate)", calls)
	}
	if len(saw) != len(bindings) {
		t.Fatalf("candidate saw %d bindings, want the full binding map (%d)", len(saw), len(bindings))
	}
	if len(got) != 1 {
		t.Fatalf("prefix = %v, want exactly the candidate's answer {p2}", keysOf(got))
	}
	if _, ok := got[p2]; !ok {
		t.Fatalf("prefix = %v, want the candidate's across-the-gap keep of p2; a generic in-order "+
			"sargable walk would have returned {p0, p1, p2} or {p0}", keysOf(got))
	}
}

// TestGetBoundParameterPrefixMap_NoCandidate pins the degenerate case: no
// candidate means nothing is bound, not a nil map consumers must nil-check.
func TestGetBoundParameterPrefixMap_NoCandidate(t *testing.T) {
	t.Parallel()

	scanExpr := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	mi := NewRegularMatchInfo(nil, nil, nil, nil, nil, nil, nil, nil)
	pm := NewPartialMatch(
		EmptyAliasMap(), nil,
		expressions.InitialOf(scanExpr), scanExpr, expressions.InitialOf(scanExpr), mi,
	)

	got := pm.GetBoundParameterPrefixMap()
	if got == nil {
		t.Fatal("prefix map must be empty, not nil")
	}
	if len(got) != 0 {
		t.Fatalf("prefix = %v, want empty", keysOf(got))
	}
}

func keysOf(m map[values.CorrelationIdentifier]*predicates.ComparisonRange) []values.CorrelationIdentifier {
	out := make([]values.CorrelationIdentifier, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestGetCompensatedAliases_IncludesPredicateOwnedLocalAliases pins the second
// half of Java's PartialMatch.computeCompensatedAliases: besides the matched
// quantifiers, every mapped query predicate contributes the correlations it
// references that this expression OWNS.
//
// Here the existential quantifier has no child match (so it is not a matched
// quantifier), but a mapped EXISTS predicate references it. Pre-fix, the alias
// was absent from the compensated set — a consumer would then treat it as a
// free alias and could double-apply or drop the compensation that owns it.
func TestGetCompensatedAliases_IncludesPredicateOwnedLocalAliases(t *testing.T) {
	t.Parallel()

	existAlias := values.NamedCorrelationIdentifier("qe")
	forEachQ := namedForEachQuantifier("q1")
	existQ := namedExistentialQuantifier("qe")

	// An EXISTS predicate correlated to the (unmatched) existential quantifier.
	existsPred := predicates.NewExistentialAlias(existAlias)
	sel := expressions.NewSelectExpression(
		nil,
		[]expressions.Quantifier{forEachQ, existQ},
		[]predicates.QueryPredicate{existsPred},
	)

	pmBuilder := NewPredicateMultiMapBuilder()
	pmBuilder.Put(existsPred, &PredicateMapping{
		mappingKey: NewMappingKey(existsPred, existsPred, MappingRegularImpliesCandidate),
	})
	mi := NewRegularMatchInfo(nil, nil, pmBuilder.Build(), nil, nil, nil, nil, nil)
	mi.SetChildPartialMatch(forEachQ.GetAlias(), hazardScanPM(t, nil, nil))

	pm := NewPartialMatch(
		EmptyAliasMap(),
		stubMatchCandidate{name: "idx"},
		expressions.InitialOf(sel), sel,
		expressions.InitialOf(sel), mi,
	)

	got := pm.GetCompensatedAliases()
	if _, ok := got[forEachQ.GetAlias()]; !ok {
		t.Fatalf("compensated aliases missing matched child alias %q", forEachQ.GetAlias().Name())
	}
	if _, ok := got[existAlias]; !ok {
		aliases := make([]string, 0, len(got))
		for a := range got {
			aliases = append(aliases, a.Name())
		}
		t.Fatalf("compensated aliases %v missing predicate-owned local alias %q", aliases, existAlias.Name())
	}
}

// TestGetCompensatedAliases_SeesRangeComparandCorrelations pins the second
// half of the same rule against a predicate shape whose correlations live
// only in its RANGE COMPARANDS.
//
// A PredicateWithValueAndRanges reports those through its own GetCorrelatedTo.
// The GetCorrelatedToOfPredicate helper used to be a manual type switch with
// no case for this shape and reported nothing at all, so the alias was invisible
// to compensation. Both routes must now agree.
func TestGetCompensatedAliases_SeesRangeComparandCorrelations(t *testing.T) {
	t.Parallel()

	forEachQ := namedForEachQuantifier("q1")
	rangeSourceQ := namedForEachQuantifier("q_range")
	rangeAlias := rangeSourceQ.GetAlias()

	// value IN {> QOV(q_range)} — the ONLY mention of q_range is the
	// comparand buried in the range constraint.
	comparand := predicates.Comparison{
		Type:    predicates.ComparisonGreaterThan,
		Operand: values.NewQuantifiedObjectValue(rangeAlias),
	}
	rc := predicates.NewRangeConstraints(nil, []predicates.Comparison{comparand})
	rangePred := predicates.NewPredicateWithValueAndRanges(
		values.NewQuantifiedObjectValue(forEachQ.GetAlias()),
		[]*predicates.RangeConstraints{rc},
	)

	if _, direct := rangePred.GetCorrelatedTo()[rangeAlias]; !direct {
		t.Fatal("setup wrong: the predicate itself does not report the range comparand alias")
	}
	// The shared helper must agree with the predicate — it is what the rest of
	// the planner (compensation apply, intersect, the join rules) calls.
	if _, viaHelper := predicates.GetCorrelatedToOfPredicate(rangePred)[rangeAlias]; !viaHelper {
		t.Fatal("GetCorrelatedToOfPredicate misses the range comparand alias; every caller " +
			"deciding which quantifiers to retain will leave it dangling")
	}

	sel := expressions.NewSelectExpression(
		nil,
		[]expressions.Quantifier{forEachQ, rangeSourceQ},
		[]predicates.QueryPredicate{rangePred},
	)

	pmBuilder := NewPredicateMultiMapBuilder()
	pmBuilder.Put(rangePred, &PredicateMapping{
		mappingKey: NewMappingKey(rangePred, rangePred, MappingRegularImpliesCandidate),
	})
	mi := NewRegularMatchInfo(nil, nil, pmBuilder.Build(), nil, nil, nil, nil, nil)

	pm := NewPartialMatch(
		EmptyAliasMap(), stubMatchCandidate{name: "idx"},
		expressions.InitialOf(sel), sel, expressions.InitialOf(sel), mi,
	)

	if _, ok := pm.GetCompensatedAliases()[rangeAlias]; !ok {
		t.Fatalf("compensated aliases missing %q — the alias is reachable only through "+
			"the predicate's range comparands", rangeAlias.Name())
	}
}

// TestGetCompensatedAliases_ExcludesForeignPredicateCorrelations pins the
// OTHER side of the same rule: a predicate correlation that this expression
// does NOT own (an outer/deeper alias) must not be claimed as compensated.
func TestGetCompensatedAliases_ExcludesForeignPredicateCorrelations(t *testing.T) {
	t.Parallel()

	foreign := values.NamedCorrelationIdentifier("outer_not_ours")
	forEachQ := namedForEachQuantifier("q1")
	foreignPred := predicates.NewExistentialAlias(foreign)

	sel := expressions.NewSelectExpression(
		nil,
		[]expressions.Quantifier{forEachQ},
		[]predicates.QueryPredicate{foreignPred},
	)

	pmBuilder := NewPredicateMultiMapBuilder()
	pmBuilder.Put(foreignPred, &PredicateMapping{
		mappingKey: NewMappingKey(foreignPred, foreignPred, MappingRegularImpliesCandidate),
	})
	mi := NewRegularMatchInfo(nil, nil, pmBuilder.Build(), nil, nil, nil, nil, nil)

	pm := NewPartialMatch(
		EmptyAliasMap(), stubMatchCandidate{name: "idx"},
		expressions.InitialOf(sel), sel, expressions.InitialOf(sel), mi,
	)

	if _, ok := pm.GetCompensatedAliases()[foreign]; ok {
		t.Fatalf("compensated aliases must not claim the un-owned alias %q", foreign.Name())
	}
}

// TestMatchedForEachAliasMaybe_RequiresExactlyOneBaseForEach pins the
// precondition for applying a compensation: the match must cover exactly the
// aliases it claims to compensate, and exactly one of them must be a ForEach.
//
// Applying means rebuilding the expression on a base quantifier over that
// alias. With zero matched ForEach quantifiers there is no alias to build on;
// with two there is no way to choose, and the loser's correlations dangle.
// Java asserts both and crashes; this must report the failure so the caller
// can drop the match instead of guessing.
func TestMatchedForEachAliasMaybe_RequiresExactlyOneBaseForEach(t *testing.T) {
	t.Parallel()

	fe1 := namedForEachQuantifier("q1")
	fe2 := namedForEachQuantifier("q2")
	ex := namedExistentialQuantifier("qe")

	for _, tc := range []struct {
		name       string
		matched    []expressions.Quantifier
		compensats map[values.CorrelationIdentifier]struct{}
		wantOK     bool
		wantAlias  values.CorrelationIdentifier
	}{
		{
			name:       "exactly_one_forEach_and_sets_agree",
			matched:    []expressions.Quantifier{fe1, ex},
			compensats: aliasesOf(fe1, ex),
			wantOK:     true,
			wantAlias:  fe1.GetAlias(),
		},
		{
			name:       "two_matched_forEach_has_no_single_base",
			matched:    []expressions.Quantifier{fe1, fe2},
			compensats: aliasesOf(fe1, fe2),
			wantOK:     false,
		},
		{
			name:       "no_matched_forEach_has_nothing_to_build_on",
			matched:    []expressions.Quantifier{ex},
			compensats: aliasesOf(ex),
			wantOK:     false,
		},
		{
			// Compensating an alias the match does not cover means the two
			// sets describe different things; the base alias is not derivable.
			name:       "compensated_alias_not_matched",
			matched:    []expressions.Quantifier{fe1},
			compensats: aliasesOf(fe1, ex),
			wantOK:     false,
		},
		{
			name:       "matched_alias_not_compensated",
			matched:    []expressions.Quantifier{fe1, ex},
			compensats: aliasesOf(fe1),
			wantOK:     false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			comp := NewForMatchCompensation(
				false, NoCompensation, NewPredicateCompensationMap(nil, nil),
				tc.matched, nil, tc.compensats,
				NoResultCompensation(), EmptyGroupByMappings(),
			)
			alias, ok := comp.MatchedForEachAliasMaybe()
			if ok != tc.wantOK {
				t.Fatalf("ok=%v, want %v (alias=%q)", ok, tc.wantOK, alias.Name())
			}
			if tc.wantOK && alias != tc.wantAlias {
				t.Fatalf("alias=%q, want %q", alias.Name(), tc.wantAlias.Name())
			}
		})
	}
}

// TestNestPullUp_FailsClosedOnNonSingletonCandidateRef pins that the pull-up
// chain is never built off a guessed candidate expression.
//
// A candidate reference holding several members means the match was proved
// against one of them, and nothing in the reference says which. Picking the
// first builds a pull-up for an expression the match may never have seen, and
// every value pulled through it is then silently wrong. Returning nil makes
// the caller degrade the match to an impossible compensation instead.
func TestNestPullUp_FailsClosedOnNonSingletonCandidateRef(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("cand")

	// Singleton reference: the chain builds.
	single := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
	mi := NewRegularMatchInfo(nil, EmptyAliasMap(), nil, nil, nil, EmptyGroupByMappings(), nil, nil)
	pmSingle := NewPartialMatch(
		EmptyAliasMap(), stubMatchCandidate{name: "idx"},
		single, single.Get(), single, mi,
	)
	if _, current := NestPullUp(pmSingle, nil, alias); current == nil {
		t.Fatal("a singleton candidate reference must yield a pull-up")
	}

	// Two members: which one was matched is unknowable, so fail closed.
	multi := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
	if !multi.Insert(expressions.NewFullUnorderedScanExpression([]string{"U"}, values.UnknownType)) {
		t.Fatal("setup: second member not inserted")
	}
	if len(multi.AllMembers()) != 2 {
		t.Fatalf("setup: reference has %d members, want 2", len(multi.AllMembers()))
	}
	pmMulti := NewPartialMatch(
		EmptyAliasMap(), stubMatchCandidate{name: "idx"},
		multi, multi.AllMembers()[0], multi, mi,
	)
	root, current := NestPullUp(pmMulti, nil, alias)
	if current != nil || root != nil {
		t.Fatal("a multi-member candidate reference must fail closed (nil pull-up), not guess a member")
	}

	// Nil candidate reference likewise.
	pmNil := NewPartialMatch(
		EmptyAliasMap(), stubMatchCandidate{name: "idx"},
		single, single.Get(), nil, mi,
	)
	if _, current := NestPullUp(pmNil, nil, alias); current != nil {
		t.Fatal("a nil candidate reference must fail closed")
	}

	// A reference can be structurally singleton while its only member is nil.
	// It is still not a candidate expression and must fail before ForMatch
	// dereferences it.
	nilMemberRef := expressions.InitialOf(nil)
	pmNilMember := NewPartialMatch(
		EmptyAliasMap(), stubMatchCandidate{name: "idx"},
		single, single.Get(), nilMemberRef, mi,
	)
	root, current = NestPullUp(pmNilMember, nil, alias)
	if root != nil || current != nil {
		t.Fatal("a singleton reference containing a nil member must fail closed")
	}
}

// TestNestPullUp_AdjustedChainBuildsOuterRootAndInnerCurrent pins the two-level
// adjusted path. Predicates start at the inner current level and must traverse
// its outer parent; result compensation starts at the outer root and must not
// traverse the inner level a second time.
func TestNestPullUp_AdjustedChainBuildsOuterRootAndInnerCurrent(t *testing.T) {
	t.Parallel()

	innerScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	innerRef := expressions.InitialOf(innerScan)
	innerAlias := values.NamedCorrelationIdentifier("candidate_inner")
	outerQ := expressions.NamedForEachQuantifier(innerAlias, innerRef)
	outerProjection := expressions.NewLogicalProjectionExpression(
		[]values.Value{outerQ.GetFlowedObjectValue()},
		outerQ,
	)
	outerRef := expressions.InitialOf(outerProjection)

	regular := NewRegularMatchInfo(
		nil, EmptyAliasMap(), nil, nil, nil, EmptyGroupByMappings(), nil, nil,
	)
	adjusted := NewAdjustedMatchInfo(regular, nil, nil, EmptyGroupByMappings())
	pm := NewPartialMatch(
		EmptyAliasMap(), stubMatchCandidate{name: "idx"},
		outerRef, outerProjection, outerRef, adjusted,
	)

	topAlias := values.NamedCorrelationIdentifier("candidate_top")
	root, current := NestPullUp(pm, nil, topAlias)
	if root == nil || current == nil {
		t.Fatal("two-level adjusted match did not produce both root and current pull-ups")
	}
	if root == current {
		t.Fatal("two-level adjusted match collapsed root and current into one level")
	}
	if root.GetCandidateAlias() != topAlias {
		t.Fatalf("root alias = %q, want %q", root.GetCandidateAlias().Name(), topAlias.Name())
	}
	if current.GetCandidateAlias() != innerAlias {
		t.Fatalf("current alias = %q, want %q", current.GetCandidateAlias().Name(), innerAlias.Name())
	}
	if current.GetParent() != root {
		t.Fatal("inner current pull-up must have the outer root as its parent")
	}

	assertPulledToTop := func(label string, pulled values.Value) {
		t.Helper()
		qov, ok := pulled.(*values.QuantifiedObjectValue)
		if !ok {
			t.Fatalf("%s pulled to %T, want *QuantifiedObjectValue", label, pulled)
		}
		if qov.Correlation != topAlias {
			t.Fatalf("%s pulled to alias %q, want %q", label, qov.Correlation.Name(), topAlias.Name())
		}
	}

	// Predicate/child values begin at the innermost level and traverse both
	// current and root.
	assertPulledToTop("inner value", current.PullUpValueMaybe(innerScan.GetResultValue()))
	// Query result values begin at the root level and traverse it exactly once.
	assertPulledToTop("outer result", root.PullUpValueMaybe(outerProjection.GetResultValue()))
}

// TestCompensateCompleteMatch_AdjustedResultUsesWrapperMapAndRoot exercises the
// adjusted path through the public compensation entry point. The wrapper and
// underlying MaxMatchMaps deliberately live in different candidate namespaces:
// only the wrapper query value can be pulled through the outer root level.
//
// Reading the underlying regular map makes result compensation impossible.
// Starting at the inner current level does too. The valid combination is the
// adjusted wrapper map at the outer root, which translates to the top alias and
// therefore needs no result-shape compensation.
func TestCompensateCompleteMatch_AdjustedResultUsesWrapperMapAndRoot(t *testing.T) {
	t.Parallel()

	innerScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	innerRef := expressions.InitialOf(innerScan)
	innerAlias := values.NamedCorrelationIdentifier("candidate_inner")
	outerQ := expressions.NamedForEachQuantifier(innerAlias, innerRef)
	outerProjection := expressions.NewLogicalProjectionExpression(
		[]values.Value{outerQ.GetFlowedObjectValue()},
		outerQ,
	)
	outerRef := expressions.InitialOf(outerProjection)

	// This value cannot be pulled through the outer projection. If
	// ComputeResultCompensation accidentally reads the underlying regular map,
	// the complete match becomes impossible.
	underlyingQuery := values.LiteralValue("wrong-level")
	regular := NewRegularMatchInfo(
		nil,
		EmptyAliasMap(),
		nil,
		nil,
		NewMaxMatchMap(
			map[values.Value]values.Value{underlyingQuery: innerScan.GetResultValue()},
			underlyingQuery,
			innerScan.GetResultValue(),
		),
		EmptyGroupByMappings(),
		nil,
		nil,
	)

	// The adjusted query value is expressed at the outer candidate level. It
	// pulls through the root projection to QOV(candidate_top), which is the
	// exact no-result-compensation shape.
	outerResult := outerProjection.GetResultValue()
	adjusted := NewAdjustedMatchInfo(
		regular,
		nil,
		NewMaxMatchMap(
			map[values.Value]values.Value{outerResult: outerResult},
			outerResult,
			outerResult,
		),
		EmptyGroupByMappings(),
	)

	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryRef := expressions.InitialOf(queryScan)
	pm := NewPartialMatch(
		EmptyAliasMap(),
		stubMatchCandidate{name: "idx"},
		queryRef,
		queryScan,
		outerRef,
		adjusted,
	)

	compensation := pm.CompensateCompleteMatch(
		nil,
		values.NamedCorrelationIdentifier("candidate_top"),
	)
	if compensation != NoCompensation {
		t.Fatalf("adjusted complete-match compensation = %T (%v), want NoCompensation",
			compensation, compensation)
	}
}

func TestNestPullUp_AdjustedLevelRequiresOneQuantifier(t *testing.T) {
	t.Parallel()

	regular := NewRegularMatchInfo(
		nil, EmptyAliasMap(), nil, nil, nil, EmptyGroupByMappings(), nil, nil,
	)
	adjusted := NewAdjustedMatchInfo(regular, nil, nil, EmptyGroupByMappings())
	topAlias := values.NamedCorrelationIdentifier("candidate_top")

	for _, tc := range []struct {
		name string
		expr expressions.RelationalExpression
	}{
		{
			name: "zero",
			expr: expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType),
		},
		{
			name: "two",
			expr: expressions.NewSelectExpression(
				nil,
				[]expressions.Quantifier{
					namedForEachQuantifier("q1"),
					namedForEachQuantifier("q2"),
				},
				nil,
			),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ref := expressions.InitialOf(tc.expr)
			pm := NewPartialMatch(
				EmptyAliasMap(), stubMatchCandidate{name: "idx"},
				ref, tc.expr, ref, adjusted,
			)
			root, current := NestPullUp(pm, nil, topAlias)
			if root != nil || current != nil {
				t.Fatalf("adjusted candidate with %s quantifiers must fail closed", tc.name)
			}
		})
	}
}

// hazardScanPM builds a leaf partial match over a singleton candidate
// reference whose match info is self-consistent (the MaxMatchMap describes the
// very expression the candidate reference holds).
func hazardScanPM(t *testing.T, preds []predicates.QueryPredicate, pMap *PredicateMultiMap) *PartialMatchImpl {
	t.Helper()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	ref := expressions.InitialOf(scan)
	rv := scan.GetResultValue()
	mi := NewRegularMatchInfo(
		nil, EmptyAliasMap(), pMap, nil,
		NewMaxMatchMap(map[values.Value]values.Value{rv: rv}, rv, rv),
		EmptyGroupByMappings(), nil, nil,
	)
	var queryExpr expressions.RelationalExpression = scan
	if len(preds) > 0 {
		queryExpr = expressions.NewLogicalFilterExpression(
			preds, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	}
	return NewPartialMatch(
		EmptyAliasMap(), stubMatchCandidate{name: "idx"},
		expressions.InitialOf(queryExpr), queryExpr, ref, mi,
	)
}

// TestCompensate_ThreadsParentPullUpIntoChild pins the compensation CALL
// SHAPE: a child match compensates UNDER its parent's pull-up level, not from
// a fresh root.
//
// Java hands each child the `current` pull-up this level just nested, so the
// child's values translate through the parent level on their way out. Go used
// to call the child with a nil incoming pull-up, which produced a child chain
// rooted at the child — one translation level short. Nothing about the rows
// looks wrong at the child; the loss only shows up once the value has to reach
// the parent's scope, which is why no plan-level test caught it. Asserting the
// captured pull-up's PARENT is the parent level is the direct pin.
func TestCompensate_ThreadsParentPullUpIntoChild(t *testing.T) {
	t.Parallel()

	// The child carries one mapped predicate whose compensation closure
	// captures the pull-up it is invoked with.
	var capturedPullUp *PullUp
	var captured bool
	childPred := predicates.NewExistentialAlias(values.NamedCorrelationIdentifier("unused"))
	childMapBuilder := NewPredicateMultiMapBuilder()
	childMapBuilder.Put(childPred, &PredicateMapping{
		mappingKey: NewMappingKey(childPred, childPred, MappingRegularImpliesCandidate),
		predicateCompensation: func(
			_ PartialMatch,
			_ map[values.CorrelationIdentifier]*predicates.ComparisonRange,
			pullUp *PullUp,
		) PredicateCompensationFunc {
			capturedPullUp, captured = pullUp, true
			return NoPredicateCompensationNeeded()
		},
	})
	childPM := hazardScanPM(t, []predicates.QueryPredicate{childPred}, childMapBuilder.Build())

	// Parent: a Select with one ForEach quantifier that has the child match.
	parentScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	parentRef := expressions.InitialOf(parentScan)
	parentRV := parentScan.GetResultValue()
	qFE := namedForEachQuantifier("qFE")
	cFE := values.NamedCorrelationIdentifier("cFE")

	parentMI := NewRegularMatchInfo(
		nil,
		AliasMapOfAliases(qFE.GetAlias(), cFE),
		nil, nil,
		NewMaxMatchMap(map[values.Value]values.Value{parentRV: parentRV}, parentRV, parentRV),
		EmptyGroupByMappings(), nil, nil,
	)
	parentMI.SetChildPartialMatch(qFE.GetAlias(), childPM)

	parentSelect := expressions.NewSelectExpression(nil, []expressions.Quantifier{qFE}, nil)
	parentPM := NewPartialMatch(
		EmptyAliasMap(), stubMatchCandidate{name: "idx"},
		expressions.InitialOf(parentSelect), parentSelect, parentRef, parentMI,
	)

	parentPM.CompensateCompleteMatch(nil, values.NamedCorrelationIdentifier("top"))

	if !captured {
		t.Fatal("child predicate compensation was never invoked — the test proves nothing")
	}
	if capturedPullUp == nil {
		t.Fatal("child compensated against a nil pull-up")
	}
	if capturedPullUp.GetParent() == nil {
		t.Fatal("child pull-up has no parent: the child was compensated from a fresh root " +
			"instead of under the parent's match level (the pre-fix nil-threading bug)")
	}
	if !capturedPullUp.GetParent().IsMatch() {
		t.Fatal("child pull-up's parent must be the parent's match level")
	}
	if got := capturedPullUp.GetParent().GetCandidateAlias(); got != values.NamedCorrelationIdentifier("top") {
		t.Fatalf("parent level candidate alias = %q, want the top candidate alias", got.Name())
	}
	// The child level itself is anchored at the alias the binding map maps the
	// matched quantifier to.
	if got := capturedPullUp.GetCandidateAlias(); got != cFE {
		t.Fatalf("child level candidate alias = %q, want %q", got.Name(), cFE.Name())
	}
}

func TestCompensate_PrefersPossiblePredicateAlternative(t *testing.T) {
	t.Parallel()

	queryPredicate := predicates.NewConstantPredicate(predicates.TriTrue)
	firstCandidate := predicates.NewConstantPredicate(predicates.TriFalse)
	secondCandidate := predicates.NewConstantPredicate(predicates.TriTrue)

	builder := NewPredicateMultiMapBuilder()
	builder.Put(queryPredicate, &PredicateMapping{
		mappingKey: NewMappingKey(
			queryPredicate,
			firstCandidate,
			MappingRegularImpliesCandidate,
		),
		predicateCompensation: func(
			PartialMatch,
			map[values.CorrelationIdentifier]*predicates.ComparisonRange,
			*PullUp,
		) PredicateCompensationFunc {
			return ImpossiblePredicateCompensation()
		},
	})
	builder.Put(queryPredicate, &PredicateMapping{
		mappingKey: NewMappingKey(
			queryPredicate,
			secondCandidate,
			MappingRegularImpliesCandidate,
		),
		predicateCompensation: func(
			PartialMatch,
			map[values.CorrelationIdentifier]*predicates.ComparisonRange,
			*PullUp,
		) PredicateCompensationFunc {
			return OfPredicateCompensation(queryPredicate, false)
		},
	})

	pm := hazardScanPM(
		t,
		[]predicates.QueryPredicate{queryPredicate},
		builder.Build(),
	)
	compensation := pm.CompensateCompleteMatch(
		nil,
		values.NamedCorrelationIdentifier("candidate_top"),
	)
	forMatch, ok := compensation.(*ForMatchCompensation)
	if !ok {
		t.Fatalf("compensation = %T, want *ForMatchCompensation", compensation)
	}
	selected := forMatch.GetPredicateCompensationMap().Get(queryPredicate)
	if selected == nil {
		t.Fatal("predicate compensation alternative was dropped")
	}
	if selected.IsImpossible() {
		t.Fatal("first impossible alternative won over a later possible alternative")
	}
}

// TestNestPullUp_RootIsOnlyOwnedWhenNestingStartsAMatch pins which of the two
// returned pull-ups is the "root of match".
//
// The root is what result compensation pulls the query result value through,
// so it must name the level this nesting introduced. When the incoming pull-up
// is already a match level, this nesting is CONTINUING someone else's match —
// that enclosing match owns the result, and this level must report no root at
// all. Reporting one anyway makes a child re-compensate a result its parent
// already handles.
func TestNestPullUp_RootIsOnlyOwnedWhenNestingStartsAMatch(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("cand")
	pm := hazardScanPM(t, nil, nil)

	// No incoming pull-up: this nesting roots the match.
	root, current := NestPullUp(pm, nil, alias)
	if root == nil || current == nil {
		t.Fatal("nesting from nil must produce both a root and a current level")
	}
	if root != current {
		t.Fatal("a single-level nesting's root and current are the same level")
	}

	// Incoming UNIFICATION pull-up: still this nesting's match, so it owns a root.
	unification := ForUnification(alias, pm.GetRegularMatchInfo().GetMaxMatchMap().GetCandidateValue(), nil)
	if unification.IsMatch() {
		t.Fatal("setup: a unification pull-up must not be a match level")
	}
	root, current = NestPullUp(pm, unification, alias)
	if root == nil || current == nil {
		t.Fatal("nesting under a unification pull-up must produce a root")
	}
	if root != current || current.GetParent() != unification {
		t.Fatal("the new level must sit directly on the unification pull-up and be the root")
	}

	// Incoming MATCH pull-up: continuing an enclosing match — no root here.
	incoming := ForMatch(nil, alias, expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
	if !incoming.IsMatch() {
		t.Fatal("setup: ForMatch must produce a match level")
	}
	root, current = NestPullUp(pm, incoming, alias)
	if current == nil {
		t.Fatal("nesting under a match pull-up must still produce a current level")
	}
	if current.GetParent() != incoming {
		t.Fatal("the new level must sit directly on the incoming match level")
	}
	if root != nil {
		t.Fatal("nesting that continues an enclosing match must NOT claim a root of match")
	}
}

// TestApply_RetainsQuantifierReferencedOnlyByRangeComparand pins the
// production consequence of the correlation-helper hole, at the boundary that
// actually builds the expression.
//
// Apply decides which quantifiers to pull back into the compensated Select by
// asking which correlations the residual predicates reference. When that
// question was answered by a type switch that did not know
// PredicateWithValueAndRanges, a residual whose only mention of an existential
// lived in a range comparand looked unrelated to it — the quantifier was
// dropped and the rebuilt expression referenced an alias nothing supplied.
func TestApply_RetainsQuantifierReferencedOnlyByRangeComparand(t *testing.T) {
	t.Parallel()

	baseQ := namedForEachQuantifier("qBase")
	existQ := namedExistentialQuantifier("qExist")
	existAlias := existQ.GetAlias()

	// residual: qBase.x IN {> QOV(qExist)} — qExist appears ONLY in the range.
	comparand := predicates.Comparison{
		Type:    predicates.ComparisonGreaterThan,
		Operand: values.NewQuantifiedObjectValue(existAlias),
	}
	residual := predicates.NewPredicateWithValueAndRanges(
		values.NewQuantifiedObjectValue(baseQ.GetAlias()),
		[]*predicates.RangeConstraints{
			predicates.NewRangeConstraints(nil, []predicates.Comparison{comparand}),
		},
	)

	comp := NewForMatchCompensation(
		false, NoCompensation,
		NewPredicateCompensationMap(
			[]predicates.QueryPredicate{residual},
			[]PredicateCompensationFunc{&predicateCompensationOfPredicate{predicate: residual}},
		),
		[]expressions.Quantifier{baseQ}, []expressions.Quantifier{existQ},
		aliasesOf(baseQ),
		NoResultCompensation(), EmptyGroupByMappings(),
	)
	if comp.IsImpossible() {
		t.Fatal("setup: compensation must be possible to reach Apply")
	}

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	applied, ok := comp.Apply(scan, nil)
	if !ok {
		t.Fatal("Apply unexpectedly failed")
	}

	sel, ok := applied.(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("Apply produced %T, want a SelectExpression joining the pulled-up quantifier", applied)
	}
	found := false
	for _, q := range sel.GetQuantifiers() {
		if q.GetAlias() == existAlias {
			found = true
		}
	}
	if !found {
		aliases := make([]string, 0, len(sel.GetQuantifiers()))
		for _, q := range sel.GetQuantifiers() {
			aliases = append(aliases, q.GetAlias().Name())
		}
		t.Fatalf("compensated Select has quantifiers %v, missing %q — the residual references it "+
			"only through a range comparand and it was dropped", aliases, existAlias.Name())
	}
}
