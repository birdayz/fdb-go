package plans

// ContinuableWithoutDuplicatesProperty — port of Java's
// ContinuableWithoutDuplicatesProperty.
//
// THE INVARIANT: a plan is continuable-without-duplicates when resuming it from
// a continuation can never re-emit a row an earlier page already emitted. It is
// a precondition of STREAMING AGGREGATION, and only of streaming aggregation:
// an aggregate folds each input row into an accumulator, so a row delivered
// twice across a page boundary is counted twice and COUNT/SUM/AVG come out
// wrong. Every other consumer of a re-emitted row is merely redundant; the
// aggregate is the one that is WRONG.
//
// PLACEMENT. Java files this under cascades/properties. Go cannot: the
// properties package is plan-type-blind BY CONSTRUCTION, because plans imports
// properties (cardinality_bounds.go), so a property that dispatches on plan
// types cannot live there without a cycle. It lives beside the plans it
// classifies instead, next to the other plan-level properties already here
// (physical_equality_properties.go, cardinality_bounds.go).
//
// THE FALSE SET IS EMPTY IN GO, AND THAT IS A DERIVED RESULT, NOT AN OMISSION.
// WHAT THIS PROPERTY DOES NOT COVER, stated first because the name invites the
// wrong reading: it is not a duplicate-freeness proof. It answers only whether
// RESUMING can re-emit a row already emitted before the continuation. A plan
// that produces duplicates WITHIN a single page — an unordered union over two
// overlapping index scans, say, which double-counts with no continuation
// involved — answers TRUE here and is correct to. Java draws the same line and
// owns that case with a separate DistinctRecordsProperty.
//
// Java's visitor returns false for exactly two plans —
// RecordQueryUnorderedPrimaryKeyDistinctPlan and RecordQueryUnorderedDistinctPlan
// — because JAVA rebuilds their dedup set per execution:
// RecordQueryUnorderedPrimaryKeyDistinctPlan.java:100-104 mints a fresh
// `new HashSet<>()` and passes the inner's continuation through untouched, so a
// duplicate spanning a resume is silently re-admitted. That is a statement about
// Java's executor, not about what a DISTINCT plan means.
//
// Go removed that premise. The seen-set is carried ACROSS PAGES BY REFERENCE
// through the statement-scoped ExecutionScratch (executor/execution_scratch.go):
// a continuation names the live set with a token, the next page adopts it
// untouched, and adoption/retirement is keyed on nameability so a retried page
// cannot observe a dying attempt's writes. Go's counterparts of Java's two
// plans therefore do NOT re-admit a duplicate across a continuation, and they
// answer TRUE here. Both are still listed explicitly below, at exactly the arms
// Java overrides, so the divergence is visible at the decision site rather than
// inferred from an absence.
//
// THIS IS THE ONE WRONG-ANSWER DIRECTION IN THIS FILE — Go says TRUE where Java
// says FALSE — so the premise is named by the tests that HOLD it rather than by
// a PR number, and a reader can check it in one hop:
//
//	executor/executor_test.go:259
//	  TestExecuteUnorderedPrimaryKeyDistinct_ContinuationCarriesSeenSet
//	executor/distinct_scratch_lifecycle_test.go     (adoption + retirement)
//	executor/distinct_continuation_growth_test.go   (the set survives paging)
//	executor/distinct_nameability_test.go           (a retried page cannot
//	                                                 observe a dying attempt)
//	executor/distinct_proof_stamp_identity_test.go  (the token names THAT set)
//
// If any of those five stops holding, this file's TRUE for the two distinct
// plans becomes a wrong answer, not a divergence.
//
// Importing Java's conclusion instead of its reasoning would have been the
// error the strictlySorted refusal already named: never import a conclusion
// whose premise Go removed. The invariant is what ports; the false set is
// derived from GO's continuation semantics.
//
// The audit behind the empty set covered every plan the streaming-aggregation
// rule can sit over. Each one either resumes POSITIONALLY (scan, index,
// nested-loop join and flat-map, which re-materialize an inner but restore the
// exact index and verify it against a saved key), SERIALIZES its whole state
// into the continuation (in-memory sort carries its remaining sorted buffer;
// unordered union carries a per-child slot; the recursive union carries its
// temp-table frontier on every emitted row), carries state BESIDE the bytes in
// the scratch (the two distinct plans), or REFUSES to resume at all rather than
// restarting (the buffered union fallback returns UnsupportedContinuationError
// when the branch column names are not statically known). None re-emits.
//
// WHAT WOULD RE-ARM THIS. A future cursor whose resume can re-emit an
// already-emitted row: one that restarts an inner instead of resuming it, or
// that holds emission-affecting state in memory without either serializing it
// or parking it in the ExecutionScratch. Such a plan belongs in the false set
// below — but adding it there is NOT sufficient on its own, because Go has no
// hash-aggregation fallback. See the precondition recorded on
// ImplementStreamingAggregationRule: with a non-empty false set, GROUP BY over
// the declined shape would fail to plan rather than fall back, so the fallback
// has to land first.
//
// A SECOND GAP SITS BESIDE THAT ONE, and it is recorded here because it becomes
// live under the same condition. Java uses this property in TWO roles at
// ImplementStreamingAggregationRule.java:67-78: as a FILTER, and as part of the
// partition ROLL-UP KEY — `Set.of(continuable, ordering)` — so continuable and
// non-continuable plans can never be rolled into a single partition. Go ports
// only the filter. That is harmless while the false set is empty, because one
// class means the key has nothing to separate; the moment a plan answers FALSE,
// Go would merge plans Java keeps apart and the filter would be deciding over a
// partition that should never have been formed. Both gaps — the missing
// fallback and the missing roll-up key — have to close together with the first
// false entry.

