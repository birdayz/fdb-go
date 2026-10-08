package cascades

import (
	"slices"
	"sort"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// PartitionSelectRule splits a SelectExpression with N >= 3 quantifiers
// into two levels: a lower SelectExpression containing a subset of the
// quantifiers (the "lower" partition) and an upper SelectExpression
// containing the remaining quantifiers plus a new ForEach quantifier
// over the lower Select. Predicates are classified by their correlations
// and distributed to the level where they can be evaluated earliest.
//
// This is the core of join enumeration in the Cascades optimizer:
// if a query has FROM a, b, c WHERE a.x = b.x AND b.y = c.y, this
// rule partitions the quantifiers into connected components (a,b and
// b,c share predicates so they form one component — or the rule
// explores every possible bipartition and lets cost decide).
//
// The rule fires once per distinct bipartition of the quantifier set.
// Each firing produces at most one yield — the upper SelectExpression.
// Convergence is guaranteed because each yielded expression has strictly
// fewer quantifiers at the top level than the input.
//
// Ports Java's PartitionSelectRule (ExplorationCascadesRule).
type PartitionSelectRule struct {
	matcher matching.BindingMatcher
}

func NewPartitionSelectRule() *PartitionSelectRule {
	return &PartitionSelectRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("partition_select").WithRootPredicate(
			func(sel *expressions.SelectExpression) bool { return len(sel.GetQuantifiers()) >= 3 }),
	}
}

func (r *PartitionSelectRule) ConstraintDependencies() []any { return nil }

