package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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
	return NewPullUp(parent, candidateAlias, pullThroughValue, rangedOverAliases)
}

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
// Returns (rootOfMatchPullUp, currentPullUp). rootOfMatchPullUp is the
// first MatchPullUp level created (the topmost match-specific pullup).
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

	for {
		if currentCandidateRef == nil {
			break
		}
		members := currentCandidateRef.AllMembers()
		if len(members) == 0 {
			break
		}
		candidateExpr := members[0]

		currentPullUp = ForMatch(currentPullUp, currentCandidateAlias, candidateExpr)
		if rootOfMatchPullUp == nil {
			rootOfMatchPullUp = currentPullUp
		}

		if !currentMatchInfo.IsAdjusted() {
			break
		}

		qs := candidateExpr.GetQuantifiers()
		if len(qs) != 1 {
			break
		}
		currentCandidateAlias = qs[0].GetAlias()
		currentCandidateRef = qs[0].GetRangesOver()

		if adj, ok := currentMatchInfo.(*AdjustedMatchInfo); ok {
			currentMatchInfo = adj.GetUnderlying()
		} else {
			break
		}
	}

	if rootOfMatchPullUp == nil {
		rootOfMatchPullUp = currentPullUp
	}
	return rootOfMatchPullUp, currentPullUp
}