// ContinuableWithoutDuplicatesVisitor is Java's
// ContinuableWithoutDuplicatesPropertyVisitor: a per-plan-type override point
// over a default that recurses into children.
type ContinuableWithoutDuplicatesVisitor struct{}

// Visit reports whether the plan tree rooted at p can be resumed from a
// continuation without re-emitting an already-emitted row.
//
// Java's visitor short-circuits by returning false from an overridden arm. Go
// splits the same logic in two so that a per-node verdict can never
// accidentally skip the subtree: selfContinuableWithoutDuplicates answers for
// THIS NODE ONLY, and the children are always folded in here. A node that is
// itself safe but sits over an unsafe child is unsafe, which is what Java's
// fromChildren default expresses.
func (v ContinuableWithoutDuplicatesVisitor) Visit(p RecordQueryPlan) bool {
	if p == nil {
		return true
	}
	if !v.selfContinuableWithoutDuplicates(p) {
		return false
	}
	return v.fromChildren(p.GetChildren())
}

// selfContinuableWithoutDuplicates is the per-plan-type override point — the
// visitXxx arms in Java. It answers for the node alone; Visit folds in the
// children.
//
// Java's false set is {RecordQueryUnorderedPrimaryKeyDistinctPlan,
// RecordQueryUnorderedDistinctPlan}. Go's is EMPTY. The two arms below are the
// Go counterparts of exactly those Java plans, kept explicit so the divergence
// is recorded where the decision is made.
func (v ContinuableWithoutDuplicatesVisitor) selfContinuableWithoutDuplicates(p RecordQueryPlan) bool {
	switch p.(type) {
	case *RecordQueryUnorderedPrimaryKeyDistinctPlan:
		// Java: FALSE — its HashSet of seen primary keys is minted per
		// execution and lost across the continuation.
		// Go: TRUE — the seen-set is carried across pages by reference through
		// the ExecutionScratch, so a duplicate spanning a page boundary is not
		// re-admitted.
		return true
	case *RecordQueryDistinctPlan:
		// Go's unordered-by-row distinct, the counterpart of Java's
		// RecordQueryUnorderedDistinctPlan.
		// Java: FALSE — same per-execution HashSet, on a comparison key.
		// Go: TRUE on BOTH of its executors. The Streaming form dedups adjacent
		// rows and rides its single previous key in the continuation; the hash
		// form carries its whole set in the ExecutionScratch. Neither rebuilds
		// empty on resume.
		return true
	default:
		// Java's visitDefault: nothing else maintains ephemeral emission state.
		return true
	}
}

// fromChildren is Java's fromChildren: every child must also be continuable
// without duplicates.
func (v ContinuableWithoutDuplicatesVisitor) fromChildren(children []RecordQueryPlan) bool {
	for _, c := range children {
		if !v.Visit(c) {
			return false
		}
	}
	return true
}

// EvaluateContinuableWithoutDuplicates is Java's
// ContinuableWithoutDuplicatesProperty.evaluate(plan).
func EvaluateContinuableWithoutDuplicates(p RecordQueryPlan) bool {
	return ContinuableWithoutDuplicatesVisitor{}.Visit(p)
}