func (r *PartitionSelectRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PartitionSelectRule) OnMatch(call *ExpressionRuleCall) {
	if call.CancellationErr() != nil {
		return
	}
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)
	quantifiers := sel.GetQuantifiers()

	if len(quantifiers) < 3 {
		return
	}

	// Both halves are rebuilt via NewSelectExpressionWithAliases, which has no
	// joinType parameter and so always defaults to JoinInner — this rule NEVER
	// carries the original select's JoinType forward. A LEFT/RIGHT/FULL OUTER
	// select's null-extension is directional and belongs to exactly the binary
	// {preserved, null-supplying} shape RewriteOuterJoinRule builds, so
	// bipartitioning it into two always-JoinInner halves would erase that
	// null-extension outright (not merely mis-time a predicate), silently
	// dropping unmatched rows. ChildrenAsSet() (JoinInner || JoinCross) is the
	// gate: a CROSS select carries no predicates and no directional semantics, so
	// splitting it into two default-JoinInner halves reproduces identical rows.
	//
	// THIS BLUNTNESS WAS TESTED AND IS KEPT. The obvious refinement — refuse only
	// the bipartitions that SPLIT the join's two sides, rebuild the lower with
	// the original JoinType, and let the semi-join filters peel off — was built
	// (SealedGraphExpansion.BuildSelectWithJoinType) and measured. It fixed the
	// uncorrelated arm of projected-EXISTS-over-LEFT-JOIN and produced WRONG ROWS
	// on the other two: the winning plan came out as
	// `FlatMap(outer=FlatMap(outer=Scan(EMP), inner=Scan(DEPT,[=])), …)` — driving
	// from the NULL-SUPPLYING side with no DefaultOnEmpty anywhere — and returned
	// 0 rows where 2 are correct.
	//
	// WHERE THE ROWS WENT IS STILL UNKNOWN, and one plausible answer is already
	// ruled out. RewriteOuterJoinRule yields a TWO-quantifier select carrying
	// JoinInner plus a NullOnEmpty quantifier (rule_rewrite_outer_join.go:172-205),
	// so this rule never sees that shape at all — it needs >= 3 quantifiers. The
	// select the refinement partitioned was the ORIGINAL JoinLeftOuter one, and
	// its lower rebuilt as JoinLeftOuter with the ON-predicate intact, which is
	// precisely what RewriteOuterJoinRule accepts. So the wrong plan came from a
	// member the refinement admitted, and WHICH member has not been identified.
	//
	// Until it is, a blunt refusal that declines a plannable query is strictly
	// better than a precise one that answers it wrongly.
	//
	// THE WRONG ROWS WERE NOT SILENT, and the distinction is worth keeping
	// straight because it changes what the safety net is owed.
	// TestFDB_ProjectedExistsOverLeftJoin and TestFDB_LeftJoinExistsResidual both
	// assert ROWS and both caught it. What they could not do is CHANGE STATE: they
	// were already red on the 0AF00 decline, so the refinement moved them from one
	// red to another and the failure COUNT never moved. A red test cannot go
	// redder — so in an area with known-failing tests, a count is not a regression
	// detector and the per-test messages have to be read.
	//
	// What this costs is real and measured, not theoretical: projected EXISTS
	// over a LEFT JOIN is unplannable in Go and Java answers it — see
	// conformance/projected_exists_left_join_java_probe_test.go, where Java
	// returns [[10 false] [20 false]] and Go returns 0AF00. That was covered by
	// the NLJ rule's three-quantifier arm until RFC-235 retired it, and it is the
	// one capability that retirement removed.
	if !sel.ChildrenAsSet() {
		return
	}

	// NOTE — A NULL-ON-EMPTY QUANTIFIER MAY BE SEPARATED FROM ITS PARTNER, and
	// that is not the same question the join-type gate above answers.
	//
	// A guard was added here requiring both sides of an outer join to land in the
	// same half whenever any quantifier was marked NullOnEmpty, on the reasoning
	// that splitting the pair erases the extension. It does not, and the reason is
	// WHERE each spelling keeps the property:
	//
	//   - a non-set JoinType puts it on the PAIR. Rebuilding either half as
	//     JoinInner loses it, which is what the gate above refuses.
	//   - NullOnEmpty puts it on the QUANTIFIER, and the quantifier travels. A
	//     null-on-empty leg still null-extends in whichever half it lands.
	//
	// Measured: the guard took `SELECT * FROM la a LEFT JOIN lb b ON a.gid = b.gid
	// WHERE EXISTS (…)` from a correct plan to 0AF00
	// (TestFDB_LeftBoxWhereExistsOrdinal), because that query is planned through a
	// bipartition that separates them.

	// StrictSingle is a semantic edge contract owned by the binary
	// correlated-scalar lowering. This N-way partitioner rebuilds the lower edge
	// as a plain ForEach, so accepting a flagged input would erase its
	// at-most-one-row guarantee before ImplementNestedLoopJoinRule can install
	// the strict FirstOrDefault. SQL lowering does not emit supported N-way
	// strict shapes; fail closed until partitioning can preserve and orient one.
	if hasStrictSingleQuantifier(quantifiers) {
		return
	}

	// Existential quantifiers partition like any other. Java's
	// PartitionSelectRule enumerates EVERY subset of the quantifiers
	// (`combinations(combinationQuantifierMatcher, c -> 0, Collection::size)`,
	// PartitionSelectRule.java:65-66) and has no notion of an existential being
	// ineligible; the shape that reaches ImplementNestedLoopJoinRule is always
	// binary, because that rule matches `exactlyInAnyOrder(outer, inner)`
	// (ImplementNestedLoopJoinRule.java:98).
	//
	// A bail here for `1 existential + <=2 ForEach` used to hand that exact shape
	// to the NLJ rule's three-quantifier arm instead. It was not a preference
	// between two working decompositions: the peel it suppressed produced a plan
	// that returned zero rows, because the peeled lower's EXISTS predicate reached
	// a PredicatesFilter in its structural form. That is fixed at its cause —
	// predicates are residualised before they become filters, as Java does, and a
	// filter carrying a structural predicate is now rejected at construction —
	// so the bail has nothing left to protect against (RFC-235).
	// Projected existentials partition too, as in Java: each one's nullable
	// witness flows through its own FlatMap (several projected EXISTS beside
	// WHERE-EXISTS filters included; Java corpus exists-in-select.yamsql).

	plannerCfg := call.Context.GetPlannerConfiguration()

	// Preserve explicit source bindings across partitioning; programmatic selects
	// can name runtime sources independently of their quantifier aliases.
	srcAliases := sel.GetSourceAliases()
	quantAliasToSource := make(map[values.CorrelationIdentifier]string, len(quantifiers))
	for i, q := range quantifiers {
		if i < len(srcAliases) {
			quantAliasToSource[q.GetAlias()] = srcAliases[i]
		}
	}
	applyExistentialSourceAliases := func(s *expressions.SelectExpression) *expressions.SelectExpression {
		if len(s.GetSourceAliases()) > 0 {
			return s
		}
		orig := s.GetQuantifiers()
		hasExistential := false
		for _, q := range orig {
			if q.Kind() == expressions.QuantifierExistential {
				hasExistential = true
				break
			}
		}
		if !hasExistential {
			return s
		}
		// ImplementNestedLoopJoinRule reads the OUTER from source-alias/quantifier
		// slot 0 and the existential INNER from slot 1, so the quantifiers must be
		// ordered [ForEach outer, Existential inner] — the shape the flat translator
		// build always emits but a partition bipartition can invert. Reorder
		// ForEach-first (a select's quantifier order is not semantically load-bearing
		// — the result value binds by alias), and set source aliases parallel.
		qs := make([]expressions.Quantifier, 0, len(orig))
		for _, q := range orig {
			if q.Kind() == expressions.QuantifierForEach {
				qs = append(qs, q)
			}
		}
		for _, q := range orig {
			if q.Kind() != expressions.QuantifierForEach {
				qs = append(qs, q)
			}
		}
		srcs := make([]string, len(qs))
		for i, q := range qs {
			if src, ok := quantAliasToSource[q.GetAlias()]; ok {
				srcs[i] = src
			} else {
				srcs[i] = q.GetAlias().Name() // a fresh lower/merge quantifier: alias IS its source
			}
		}
		rebuilt, err := expressions.NewSelectExpressionWithAliases(s.GetResultValue(), qs, s.GetPredicates(), srcs)
		if err != nil {
			call.Fail(err)
			return nil
		}
		return rebuilt
	}

	// Build alias → quantifier map.
	aliasToQ := make(map[values.CorrelationIdentifier]expressions.Quantifier, len(quantifiers))
	for _, q := range quantifiers {
		aliasToQ[q.GetAlias()] = q
	}

	// Legacy programmatic predicates may name a source inside an existential.
	// Keep those predicates with their owner rather than stranding the source.
	buriedToExistential := make(map[values.CorrelationIdentifier]values.CorrelationIdentifier)
	for _, q := range quantifiers {
		if q.Kind() != expressions.QuantifierExistential {
			continue
		}
		for buried := range boundAliasesOfReference(q.GetRangesOver()) {
			if _, isSelectAlias := aliasToQ[buried]; isSelectAlias {
				continue
			}
			buriedToExistential[buried] = q.GetAlias()
		}
	}

	// Compute the full transitive correlation closure among quantifiers.
	// For each quantifier alias A, fullCorrelationOrder[A] is the set of
	// other quantifier aliases that A transitively depends on.
	fullCorrelationOrder := computeTransitiveCorrelationOrder(quantifiers)

	// Compute independent quantifiers partitioning — used to defer
	// cross products when configured to do so.
	independentPartitioning := computeIndependentQuantifiersPartitioning(sel, fullCorrelationOrder)
	projectedContinuation, hasProjectedContinuation := independentForEachBlockBelowProjectedExistential(sel, fullCorrelationOrder)
	filteringContinuation, hasFilteringContinuation := independentForEachBlockBelowFilteringExistential(sel, fullCorrelationOrder)

	// The select's conjuncts (its list, lifted flat by the constructor): the classifier loop
	// consumes them per bipartition, and the disconnected-lower guard judges
	// lower connectivity against the FULL set, including spanning predicates
	// whose upper dependencies prevent placing them in the lower.
	allPredicates := splitRangesByLocalCorrelation(sel.GetPredicates(), aliasToQ, buriedToExistential)

	// Enumerate all non-trivial bipartitions of the quantifier set.
	// "lower" is each non-empty proper subset; "upper" is the complement.
	allAliases := make([]values.CorrelationIdentifier, len(quantifiers))
	for i, q := range quantifiers {
		allAliases[i] = q.GetAlias()
	}

	// We iterate over all 2^N - 2 non-trivial subsets via bitmask.
	n := len(allAliases)
	total := 1 << n
	for mask := 1; mask < total-1; mask++ {
		// This is the rule's exponential seam (2^N bipartitions). A task-loop
		// check alone cannot interrupt one large invocation.
		if call.CancellationErr() != nil {
			return
		}
		lowerAliases := make(map[values.CorrelationIdentifier]struct{})
		for bit := 0; bit < n; bit++ {
			if mask&(1<<bit) != 0 {
				lowerAliases[allAliases[bit]] = struct{}{}
			}
		}

		if len(lowerAliases) == 0 {
			continue
		}

		// If right-deep only, require upper has exactly 1 quantifier.
		if plannerCfg.ShouldJoinRightDeep && len(lowerAliases) != len(quantifiers)-1 {
			continue
		}
		upperAliases := make(map[values.CorrelationIdentifier]struct{})
		for _, q := range quantifiers {
			a := q.GetAlias()
			if _, inLower := lowerAliases[a]; !inLower {
				upperAliases[a] = struct{}{}
			}
		}
		if len(upperAliases) == 0 {
			continue
		}

		// Search preferences cannot eliminate the sole implementable projected
		// existential continuation. Other partitions still honor deferral.
		_, continuationAbove := upperAliases[projectedContinuation]
		keepsForEachBlock := hasProjectedContinuation && len(upperAliases) == 1 && continuationAbove
		// Its FILTERING twin: a `WHERE EXISTS` whose conjuncts each read a
		// different leg (`z.k = w.id AND z.v > h.f` over `FROM w, h`) connects
		// the legs only THROUGH itself, so the whole ForEach block below it is
		// the one bipartition that keeps the semi-join's predicates together
		// (other splits can strand the upper structural existential predicate).
		// The block is disconnected on its own, and the guard below would
		// prune it, leaving the select with no plan (0AF00 where the Java
		// target answers). Admit it exactly when counting the existential's
		// predicates connects the block.
		_, filteringAbove := upperAliases[filteringContinuation]
		keepsFilteredBlock := hasFilteringContinuation && len(upperAliases) == 1 && filteringAbove &&
			aliasesConnectedByPredicates(withAlias(lowerAliases, filteringContinuation), allPredicates)
		// Check independent quantifiers partitioning for cross-product deferral.
		if len(independentPartitioning) > 1 && !keepsForEachBlock {
			if plannerCfg.ShouldDeferCrossProducts {
				if !isCrossProduct(independentPartitioning, lowerAliases, upperAliases) {
					continue
				}
			}
		}

		// A LOWER quantifier may depend on an UPPER one: the lower select is then
		// correlated to that upper quantifier, and the upper select's own
		// partitioning makes it the inner of the leg it reads — Java's rule has
		// no check here, and for lateral legs correlated to an earlier leg
		// (`FROM w, (… w.arr …) AS d, (… d.k …) AS e`) the lower {d, e} under
		// {w} is the only bipartition there is. What admitting such a lower
		// newly allows is a null-on-empty quantifier (a LEFT JOIN's
		// null-supplying leg) as the lower's OUTER, whose extension must not be
		// lost; the NLJ rule wraps that outer in DefaultOnEmpty as Java's
		// planPartitionToPhysical does, and declines the existential lowering
		// that has no such wrap. The LEFT JOIN shapes themselves plan without
		// this admission (restoring the old refusal leaves them green).

		// Reject partitioning if it would cause a dependency cycle.
		// Collect upper aliases that depend on lower aliases.
		uppersDependingOnLowers := make(map[values.CorrelationIdentifier]struct{})
		for upperAlias := range upperAliases {
			deps := fullCorrelationOrder[upperAlias]
			for lowerAlias := range lowerAliases {
				if _, ok := deps[lowerAlias]; ok {
					uppersDependingOnLowers[upperAlias] = struct{}{}
					break
				}
			}
		}

		// Check if any lower aliases depend on those upper aliases.
		cycle := false
		for lowerAlias := range lowerAliases {
			deps := fullCorrelationOrder[lowerAlias]
			for upperDep := range uppersDependingOnLowers {
				if _, ok := deps[upperDep]; ok {
					cycle = true
					break
				}
			}
			if cycle {
				break
			}
		}
		if cycle {
			continue
		}

		// Prefer right-deep DAGs: reject partitioning that would force
		// rebasing the outer side (when multiple lowers are correlated to
		// by uppers).
		lowersCorrelatedToByUpperAliases := make(map[values.CorrelationIdentifier]struct{})
		for upperAlias := range upperAliases {
			deps := fullCorrelationOrder[upperAlias]
			for lowerAlias := range lowerAliases {
				if _, ok := deps[lowerAlias]; ok {
					lowersCorrelatedToByUpperAliases[lowerAlias] = struct{}{}
				}
			}
		}
		if len(lowersCorrelatedToByUpperAliases) > 1 {
			continue
		}

		var lowerAliasCorrelatedToByUpperAliases values.CorrelationIdentifier
		if len(lowersCorrelatedToByUpperAliases) == 0 {
			lowerAliasCorrelatedToByUpperAliases = values.UniqueCorrelationIdentifier()
		} else {
			for a := range lowersCorrelatedToByUpperAliases {
				lowerAliasCorrelatedToByUpperAliases = a
				break
			}
		}

		// Track which lower aliases are referenced by upper predicates
		// or the result value (the "live" set this lower must flow up).
		lowersCorrelatedToByUppers := make([]values.CorrelationIdentifier, 0)

		resultValue := sel.GetResultValue()

		// Determine which lower aliases the result value needs ("live" via the
		// result): exactly the lowers it references (GetCorrelatedToOfValue) —
		// an ordinal seed's referenced set is trustworthy (no RV hides its
		// real projection).
		// liveViaPredicate records the lowers made live by a SPANNING PREDICATE, as
		// against those made live only by the result value. The two are the same
		// thing for a ForEach and different things for an existential.
		liveViaPredicate := map[values.CorrelationIdentifier]struct{}{}

		resultCorrelatedToLowers := intersectAliases(lowerAliases, values.GetCorrelatedToOfValue(resultValue))
		for a := range resultCorrelatedToLowers {
			lowersCorrelatedToByUppers = append(lowersCorrelatedToByUppers, a)
		}

		// Classify predicates.
		var lowerPredicates []predicates.QueryPredicate
		var upperPredicates []predicates.QueryPredicate
		var deeplyCorrelatedPredicates []predicates.QueryPredicate

		for _, pred := range allPredicates {
			correlatedTo := predicates.GetCorrelatedToOfPredicate(pred)
			// A reference to an alias BURIED inside an existential quantifier's
			// subgraph correlates the predicate to THAT quantifier (see
			// buriedToExistential above) — it must land in the half holding the
			// existential, never float free of it.
			for c := range correlatedTo {
				if esqAlias, buried := buriedToExistential[c]; buried {
					correlatedTo[esqAlias] = struct{}{}
				}
			}
			correlatedToLower := intersectAliases(lowerAliases, correlatedTo)
			correlatedToUpper := intersectAliases(upperAliases, correlatedTo)

			if len(correlatedToUpper) > 0 {
				if len(correlatedToLower) > 0 {
					// Java allows a correlated lower unless an upper dependency
					// would turn the new predicate correlation into a cycle.
					if len(intersectAliases(correlatedToUpper, uppersDependingOnLowers)) == 0 {
						lowerPredicates = append(lowerPredicates, pred)
						continue
					}
					upperPredicates = append(upperPredicates, pred)
					for a := range correlatedToLower {
						lowersCorrelatedToByUppers = append(lowersCorrelatedToByUppers, a)
						// Structural existential predicates cannot read a positional witness.
						liveViaPredicate[a] = struct{}{}
					}
				} else {
					upperPredicates = append(upperPredicates, pred)
				}
			} else {
				if len(correlatedToLower) > 0 {
					lowerPredicates = append(lowerPredicates, pred)
				} else {
					deeplyCorrelatedPredicates = append(deeplyCorrelatedPredicates, pred)
				}
			}
		}

		// Dedup the lower aliases the upper correlates to (the live set — the same
		// alias can be added by both the result value and a spanning predicate).
		// Without this a merged lower would list duplicate
		// aliases.
		lowersCorrelatedToByUppers = dedupAliases(lowersCorrelatedToByUppers)

		// Structural existential predicates require a QOV operand; they cannot read
		// a positional witness field. Projected ExistsValue has no such restriction.
		liveExistentialPredicate := false
		for _, a := range lowersCorrelatedToByUppers {
			if aliasToQ[a].Kind() == expressions.QuantifierExistential {
				if _, viaPredicate := liveViaPredicate[a]; viaPredicate {
					liveExistentialPredicate = true
					break
				}
			}
		}
		if liveExistentialPredicate {
			continue
		}

		// Skip a disconnected lower: ≥2 quantifiers no predicate links (a pure
		// cross product, e.g. {A,C} for chain A—B—C or {xx,yy} for a star). Its
		// tables share no join, so the partition is a genuine cartesian product
		// — never the cost-optimal shape, and the connected associativities
		// cover the same join orders. Go-tighter than Java (Java explores such
		// lowers and lets cost pruning kill them — dropping this guard blows
		// the 4-chain task baseline past the MaxTasks budget), so it must
		// never eat a bipartition Java NEEDS:
		//   - connectivity is judged by ANY select predicate touching both
		//     aliases — a spanning N-ary predicate (`a.x = b.y OR a.x = c.y`)
		//     connects its aliases even when dependencies keep it UPPER
		//     (judging by lowerPredicates alone starved that select of every
		//     bipartition → 0AF00); binary equijoins behave identically under
		//     both readings, so the pinned task baselines are unchanged;
		//   - a COMPONENT-ALIGNED split (isCrossProduct — no component
		//     straddles the halves) whose lower unions SINGLETON components is
		//     exempt: those are the UNAVOIDABLE cross products (`FROM a, b,
		//     c`, the EXISTS body `SELECT 1 FROM t2, t3, t4 …`), the only
		//     bipartitions such a select has. BOTH restrictions are
		//     load-bearing: the component-ALIGNED requirement (isCrossProduct)
		//     rejects a straddling lower ({d,PB} for
		//     `FROM (derived) d, PB, PB.ARR AS X`), which tears a lateral unnest
		//     from its array source; the SINGLETON requirement
		//     (lowerComponentsAreSingletons) bounds the exemption itself — a
		//     multi-alias component in the lower is a real join component, not an
		//     unavoidable cross product. Dropping the singleton conjunct does NOT
		//     surface as a plan-time error: the mis-partition (a lower unioning a
		//     whole multi-alias component with an extra leg, e.g. {A,Explode(A.ARR),B}
		//     for `FROM A, B, EE, A.ARR AS X`) PLANS cleanly and is cost-CHOSEN,
		//     then fails only at RUNTIME in the executor via the RFC-173 ordinal
		//     tripwire — RowEvalContext "multi-leg row cannot serve a source-relative
		//     ordinal" — because positionalMergeCase mis-wires the source-relative
		//     references for such a lower. That deeper gap (Go's positionalMergeCase
		//     cannot correctly wire a bipartition Java's PartitionSelectRule — which
		//     has only isCrossProduct — both plans AND executes) is a tracked
		//     follow-on; this guard keeps Go correct meanwhile by never admitting the
		//     shape into the search.
		//   - For an ORDINAL parent (the
		//     gathered flat unnest seed — every parent),
		//     a quantifier-level correlation edge IS
		//     connectivity: a lower of {source, Explode} is exactly the correct
		//     FlatMap pairing (the binary unnest shape), and rejecting it left
		//     the flat (N+1)-way select unimplementable.
		lowerConnected := aliasesConnectedByPredicates(lowerAliases, allPredicates)
		if !lowerConnected {
			lowerConnected = aliasesConnectedByPredicatesOrCorrelation(lowerAliases, allPredicates, fullCorrelationOrder)
		}
		disconnectedLower := len(lowerAliases) >= 2 && !lowerConnected
		// Preserve the complete row-producing block even when it is disconnected;
		// an existential dependency on several lower rows can require this split.
		if disconnectedLower && !keepsForEachBlock && !keepsFilteredBlock &&
			!(isCrossProduct(independentPartitioning, lowerAliases, upperAliases) &&
				lowerComponentsAreSingletons(independentPartitioning, lowerAliases)) {
			continue
		}

		// Validate upper-quantifier dependency constraints.
		if len(lowersCorrelatedToByUpperAliases) > 0 {
			if len(lowersCorrelatedToByUppers) > 1 {
				continue
			}
			if len(lowersCorrelatedToByUppers) == 1 {
				if lowersCorrelatedToByUppers[0] != lowerAliasCorrelatedToByUpperAliases {
					continue
				}
			}
		}

		// Only proceed if the partitioning is useful.
		if len(lowerAliases) == 1 {
			if len(lowerPredicates) == 0 {
				continue
			}
		}

		// Build the lower GraphExpansion.
		lowerBuilder := NewGraphExpansionBuilder()
		for _, a := range allAliases {
			if _, inLower := lowerAliases[a]; inLower {
				lowerBuilder.AddQuantifier(aliasToQ[a])
			}
		}
		for _, p := range lowerPredicates {
			lowerBuilder.AddPredicate(p)
		}
		for _, p := range deeplyCorrelatedPredicates {
			lowerBuilder.AddPredicate(p)
		}

		// Build the upper SelectExpression.
		var upperSelectExpression *expressions.SelectExpression

		noLowersCorrelatedToByUpperAliases := len(lowersCorrelatedToByUpperAliases) == 0
		noLowersCorrelatedToByUppers := len(lowersCorrelatedToByUppers) == 0

		// The upper select flows the parent result value UNCHANGED — an
		// ordinal seed names exactly the aliases it references, and the
		// positional merge
		// arm (positionalMergeCase) owns the ≥2-live-lowers collapse.

		// addUpper appends the new lower quantifier, the upper tables, and the
		// upper predicates to a fresh upper builder. It serves case 1 / case 2
		// only (the ≥2-live-lowers merge case routes through
		// positionalMergeCase, which translates the upper predicates onto the
		// merge quantifier itself): the lower keeps each live table under its
		// ORIGINAL alias (case 2 flows the single live table's row unchanged),
		// so an upper predicate referencing it resolves directly and passes
		// through unchanged. (RFC-043.)
		addUpper := func(newLowerQ expressions.Quantifier) *GraphExpansionBuilder {
			upperBuilder := NewGraphExpansionBuilder()
			upperBuilder.AddQuantifier(newLowerQ)
			for _, a := range allAliases {
				if _, inUpper := upperAliases[a]; inUpper {
					upperBuilder.AddQuantifier(aliasToQ[a])
				}
			}
			for _, p := range upperPredicates {
				upperBuilder.AddPredicate(p)
			}
			return upperBuilder
		}

		if noLowersCorrelatedToByUpperAliases && noLowersCorrelatedToByUppers {
			// Case 1: No upper-to-lower correlation. Lower result is a
			// literal scalar 1 (cross-product style).
			lowerBuilder.AddColumn("", &values.ConstantValue{
				Value: int64(1),
				Typ:   values.NotNullLong,
			})
			lowerSelectExpr, err := lowerBuilder.Build().Seal().BuildSelect()
			if err != nil {
				call.Fail(err)
				return
			}

			lowerSelectExpr = applyExistentialSourceAliases(lowerSelectExpr)
			if lowerSelectExpr == nil {
				return
			}
			newLowerQ := expressions.NamedForEachQuantifier(
				lowerAliasCorrelatedToByUpperAliases,
				call.MemoizeExpression(lowerSelectExpr),
			)
			upperSelectExpression, err = addUpper(newLowerQ).Build().Seal().
				BuildSelectWithResultValue(resultValue)
			if err != nil {
				call.Fail(err)
				return
			}

		} else if len(lowersCorrelatedToByUppers) >= 2 {
			// Merge case: ≥2 live lower tables. The POSITIONAL merge arm is the
			// sole owner.
			upperSelectExpression = r.positionalMergeCase(call, sel, resultValue, aliasToQ, allAliases, upperAliases, lowersCorrelatedToByUppers, lowerBuilder, upperPredicates)
			if upperSelectExpression == nil {
				continue
			}
			upperSelectExpression = applyExistentialSourceAliases(upperSelectExpression)
			if upperSelectExpression == nil {
				return
			}
			call.Yield(upperSelectExpression)
			continue
		} else {
			// Case 2: Exactly one live lower alias. Lower's result value is that
			// alias's flowed object value (a single table's row).
			var lowerAlias values.CorrelationIdentifier
			if len(lowersCorrelatedToByUpperAliases) == 0 {
				lowerAlias = lowersCorrelatedToByUppers[0]
			} else {
				lowerAlias = lowerAliasCorrelatedToByUpperAliases
			}

			// Java's `Quantifier.getFlowedObjectValue()` is
			// `QuantifiedObjectValue.of(getAlias(), getFlowedObjectType())`
			// (Quantifier.java:801-803) — ALWAYS typed, so no Java site flows a row
			// whose shape is unstated. This lower select flows one table's whole row,
			// and the row it flows is that quantifier's, type included.
			//
			// The type is not decoration here. A downstream reader that has to bake an
			// ordinal against this row has no domain to bake in without it, so every
			// read through the leg falls through to the qualified NAME; and a
			// reference left source-relative evaluates to NULL against the build-bound
			// row, which is a join returning nothing with no error.
			//
			// A member DISAGREEMENT declines this bipartition rather than falling back
			// to an untyped row. The fallback is the tempting move and it is the wrong
			// one: it would flow a row shape chosen by memo insertion order, which is
			// precisely what the agreement verification exists to refuse. Declining
			// costs one bipartition; the others still yield, and the rules that do not
			// partition are untouched.
			flowedValue, flowedErr := aliasToQ[lowerAlias].RequireFlowedObjectValue()
			if flowedErr != nil {
				continue
			}
			lowerSelectExpr, err := lowerBuilder.Build().Seal().BuildSelectWithResultValue(flowedValue)
			if err != nil {
				call.Fail(err)
				return
			}

			lowerSelectExpr = applyExistentialSourceAliases(lowerSelectExpr)
			if lowerSelectExpr == nil {
				return
			}
			newLowerQ := expressions.NamedForEachQuantifier(
				lowerAlias,
				call.MemoizeExpression(lowerSelectExpr),
			)
			// The lower flows its single live table's row UNDER ITS ORIGINAL
			// ALIAS, so an upper predicate referencing that alias still resolves
			// directly — nothing buried under a new name.
			// (Every lower alias a spanning predicate touches is added to the live
			// set, so with exactly one live lower the only lower alias an upper
			// predicate can name is this one.)
			upperSelectExpression, err = addUpper(newLowerQ).Build().Seal().
				BuildSelectWithResultValue(resultValue)
			if err != nil {
				call.Fail(err)
				return
			}
		}

		upperSelectExpression = applyExistentialSourceAliases(upperSelectExpression)
		if upperSelectExpression == nil {
			return
		}
		call.Yield(upperSelectExpression)
	}
}

