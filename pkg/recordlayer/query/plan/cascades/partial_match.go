package cascades

import (
	"fmt"
	"reflect"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// PartialMatchImpl is the concrete implementation of PartialMatch.
// Links a query-side Reference/Expression to a candidate-side
// Reference via MatchInfo, establishing that the query subgraph rooted
// at queryRef is result-equivalent to the candidate subgraph rooted at
// candidateRef under the bindings in boundAliasMap (modulo
// compensation).
//
// Ports Java's com.apple.foundationdb.record.query.plan.cascades.PartialMatch.
type PartialMatchImpl struct {
	boundAliasMap   *AliasMap
	matchCandidate  MatchCandidate
	queryRef        *expressions.Reference
	queryExpression expressions.RelationalExpression
	candidateRef    *expressions.Reference
	matchInfo       MatchInfo
}

// NewPartialMatch constructs a PartialMatchImpl with all six core
// fields. Mirrors Java's PartialMatch constructor.
func NewPartialMatch(
	boundAliasMap *AliasMap,
	matchCandidate MatchCandidate,
	queryRef *expressions.Reference,
	queryExpression expressions.RelationalExpression,
	candidateRef *expressions.Reference,
	matchInfo MatchInfo,
) *PartialMatchImpl {
	return &PartialMatchImpl{
		boundAliasMap:   boundAliasMap,
		matchCandidate:  matchCandidate,
		queryRef:        queryRef,
		queryExpression: queryExpression,
		candidateRef:    candidateRef,
		matchInfo:       matchInfo,
	}
}

// GetBoundAliasMap returns the alias map of all bound correlated
// references. Mirrors Java's PartialMatch.getBoundAliasMap().
func (p *PartialMatchImpl) GetBoundAliasMap() *AliasMap {
	return p.boundAliasMap
}

// GetMatchCandidate returns the match candidate this partial match
// was established against. Satisfies the PartialMatch interface.
func (p *PartialMatchImpl) GetMatchCandidate() MatchCandidate {
	return p.matchCandidate
}

// GetQueryRef returns the expression reference on the query graph
// side. Mirrors Java's PartialMatch.getQueryRef().
func (p *PartialMatchImpl) GetQueryRef() *expressions.Reference {
	return p.queryRef
}

// GetQueryExpression returns the expression on the query graph side.
// Mirrors Java's PartialMatch.getQueryExpression().
func (p *PartialMatchImpl) GetQueryExpression() expressions.RelationalExpression {
	return p.queryExpression
}

// GetCandidateRef returns the expression reference on the match
// candidate side. Mirrors Java's PartialMatch.getCandidateRef().
func (p *PartialMatchImpl) GetCandidateRef() *expressions.Reference {
	return p.candidateRef
}

// GetMatchInfo returns the match information. Satisfies the
// PartialMatch interface.
func (p *PartialMatchImpl) GetMatchInfo() MatchInfo {
	return p.matchInfo
}

// GetRegularMatchInfo delegates to matchInfo.GetRegularMatchInfo().
// Mirrors Java's PartialMatch.getRegularMatchInfo().
func (p *PartialMatchImpl) GetRegularMatchInfo() *RegularMatchInfo {
	return p.matchInfo.GetRegularMatchInfo()
}

// String returns "ExprTypeName[CandidateName]", mirroring Java's
// PartialMatch.toString(). Uses the Go type name of the query
// expression (without package prefix) as the expression type name.
func (p *PartialMatchImpl) String() string {
	exprType := reflect.TypeOf(p.queryExpression)
	if exprType.Kind() == reflect.Ptr {
		exprType = exprType.Elem()
	}
	return fmt.Sprintf("%s[%s]", exprType.Name(), p.matchCandidate.CandidateName())
}

// GetBoundParameterPrefixMap returns the parameter binding map from
// the match info. Ports Java's PartialMatch.getBoundParameterPrefixMap().
func (p *PartialMatchImpl) GetBoundParameterPrefixMap() map[values.CorrelationIdentifier]*predicates.ComparisonRange {
	return p.GetRegularMatchInfo().GetParameterBindingMap()
}

// PullUp computes the PullUp chain for this partial match from the
// candidate side. The rangedOverAliases are the candidate-side
// quantifier aliases (targets in the binding alias map).
// Ports Java's PartialMatch.pullUp(candidateAlias).
func (p *PartialMatchImpl) PullUp(candidateAlias values.CorrelationIdentifier) *PullUp {
	mi := p.GetRegularMatchInfo()
	mmm := mi.GetMaxMatchMap()
	if mmm == nil {
		return nil
	}
	bam := mi.GetBindingAliasMap()
	rangedOver := make(map[values.CorrelationIdentifier]struct{})
	for _, src := range bam.Sources() {
		rangedOver[bam.GetTarget(src)] = struct{}{}
	}
	return NewPullUp(nil, candidateAlias, mmm.GetCandidateValue(), rangedOver)
}

// CompensateCompleteMatch computes compensation for a complete match.
// Computes child compensation (union of matched quantifier compensations),
// predicate compensation (residual filters), and result compensation.
//
// Ports Java's PartialMatch.compensateCompleteMatch +
// SelectExpression.compensate (full predicate compensation computation).
func (p *PartialMatchImpl) CompensateCompleteMatch(
	unificationPullUp *PullUp,
	candidateTopAlias values.CorrelationIdentifier,
) Compensation {
	// Build the PullUp. For adjusted match infos (match wrapping),
	// use NestPullUp to build a chain through the candidate expression
	// hierarchy. For simple matches, use the flat PullUp.
	var pullUp *PullUp
	mi := p.GetRegularMatchInfo()
	if p.GetMatchInfo().IsAdjusted() && p.GetCandidateRef() != nil {
		rootOfMatchPullUp, _ := NestPullUp(p, unificationPullUp, candidateTopAlias)
		pullUp = rootOfMatchPullUp
	}
	if pullUp == nil {
		pullUp = p.PullUp(candidateTopAlias)
		if pullUp == nil {
			return ImpossibleCompensation
		}
	}

	quantifiers := p.queryExpression.GetQuantifiers()

	// Phase 1: Compute child compensation — union of compensations from
	// matched ForEach quantifiers' child partial matches.
	var childCompensations []Compensation
	for _, q := range quantifiers {
		if q.Kind() != expressions.QuantifierForEach {
			continue
		}
		childPM := mi.GetChildPartialMatchMaybe(q.GetAlias())
		if childPM == nil {
			continue
		}
		if childPMI, ok := childPM.(*PartialMatchImpl); ok {
			bam := mi.GetBindingAliasMap()
			childAlias := bam.GetTarget(q.GetAlias())
			childComp := childPMI.CompensateCompleteMatch(nil, childAlias)
			childCompensations = append(childCompensations, childComp)
		}
	}
	childCompensation := UnionCompensations(childCompensations)
	if childCompensation.IsImpossible() || !childCompensation.CanBeDeferred() {
		return ImpossibleCompensation
	}

	// Phase 2: Predicate compensation — iterate over query predicates,
	// look up their mappings in the predicate map, and compute per-
	// predicate compensation functions.
	predicateMap := mi.GetPredicateMap()
	unmatchedQs := p.GetUnmatchedQuantifiers()
	unmatchedAliases := make(map[values.CorrelationIdentifier]struct{}, len(unmatchedQs))
	for _, q := range unmatchedQs {
		unmatchedAliases[q.GetAlias()] = struct{}{}
	}

	boundPrefixMap := p.GetBoundParameterPrefixMap()
	isAnyCompensationFunctionImpossible := false
	isAnyCompensationFunctionNeeded := false

	var predCompKeys []predicates.QueryPredicate
	var predCompVals []PredicateCompensationFunc

	if sel, ok := p.queryExpression.(*expressions.SelectExpression); ok {
		for _, pred := range sel.GetPredicates() {
			mappings := predicateMap.Get(pred)
			if len(mappings) == 0 {
				continue
			}

			// If the predicate references an unmatched quantifier,
			// compensation is impossible.
			predCorrelated := predicates.GetCorrelatedToOfPredicate(pred)
			for alias := range predCorrelated {
				if _, unmatched := unmatchedAliases[alias]; unmatched {
					isAnyCompensationFunctionImpossible = true
				}
			}

			// Iterate over mappings: use first non-empty compensation.
			// If any mapping says "not needed", skip this predicate entirely.
			var compensationFunction PredicateCompensationFunc
			isCompensationFunctionNeeded := true
			isCompensationFunctionImpossible := true

			for _, mapping := range mappings {
				predComp := mapping.GetPredicateCompensation()
				compFn := predComp(p, boundPrefixMap)
				if !compFn.IsNeeded() {
					isCompensationFunctionNeeded = false
					break
				}
				if compensationFunction == nil {
					compensationFunction = compFn
				}
				if !compFn.IsImpossible() {
					isCompensationFunctionImpossible = false
				}
			}

			if isCompensationFunctionNeeded && compensationFunction != nil {
				isAnyCompensationFunctionNeeded = true
				if isCompensationFunctionImpossible {
					isAnyCompensationFunctionImpossible = true
				}
				predCompKeys = append(predCompKeys, pred)
				predCompVals = append(predCompVals, compensationFunction)
			}
		}
	}

	predicateCompensationMap := NewPredicateCompensationMap(predCompKeys, predCompVals)

	// Phase 3: Result compensation via PullUp.
	cr := ComputeResultCompensation(p, pullUp)
	if cr == nil {
		return ImpossibleCompensation
	}
	isAnyCompensationFunctionImpossible = isAnyCompensationFunctionImpossible || cr.Impossible

	// Phase 4: Determine whether compensation is needed at all.
	matchedQs := p.GetMatchedQuantifiers()

	isCompensationNeeded := childCompensation.IsNeeded() ||
		len(unmatchedQs) > 0 ||
		isAnyCompensationFunctionNeeded ||
		cr.ResultCompensationFn.IsNeeded()

	if !isCompensationNeeded {
		return NoCompensation
	}

	// Phase 5: Multi-quantifier guard — if compensation is needed and
	// more than one ForEach quantifier is matched (has a child partial
	// match), compensation is impossible (requires cross-reference
	// value translation not yet supported).
	forEachMatchedCount := 0
	for _, q := range quantifiers {
		if q.Kind() == expressions.QuantifierForEach && mi.GetChildPartialMatchMaybe(q.GetAlias()) != nil {
			forEachMatchedCount++
		}
	}
	if forEachMatchedCount > 1 {
		return ImpossibleCompensation
	}

	return NewForMatchCompensation(
		isAnyCompensationFunctionImpossible,
		childCompensation,
		predicateCompensationMap,
		matchedQs,
		unmatchedQs,
		p.GetCompensatedAliases(),
		cr.ResultCompensationFn,
		cr.GroupByMappings,
	)
}

// GetMatchedQuantifiers returns the query expression's quantifiers
// that have child partial matches in the match info. Ports Java's
// PartialMatch.getMatchedQuantifiers().
func (p *PartialMatchImpl) GetMatchedQuantifiers() []expressions.Quantifier {
	mi := p.GetRegularMatchInfo()
	var matched []expressions.Quantifier
	for _, q := range p.queryExpression.GetQuantifiers() {
		if mi.GetChildPartialMatchMaybe(q.GetAlias()) != nil {
			matched = append(matched, q)
		}
	}
	return matched
}

// GetUnmatchedQuantifiers returns the query expression's quantifiers
// that do NOT have child partial matches. Ports Java's
// PartialMatch.getUnmatchedQuantifiers().
func (p *PartialMatchImpl) GetUnmatchedQuantifiers() []expressions.Quantifier {
	mi := p.GetRegularMatchInfo()
	var unmatched []expressions.Quantifier
	for _, q := range p.queryExpression.GetQuantifiers() {
		if mi.GetChildPartialMatchMaybe(q.GetAlias()) == nil {
			unmatched = append(unmatched, q)
		}
	}
	return unmatched
}

// CompensationCanBeDeferred reports whether compensation for this
// match can be deferred to a higher level. Returns false if any
// unmatched quantifier is ForEach (affects cardinality). Ports
// Java's PartialMatch.compensationCanBeDeferred().
func (p *PartialMatchImpl) CompensationCanBeDeferred() bool {
	for _, q := range p.GetUnmatchedQuantifiers() {
		if q.Kind() == expressions.QuantifierForEach {
			return false
		}
	}
	return true
}

// GetBoundSargableAliases returns the sargable aliases that have
// non-empty parameter bindings. Ports Java's
// PartialMatch.getBoundSargableAliases.
func (p *PartialMatchImpl) GetBoundSargableAliases() map[values.CorrelationIdentifier]struct{} {
	result := make(map[values.CorrelationIdentifier]struct{})
	for alias, cr := range p.GetBoundParameterPrefixMap() {
		if !cr.IsEmpty() {
			result[alias] = struct{}{}
		}
	}
	return result
}

// GetCompensatedAliases returns the set of quantifier aliases that
// this partial match compensates for. Ports Java's
// PartialMatch.getCompensatedAliases.
func (p *PartialMatchImpl) GetCompensatedAliases() map[values.CorrelationIdentifier]struct{} {
	result := make(map[values.CorrelationIdentifier]struct{})
	for _, q := range p.GetMatchedQuantifiers() {
		result[q.GetAlias()] = struct{}{}
	}
	return result
}

// Remaining not yet ported: nestPullUp, prepareForUnification,
// pullUpToParent, getPulledUpPredicateMappings, compensate (full
// SelectExpression delegation), compensateExistential,
// getAccumulatedPredicateMap, matchInfosFromMap.

// Compile-time interface satisfaction check.
var _ PartialMatch = (*PartialMatchImpl)(nil)
