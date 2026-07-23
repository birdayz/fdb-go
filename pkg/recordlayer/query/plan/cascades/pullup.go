package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// PullUp tracks how values are translated as matching walks up through
// expression boundaries. Each PullUp level represents one candidate
// expression in the match path, carrying the candidate alias and the
// "pull-through" value (the result value of that expression). The
// chain is walked bottom-up when pulling up values from a match's
// inner scope to the top-level candidate scope.
//
// Ports Java's com.apple.foundationdb.record.query.plan.cascades.values.translation.PullUp.
type PullUp struct {
	parent            *PullUp
	candidateAlias    values.CorrelationIdentifier
	pullThroughValue  values.Value
	rangedOverAliases map[values.CorrelationIdentifier]struct{}
	root              *PullUp
	// isMatch distinguishes Java's PullUp.MatchPullUp (built per candidate
	// expression while nesting through a match) from PullUp.UnificationPullUp
	// (built once at unification). NestPullUp reads it to decide whether this
	// nesting owns a "root of match" level, so it is load-bearing, not
	// bookkeeping.
	isMatch bool
}

// NewPullUp constructs a PullUp level.
func NewPullUp(
	parent *PullUp,
	candidateAlias values.CorrelationIdentifier,
	pullThroughValue values.Value,
	rangedOverAliases map[values.CorrelationIdentifier]struct{},
) *PullUp {
	p := &PullUp{
		parent:            parent,
		candidateAlias:    candidateAlias,
		pullThroughValue:  pullThroughValue,
		rangedOverAliases: rangedOverAliases,
	}
	if parent == nil {
		p.root = p
	} else {
		p.root = parent.GetRoot()
	}
	return p
}

func (p *PullUp) GetParent() *PullUp                              { return p.parent }
func (p *PullUp) GetRoot() *PullUp                                { return p.root }
func (p *PullUp) GetCandidateAlias() values.CorrelationIdentifier { return p.candidateAlias }
func (p *PullUp) GetPullThroughValue() values.Value               { return p.pullThroughValue }
func (p *PullUp) GetRangedOverAliases() map[values.CorrelationIdentifier]struct{} {
	return p.rangedOverAliases
}
func (p *PullUp) IsRoot() bool { return p.parent == nil }

// PullUpValueMaybe translates a Value from the match scope to the
// top-level candidate scope by walking up the PullUp chain. At each
// level, it computes a MaxMatchMap between the current value and the
// pull-through value, then translates via the candidate alias.
//
// Returns nil if the value cannot be pulled up at any level.
//
// Ports Java's PullUp.pullUpValueMaybe.
func (p *PullUp) PullUpValueMaybe(v values.Value) values.Value {
	return p.PullUpValueMaybeWithEquivalence(v, nil)
}

// PullUpValueMaybeWithEquivalence is like PullUpValueMaybe but accepts
// a ValueEquivalence for cross-alias matching during MaxMatchMap
// computation. Ports Java's overload that threads ValueEquivalence.
func (p *PullUp) PullUpValueMaybeWithEquivalence(v values.Value, ve ValueEquivalence) values.Value {
	currentValue := v
	for cur := p; ; cur = cur.parent {
		mmm := ComputeMaxMatchMapWithEquivalence(currentValue, cur.pullThroughValue, cur.rangedOverAliases, ve)
		translated := mmm.TranslateQueryValueMaybe(cur.candidateAlias)
		if translated == nil {
			return nil
		}
		currentValue = values.SimplifyValue(translated)

		if cur.parent == nil {
			return currentValue
		}
	}
}

// ForUnification creates a PullUp for the unification case (no parent).
// Ports Java's PullUp.forUnification.
func ForUnification(
	candidateAlias values.CorrelationIdentifier,
	pullThroughValue values.Value,
	rangedOverAliases map[values.CorrelationIdentifier]struct{},
) *PullUp {
	return NewPullUp(nil, candidateAlias, pullThroughValue, rangedOverAliases)
}

// ForMatch creates a PullUp for the match case by visiting the
// candidate expression to determine the pull-through value and
// ranged-over aliases.
//
// Ports Java's PullUp.forMatch + PullUpVisitor.visit.
func ForMatch(
	parent *PullUp,
	candidateAlias values.CorrelationIdentifier,
	candidateExpression expressions.RelationalExpression,
) *PullUp {
	pullThroughValue, rangedOverAliases := visitForPullUp(candidateExpression)
	p := NewPullUp(parent, candidateAlias, pullThroughValue, rangedOverAliases)
	p.isMatch = true
	return p
}

