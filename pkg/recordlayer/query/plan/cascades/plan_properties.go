package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// PlanPropertiesMap stores computed property values for each physical-plan
// wrapper expression in a Reference's final members.
type PlanPropertiesMap struct {
	props map[expressions.RelationalExpression]properties.PropertyMap
}

// NewPlanPropertiesMap creates a new empty properties map.
func NewPlanPropertiesMap() *PlanPropertiesMap {
	return &PlanPropertiesMap{
		props: make(map[expressions.RelationalExpression]properties.PropertyMap),
	}
}

// Add computes and stores properties for the given physical wrapper.
func (m *PlanPropertiesMap) Add(w physicalPlanExpression) {
	m.props[w] = computeWrapperProperties(w)
}

// GetProperties returns the computed properties for a wrapper expression.
func (m *PlanPropertiesMap) GetProperties(expr expressions.RelationalExpression) properties.PropertyMap {
	return m.props[expr]
}

// Expressions returns all wrapper expressions in this map.
func (m *PlanPropertiesMap) Expressions() []expressions.RelationalExpression {
	result := make([]expressions.RelationalExpression, 0, len(m.props))
	for e := range m.props {
		result = append(result, e)
	}
	return result
}

// All returns the full underlying map.
func (m *PlanPropertiesMap) All() map[expressions.RelationalExpression]properties.PropertyMap {
	return m.props
}

func computeWrapperProperties(w physicalPlanExpression) properties.PropertyMap {
	plan := w.GetRecordQueryPlan()
	return properties.PropertyMap{
		properties.PropDistinctRecords: computeDistinctRecords(w, plan),
		properties.PropStoredRecord:    computeStoredRecord(plan),
		properties.PropPrimaryKey:      computePrimaryKey(plan),
		properties.PropOrdering:        computeWrapperOrdering(w),
	}
}

func computeDistinctRecords(w physicalPlanExpression, plan plans.RecordQueryPlan) bool {
	switch plan.(type) {
	case *plans.RecordQueryScanPlan:
		return true
	case *plans.RecordQueryIndexPlan:
		if iw, ok := w.(*physicalIndexScanWrapper); ok {
			return iw.unique
		}
		return false
	case *plans.RecordQueryFilterPlan,
		*plans.RecordQueryPredicatesFilterPlan,
		*plans.RecordQuerySortPlan,
		*plans.RecordQueryTypeFilterPlan,
		*plans.RecordQueryLimitPlan,
		*plans.RecordQueryProjectionPlan,
		*plans.RecordQueryMapPlan,
		*plans.RecordQueryInsertPlan,
		*plans.RecordQueryDeletePlan,
		*plans.RecordQueryUpdatePlan,
		*plans.RecordQueryTempTableInsertPlan:
		return distinctRecordsFromChildRef(w)
	case *plans.RecordQueryFirstOrDefaultPlan:
		return true
	case *plans.RecordQueryDefaultOnEmptyPlan,
		*plans.RecordQueryInJoinPlan:
		return distinctRecordsFromChildRef(w)
	case *plans.RecordQueryDistinctPlan,
		*plans.RecordQueryUnionPlan,
		*plans.RecordQueryMergeSortUnionPlan,
		*plans.RecordQueryIntersectionPlan,
		*plans.RecordQueryInUnionPlan:
		return true
	case *plans.RecordQueryUnorderedUnionPlan:
		return false
	default:
		return false
	}
}

func distinctRecordsFromChildRef(w physicalPlanExpression) bool {
	qs := w.GetQuantifiers()
	if len(qs) != 1 {
		return false
	}
	ref := qs[0].GetRangesOver()
	return distinctRecordsForRef(ref)
}

func distinctRecordsForRef(ref *expressions.Reference) bool {
	pm := GetRefPlanPropertiesMap(ref)
	if pm == nil {
		return false
	}
	for _, props := range pm.All() {
		if !props.GetBool(properties.PropDistinctRecords) {
			return false
		}
	}
	return len(pm.All()) > 0
}

func computeStoredRecord(plan plans.RecordQueryPlan) bool {
	switch plan.(type) {
	case *plans.RecordQueryScanPlan,
		*plans.RecordQueryIndexPlan,
		*plans.RecordQueryDistinctPlan,
		*plans.RecordQueryInsertPlan,
		*plans.RecordQueryDeletePlan,
		*plans.RecordQueryUpdatePlan:
		return true
	case *plans.RecordQueryFilterPlan,
		*plans.RecordQueryPredicatesFilterPlan,
		*plans.RecordQuerySortPlan,
		*plans.RecordQueryTypeFilterPlan,
		*plans.RecordQueryLimitPlan,
		*plans.RecordQueryProjectionPlan,
		*plans.RecordQueryMapPlan:
		return storedRecordFromChildren(plan.GetChildren())
	case *plans.RecordQueryFirstOrDefaultPlan,
		*plans.RecordQueryDefaultOnEmptyPlan:
		return false
	case *plans.RecordQueryInJoinPlan:
		return storedRecordFromChildren(plan.GetChildren())
	case *plans.RecordQueryInUnionPlan:
		return storedRecordFromChildren(plan.GetChildren())
	case *plans.RecordQueryUnionPlan,
		*plans.RecordQueryMergeSortUnionPlan,
		*plans.RecordQueryIntersectionPlan,
		*plans.RecordQueryUnorderedUnionPlan,
		*plans.RecordQueryRecursiveDfsJoinPlan,
		*plans.RecordQueryRecursiveLevelUnionPlan:
		return storedRecordAllChildren(plan.GetChildren())
	default:
		return false
	}
}

