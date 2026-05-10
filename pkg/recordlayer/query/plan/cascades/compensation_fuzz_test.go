package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// FuzzCompensationIntersect_NoPanic verifies that intersecting any
// two random ForMatchCompensation values never panics.
func FuzzCompensationIntersect_NoPanic(f *testing.F) {
	f.Add(uint8(0), uint8(0), false, false)
	f.Add(uint8(2), uint8(3), true, false)
	f.Add(uint8(1), uint8(1), false, true)
	f.Fuzz(func(t *testing.T, nPreds1, nPreds2 uint8, imp1, imp2 bool) {
		n1 := int(nPreds1 % 5)
		n2 := int(nPreds2 % 5)
		c1 := randomForMatchCompensation(n1, imp1)
		c2 := randomForMatchCompensation(n2, imp2)
		result := c1.Intersect(c2)
		_ = result.IsNeeded()
		_ = result.IsImpossible()
	})
}

// FuzzCompensationUnion_NoPanic verifies that union-ing any two
// random ForMatchCompensation values never panics.
func FuzzCompensationUnion_NoPanic(f *testing.F) {
	f.Add(uint8(0), uint8(0), false, false)
	f.Add(uint8(2), uint8(3), false, false)
	f.Fuzz(func(t *testing.T, nPreds1, nPreds2 uint8, imp1, imp2 bool) {
		n1 := int(nPreds1 % 5)
		n2 := int(nPreds2 % 5)
		c1 := randomForMatchCompensation(n1, imp1)
		c2 := randomForMatchCompensation(n2, imp2)
		result := c1.Union(c2)
		_ = result.IsNeeded()
		_ = result.IsImpossible()
	})
}

func randomForMatchCompensation(nPreds int, impossible bool) *ForMatchCompensation {
	keys := make([]predicates.QueryPredicate, nPreds)
	vals := make([]PredicateCompensationFunc, nPreds)
	for i := range keys {
		keys[i] = predicates.NewConstantPredicate(predicates.TriTrue)
		vals[i] = NoPredicateCompensationNeeded()
	}
	return NewForMatchCompensation(
		impossible,
		NoCompensation,
		NewPredicateCompensationMap(keys, vals),
		nil,
		nil,
		map[values.CorrelationIdentifier]struct{}{},
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)
}