// boundAliasesOfReference collects every quantifier alias bound anywhere
// inside ref's expression subgraph (recursively, nested subqueries included).
// Alias binding is semantic — identical across a group's memo members — so
// inspecting the reference's canonical member suffices. A visited set guards
// against reference cycles (recursive CTE self-references).
func boundAliasesOfReference(ref *expressions.Reference) map[values.CorrelationIdentifier]struct{} {
	out := make(map[values.CorrelationIdentifier]struct{})
	var walk func(r *expressions.Reference, visited map[*expressions.Reference]struct{})
	walk = func(r *expressions.Reference, visited map[*expressions.Reference]struct{}) {
		if r == nil {
			return
		}
		if _, seen := visited[r]; seen {
			return
		}
		visited[r] = struct{}{}
		expr := r.Get()
		if expr == nil {
			return
		}
		for _, q := range expr.GetQuantifiers() {
			out[q.GetAlias()] = struct{}{}
			walk(q.GetRangesOver(), visited)
		}
	}
	walk(ref, make(map[*expressions.Reference]struct{}))
	return out
}

// isCrossProduct checks whether the given lower/upper partition
// aligns with the independent quantifiers partitioning. If any
// independent partition has members in BOTH lower and upper, this
// partition is not a cross product.
func isCrossProduct(
	independentPartitioning []map[values.CorrelationIdentifier]struct{},
	lowerAliases, upperAliases map[values.CorrelationIdentifier]struct{},
) bool {
	for _, partition := range independentPartitioning {
		inLower := false
		inUpper := false
		for alias := range partition {
			if _, ok := lowerAliases[alias]; ok {
				inLower = true
			} else if _, ok := upperAliases[alias]; ok {
				inUpper = true
			}
			if inLower && inUpper {
				return false
			}
		}
	}
	return true
}

