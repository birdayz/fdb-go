package cascades

import (
	"fmt"
	"reflect"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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

// GetBoundParameterPrefixMap returns the candidate-defined subset of bound
// parameters that its physical scan can actually satisfy — NOT the whole
// binding map. For ordinary ordered scans that subset is a leading prefix;
// specialized candidates may retain independent index-only bindings. Ports Java's
// PartialMatch.getBoundParameterPrefixMap → MatchCandidate.
// computeBoundParameterPrefixMap (MatchCandidate.java:147-180).
//
// The distinction is load-bearing: a scan binds an index prefix, so a
// parameter bound out of order (or after a gap) is NOT usable by the scan
// and must stay a residual the compensation re-applies. Returning the full
// map claimed bindings the scan never performs — under-compensating (the
// dropped predicate silently stops filtering) and over-counting bound
// parameters wherever the count drives ranking.
//
// Which bindings form a usable prefix is the CANDIDATE's decision, not a
// generic rule, so this delegates rather than walking the sargable aliases
// itself. A value index consumes a contiguous equality run and may end on one
// inequality; a vector index keeps its DistanceRank binding even across a
// partial partition prefix and refuses partition inequalities outright. Every
// other consumer of a prefix map already goes through the candidate, and a
// second, generic implementation here would silently disagree with the scan
// that ultimately gets built.
func (p *PartialMatchImpl) GetBoundParameterPrefixMap() map[values.CorrelationIdentifier]*predicates.ComparisonRange {
	if p.matchCandidate == nil {
		return map[values.CorrelationIdentifier]*predicates.ComparisonRange{}
	}
	return p.matchCandidate.ComputeBoundParameterPrefixMap(
		p.GetRegularMatchInfo().GetParameterBindingMap())
}

// PullUp computes the PullUp chain for this partial match from the
// candidate side, by nesting through the candidate expression hierarchy.
// Returns nil when the chain cannot be built faithfully (see NestPullUp).
//
// Ports Java's PartialMatch.pullUp(candidateAlias), which is defined as
// nestPullUp(null, candidateAlias).getRight().
func (p *PartialMatchImpl) PullUp(candidateAlias values.CorrelationIdentifier) *PullUp {
	_, current := NestPullUp(p, nil, candidateAlias)
	return current
}

// CompensateCompleteMatch computes compensation for a complete match, at the
// top of a match tree. The bound-parameter prefix map computed here is the one
// the WHOLE tree compensates against — the parameters belong to the match
// candidate, not to any single level — so it is threaded down unchanged.
//
// Ports Java's PartialMatch.compensateCompleteMatch.
func (p *PartialMatchImpl) CompensateCompleteMatch(
	unificationPullUp *PullUp,
	candidateTopAlias values.CorrelationIdentifier,
) Compensation {
	return p.compensate(p.GetBoundParameterPrefixMap(), unificationPullUp, candidateTopAlias)
}

// compensate computes child compensation (union of matched quantifier
// compensations), predicate compensation (residual filters), and result
// compensation for this match level.
//
// The two pull-ups this level derives are NOT interchangeable. `current` is
// the innermost level: predicates and child matches live in that scope, so
// they translate through it. `root` is the level this nesting introduced on
// top of the incoming chain, which is the scope the query's RESULT value has
// to reach; it is nil when this level continues someone else's match, meaning
// no result compensation is owed here. Using one where the other belongs
// either loses a level of translation or invents one.
//
// Ports Java's PartialMatch.compensate + SelectExpression.compensate.
func (p *PartialMatchImpl) compensate(
	boundPrefixMap map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	incomingPullUp *PullUp,
	candidateTopAlias values.CorrelationIdentifier,
) Compensation {
	mi := p.GetRegularMatchInfo()

	rootOfMatchPullUp, pullUp := NestPullUp(p, incomingPullUp, candidateTopAlias)
	if pullUp == nil {
		// The candidate chain could not be rebuilt faithfully; compensating
		// off a guessed chain is how wrong rows get shipped.
		return ImpossibleCompensation
	}

	quantifiers := p.queryExpression.GetQuantifiers()

	// Phase 1: Compute child compensation — union of compensations from
	// matched ForEach quantifiers' child partial matches. Each child nests
	// UNDER this level's pull-up and compensates against the same
	// candidate-wide prefix map.
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
			childComp := childPMI.compensate(boundPrefixMap, pullUp, childAlias)
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

	isAnyCompensationFunctionImpossible := false
	isAnyCompensationFunctionNeeded := false

	var predCompKeys []predicates.QueryPredicate
	var predCompVals []PredicateCompensationFunc

	// Predicate compensation applies to any query expression that carries
	// predicates — a SelectExpression OR a LogicalFilterExpression. Java
	// has only SelectExpression (filters ARE Selects); Go's matching path
	// (matchSingleSourceAgainstSelect) already treats both uniformly, so
	// compensation must too. Gating on *SelectExpression alone silently
	// dropped residual filters on a bare LogicalFilter query (wrong rows).
	if pp, ok := p.queryExpression.(interface {
		GetPredicates() []predicates.QueryPredicate
	}); ok {
		compensatePredicate := func(
			pred predicates.QueryPredicate,
			mappings []*PredicateMapping,
		) {
			// If the predicate references an unmatched quantifier,
			// compensation is impossible.
			for alias := range predicates.GetCorrelatedToOfPredicate(pred) {
				if _, unmatched := unmatchedAliases[alias]; unmatched {
					isAnyCompensationFunctionImpossible = true
				}
			}

			// Several mappings can compensate the same predicate. If any of
			// them says "not needed" the predicate needs no compensation at
			// all (that mapping is a tautological placeholder). Otherwise take
			// a POSSIBLE alternative in preference to an impossible one:
			// keeping the first-seen function while reporting "possible"
			// because some later mapping was possible hands the applier a
			// function that cannot be applied. Falls back to the first
			// function when every alternative is impossible, so the
			// impossibility still propagates.
			var compensationFunction PredicateCompensationFunc
			isCompensationFunctionNeeded := true
			isCompensationFunctionImpossible := true

			for _, mapping := range mappings {
				predComp := mapping.GetPredicateCompensation()
				compFn := predComp(p, boundPrefixMap, pullUp)
				if !compFn.IsNeeded() {
					isCompensationFunctionNeeded = false
					break
				}
				if compFn.IsImpossible() {
					if compensationFunction == nil {
						compensationFunction = compFn
					}
					continue
				}
				if isCompensationFunctionImpossible {
					// First possible alternative — it wins over any impossible
					// one recorded so far.
					compensationFunction = compFn
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

		for _, topLevelPredicate := range pp.GetPredicates() {
			// Prefer the original predicate identity when it is mapped
			// directly. Select subsumption and the legacy Filter-to-Select
			// adapter both flatten ANDs into leaf mappings, so fall back to
			// those conjunct identities below.
			if mappings := predicateMap.Get(topLevelPredicate); len(mappings) > 0 {
				compensatePredicate(topLevelPredicate, mappings)
				continue
			}

			// A mapped top-level AND has no entry under the parent node. Visit
			// its leaves so every residual conjunct reaches compensation.
			for _, conjunct := range flattenConjuncts(
				[]predicates.QueryPredicate{topLevelPredicate},
			) {
				if mappings := predicateMap.Get(conjunct); len(mappings) > 0 {
					compensatePredicate(conjunct, mappings)
				}
			}
		}
	}

	predicateCompensationMap := NewPredicateCompensationMap(predCompKeys, predCompVals)

	// Phase 3: Result compensation, against the ROOT of this match's nesting
	// (nil when this level continues an enclosing match, which means the
	// enclosing level owns the result and nothing is owed here).
	cr := ComputeResultCompensation(p, rootOfMatchPullUp)
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

	// NewForMatchCompensation enforces the base-ForEach invariant itself, so a
	// compensation that comes back needed is one that can actually be applied.
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
// PartialMatch.computeCompensatedAliases (PartialMatch.java:284-305):
// the matched quantifiers' aliases PLUS, for every query predicate the
// match mapped, that predicate's correlations which are OWNED by the query
// expression (i.e. name one of its own quantifiers).
//
// The predicate-owned half matters because a mapped predicate can reference
// a local quantifier that is not itself a matched quantifier (an existential
// carried by an EXISTS predicate is the common case). Omitting those aliases
// under-reports what the match covers, so a consumer intersecting or
// composing matches can conclude an alias is still free and double-apply or
// drop the compensation that owns it.
func (p *PartialMatchImpl) GetCompensatedAliases() map[values.CorrelationIdentifier]struct{} {
	result := make(map[values.CorrelationIdentifier]struct{})
	for _, q := range p.GetMatchedQuantifiers() {
		result[q.GetAlias()] = struct{}{}
	}

	// Aliases this query expression owns — only these may be added from a
	// predicate's correlation set (a deeper/outer correlation is not ours to
	// claim as compensated).
	owned := make(map[values.CorrelationIdentifier]struct{})
	if p.queryExpression != nil {
		for _, q := range p.queryExpression.GetQuantifiers() {
			owned[q.GetAlias()] = struct{}{}
		}
	}
	if len(owned) == 0 {
		return result
	}
	if pm := p.GetRegularMatchInfo().GetPredicateMap(); pm != nil {
		for _, queryPredicate := range pm.KeySet() {
			// QueryPredicate's contract is transitive across carried Values,
			// comparisons, ranges, and child predicates. Under-reporting here
			// hands a consumer an alias it incorrectly believes is free.
			for alias := range predicates.GetCorrelatedToOfPredicate(queryPredicate) {
				if _, isOwned := owned[alias]; isOwned {
					result[alias] = struct{}{}
				}
			}
		}
	}
	return result
}

// Remaining Java parity work includes prepareForUnification,
// pullUpToParent, getPulledUpPredicateMappings, compensateExistential,
// getAccumulatedPredicateMap, and matchInfosFromMap.

// Compile-time interface satisfaction check.
var _ PartialMatch = (*PartialMatchImpl)(nil)
