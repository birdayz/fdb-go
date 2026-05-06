package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryRecursiveLevelUnionPlan implements a recursive
// level-order (breadth-first) union: the initial-state plan seeds the
// first level, and the recursive-state plan is re-evaluated for each
// level using two temp tables (scan/insert) that are flipped between
// levels. Mirrors Java's RecordQueryRecursiveLevelUnionPlan.
type RecordQueryRecursiveLevelUnionPlan struct {
	initialState         RecordQueryPlan
	recursiveState       RecordQueryPlan
	tempTableScanAlias   values.CorrelationIdentifier
	tempTableInsertAlias values.CorrelationIdentifier
}

func NewRecordQueryRecursiveLevelUnionPlan(
	initialState, recursiveState RecordQueryPlan,
	tempTableScanAlias, tempTableInsertAlias values.CorrelationIdentifier,
) *RecordQueryRecursiveLevelUnionPlan {
	return &RecordQueryRecursiveLevelUnionPlan{
		initialState:         initialState,
		recursiveState:       recursiveState,
		tempTableScanAlias:   tempTableScanAlias,
		tempTableInsertAlias: tempTableInsertAlias,
	}
}

func (p *RecordQueryRecursiveLevelUnionPlan) GetInitialState() RecordQueryPlan { return p.initialState }
func (p *RecordQueryRecursiveLevelUnionPlan) GetRecursiveState() RecordQueryPlan {
	return p.recursiveState
}

func (p *RecordQueryRecursiveLevelUnionPlan) GetTempTableScanAlias() values.CorrelationIdentifier {
	return p.tempTableScanAlias
}

func (p *RecordQueryRecursiveLevelUnionPlan) GetTempTableInsertAlias() values.CorrelationIdentifier {
	return p.tempTableInsertAlias
}

func (p *RecordQueryRecursiveLevelUnionPlan) GetResultType() values.Type { return values.UnknownType }

func (p *RecordQueryRecursiveLevelUnionPlan) GetChildren() []RecordQueryPlan {
	return []RecordQueryPlan{p.initialState, p.recursiveState}
}

func (p *RecordQueryRecursiveLevelUnionPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryRecursiveLevelUnionPlan)
	if !ok {
		return false
	}
	return p.tempTableScanAlias == o.tempTableScanAlias &&
		p.tempTableInsertAlias == o.tempTableInsertAlias
}

func (p *RecordQueryRecursiveLevelUnionPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("recursivelevel|"))
	h.Write([]byte(p.tempTableScanAlias.Name()))
	h.Write([]byte("|"))
	h.Write([]byte(p.tempTableInsertAlias.Name()))
	return h.Sum64()
}

func (p *RecordQueryRecursiveLevelUnionPlan) Explain() string {
	var sb strings.Builder
	sb.WriteString("RecursiveLevelUnion(")
	if p.initialState != nil {
		sb.WriteString(p.initialState.Explain())
	}
	sb.WriteString(", ")
	if p.recursiveState != nil {
		sb.WriteString(p.recursiveState.Explain())
	}
	sb.WriteString(fmt.Sprintf(", scan=%s, insert=%s)", p.tempTableScanAlias.Name(), p.tempTableInsertAlias.Name()))
	return sb.String()
}

var _ RecordQueryPlan = (*RecordQueryRecursiveLevelUnionPlan)(nil)
