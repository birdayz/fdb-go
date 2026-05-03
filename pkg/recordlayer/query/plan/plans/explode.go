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
}

func NewRecordQueryExplodePlan(collectionValue values.Value) *RecordQueryExplodePlan {
	return &RecordQueryExplodePlan{collectionValue: collectionValue}
}

func (p *RecordQueryExplodePlan) GetCollectionValue() values.Value { return p.collectionValue }

func (p *RecordQueryExplodePlan) GetResultType() values.Type {
	if p.collectionValue == nil {
		return values.UnknownType
	}
	if at, ok := p.collectionValue.Type().(*values.ArrayType); ok && at.ElementType != nil {
		return at.ElementType
	}
	return values.UnknownType
}

func (p *RecordQueryExplodePlan) GetChildren() []RecordQueryPlan { return nil }

func (p *RecordQueryExplodePlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryExplodePlan)
	if !ok {
		return false
	}
	return p.collectionValue == o.collectionValue
}

func (p *RecordQueryExplodePlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("explodeplan|"))
	if p.collectionValue != nil {
		h.Write([]byte(p.collectionValue.Name()))
	}
	return h.Sum64()
}

func (p *RecordQueryExplodePlan) Explain() string {
	if p.collectionValue != nil {
		return fmt.Sprintf("Explode(%s)", p.collectionValue.Name())
	}
	return "Explode(<nil>)"
}

var _ RecordQueryPlan = (*RecordQueryExplodePlan)(nil)
