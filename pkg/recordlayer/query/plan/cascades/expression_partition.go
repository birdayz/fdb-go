package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// PlanPartition groups physical-plan wrapper expressions that share
// common partitioning property values. Ports Java's PlanPartition.
type PlanPartition struct {
	partitionProps properties.PropertyMap
	exprProps      map[expressions.RelationalExpression]properties.PropertyMap
}

// NewPlanPartition creates a partition from property maps.
func NewPlanPartition(
	partitionProps properties.PropertyMap,
	exprProps map[expressions.RelationalExpression]properties.PropertyMap,
) *PlanPartition {
	return &PlanPartition{
		partitionProps: partitionProps,
		exprProps:      exprProps,
	}
}

// GetExpressions returns the wrapper expressions in this partition.
func (p *PlanPartition) GetExpressions() []expressions.RelationalExpression {
	result := make([]expressions.RelationalExpression, 0, len(p.exprProps))
	for e := range p.exprProps {
		result = append(result, e)
	}
	return result
}

// GetPlans returns the underlying RecordQueryPlans from the wrapper expressions.
func (p *PlanPartition) GetPlans() []plans.RecordQueryPlan {
	result := make([]plans.RecordQueryPlan, 0, len(p.exprProps))
	for e := range p.exprProps {
		if ph, ok := e.(physicalPlanExpression); ok {
			result = append(result, ph.GetRecordQueryPlan())
		}
	}
	return result
}

// GetPartitionPropertyValue returns the partitioning property value.
func (p *PlanPartition) GetPartitionPropertyValue(prop *properties.ExpressionProperty) any {
	return p.partitionProps[prop]
}

// GetPartitionPropertiesMap returns the full partitioning property map.
func (p *PlanPartition) GetPartitionPropertiesMap() properties.PropertyMap {
	return p.partitionProps
}

// IsDistinct returns true if the partition's DistinctRecords is true.
func (p *PlanPartition) IsDistinct() bool {
	return p.partitionProps.GetBool(properties.PropDistinctRecords)
}

// IsStoredRecord returns true if the partition's StoredRecord is true.
func (p *PlanPartition) IsStoredRecord() bool {
	return p.partitionProps.GetBool(properties.PropStoredRecord)
}

// HasPrimaryKey returns true if the partition's PrimaryKey is non-nil.
func (p *PlanPartition) HasPrimaryKey() bool {
	v, ok := p.partitionProps[properties.PropPrimaryKey]
	return ok && v != nil
}

// GetOrdering returns the partition's Ordering property.
func (p *PlanPartition) GetOrdering() properties.Ordering {
	return p.partitionProps.GetOrdering()
}

// ToPlanPartitions computes plan partitions for a Reference by reading
// the pre-computed PlanPropertiesMap (set during PLANNING phase).
func ToPlanPartitions(ref *expressions.Reference) []*PlanPartition {
	pm := GetRefPlanPropertiesMap(ref)
	if pm == nil {
		return toPlanPartitionsFallback(ref)
	}
	return toPartitionsFromMap(pm)
}

func toPlanPartitionsFallback(ref *expressions.Reference) []*PlanPartition {
	if ref == nil {
		return nil
	}
	members := ref.FinalMembers()
	if len(members) == 0 {
		members = ref.AllMembers()
	}
	exprProps := make(map[expressions.RelationalExpression]properties.PropertyMap)
	for _, m := range members {
		if ph, ok := m.(physicalPlanExpression); ok {
			exprProps[m] = computeWrapperProperties(ph)
		}
	}
	if len(exprProps) == 0 {
		return nil
	}
	return []*PlanPartition{{
		partitionProps: properties.PropertyMap{},
		exprProps:      exprProps,
	}}
}

func toPartitionsFromMap(pm *PlanPropertiesMap) []*PlanPartition {
	type propKey struct {
		distinct bool
		stored   bool
	}

	groups := make(map[propKey]*PlanPartition)
	for expr, props := range pm.All() {
		key := propKey{
			distinct: props.GetBool(properties.PropDistinctRecords),
			stored:   props.GetBool(properties.PropStoredRecord),
		}
		part, ok := groups[key]
		if !ok {
			partProps := properties.PropertyMap{
				properties.PropDistinctRecords: key.distinct,
				properties.PropStoredRecord:    key.stored,
				properties.PropPrimaryKey:      props[properties.PropPrimaryKey],
				properties.PropOrdering:        props[properties.PropOrdering],
			}
			part = &PlanPartition{
				partitionProps: partProps,
				exprProps:      make(map[expressions.RelationalExpression]properties.PropertyMap),
			}
			groups[key] = part
		}
		part.exprProps[expr] = props
	}

	result := make([]*PlanPartition, 0, len(groups))
	for _, part := range groups {
		result = append(result, part)
	}
	return result
}

// RollUpPlanPartitions merges partitions by retaining only the specified
// interesting properties as partition keys.
func RollUpPlanPartitions(partitions []*PlanPartition, interestingProps ...*properties.ExpressionProperty) []*PlanPartition {
	if len(partitions) == 0 {
		return nil
	}
	if len(interestingProps) == 0 {
		merged := &PlanPartition{
			partitionProps: properties.PropertyMap{},
			exprProps:      make(map[expressions.RelationalExpression]properties.PropertyMap),
		}
		for _, p := range partitions {
			for e, props := range p.exprProps {
				merged.exprProps[e] = props
			}
		}
		return []*PlanPartition{merged}
	}

	type rollupKey struct {
		vals [4]any
	}
	makeKey := func(p *PlanPartition) rollupKey {
		var k rollupKey
		for i, prop := range interestingProps {
			if i >= len(k.vals) {
				break
			}
			k.vals[i] = p.partitionProps[prop]
		}
		return k
	}

	groups := make(map[rollupKey]*PlanPartition)
	order := make([]rollupKey, 0)
	for _, p := range partitions {
		key := makeKey(p)
		existing, ok := groups[key]
		if !ok {
			filteredProps := make(properties.PropertyMap, len(interestingProps))
			for _, prop := range interestingProps {
				filteredProps[prop] = p.partitionProps[prop]
			}
			existing = &PlanPartition{
				partitionProps: filteredProps,
				exprProps:      make(map[expressions.RelationalExpression]properties.PropertyMap),
			}
			groups[key] = existing
			order = append(order, key)
		}
		for e, props := range p.exprProps {
			existing.exprProps[e] = props
		}
	}

	result := make([]*PlanPartition, 0, len(order))
	for _, key := range order {
		result = append(result, groups[key])
	}
	return result
}

// AllAttributesExcept returns plan properties excluding the specified ones.
func AllAttributesExcept(except ...*properties.ExpressionProperty) []*properties.ExpressionProperty {
	exceptSet := make(map[*properties.ExpressionProperty]struct{}, len(except))
	for _, p := range except {
		exceptSet[p] = struct{}{}
	}
	var result []*properties.ExpressionProperty
	for _, p := range properties.AllPlanProperties {
		if _, excluded := exceptSet[p]; !excluded {
			result = append(result, p)
		}
	}
	return result
}
