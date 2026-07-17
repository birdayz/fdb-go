package cascades

import (
	"hash/fnv"
	"reflect"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// PlanPartition groups physical-plan wrapper expressions that share
// common partitioning property values. Ports Java's PlanPartition.
type PlanPartition struct {
	partitionProps properties.PropertyMap
	exprProps      map[expressions.RelationalExpression]properties.PropertyMap
	orderedExprs   []expressions.RelationalExpression
}

// NewPlanPartition creates a partition from property maps.
func NewPlanPartition(
	partitionProps properties.PropertyMap,
	exprProps map[expressions.RelationalExpression]properties.PropertyMap,
) *PlanPartition {
	p := &PlanPartition{
		partitionProps: partitionProps,
		exprProps:      make(map[expressions.RelationalExpression]properties.PropertyMap, len(exprProps)),
	}
	for e, props := range exprProps {
		p.addExpression(e, props)
	}
	return p
}

// GetExpressions returns the wrapper expressions in this partition.
func (p *PlanPartition) GetExpressions() []expressions.RelationalExpression {
	if len(p.orderedExprs) > 0 {
		return p.orderedExprs
	}
	result := make([]expressions.RelationalExpression, 0, len(p.exprProps))
	for e := range p.exprProps {
		result = append(result, e)
	}
	return result
}

// GetPlans returns the underlying RecordQueryPlans, in the same order
// as GetExpressions. plans[i] corresponds to exprs[i].
func (p *PlanPartition) GetPlans() []plans.RecordQueryPlan {
	exprs := p.GetExpressions()
	result := make([]plans.RecordQueryPlan, 0, len(exprs))
	for _, e := range exprs {
		if ph, ok := e.(physicalPlanExpression); ok {
			result = append(result, ph.GetRecordQueryPlan())
		}
	}
	return result
}

func (p *PlanPartition) addExpression(e expressions.RelationalExpression, props properties.PropertyMap) {
	p.exprProps[e] = props
	p.orderedExprs = append(p.orderedExprs, e)
}

// GetPartitionPropertyValue returns the partitioning property value
// for partitioning properties (DistinctRecords, StoredRecord). For
// non-partitioning properties (Ordering, PrimaryKey), returns the
// value from the first expression.
func (p *PlanPartition) GetPartitionPropertyValue(prop *properties.ExpressionProperty) any {
	if v, ok := p.partitionProps[prop]; ok {
		return v
	}
	exprs := p.GetExpressions()
	if len(exprs) > 0 {
		if props, ok := p.exprProps[exprs[0]]; ok {
			return props[prop]
		}
	}
	return nil
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

// HasPrimaryKey returns true if ANY expression in this partition has
// a non-nil PrimaryKey property. Per-expression property — not a
// partitioning dimension.
func (p *PlanPartition) HasPrimaryKey() bool {
	if v, ok := p.partitionProps[properties.PropPrimaryKey]; ok && v != nil {
		return true
	}
	for _, props := range p.exprProps {
		if v, ok := props[properties.PropPrimaryKey]; ok && v != nil {
			return true
		}
	}
	return false
}

// GetOrdering returns the Ordering from the first expression in this
// partition. Ordering IS a partitioning dimension — the partition key
// folds orderingPartitionHash — so members share an ordering hash;
// the first expression's Ordering is representative. For the precise
// per-expression value, use GetExpressionPropertyValue.
func (p *PlanPartition) GetOrdering() properties.Ordering {
	for _, e := range p.GetExpressions() {
		if props, ok := p.exprProps[e]; ok {
			return props.GetOrdering()
		}
	}
	return properties.Ordering{}
}

// GetExpressionPropertyValue returns a per-expression property value.
func (p *PlanPartition) GetExpressionPropertyValue(
	expr expressions.RelationalExpression,
	prop *properties.ExpressionProperty,
) any {
	if props, ok := p.exprProps[expr]; ok {
		return props[prop]
	}
	return nil
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
	// RFC-180 D4: a nil PlanPropertiesMap means computeRefPlanProperties
	// never ran for this Reference — a planner sequencing bug. The retired
	// fallback lumped every member into ONE unordered partition,
	// contradicting ImplementSortRule's invariant (partitions keyed on
	// ordering properties) and leaking unordered members as sort-free
	// finals (silently wrong ordering). Decline instead: no partitions →
	// the consuming rule yields nothing → the planner fails loudly with
	// could-not-plan rather than approximating.
	return nil
}

func toPartitionsFromMap(pm *PlanPropertiesMap) []*PlanPartition {
	type propKey struct {
		distinct     bool
		stored       bool
		orderingHash uint64
	}

	groups := make(map[propKey]*PlanPartition)
	var order []propKey // preserve first-seen order
	for _, expr := range pm.Expressions() {
		props := pm.GetProperties(expr)
		ordering := props.GetOrdering()
		key := propKey{
			distinct:     props.GetBool(properties.PropDistinctRecords),
			stored:       props.GetBool(properties.PropStoredRecord),
			orderingHash: orderingPartitionHash(ordering),
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
			order = append(order, key)
		}
		part.addExpression(expr, props)
	}

	result := make([]*PlanPartition, 0, len(order))
	for _, key := range order {
		result = append(result, groups[key])
	}
	return result
}

// orderingPartitionHash produces a hash of the ordering keys and their
// directions (and NULL placement) so that ASC vs DESC — and natural vs
// counterflow NULL placement (ASC_NULLS_FIRST vs ASC_NULLS_LAST) — of the
// same columns fall into separate partitions. Matches Java where
// Ordering.equals includes the full ProvidedSortOrder (direction AND
// counterflow nulls) in the bindingMap. Hashing only the direction would
// collide a counterflow in-memory sort with the natural ordering of the
// same column+direction, re-opening the "counterflow == natural" elision
// bug on the set-op / data-access merge path (RollUpPlanPartitions).
func orderingPartitionHash(o properties.Ordering) uint64 {
	if !o.IsKnown || len(o.Keys) == 0 {
		return 0
	}
	h := fnv.New64a()
	for i, k := range o.Keys {
		if fv, ok := k.(*values.FieldValue); ok {
			h.Write([]byte(fv.Field))
		} else {
			h.Write([]byte(values.ExplainValue(k)))
		}
		if o.DescendingAt(i) {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
		if o.NullsFirstAt(i) {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
	}
	return h.Sum64()
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
			for _, e := range p.GetExpressions() {
				merged.addExpression(e, p.exprProps[e])
			}
		}
		return []*PlanPartition{merged}
	}

	// Group by property EQUALITY, never by a rendered string (RFC-180 D3):
	// %v of an ordering stringified its Value interface pointers as
	// addresses, so semantically-equal orderings NEVER merged —
	// under-merging inflated enumeration and diverged the alternatives
	// from Java's PlanPartitions.rollUpTo, which groups by property
	// equals(). Partitions per Reference are few; the pairwise scan is
	// Java's own shape.
	sameProps := func(a, b *PlanPartition) bool {
		for _, prop := range interestingProps {
			if !partitionPropValueEqual(a.partitionProps[prop], b.partitionProps[prop]) {
				return false
			}
		}
		return true
	}
	var result []*PlanPartition
	for _, p := range partitions {
		var existing *PlanPartition
		for _, g := range result {
			if sameProps(g, p) {
				existing = g
				break
			}
		}
		if existing == nil {
			filteredProps := make(properties.PropertyMap, len(interestingProps))
			for _, prop := range interestingProps {
				filteredProps[prop] = p.partitionProps[prop]
			}
			existing = &PlanPartition{
				partitionProps: filteredProps,
				exprProps:      make(map[expressions.RelationalExpression]properties.PropertyMap),
			}
			result = append(result, existing)
		}
		for _, e := range p.GetExpressions() {
			existing.addExpression(e, p.exprProps[e])
		}
	}
	return result
}

// partitionPropValueEqual compares two partition property VALUES the way
// Java's PlanPartitions.rollUpTo compares via equals(): typed, never a
// rendered string. Orderings compare structurally (keys via
// ValuesStructurallyEqual + parallel direction/null flags); comparable
// scalars via ==; everything else via reflect.DeepEqual (the Go analog of
// a value-object equals).
func partitionPropValueEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if ao, ok := a.(properties.Ordering); ok {
		bo, ok2 := b.(properties.Ordering)
		if !ok2 {
			return false
		}
		return orderingsEqual(ao, bo)
	}
	ra, rb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ra != rb {
		return false
	}
	if ra.Comparable() {
		return a == b
	}
	return reflect.DeepEqual(a, b)
}

func orderingsEqual(a, b properties.Ordering) bool {
	if a.IsKnown != b.IsKnown || len(a.Keys) != len(b.Keys) {
		return false
	}
	dir := func(o properties.Ordering, i int) (bool, bool) {
		d := i < len(o.Descending) && o.Descending[i]
		nf := i < len(o.NullsFirst) && o.NullsFirst[i]
		return d, nf
	}
	for i := range a.Keys {
		if !values.ValuesStructurallyEqual(a.Keys[i], b.Keys[i]) {
			return false
		}
		ad, anf := dir(a, i)
		bd, bnf := dir(b, i)
		if ad != bd || anf != bnf {
			return false
		}
	}
	return true
}

// FilterPlanPartitions returns partitions that satisfy the predicate.
// Ports Java's PlanPartitionMatchers.filterPlanPartitions.
func FilterPlanPartitions(partitions []*PlanPartition, pred func(*PlanPartition) bool) []*PlanPartition {
	var result []*PlanPartition
	for _, p := range partitions {
		if pred(p) {
			result = append(result, p)
		}
	}
	return result
}

// SelectMinCostPartition returns the partition whose first expression
// has the lowest estimated cost. Ties broken by first occurrence.
// Returns nil if partitions is empty.
// Ports Java's ExpressionPartitionMatchers.argmin (simplified).
func SelectMinCostPartition(partitions []*PlanPartition) *PlanPartition {
	if len(partitions) == 0 {
		return nil
	}
	best := partitions[0]
	bestCost := partitionCost(best)
	for _, p := range partitions[1:] {
		c := partitionCost(p)
		if c < bestCost {
			best = p
			bestCost = c
		}
	}
	return best
}

func partitionCost(p *PlanPartition) float64 {
	exprs := p.GetExpressions()
	if len(exprs) == 0 {
		return 1e18
	}
	if hc, ok := exprs[0].(interface {
		HintCost([]properties.Cost, properties.StatisticsProvider) properties.Cost
	}); ok {
		c := hc.HintCost(nil, properties.DefaultStatistics{})
		return c.Cardinality + c.CPU
	}
	return 1e18
}

// WhereDistinct returns partitions where DistinctRecords is true.
func WhereDistinct(partitions []*PlanPartition) []*PlanPartition {
	return FilterPlanPartitions(partitions, func(p *PlanPartition) bool {
		return p.IsDistinct()
	})
}

// WhereStored returns partitions where StoredRecord is true.
func WhereStored(partitions []*PlanPartition) []*PlanPartition {
	return FilterPlanPartitions(partitions, func(p *PlanPartition) bool {
		return p.IsStoredRecord()
	})
}

// WhereOrdered returns partitions that have a known ordering.
func WhereOrdered(partitions []*PlanPartition) []*PlanPartition {
	return FilterPlanPartitions(partitions, func(p *PlanPartition) bool {
		o := p.GetOrdering()
		return o.IsKnown && len(o.Keys) > 0
	})
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
