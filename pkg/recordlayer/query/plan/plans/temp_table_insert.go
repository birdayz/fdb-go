package plans

import (
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryTempTableInsertPlan inserts the output of an inner plan
// into a temporary table identified by a correlation alias. The owning
// flag controls whether this plan owns the temp table lifecycle.
// Mirrors Java's
// `com.apple.foundationdb.record.query.plan.plans.RecordQueryTempTableInsertPlan`.
type RecordQueryTempTableInsertPlan struct {
	inner          RecordQueryPlan
	tempTableAlias values.CorrelationIdentifier
	owning         bool
}

func NewRecordQueryTempTableInsertPlan(
	inner RecordQueryPlan,
	alias values.CorrelationIdentifier,
	owning bool,
) *RecordQueryTempTableInsertPlan {
	return &RecordQueryTempTableInsertPlan{
		inner:          inner,
		tempTableAlias: alias,
		owning:         owning,
	}
}

func (p *RecordQueryTempTableInsertPlan) GetInner() RecordQueryPlan { return p.inner }

func (p *RecordQueryTempTableInsertPlan) GetTempTableAlias() values.CorrelationIdentifier {
	return p.tempTableAlias
}

func (p *RecordQueryTempTableInsertPlan) IsOwning() bool { return p.owning }

func (p *RecordQueryTempTableInsertPlan) GetResultType() values.Type { return values.UnknownType }

func (p *RecordQueryTempTableInsertPlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

func (p *RecordQueryTempTableInsertPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryTempTableInsertPlan)
	if !ok {
		return false
	}
	return p.tempTableAlias == o.tempTableAlias && p.owning == o.owning
}

func (p *RecordQueryTempTableInsertPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("temptableinsert|"))
	h.Write([]byte(p.tempTableAlias.Name()))
	if p.owning {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	return h.Sum64()
}

func (p *RecordQueryTempTableInsertPlan) Explain() string {
	inner := "<nil>"
	if p.inner != nil {
		inner = p.inner.Explain()
	}
	return "TempTableInsert(" + p.tempTableAlias.Name() + ", " + inner + ")"
}

var _ RecordQueryPlan = (*RecordQueryTempTableInsertPlan)(nil)
