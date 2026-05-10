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
// result compensation via PullUp, and combines them.
// Ports Java's PartialMatch.compensateCompleteMatch +
// SelectExpression.compensate (simplified).
func (p *PartialMatchImpl) CompensateCompleteMatch(
	unificationPullUp *PullUp,
	candidateTopAlias values.CorrelationIdentifier,
) Compensation {
	pullUp := p.PullUp(candidateTopAlias)
	if pullUp == nil {
		return ImpossibleCompensation
	}

	// Compute child compensation: union of compensations from matched
	// ForEach quantifiers' child partial matches.
	mi := p.GetRegularMatchInfo()
	var childCompensations []Compensation
	for _, q := range p.queryExpression.GetQuantifiers() {
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

	// Compute result compensation via PullUp.
	cr := ComputeResultCompensation(p, pullUp)
	if cr == nil {
		return ImpossibleCompensation
	}

	unmatchedQs := p.GetUnmatchedQuantifiers()
	matchedQs := p.GetMatchedQuantifiers()

	if !childCompensation.IsNeededForFiltering() &&
		!cr.ResultCompensationFn.IsNeeded() &&
		len(unmatchedQs) == 0 {
		return NoCompensation
	}

	return NewForMatchCompensation(
		cr.Impossible,
		childCompensation,
		EmptyPredicateCompensationMap(),
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
