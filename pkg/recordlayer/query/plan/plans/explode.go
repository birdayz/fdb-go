package plans

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryExplodePlan "explodes" a collection-typed Value into a
// stream of element values. Leaf plan (no children). Mirrors Java's
// RecordQueryExplodePlan.
type RecordQueryExplodePlan struct {
	collectionValue values.Value
	// withOrdinality, when true, makes executePlan emit a 2-field record
	// (element, 1-based ordinal) per element instead of the bare element.
	// Mirrors Java's `RecordQueryExplodePlan.withOrdinality`.
	withOrdinality bool
}

// NewRecordQueryExplodePlan builds a bare (non-ordinal) Explode plan.
func NewRecordQueryExplodePlan(collectionValue values.Value) *RecordQueryExplodePlan {
	return &RecordQueryExplodePlan{collectionValue: collectionValue}
}

// NewRecordQueryExplodePlanWithOrdinality builds an Explode plan that
// also emits a 1-based ordinal alongside each element.
func NewRecordQueryExplodePlanWithOrdinality(collectionValue values.Value, withOrdinality bool) *RecordQueryExplodePlan {
	return &RecordQueryExplodePlan{collectionValue: collectionValue, withOrdinality: withOrdinality}
}

func (p *RecordQueryExplodePlan) GetCollectionValue() values.Value { return p.collectionValue }

// IsWithOrdinality reports whether the plan emits 1-based ordinals.
func (p *RecordQueryExplodePlan) IsWithOrdinality() bool { return p.withOrdinality }

// GetElementType returns the array element type, or UnknownType when the
// collection is not array-typed.
func (p *RecordQueryExplodePlan) GetElementType() values.Type {
	if p.collectionValue == nil {
		return values.UnknownType
	}
	if at, ok := p.collectionValue.Type().(*values.ArrayType); ok && at.ElementType != nil {
		return at.ElementType
	}
	return values.UnknownType
}

func (p *RecordQueryExplodePlan) GetResultType() values.Type {
	elem := p.GetElementType()
	if p.withOrdinality {
		return values.ExplodeOrdinalityResultType(elem)
	}
	return elem
}

func (p *RecordQueryExplodePlan) GetChildren() []RecordQueryPlan { return nil }

func (p *RecordQueryExplodePlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryExplodePlan)
	if !ok {
		return false
	}
	return p.collectionValue == o.collectionValue && p.withOrdinality == o.withOrdinality
}

func (p *RecordQueryExplodePlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("explodeplan|"))
	if p.collectionValue != nil {
		h.Write([]byte(p.collectionValue.Name()))
	}
	if p.withOrdinality {
		h.Write([]byte("|ord"))
	}
	return h.Sum64()
}

func (p *RecordQueryExplodePlan) Explain() string {
	name := "<nil>"
	if p.collectionValue != nil {
		name = p.collectionValue.Name()
	}
	if p.withOrdinality {
		return fmt.Sprintf("Explode(%s WITH ORDINALITY)", name)
	}
	return fmt.Sprintf("Explode(%s)", name)
}

var _ RecordQueryPlan = (*RecordQueryExplodePlan)(nil)