// computeTransitiveCorrelationOrder computes the transitive closure
// of the depends-on relation among a set of quantifiers. For each
// quantifier alias A, the result maps A to the set of quantifier
// aliases that A transitively depends on (limited to aliases owned
// by quantifiers in the input set).
//
// This is the Go equivalent of Java's
// getCorrelationOrder().getTransitiveClosure().
//
// Implementation: builds a direct dependency map from each quantifier's
// GetCorrelatedTo() (filtering to only owned aliases), then computes
// the transitive closure via topological-order BFS.
func computeTransitiveCorrelationOrder(
	quantifiers []expressions.Quantifier,
) map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{} {
	// Owned aliases.
	owned := make(map[values.CorrelationIdentifier]struct{}, len(quantifiers))
	for _, q := range quantifiers {
		owned[q.GetAlias()] = struct{}{}
	}

	// Direct dependency map: alias → set of owned aliases it depends on.
	// Quantifier.GetCorrelatedTo() delegates to the ranged-over Reference's
	// transitive walk (Java parity) — this captures the quantifier's OWN
	// expression-level correlations to sibling quantifiers, e.g. a plain
	// lateral UNNEST's Explode correlating to its array source with NO
	// predicate between them (without the edge the source and the unnest look
	// like separate independent components and a cross-product bipartition
	// silently NULLs the AS/AT columns). Self-filter dep != q.GetAlias():
	// q.alias is bound at the parent, but Go reuses human-readable aliases, so
	// guard against a leg re-exposing its own alias.
	directDeps := make(map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{}, len(quantifiers))
	for _, q := range quantifiers {
		deps := make(map[values.CorrelationIdentifier]struct{})
		for dep := range q.GetCorrelatedTo() {
			if _, ok := owned[dep]; ok && dep != q.GetAlias() {
				deps[dep] = struct{}{}
			}
		}
		directDeps[q.GetAlias()] = deps
	}

	// Compute transitive closure via Kahn's algorithm / topological BFS.
	// Inverse map: alias → set of aliases that depend on it.
	inverse := make(map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{})
	inDegree := make(map[values.CorrelationIdentifier]int)
	for _, q := range quantifiers {
		a := q.GetAlias()
		if _, ok := inDegree[a]; !ok {
			inDegree[a] = 0
		}
		for dep := range directDeps[a] {
			if _, ok := inverse[dep]; !ok {
				inverse[dep] = make(map[values.CorrelationIdentifier]struct{})
			}
			inverse[dep][a] = struct{}{}
			inDegree[a]++
		}
	}

	// BFS queue starting from zero-in-degree nodes.
	result := make(map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{}, len(quantifiers))
	for _, q := range quantifiers {
		result[q.GetAlias()] = make(map[values.CorrelationIdentifier]struct{})
	}

	queue := make([]values.CorrelationIdentifier, 0, len(quantifiers))
	for _, q := range quantifiers {
		if inDegree[q.GetAlias()] == 0 {
			queue = append(queue, q.GetAlias())
		}
	}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		users := inverse[current]
		for using := range users {
			inDegree[using]--
			if inDegree[using] == 0 {
				queue = append(queue, using)
				// Compute transitive deps for 'using'.
				for dep := range directDeps[using] {
					result[using][dep] = struct{}{}
					for ancestor := range result[dep] {
						result[using][ancestor] = struct{}{}
					}
				}
			}
		}
	}

	return result
}

