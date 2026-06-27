package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// MatchLeafRule is the Cascades rule that seeds the partial-match
// infrastructure by matching query leaf expressions against candidate
// leaf expressions. For every query expression with zero quantifiers,
// the rule iterates all MatchCandidates from the PlanContext, walks
// each candidate's Traversal to find its leaf References, and attempts
// a structural match (EqualsWithoutChildren) between the query leaf
// and each candidate leaf. On match, a PartialMatch is created and
// stored on the query Reference.
//
// This rule seeds the memoisation structure for partial matches kept
// on Reference. It prepares further rules such as
// MatchIntermediateRule and AdjustMatchRule.
//
// Ports Java's
// com.apple.foundationdb.record.query.plan.cascades.rules.MatchLeafRule.
type MatchLeafRule struct {
	matcher *ExpressionMatcher[expressions.RelationalExpression]
}

// NewMatchLeafRule constructs a MatchLeafRule.
func NewMatchLeafRule() *MatchLeafRule {
	return &MatchLeafRule{
		matcher: NewExpressionMatcher[expressions.RelationalExpression]("match_leaf"),
	}
}

// Matcher returns the binding matcher. Matches any RelationalExpression
// (the leaf check is performed inside OnMatch). This mirrors Java's
// MatchLeafRule which returns Optional.empty() from getRootOperator()
// so it fires on all expression types.
func (r *MatchLeafRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch iterates all MatchCandidates, finds leaf references in each
// candidate's Traversal, and attempts a structural match between the
// query expression and each candidate leaf expression.
func (r *MatchLeafRule) OnMatch(call *ExpressionRuleCall) {
	expr := call.Bindings.Get(r.matcher).(expressions.RelationalExpression)

	// Only match leaf expressions (no quantifiers).
	if len(expr.GetQuantifiers()) != 0 {
		return
	}

	ctx := call.Context
	if ctx == nil {
		return
	}

	for _, candidate := range ctx.GetMatchCandidates() {
		traversal := candidate.GetTraversal()
		if traversal == nil {
			continue
		}

		for _, leafRef := range traversal.GetLeafReferences() {
			for _, leafExpr := range leafRef.AllMembers() {
				// A leaf reference may contain members with quantifiers
				// (e.g. a reference that also holds a non-leaf
				// alternative). Filter to actual leaf expressions.
				if len(leafExpr.GetQuantifiers()) != 0 {
					continue
				}

				matchResults := matchLeafWithCandidate(expr, leafExpr)
				for _, mr := range matchResults {
					pm := NewPartialMatch(
						mr.boundAliasMap,
						candidate,
						call.Reference, // query reference
						expr,
						leafRef, // candidate reference
						mr.matchInfo,
					)
					AddPartialMatchForCandidate(call.Reference, candidate, pm)
				}
			}
		}
	}
}

// leafMatchResult pairs a bound alias map with its resulting MatchInfo.
type leafMatchResult struct {
	boundAliasMap *AliasMap
	matchInfo     MatchInfo
}

// matchLeafWithCandidate checks if queryExpr is subsumed by
// candidateExpr. For leaf expressions (no quantifiers, no
// correlations), the alias enumeration is trivial — only the empty
// alias map needs to be tested. Subsumption is checked via structural
// equality (EqualsWithoutChildren).
//
// This is the seed implementation of Java's
// RelationalExpression.subsumedBy for leaf expressions. The full
// implementation will be expression-type-specific (e.g.
// FullUnorderedScanExpression checks access hints); the seed covers
// the structural-equality path which handles the common case.
func matchLeafWithCandidate(
	queryExpr expressions.RelationalExpression,
	candidateExpr expressions.RelationalExpression,
) []leafMatchResult {
	// For leaf expressions with no correlations,
	// enumerateUnboundCorrelatedTo yields exactly one entry: the
	// empty alias map. This mirrors the Java flow where leaf
	// expressions have empty correlatedTo sets.
	boundAliasMap := EmptyAliasMap()

	// Subsumption check: structural equality under the empty alias map.
	// Converts from cascades.AliasMap to expressions.AliasMap for the
	// EqualsWithoutChildren call.
	exprAliasMap := expressions.EmptyAliasMap()
	if !queryExpr.EqualsWithoutChildren(candidateExpr, exprAliasMap) {
		return nil
	}

	// Build a RegularMatchInfo with empty bindings — the leaf match
	// carries no parameter bindings, no predicate mappings, and no
	// ordering information. The MaxMatchMap, however, is mandatory:
	// Java's RelationalExpression.exactlySubsumedBy always computes one
	// (between the leaf's result value and the candidate leaf's result
	// value). Without it PartialMatch.PullUp returns nil and compensation
	// is impossible, so the data-access path never produces a scan.
	// The leaf subsumption above proved EqualsWithoutChildren(query,
	// candidate): the two leaves flow the identical row. Java relates them
	// via the leaf translationMap (query scan alias → candidate scan
	// alias), so translatedQueryResultValue == candidateResultValue — an
	// identity MaxMatchMap. Go's leaves carry no stable result-value alias
	// (FullUnorderedScanExpression.GetResultValue mints a fresh QOV each
	// call), so model the identity directly: use the candidate's result
	// value for both sides.
	candResult := candidateExpr.GetResultValue()
	mmm := buildMatchMaxMatchMap(candResult, candResult, boundAliasMap)
	mi := NewRegularMatchInfo(
		nil,                    // parameterBindingMap
		boundAliasMap,          // bindingAliasMap
		nil,                    // predicateMap
		nil,                    // matchedOrderingParts
		mmm,                    // maxMatchMap
		EmptyGroupByMappings(), // groupByMappings
		nil,                    // rollUpToGroupingValues
		nil,                    // additionalPlanConstraint
	)

	return []leafMatchResult{{boundAliasMap: boundAliasMap, matchInfo: mi}}
}

var _ ExpressionRule = (*MatchLeafRule)(nil)
