package cascades

import "fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"

// tieBreakHashProvider separates a node's deterministic ranking identity from
// its memo identity. Most expressions use one hash for both. Schema-bearing
// projections are the exception: output names must prevent memo collapse, but
// must not perturb cost ties between plans that perform identical work.
type tieBreakHashProvider interface {
	TieBreakHashCodeWithoutChildren() uint64
}

// tieBreakNodeHash returns the schema-neutral per-node hash used by every
// logical/designation/extraction tie-break. HashCodeWithoutChildren remains the
// sole memo identity hash.
func tieBreakNodeHash(e expressions.RelationalExpression) uint64 {
	if provider, ok := e.(tieBreakHashProvider); ok {
		return provider.TieBreakHashCodeWithoutChildren()
	}
	return e.HashCodeWithoutChildren()
}