// computeIndependentQuantifiersPartitioning computes the partitioning
// of quantifiers into independent groups. Two quantifiers are in the
// same group if they are connected by correlation dependencies or by
// shared predicates (transitively).
//
// Ports Java's SelectExpression.computeIndependentQuantifiersPartitioning.
func computeIndependentQuantifiersPartitioning(
	sel *expressions.SelectExpression,
	fullCorrelationOrder map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{},
) []map[values.CorrelationIdentifier]struct{} {
	quantifiers := sel.GetQuantifiers()

	// Initially: one partition per quantifier.
	type partition = map[values.CorrelationIdentifier]struct{}
	partitions := make([]partition, len(quantifiers))
	for i, q := range quantifiers {
		partitions[i] = map[values.CorrelationIdentifier]struct{}{q.GetAlias(): {}}
	}

	// Compute transitive correlation of each predicate.
	predicateTransCorr := make([]map[values.CorrelationIdentifier]struct{}, len(sel.GetPredicates()))
	for i, pred := range sel.GetPredicates() {
		corr := predicates.GetCorrelatedToOfPredicate(pred)
		transCorr := make(map[values.CorrelationIdentifier]struct{})
		for alias := range corr {
			transCorr[alias] = struct{}{}
			for ancestor := range fullCorrelationOrder[alias] {
				transCorr[ancestor] = struct{}{}
			}
		}
		predicateTransCorr[i] = transCorr
	}

	// Union-find via list manipulation — for each quantifier, merge
	// partitions that share connectivity.
	for _, q := range quantifiers {
		alias := q.GetAlias()

		connectedAliases := make(map[values.CorrelationIdentifier]struct{})
		connectedAliases[alias] = struct{}{}
		for a := range fullCorrelationOrder[alias] {
			connectedAliases[a] = struct{}{}
		}

		for _, transCorr := range predicateTransCorr {
			if _, ok := transCorr[alias]; ok {
				for a := range transCorr {
					connectedAliases[a] = struct{}{}
				}
			}
		}

		// Merge all partitions that intersect with connectedAliases.
		// Use a fresh slice rather than partitions[:0]: the range below reads
		// `partitions` while we build `remaining`, and aliasing the backing
		// array is subtle to reason about. N is tiny (one entry per quantifier),
		// so a fresh allocation costs nothing and is unambiguously correct.
		newPartition := make(partition)
		var remaining []partition
		for _, p := range partitions {
			if aliasesIntersect(connectedAliases, p) {
				for a := range p {
					newPartition[a] = struct{}{}
				}
			} else {
				remaining = append(remaining, p)
			}
		}
		remaining = append(remaining, newPartition)
		partitions = remaining

		if len(partitions) == 1 {
			return partitions
		}
	}

	return partitions
}

