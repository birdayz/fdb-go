package cascades

import (
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// physicalScanWrapper adapts a `*plans.RecordQueryScanPlan` to the
// `expressions.RelationalExpression` interface so Batch A rules can
// yield it into the existing Reference dedup machinery without a
// Memo overhaul.
//
// This is a SEED workaround. Java's planner has a unified
// RelationalExpression hierarchy where physical plans (RecordQueryPlan)
// implement RelationalExpressionWithChildren too. Our seed kept the
// two hierarchies separate (per RFC-022 design choice) — the
// adapter bridges them until a proper plan-aware Reference lands.
//
// The wrapper is leaf-like (no Quantifiers, no children) — the
// underlying RecordQueryScanPlan IS a leaf physical plan. Future
// wrappers for filter / sort plans need to expose their inner
// RecordQueryPlan as a Quantifier-equivalent to enable Memo
// integration; for the seed, only the leaf wrapper exists.
type physicalScanWrapper struct {
	plan *plans.RecordQueryScanPlan
}

// GetPlan exposes the wrapped physical plan.
func (w *physicalScanWrapper) GetPlan() *plans.RecordQueryScanPlan { return w.plan }

// GetResultValue returns a fresh QuantifiedObjectValue whose Type is
// the plan's flowed Type. Mirrors FullUnorderedScanExpression's
// shape so callers can interrogate type without unwrapping.
func (w *physicalScanWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

// GetQuantifiers returns the empty list — the wrapped plan is a leaf.
func (w *physicalScanWrapper) GetQuantifiers() []expressions.Quantifier { return nil }

// CanCorrelate is false — leaf can't anchor correlation.
func (w *physicalScanWrapper) CanCorrelate() bool { return false }

// ChildrenAsSet is false — leaf has no children.
func (w *physicalScanWrapper) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the empty set.
func (w *physicalScanWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares wrapped plans via plans.Equals on
// the same wrapper concrete type.
func (w *physicalScanWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalScanWrapper)
	if !ok {
		return false
	}
	return plans.Equals(w.plan, o.plan)
}

// HashCodeWithoutChildren mixes the class discriminator with the
// wrapped plan's hash.
func (w *physicalScanWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physcanwrap|"))
	if w.plan != nil {
		var b [8]byte
		ph := w.plan.HashCodeWithoutChildren()
		for i := 0; i < 8; i++ {
			b[i] = byte(ph >> (8 * (7 - i)))
		}
		h.Write(b[:])
	}
	return h.Sum64()
}

var _ expressions.RelationalExpression = (*physicalScanWrapper)(nil)
