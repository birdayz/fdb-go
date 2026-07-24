package cascades

import (
	"sort"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
// candidateExpr. Leaf expressions can still carry external correlations
// (Explode(FieldValue(QOV(outer), array)) is the important case), so it
// enumerates the same unbound-correlation bijections as Java's
// RelationalExpression.enumerateUnboundCorrelatedTo. Subsumption is checked
// via structural equality (EqualsWithoutChildren) under each map.
//
// This is Go's implementation of Java's
// RelationalExpression.subsumedBy for leaf expressions. Java's is
// expression-type-specific (e.g. FullUnorderedScanExpression also
// checks access hints — a surface Go does not carry); Go covers the
// structural-equality path, which handles the leaf shapes the
// candidate builders produce.
func matchLeafWithCandidate(
	queryExpr expressions.RelationalExpression,
	candidateExpr expressions.RelationalExpression,
) []leafMatchResult {
	var results []leafMatchResult
	for _, boundAliasMap := range enumerateLeafCorrelationAliasMaps(
		queryExpr,
		candidateExpr,
	) {
		exprAliasMap := expressions.EmptyAliasMap()
		valid := true
		for _, source := range boundAliasMap.Sources() {
			exprAliasMap, valid = exprAliasMap.With(
				source,
				boundAliasMap.GetTarget(source),
			)
			if !valid {
				break
			}
		}
		if !valid ||
			!queryExpr.EqualsWithoutChildren(candidateExpr, exprAliasMap) {
			continue
		}

		// Build a RegularMatchInfo with empty bindings — the leaf match
		// carries no parameter bindings, no predicate mappings, and no
		// ordering information. The MaxMatchMap, however, is mandatory:
		// Java's RelationalExpression.exactlySubsumedBy always computes one.
		candidateResult := candidateExpr.GetResultValue()
		maxMatchMap := buildMatchMaxMatchMap(
			candidateResult,
			candidateResult,
			boundAliasMap,
		)
		matchInfo := NewRegularMatchInfo(
			nil,
			boundAliasMap,
			nil,
			nil,
			maxMatchMap,
			EmptyGroupByMappings(),
			nil,
			nil,
		)
		results = append(results, leafMatchResult{
			boundAliasMap: boundAliasMap,
			matchInfo:     matchInfo,
		})
	}
	return results
}

func enumerateLeafCorrelationAliasMaps(
	queryExpr expressions.RelationalExpression,
	candidateExpr expressions.RelationalExpression,
) []*AliasMap {
	queryCorrelations := expressions.GetCorrelatedToOfExpression(queryExpr)
	candidateCorrelations := expressions.GetCorrelatedToOfExpression(candidateExpr)
	if len(queryCorrelations) != len(candidateCorrelations) {
		return nil
	}

	builder := NewAliasMapBuilder()
	for alias := range queryCorrelations {
		if _, common := candidateCorrelations[alias]; common {
			if !builder.PutChecked(alias, alias) {
				return nil
			}
		}
	}
	base := builder.Build()
	var queryUnbound []values.CorrelationIdentifier
	var candidateUnbound []values.CorrelationIdentifier
	for alias := range queryCorrelations {
		if !base.ContainsSource(alias) {
			queryUnbound = append(queryUnbound, alias)
		}
	}
	for alias := range candidateCorrelations {
		if !base.ContainsTarget(alias) {
			candidateUnbound = append(candidateUnbound, alias)
		}
	}
	if len(queryUnbound) != len(candidateUnbound) {
		return nil
	}
	sort.Slice(queryUnbound, func(i, j int) bool {
		return queryUnbound[i].Name() < queryUnbound[j].Name()
	})
	sort.Slice(candidateUnbound, func(i, j int) bool {
		return candidateUnbound[i].Name() < candidateUnbound[j].Name()
	})

	var results []*AliasMap
	used := make([]bool, len(candidateUnbound))
	var enumerate func(int, *AliasMapBuilder)
	enumerate = func(depth int, current *AliasMapBuilder) {
		if depth == len(queryUnbound) {
			results = append(results, current.Build())
			return
		}
		for candidateIndex, candidateAlias := range candidateUnbound {
			if used[candidateIndex] {
				continue
			}
			next := NewAliasMapBuilder()
			next.PutAll(current.Build())
			if !next.PutChecked(queryUnbound[depth], candidateAlias) {
				continue
			}
			used[candidateIndex] = true
			enumerate(depth+1, next)
			used[candidateIndex] = false
		}
	}
	seed := NewAliasMapBuilder()
	seed.PutAll(base)
	enumerate(0, seed)
	return results
}

var _ ExpressionRule = (*MatchLeafRule)(nil)