// intersectAliases returns the intersection of two alias sets.
// Returns nil if the intersection is empty.
// splitRangesByLocalCorrelation undoes, for partitioning only, the merge
// SelectExpression's predicate partitioning applies to comparisons on one
// value (`o.k = 42 AND o.k = c.id` becomes a single sargable on o.k): each
// group of comparisons naming a different set of this select's quantifiers
// becomes its own sargable on the same value. Java partitions the merged
// predicate whole, so a bipartition placing o above c cannot bind `o.k = 42`
// at o and plans o as a full scan; split, the local part stays with o while
// the join part goes with c. A conjunction of ranges is the conjunction of its
// parts, and the selects the halves become merge them again.
func splitRangesByLocalCorrelation(
	preds []predicates.QueryPredicate,
	aliasToQ map[values.CorrelationIdentifier]expressions.Quantifier,
	buriedToExistential map[values.CorrelationIdentifier]values.CorrelationIdentifier,
) []predicates.QueryPredicate {
	localKey := func(comparison predicates.Comparison) string {
		var local []string
		for c := range comparison.GetCorrelatedTo() {
			if esq, buried := buriedToExistential[c]; buried {
				c = esq
			}
			if _, isLocal := aliasToQ[c]; isLocal {
				local = append(local, c.Name())
			}
		}
		slices.Sort(local)
		return strings.Join(slices.Compact(local), ",")
	}
	var out []predicates.QueryPredicate
	for i, pred := range preds {
		pvr, ok := pred.(*predicates.PredicateWithValueAndRanges)
		if !ok || len(pvr.GetRanges()) != 1 || pvr.GetRanges()[0] == nil {
			if out != nil {
				out = append(out, pred)
			}
			continue
		}
		type part struct{ compilable, deferred []predicates.Comparison }
		var keys []string
		parts := map[string]*part{}
		add := func(comparison predicates.Comparison, deferred bool) {
			key := localKey(comparison)
			p := parts[key]
			if p == nil {
				p = &part{}
				parts[key] = p
				keys = append(keys, key)
			}
			if deferred {
				p.deferred = append(p.deferred, comparison)
			} else {
				p.compilable = append(p.compilable, comparison)
			}
		}
		rng := pvr.GetRanges()[0]
		for _, comparison := range rng.GetCompilableComparisons() {
			add(comparison, false)
		}
		for _, comparison := range rng.GetDeferredRanges() {
			add(comparison, true)
		}
		if len(keys) < 2 {
			if out != nil {
				out = append(out, pred)
			}
			continue
		}
		if out == nil {
			out = append(make([]predicates.QueryPredicate, 0, len(preds)+len(keys)), preds[:i]...)
		}
		for _, key := range keys {
			p := parts[key]
			out = append(out, predicates.NewPredicateWithValueAndRanges(pvr.GetValue(),
				[]*predicates.RangeConstraints{predicates.NewRangeConstraints(p.compilable, p.deferred)}))
		}
	}
	if out == nil {
		return preds
	}
	return out
}