func storedRecordFromChildren(children []plans.RecordQueryPlan) bool {
	if len(children) != 1 {
		return false
	}
	return computeStoredRecord(children[0])
}

func storedRecordAllChildren(children []plans.RecordQueryPlan) bool {
	for _, c := range children {
		if !computeStoredRecord(c) {
			return false
		}
	}
	return len(children) > 0
}

func computePrimaryKey(plan plans.RecordQueryPlan) any {
	switch p := plan.(type) {
	case *plans.RecordQueryScanPlan:
		if pk := p.GetPrimaryKeyValues(); pk != nil {
			return pk
		}
		return nil
	case *plans.RecordQueryIndexPlan:
		return nil
	case *plans.RecordQueryFilterPlan,
		*plans.RecordQueryPredicatesFilterPlan,
		*plans.RecordQuerySortPlan,
		*plans.RecordQueryTypeFilterPlan,
		*plans.RecordQueryLimitPlan,
		*plans.RecordQueryProjectionPlan,
		*plans.RecordQueryMapPlan,
		*plans.RecordQueryDistinctPlan,
		*plans.RecordQueryInJoinPlan,
		*plans.RecordQueryInUnionPlan,
		*plans.RecordQueryFirstOrDefaultPlan,
		*plans.RecordQueryDeletePlan:
		return pkFromChildren(plan.GetChildren())
	case *plans.RecordQueryUnionPlan,
		*plans.RecordQueryMergeSortUnionPlan,
		*plans.RecordQueryIntersectionPlan,
		*plans.RecordQueryUnorderedUnionPlan:
		return commonPKFromChildren(plan.GetChildren())
	default:
		return nil
	}
}

func pkFromChildren(children []plans.RecordQueryPlan) any {
	if len(children) != 1 {
		return nil
	}
	return computePrimaryKey(children[0])
}

func commonPKFromChildren(children []plans.RecordQueryPlan) any {
	if len(children) == 0 {
		return nil
	}
	first := computePrimaryKey(children[0])
	if first == nil {
		return nil
	}
	firstPK := first.([]values.Value)
	for _, c := range children[1:] {
		childPK := computePrimaryKey(c)
		if childPK == nil {
			return nil
		}
		pk := childPK.([]values.Value)
		if len(pk) != len(firstPK) {
			return nil
		}
		for i := range pk {
			if !valuesEqual(pk[i], firstPK[i]) {
				return nil
			}
		}
	}
	return firstPK
}

func computeWrapperOrdering(w physicalPlanExpression) properties.Ordering {
	if hinter, ok := w.(properties.OrderingHinter); ok {
		return hinter.HintOrdering()
	}
	return properties.Ordering{}
}

// RichOrderingHinter is the optional interface a wrapper implements
// to provide full ordering info with bindings. Falls back to
// converting from HintOrdering if not implemented.
type RichOrderingHinter interface {
	HintRichOrdering() *RichOrdering
}

func computeWrapperRichOrdering(w physicalPlanExpression) *RichOrdering {
	if rh, ok := w.(RichOrderingHinter); ok {
		return rh.HintRichOrdering()
	}
	o := computeWrapperOrdering(w)
	if !o.IsKnown || len(o.Keys) == 0 {
		return EmptyOrdering()
	}
	bm := make(map[values.Value][]OrderingBinding, len(o.Keys))
	for i, k := range o.Keys {
		dir := ProvidedSortOrderAscending
		if i < len(o.Descending) && o.Descending[i] {
			dir = ProvidedSortOrderDescending
		}
		bm[k] = []OrderingBinding{SortedBinding(dir)}
	}
	return NewRichOrdering(bm, o.Keys, false)
}

// computeRefPlanProperties computes and stores plan properties for all
// final-member physical plans in the given Reference. Called during the
// PLANNING phase after ImplementationRules have fired on ref.
func computeRefPlanProperties(ref *expressions.Reference) {
	members := ref.FinalMembers()
	if len(members) == 0 {
		members = ref.AllMembers()
	}
	pm := NewPlanPropertiesMap()
	for _, m := range members {
		if ph, ok := m.(physicalPlanExpression); ok {
			pm.Add(ph)
		}
	}
	ref.SetPlanProperties(pm)
}

// GetRefPlanPropertiesMap retrieves the PlanPropertiesMap from a Reference,
// or nil if not yet computed.
func GetRefPlanPropertiesMap(ref *expressions.Reference) *PlanPropertiesMap {
	if ref == nil {
		return nil
	}
	pm, _ := ref.GetPlanProperties().(*PlanPropertiesMap)
	return pm
}
