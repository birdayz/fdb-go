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
//
// NOT the production path — toPartitionsFromMap is, and it feeds
// addExpression from pm.Expressions(), an ordered slice, so orderedExprs is
// deterministic there. This constructor iterates its input MAP, so the
// order it records is Go's randomized map-iteration order. Harmless while
// nothing in the planner calls it; a real nondeterminism source the moment
// something does, since GetExpressions hands that order to rules that
// enumerate alternatives from it.
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

// GetPlans returns the underlying RecordQueryPlans of the PHYSICAL members,
// in GetExpressions order.
//
// It does NOT index-align with GetExpressions, and used to claim it did
// ("plans[i] corresponds to exprs[i]"). It skips any non-physical member, so
// one such member shifts every later index and silently pairs a plan with
// the wrong expression. Callers that need the pairing must use
// GetPhysicalExpressions, which applies the identical filter — that pair IS
// aligned by construction.
//
// The stale claim mattered: rule_implement_in_join.go seeds its reference
// from GetExpressions and its plan from GetPlans, which is exactly the
// mispairing this enables (RFC-183 §12).
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

// GetPhysicalExpressions returns the PHYSICAL members in GetExpressions
// order — the expression-side twin of GetPlans, applying the identical
// filter so the two ARE index-aligned:
//
//	exprs := p.GetPhysicalExpressions()
//	plns  := p.GetPlans()
//	// exprs[i] is the expression whose plan is plns[i]
//
// This exists because GetExpressions and GetPlans are NOT aligned (see
// GetPlans). A caller needing an expression together with its plan — the
// IN-join rule seeds a memo reference from one and a plan chain from the
// other — must pair them through this rather than by indexing
// GetExpressions.
func (p *PlanPartition) GetPhysicalExpressions() []expressions.RelationalExpression {
	exprs := p.GetExpressions()
	result := make([]expressions.RelationalExpression, 0, len(exprs))
	for _, e := range exprs {
		if _, ok := e.(physicalPlanExpression); ok {
			result = append(result, e)
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
//
// A RULE MUST NOT READ A PROPERTY OFF A PARTITION THIS RETURNS. The partition
// key here is (DistinctRecords, StoredRecord, orderingHash) — see
// toPartitionsFromMap — so a partition is homogeneous in those three and in
// NOTHING else, while its partitionProps carries every property copied from
// whichever member happened to create it. Reading any other property off it is
// a fact about one member asserted of all of them.
//
// That is a DISTINCT defect from the single-member pick (a rule choosing one
// member where it should yield over many). This one is the opposite shape: the
// group is correctly enumerated and then DESCRIBED by one arbitrary element.
// The two need different fixes, and grepping for one will not find the other —
// a pick reads like a chosen winner, a read like `[0]` or a `break` on the
// first match over a member collection.
//
// The gate is roll-up: call RollUpPlanPartitions with an EXPLICIT list of the
// properties the rule reads, then read only those, off the partition. Roll-up
// groups by each expression's own value for those properties, so the resulting
// partition-level value describes every member by construction. Java needs no
// such discipline because its partition key is the full property set
// (PlanPartitions), which is why porting a Java rule that reads
// getPartitionPropertyValue without also porting its rollUpTo silently drops
// the guarantee the read depends on.
//
// ImplementInUnionRule now rolls the raw partitions up by PropRichOrdering
// before deriving merge keys, so an equality-bound access cannot share a
// representative with an unbounded directional scan. It also pins a single
// member (pinOrderedSpine), and that pin compensates for a SEPARATE defect one
// layer down:
// MemoizeFinalExpressionsFromOther (implementation_rule.go:124-151) mints a
// fresh Reference without a constraint entry, so OptimizeGroupTask's
// per-ordering retention (unified_tasks.go:663-666) looks up the new reference,
// finds nothing, and resolves by cost alone. Roll-up alone therefore does not
// make dropping the pin safe — measured: it yields an InUnion claiming ASC over
// a filtered full scan.
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
	type bucket struct {
		distinct     bool
		stored       bool
		orderingHash uint64
	}

	// The ordering hash BUCKETS candidate partitions; orderingsEqual is the
	// AUTHORITY within a bucket — the same structural equality the sibling
	// RollUpPlanPartitions path uses (the two were asymmetric before: RollUp
	// compared structurally, this grouped by hash alone). A hash collision
	// between two genuinely different orderings therefore splits into separate
	// partitions instead of silently co-partitioning, so GetOrdering never
	// reports one member's order for another (the wrong-ORDER-BY bug). With the
	// full accessor path now in the hash, collisions are rare; equality is still
	// what decides.
	byHash := make(map[bucket][]*PlanPartition)
	var result []*PlanPartition // preserve first-seen order
	for _, expr := range pm.Expressions() {
		props := pm.GetProperties(expr)
		ordering := props.GetOrdering()
		b := bucket{
			distinct:     props.GetBool(properties.PropDistinctRecords),
			stored:       props.GetBool(properties.PropStoredRecord),
			orderingHash: orderingPartitionHash(ordering),
		}
		var part *PlanPartition
		for _, cand := range byHash[b] {
			if orderingsEqual(cand.partitionProps.GetOrdering(), ordering) {
				part = cand
				break
			}
		}
		if part == nil {
			partProps := properties.PropertyMap{
				properties.PropDistinctRecords: b.distinct,
				properties.PropStoredRecord:    b.stored,
				properties.PropPrimaryKey:      props[properties.PropPrimaryKey],
				properties.PropOrdering:        props[properties.PropOrdering],
			}
			part = &PlanPartition{
				partitionProps: partProps,
				exprProps:      make(map[expressions.RelationalExpression]properties.PropertyMap),
			}
			byHash[b] = append(byHash[b], part)
			result = append(result, part)
		}
		part.addExpression(expr, props)
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
		if fv, ok := values.AsFieldValue(k); ok {
			// Hash the FULL accessor NAME path, not just the leaf Field: two
			// same-leaf-name ordering keys from different sources (addr.city vs a
			// top-level city, or two self-join legs' x) must land in DIFFERENT
			// partitions, else a sort is elided against the wrong column. The name
			// path is deterministic (no minted q$N alias — the reason the leaf-name
			// special-case existed); a dotted/pure-ordinal FieldValue the path
			// primitive cannot resolve falls back to its literal Field (still
			// alias-free, and orderingsEqual is the authority in toPartitionsFromMap).
			if path, pok := values.AccessorNamePath(fv); pok {
				for _, seg := range path {
					h.Write([]byte(seg))
					h.Write([]byte{0}) // separator: ["a","bc"] must not collide with ["ab","c"]
				}
			} else {
				h.Write([]byte(fv.DisplayName()))
			}
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
	// Grouping is driven by each EXPRESSION's own property values, never by the
	// source partition's. That is what makes the post-roll-up read TOTAL: a
	// caller that rolls up to the properties it reads and then reads them off
	// the partition is guaranteed the value holds for every member, because
	// membership was decided by that very value.
	//
	// Grouping by the SOURCE partition's value would not give that guarantee,
	// and the difference is not theoretical. Go's partition key is
	// (DistinctRecords, StoredRecord, orderingHash) — see toPartitionsFromMap —
	// so a partition is homogeneous in those three and in NOTHING else, while
	// its partitionProps carries every property copied from whichever member
	// happened to create it. Rolling up on such a value would propagate one
	// member's answer to the whole group and hand the caller a partition-level
	// read that is still a member-level fact. Java does not have to make this
	// distinction because its partition key is the full property set, so its
	// partitions are homogeneous in every property by construction.
	var result []*PlanPartition
	for _, p := range partitions {
		for _, e := range p.GetExpressions() {
			exprProps := p.exprProps[e]
			candidate := &PlanPartition{partitionProps: exprProps}
			var existing *PlanPartition
			for _, g := range result {
				if sameProps(g, candidate) {
					existing = g
					break
				}
			}
			if existing == nil {
				filteredProps := make(properties.PropertyMap, len(interestingProps))
				for _, prop := range interestingProps {
					filteredProps[prop] = exprProps[prop]
				}
				existing = &PlanPartition{
					partitionProps: filteredProps,
					exprProps:      make(map[expressions.RelationalExpression]properties.PropertyMap),
				}
				result = append(result, existing)
			}
			existing.addExpression(e, exprProps)
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
	if ao, ok := a.(*properties.RichOrdering); ok {
		bo, ok2 := b.(*properties.RichOrdering)
		return ok2 && richOrderingsEqual(ao, bo)
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

// richOrderingsEqual compares the FULL ordering structurally. reflect.DeepEqual
// cannot do this job: a RichOrdering holds a map keyed by values.Value and a
// pointer to a partially-ordered set, so two semantically identical orderings
// built by different plans compare unequal on interface identity — the same
// address-vs-value trap orderingsEqual was written for (RFC-180 D3).
//
// The comparison is deliberately CONSERVATIVE where it is unsure. Splitting two
// orderings that are in fact equal costs enumeration; merging two that differ
// puts a member into a partition whose property value does not describe it,
// which is the exact defect roll-up exists to prevent. So the safe direction is
// to split, and every uncertain case takes it.
func richOrderingsEqual(a, b *properties.RichOrdering) bool {
	if a == nil || b == nil {
		return a == b
	}
	ak, bk := a.GetKeys(), b.GetKeys()
	if len(ak) != len(bk) {
		return false
	}
	for i := range ak {
		if !values.ValuesStructurallyEqual(ak[i], bk[i]) {
			return false
		}
		// The bindings carry the direction and null placement for each key, and
		// two orderings over the same key sequence with different bindings are
		// different orderings — that is precisely the distinction PropOrdering
		// loses and this property exists to keep.
		ab, aok := bindingsForStructuralKey(a, ak[i])
		bb, bok := bindingsForStructuralKey(b, bk[i])
		if aok != bok || len(ab) != len(bb) {
			return false
		}
		for j := range ab {
			if ab[j] != bb[j] {
				return false
			}
		}
	}
	// The ORDERING SET is part of the ordering's identity, not decoration. It is
	// the partial order over the keys, and it is what
	// EnumerateSatisfyingComparisonKeyValues walks to decide which comparison
	// keys a requested ordering admits. Two orderings can carry the same key
	// list and the same bindings and still admit different comparison keys.
	//
	// Omitting it here MERGED such a pair, and the merged partition then
	// reported one member's ordering for both — reintroducing, through a
	// too-permissive equality, exactly the read-off-a-member defect the roll-up
	// exists to remove. Measured as six ordered InUnions collapsing to
	// InMemorySort. Over-merging is the unsafe direction and this is the guard
	// against it.
	if !a.OrderingSet().Equal(b.OrderingSet()) {
		return false
	}
	return a.IsDistinct() == b.IsDistinct()
}

// bindingsForStructuralKey looks a key up in an ordering's binding map by
// STRUCTURAL equality. The map is keyed by values.Value, so a direct index with
// a key taken from a different ordering matches only on interface identity and
// would report "absent" for a key that is plainly there.
func bindingsForStructuralKey(
	o *properties.RichOrdering, key values.Value,
) ([]properties.OrderingBinding, bool) {
	for k, bindings := range o.GetBindingMap() {
		if values.ValuesStructurallyEqual(k, key) {
			return bindings, true
		}
	}
	return nil, false
}

func orderingsEqual(a, b properties.Ordering) bool {
	if a.IsKnown != b.IsKnown || len(a.Keys) != len(b.Keys) {
		return false
	}
	for i := range a.Keys {
		if !values.ValuesStructurallyEqual(a.Keys[i], b.Keys[i]) {
			return false
		}
		// SEMANTIC accessors, never raw slices: an absent NullsFirst means
		// NATURAL placement (ASC → nulls-first), so raw absent-=false both
		// over-merged (absent vs explicit-false on ASC are semantically
		// DIFFERENT null placements) and under-merged (absent vs
		// explicit-true on ASC are the same).
		if a.DescendingAt(i) != b.DescendingAt(i) || a.NullsFirstAt(i) != b.NullsFirstAt(i) {
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

// partitionCost ranks a partition by its first expression's estimated cost,
// with the estimate CLAMPED into the interval that expression proves (RFC-195).
//
// This walk sees NO children (HintCost(nil, ...)), so the bounds it can derive
// are the STRUCTURAL ones that need no child — an ungrouped aggregate's max=1,
// a LIMIT 0's exact zero, a unique point probe's at-most-one. Child-floor bounds
// are unknown here and the clamp is a deterministic no-op for them, which is the
// honest answer rather than a guess: with no child interval there is nothing
// proved to floor against.
//
// Partition ranking feeds RULE selection, not only plan choice, so a clamp here
// can change which rule fires. That is the intended effect — a rule chosen
// against an estimate the property layer disproves is chosen wrongly — and its
// plan-shape diffs are reviewed with the same care as goldens.
func partitionCost(p *PlanPartition) float64 {
	exprs := p.GetExpressions()
	if len(exprs) == 0 {
		return 1e18
	}
	e := exprs[0]
	hc, ok := e.(interface {
		HintCost([]properties.Cost, properties.StatisticsProvider) properties.Cost
	})
	if !ok {
		return 1e18
	}
	bounds := properties.ProvenCardinalitiesFrom(e, nil)
	// DefaultStatistics, hardcoded, is safe ONLY because nothing in production
	// reaches here. SelectMinCostPartition is this function's sole caller, and it
	// has no production caller of its own — it is a port of Java's
	// ExpressionPartitionMatchers.argmin kept for parity. So there is one cost
	// model in the search today, not two.
	//
	// That is a claim about the tree, so here is how to re-check it rather than
	// trust it (the unexported-func gate in pkg/docscheck cannot: its population
	// is UNEXPORTED functions, and this one is exported):
	//
	//	grep -rn 'SelectMinCostPartition(' --include='*.go' . | grep -v _test.go |
	//	  grep -v 'func SelectMinCostPartition' | grep -vc ':[0-9]*:[[:space:]]*//'
	//
	// That counts CALL sites: it drops the definition AND comment lines. The
	// last filter is not decoration — without it the command matches the very
	// line you are reading and returns 1, which is what the first version of
	// this comment shipped while warning against exactly that. It is 0 as
	// written; the same command for FilterPlanPartitions in this file returns 3,
	// which is the control showing it can find production callers when they
	// exist. Any non-zero reading here means the paragraph below has become
	// live.
	//
	// If that changes, this becomes a real Cascades inconsistency rather than a
	// latent one: the same expression would be ranked here against a constant
	// 1e6 per record type while being COSTED for plan selection against collected
	// counts (RFC-236), so a partition could be discarded for being expensive
	// under an estimate the planner itself no longer believes. Making
	// SelectMinCostPartition reachable therefore means threading the planner's
	// StatisticsProvider down to here, not adding a caller.
	stats := properties.DefaultStatistics{}
	c := properties.CostWithinBounds(e, nil, bounds, stats, func() properties.Cost {
		return hc.HintCost(nil, stats)
	})
	return c.Cardinality + c.CPU
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