func intersectAliases(
	a map[values.CorrelationIdentifier]struct{},
	b map[values.CorrelationIdentifier]struct{},
) map[values.CorrelationIdentifier]struct{} {
	if a == nil || b == nil {
		return nil
	}
	result := make(map[values.CorrelationIdentifier]struct{})
	for k := range a {
		if _, ok := b[k]; ok {
			result[k] = struct{}{}
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// aliasesIntersect reports whether two alias sets have a non-empty
// intersection. Faster than intersectAliases when you only need the
// boolean answer.
func aliasesIntersect(
	a, b map[values.CorrelationIdentifier]struct{},
) bool {
	// Iterate over the smaller set.
	if len(a) > len(b) {
		a, b = b, a
	}
	for k := range a {
		if _, ok := b[k]; ok {
			return true
		}
	}
	return false
}

var _ ExpressionRule = (*PartitionSelectRule)(nil)

// dedupAliases returns aliases with duplicates removed, sorted by name into a
// CANONICAL order. The live set (lowersCorrelatedToByUppers) is collected from
// map iteration (non-deterministic order), so it must be canonicalized for two
// reasons: (1) determinism — the merge RC built from it would otherwise
// vary across runs, producing non-deterministic plans; (2) memoization — two
// partitions yielding the same live set must intern to the SAME merge RC
// (hence the same Reference, RFC-039), or the re-enumeration's search space
// explodes with alias-permuted duplicates of identical sub-joins. (RFC-043.)
func dedupAliases(aliases []values.CorrelationIdentifier) []values.CorrelationIdentifier {
	seen := make(map[values.CorrelationIdentifier]struct{}, len(aliases))
	out := aliases[:0:0]
	for _, a := range aliases {
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// aliasesConnectedByPredicates reports whether the alias set forms a single
// connected component, where ANY predicate whose correlation set touches two
// of the aliases connects them (union-find). Judged over the select's FULL
// conjunct list, not just the predicates that land in the lower half: a
// spanning N-ary predicate (`a.x = b.y OR a.x = c.y`) genuinely JOINS the
// tables it touches even though it can only ever be evaluated upper — judging
// by lower-resident predicates alone declared every bipartition of such a
// select disconnected and left it unimplementable. For binary equijoins the
// two readings coincide (a predicate inside the lower touches exactly the
// same pair either way), so the join re-enumeration task baselines are
// unchanged.
func aliasesConnectedByPredicates(
	aliases map[values.CorrelationIdentifier]struct{},
	preds []predicates.QueryPredicate,
) bool {
	return aliasesConnectedByPredicatesOrCorrelation(aliases, preds, nil)
}

// aliasesConnectedByPredicatesOrCorrelation is the connectivity reading for
// ORDINAL parents: the union graph of predicate edges AND quantifier-level
// correlation edges (fullCorrelationOrder — an Explode's genuine dependency
// on its array source, the edge Java's Quantifier.getCorrelatedTo carries).
// Plain table quantifiers have no correlation edges, so with a nil
// correlationOrder this IS aliasesConnectedByPredicates (which delegates
// here); admitting correlation edges additionally connects the
// unnest-with-source pairings a flat gathered seed relies on.
//
// A correlation edge connects two lower aliases in exactly two spellings:
//   - one depends on the other (an Explode and its array source);
//   - both depend on the same alias OUTSIDE the set (`FROM w, (… w.f …) AS
//     d, (… w.f …) AS e` read as lower {d, e}): each outer row pairs its own
//     d rows with its own e rows, a per-row product the query states, not a
//     free cross product. Go's join result value names every leg, so every
//     lower is live and an upper leg correlated to a lower forbids the
//     one-live-lower flow; that makes {d, e} under {w} the ONLY bipartition
//     of this select, the one Java reaches through its merge case.
//
// Not covered: two aliases whose only tie is a predicate or correlation to a
// THIRD lower alias are connected through it by the union itself, and two
// aliases correlated to DIFFERENT outside aliases stay disconnected.
func aliasesConnectedByPredicatesOrCorrelation(
	aliases map[values.CorrelationIdentifier]struct{},
	preds []predicates.QueryPredicate,
	correlationOrder map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{},
) bool {
	if len(aliases) <= 1 {
		return true
	}
	parent := make(map[values.CorrelationIdentifier]values.CorrelationIdentifier, len(aliases))
	for a := range aliases {
		parent[a] = a
	}
	var find func(values.CorrelationIdentifier) values.CorrelationIdentifier
	find = func(a values.CorrelationIdentifier) values.CorrelationIdentifier {
		for parent[a] != a {
			parent[a] = parent[parent[a]]
			a = parent[a]
		}
		return a
	}
	for _, p := range preds {
		var prev values.CorrelationIdentifier
		have := false
		for a := range intersectAliases(aliases, predicates.GetCorrelatedToOfPredicate(p)) {
			if have {
				parent[find(prev)] = find(a)
			}
			prev = a
			have = true
		}
	}
	sharedOutside := make(map[values.CorrelationIdentifier]values.CorrelationIdentifier)
	for a := range aliases {
		for dep := range correlationOrder[a] {
			if _, in := aliases[dep]; in {
				parent[find(a)] = find(dep)
				continue
			}
			if first, seen := sharedOutside[dep]; seen {
				parent[find(a)] = find(first)
			} else {
				sharedOutside[dep] = a
			}
		}
	}
	var root values.CorrelationIdentifier
	first := true
	for a := range aliases {
		if first {
			root = find(a)
			first = false
			continue
		}
		if find(a) != root {
			return false
		}
	}
	return true
}

// lowerComponentsAreSingletons reports whether every independent component
// intersecting the lower alias set is a SINGLETON — the pure-cross exemption's
// second gate: a multi-alias component inside a disconnected lower is glued by
// quantifier correlation, not predicates (a lateral unnest and its source),
// and tearing it apart builds plans the unnest machinery cannot evaluate.
// Singleton components are plain unjoined tables — the genuine
// unavoidable cross product.
func lowerComponentsAreSingletons(
	partitioning []map[values.CorrelationIdentifier]struct{},
	lowerAliases map[values.CorrelationIdentifier]struct{},
) bool {
	for _, comp := range partitioning {
		if len(comp) == 1 {
			continue
		}
		for a := range comp {
			if _, inLower := lowerAliases[a]; inLower {
				return false
			}
		}
	}
	return true
}

// independentForEachBlockBelowProjectedExistential declines ordinary joins,
// predicate-only or multiple existentials, and lower edge semantics/dependencies.
// Otherwise the sole existential's result-live identity determines the canonical
// partition: all ordinary ForEach rows below, the projected existential above.
// Existing partition dependency, liveness and exact-row guards still apply.
func independentForEachBlockBelowProjectedExistential(
	sel *expressions.SelectExpression,
	correlationOrder map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{},
) (values.CorrelationIdentifier, bool) {
	continuation, ok := soleExistentialOverIndependentForEach(sel, correlationOrder)
	if !ok {
		return values.CorrelationIdentifier{}, false
	}
	_, projected := values.GetCorrelatedToOfValue(sel.GetResultValue())[continuation]
	return continuation, projected
}

// independentForEachBlockBelowFilteringExistential is the predicate-only twin:
// the sole existential over two or more independent ForEach legs, read by the
// select's predicates and not by its result value (`WHERE EXISTS`). The caller
// admits the all-ForEach-below partition only when the existential's predicates
// are what connect that block.
func independentForEachBlockBelowFilteringExistential(
	sel *expressions.SelectExpression,
	correlationOrder map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{},
) (values.CorrelationIdentifier, bool) {
	continuation, ok := soleExistentialOverIndependentForEach(sel, correlationOrder)
	if !ok {
		return values.CorrelationIdentifier{}, false
	}
	if _, projected := values.GetCorrelatedToOfValue(sel.GetResultValue())[continuation]; projected {
		return values.CorrelationIdentifier{}, false
	}
	return continuation, true
}

func withAlias(set map[values.CorrelationIdentifier]struct{}, alias values.CorrelationIdentifier) map[values.CorrelationIdentifier]struct{} {
	out := make(map[values.CorrelationIdentifier]struct{}, len(set)+1)
	for a := range set {
		out[a] = struct{}{}
	}
	out[alias] = struct{}{}
	return out
}

// soleExistentialOverIndependentForEach declines ordinary joins, multiple
// existentials, and lower edge semantics/dependencies, and returns the one
// existential over two or more plain, mutually independent ForEach legs.
func soleExistentialOverIndependentForEach(
	sel *expressions.SelectExpression,
	correlationOrder map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{},
) (values.CorrelationIdentifier, bool) {
	var continuation values.CorrelationIdentifier
	ordinary, existentials := 0, 0
	for _, q := range sel.GetQuantifiers() {
		switch q.Kind() {
		case expressions.QuantifierForEach:
			if q.IsNullOnEmpty() || q.IsStrictSingle() || len(correlationOrder[q.GetAlias()]) != 0 {
				return values.CorrelationIdentifier{}, false
			}
			ordinary++
		case expressions.QuantifierExistential:
			existentials++
			continuation = q.GetAlias()
		default:
			return values.CorrelationIdentifier{}, false
		}
	}
	if ordinary < 2 || existentials != 1 {
		return values.CorrelationIdentifier{}, false
	}
	return continuation, true
}