// IsMatch reports whether this level was built by ForMatch (Java's
// PullUp.MatchPullUp) as opposed to ForUnification.
func (p *PullUp) IsMatch() bool { return p != nil && p.isMatch }

// visitForPullUp implements the PullUpVisitor logic: extracts the
// pull-through value and ranged-over aliases from a candidate
// expression. Special-cases LogicalTypeFilterExpression (uses inner
// quantifier's alias as the pull-through); all others use the
// expression's result value.
//
// Ports Java's PullUpVisitor.visitLogicalTypeFilterExpression +
// visitDefault.
func visitForPullUp(expr expressions.RelationalExpression) (values.Value, map[values.CorrelationIdentifier]struct{}) {
	rangedOver := quantifierAliases(expr.GetQuantifiers())

	switch e := expr.(type) {
	case *expressions.LogicalTypeFilterExpression:
		inner := e.GetInner()
		pullThrough := inner.GetFlowedObjectValue()
		return pullThrough, rangedOver
	default:
		return expr.GetResultValue(), rangedOver
	}
}

// quantifierAliases collects the aliases from a slice of Quantifiers.
func quantifierAliases(qs []expressions.Quantifier) map[values.CorrelationIdentifier]struct{} {
	result := make(map[values.CorrelationIdentifier]struct{}, len(qs))
	for _, q := range qs {
		result[q.GetAlias()] = struct{}{}
	}
	return result
}

// NestPullUp walks through the candidate reference chain to build a
// nested PullUp chain. For each level, it visits the candidate
// expression to determine the pull-through value. If the match info is
// adjusted (wrapping another), it descends through the adjustment chain.
//
// Returns (rootOfMatchPullUp, currentPullUp). currentPullUp is the
// innermost level, used to translate predicates and to nest child
// matches. rootOfMatchPullUp is the level this nesting introduced on
// top of the incoming chain — the one result compensation pulls the
// query result value through. It is nil when the incoming pull-up is
// itself a match level (this nesting continues someone else's match
// rather than rooting a new one), which is Java's signal that no result
// compensation is owed here.
//
// currentPullUp is nil when the chain cannot be built faithfully: a
// missing candidate reference, a reference that does not hold exactly
// one non-nil member, or an adjusted level whose candidate does not
// have exactly one quantifier. Java asserts those (Reference.get, which
// throws unless the reference holds exactly one member, and
// Verify.verify on the quantifier count) and
// crashes; Go fails closed instead so the caller degrades the match to
// an impossible compensation. Both refuse to compensate off a guessed
// chain — silently taking members[0] of a multi-member reference builds
// a pull-up for an expression the match was never proved against, and
// every value pulled through it is then wrong.
//
// Ports Java's PartialMatch.nestPullUp.
func NestPullUp(
	pm PartialMatch,
	pullUp *PullUp,
	candidateAlias values.CorrelationIdentifier,
) (rootOfMatchPullUp *PullUp, currentPullUp *PullUp) {
	currentPullUp = pullUp
	currentCandidateRef := pm.GetCandidateRef()
	currentMatchInfo := pm.GetMatchInfo()
	currentCandidateAlias := candidateAlias
	incomingIsMatch := pullUp.IsMatch()

	for {
		candidateExpr, ok := onlyReferenceMember(currentCandidateRef)
		if !ok {
			return nil, nil
		}

		currentPullUp = ForMatch(currentPullUp, currentCandidateAlias, candidateExpr)
		if !incomingIsMatch && rootOfMatchPullUp == nil {
			rootOfMatchPullUp = currentPullUp
		}

		if !currentMatchInfo.IsAdjusted() {
			break
		}

		qs := candidateExpr.GetQuantifiers()
		if len(qs) != 1 {
			return nil, nil
		}
		currentCandidateAlias = qs[0].GetAlias()
		currentCandidateRef = qs[0].GetRangesOver()

		adj, ok := currentMatchInfo.(*AdjustedMatchInfo)
		if !ok {
			// IsAdjusted() promised an AdjustedMatchInfo to descend into.
			return nil, nil
		}
		currentMatchInfo = adj.GetUnderlying()
	}

	return rootOfMatchPullUp, currentPullUp
}
