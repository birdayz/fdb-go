package cascades

import "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"

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
	currentValue := v
	for cur := p; ; cur = cur.parent {
		mmm := ComputeMaxMatchMap(currentValue, cur.pullThroughValue, cur.rangedOverAliases)
		translated := mmm.TranslateQueryValueMaybe(cur.candidateAlias)
		if translated == nil {
			return nil
		}
		currentValue = translated

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
