package plans

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryInsertPlan is the physical INSERT plan: consumes
// rows from an inner plan and writes them to the target record
// type. Mirrors a simplified subset of Java's
// `RecordQueryInsertPlan` (which extends
// RecordQueryAbstractDataModificationPlan).
//
// Java's full surface includes per-record transforms, save-record
// behaviour flags, plan-graph rewriting hooks. The seed ports the
// minimal node-information needed by ImplementInsertRule:
//
//   - inner: the source plan producing rows to insert
//   - targetRecordType: the destination record type name
//   - targetType: the rich Type the inserted rows must conform to
//
// Result type matches inner — INSERT typically returns the inserted
// rows for cursor consumption.
//
// Execute is NOT in the seed surface — wiring to FDBRecordStore is
// a follow-up shift gated on the rule chain producing these plans.
type RecordQueryInsertPlan struct {
	inner            RecordQueryPlan
	targetRecordType string
	targetType       values.Type
}

// NewRecordQueryInsertPlan constructs the INSERT plan.
func NewRecordQueryInsertPlan(inner RecordQueryPlan, targetRecordType string, targetType values.Type) *RecordQueryInsertPlan {
	if targetType == nil {
		targetType = values.UnknownType
	}
	return &RecordQueryInsertPlan{
		inner:            inner,
		targetRecordType: targetRecordType,
		targetType:       targetType,
	}
}

// GetInner returns the source plan.
func (p *RecordQueryInsertPlan) GetInner() RecordQueryPlan { return p.inner }

// GetTargetRecordType returns the destination record-type name.
func (p *RecordQueryInsertPlan) GetTargetRecordType() string { return p.targetRecordType }

// GetTargetType returns the rich Type the inserted rows must
// conform to.
func (p *RecordQueryInsertPlan) GetTargetType() values.Type { return p.targetType }

// GetResultType returns the inner's result type — INSERT typically
// returns the inserted rows for cursor consumption.
func (p *RecordQueryInsertPlan) GetResultType() values.Type {
	if p.inner == nil {
		return values.UnknownType
	}
	return p.inner.GetResultType()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryInsertPlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

// EqualsWithoutChildren compares targetRecordType + targetType.
func (p *RecordQueryInsertPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryInsertPlan)
	if !ok {
		return false
	}
	if p.targetRecordType != o.targetRecordType {
		return false
	}
	return typeEquals(p.targetType, o.targetType)
}

// HashCodeWithoutChildren mixes class + targetRecordType.
func (p *RecordQueryInsertPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("insertplan|"))
	h.Write([]byte(p.targetRecordType))
	return h.Sum64()
}

// Explain renders Insert(target, inner).
func (p *RecordQueryInsertPlan) Explain() string {
	innerLabel := "<nil>"
	if p.inner != nil {
		innerLabel = p.inner.Explain()
	}
	return fmt.Sprintf("Insert(%s, %s)", p.targetRecordType, innerLabel)
}

var _ RecordQueryPlan = (*RecordQueryInsertPlan)(nil)
