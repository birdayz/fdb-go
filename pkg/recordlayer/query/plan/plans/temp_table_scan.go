package plans

import (
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryTempTableScanPlan scans a temporary table identified by
// a correlation alias. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.plans.RecordQueryTempTableScanPlan`.
type RecordQueryTempTableScanPlan struct {
	tempTableAlias values.CorrelationIdentifier
}

func NewRecordQueryTempTableScanPlan(alias values.CorrelationIdentifier) *RecordQueryTempTableScanPlan {
	return &RecordQueryTempTableScanPlan{tempTableAlias: alias}
}

func (p *RecordQueryTempTableScanPlan) GetTempTableAlias() values.CorrelationIdentifier {
	return p.tempTableAlias
}

func (p *RecordQueryTempTableScanPlan) GetResultType() values.Type { return values.UnknownType }

func (p *RecordQueryTempTableScanPlan) GetChildren() []RecordQueryPlan { return nil }

func (p *RecordQueryTempTableScanPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryTempTableScanPlan)
	if !ok {
		return false
	}
	return p.tempTableAlias == o.tempTableAlias
}

func (p *RecordQueryTempTableScanPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("temptablescan|"))
	h.Write([]byte(p.tempTableAlias.Name()))
	return h.Sum64()
}

func (p *RecordQueryTempTableScanPlan) Explain() string {
	return "TempTableScan(" + p.tempTableAlias.Name() + ")"
}

var _ RecordQueryPlan = (*RecordQueryTempTableScanPlan)(nil)
