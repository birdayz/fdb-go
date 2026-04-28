package cascades

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// writeHash64 writes a uint64 to the FNV hasher in big-endian
// byte order. Shared by all four wrapper types' HashCodeWithoutChildren
// implementations.
func writeHash64(h hashWriter, v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	_, _ = h.Write(b[:])
}

// hashWriter is the minimal io.Writer surface fnv.New64a() returns.
type hashWriter interface {
	Write(p []byte) (n int, err error)
}

// physicalWrapperCostMultiplier is applied to each physical wrapper's
// inherited cost so cost-driven extraction prefers physical plans
// over their logical counterparts. 0.9 = "physical is 10% cheaper
// than logical" — enough to flip ordering on equally-shaped
// alternatives, small enough not to dominate the cost comparison
// with structurally-different alternatives.
const physicalWrapperCostMultiplier = 0.9

// physicalScanWrapper adapts a `*plans.RecordQueryScanPlan` to the
// `expressions.RelationalExpression` interface so Batch A rules can
// yield it into the existing Reference dedup machinery without a
// Memo overhaul.
//
// This is a SEED workaround. Java's planner has a unified
// RelationalExpression hierarchy where physical plans (RecordQueryPlan)
// implement RelationalExpressionWithChildren too. Our seed kept the
// two hierarchies separate (per RFC-022 design choice) — the
// adapter bridges them until a proper plan-aware Reference lands.
//
// The wrapper is leaf-like (no Quantifiers, no children) — the
// underlying RecordQueryScanPlan IS a leaf physical plan. Future
// wrappers for filter / sort plans need to expose their inner
// RecordQueryPlan as a Quantifier-equivalent to enable Memo
// integration; for the seed, only the leaf wrapper exists.
type physicalScanWrapper struct {
	plan *plans.RecordQueryScanPlan
}

// GetPlan exposes the wrapped physical plan.
func (w *physicalScanWrapper) GetPlan() *plans.RecordQueryScanPlan { return w.plan }

// GetResultValue returns a fresh QuantifiedObjectValue whose Type is
// the plan's flowed Type. Mirrors FullUnorderedScanExpression's
// shape so callers can interrogate type without unwrapping.
func (w *physicalScanWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

// GetQuantifiers returns the empty list — the wrapped plan is a leaf.
func (w *physicalScanWrapper) GetQuantifiers() []expressions.Quantifier { return nil }

// CanCorrelate is false — leaf can't anchor correlation.
func (w *physicalScanWrapper) CanCorrelate() bool { return false }

// ChildrenAsSet is false — leaf has no children.
func (w *physicalScanWrapper) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the empty set.
func (w *physicalScanWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares wrapped plans via plans.Equals on
// the same wrapper concrete type.
func (w *physicalScanWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalScanWrapper)
	if !ok {
		return false
	}
	return plans.Equals(w.plan, o.plan)
}

// HashCodeWithoutChildren mixes the class discriminator with the
// wrapped plan's hash.
func (w *physicalScanWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physcanwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// WithChildren satisfies properties.WithChildren — scan is a leaf,
// so qs must be empty. Returns the wrapper itself unchanged on
// empty input.
func (w *physicalScanWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 0 {
		return nil, fmt.Errorf("physicalScanWrapper.WithChildren: expected 0 children, got %d", len(qs))
	}
	return w, nil
}

// HintCost matches the LogicalScan equivalent (see properties/cost.go's
// FullUnorderedScanExpression arm) and applies the physical-wrapper
// discount so cost-driven extraction prefers the physical scan over
// the logical one.
func (w *physicalScanWrapper) HintCost(_ []properties.Cost) properties.Cost {
	if w.plan == nil {
		return properties.Cost{Cardinality: properties.LeafScanCardinality, CPU: 0}
	}
	types := w.plan.GetRecordTypes()
	if len(types) == 0 {
		return properties.Cost{Cardinality: properties.LeafScanCardinality * physicalWrapperCostMultiplier, CPU: 0}
	}
	total := 0.0
	for range types {
		total += properties.LeafScanCardinality
	}
	return properties.Cost{Cardinality: total * physicalWrapperCostMultiplier, CPU: 0}
}

var _ expressions.RelationalExpression = (*physicalScanWrapper)(nil)

// physicalFilterWrapper adapts a `*plans.RecordQueryFilterPlan` to
// the RelationalExpression interface. The wrapped plan has a single
// inner — exposed as a single Quantifier ranging over a fresh
// Reference holding a wrapped version of the inner physical plan.
//
// The wrapped-inner indirection is intentional: it keeps the Memo's
// Reference invariant intact (every Quantifier's Reference holds at
// least one RelationalExpression-typed member). Once a proper
// physical-plan-aware Memo lands, this wrapping goes away — plans
// will be Memo members directly, no adapter needed.
type physicalFilterWrapper struct {
	plan       *plans.RecordQueryFilterPlan
	innerQuant expressions.Quantifier
}

// NewPhysicalFilterWrapper constructs the wrapper. innerQuant must
// range over a Reference holding the wrapped inner physical plan.
func NewPhysicalFilterWrapper(plan *plans.RecordQueryFilterPlan, innerQuant expressions.Quantifier) *physicalFilterWrapper {
	return &physicalFilterWrapper{plan: plan, innerQuant: innerQuant}
}

// GetPlan exposes the wrapped physical plan.
func (w *physicalFilterWrapper) GetPlan() *plans.RecordQueryFilterPlan { return w.plan }

// GetResultValue returns the inner Quantifier's flowed object value
// — filter doesn't reshape rows.
func (w *physicalFilterWrapper) GetResultValue() values.Value {
	return w.innerQuant.GetFlowedObjectValue()
}

// GetQuantifiers returns the inner Quantifier as the only child.
func (w *physicalFilterWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

// CanCorrelate is false — filter doesn't anchor correlation.
func (w *physicalFilterWrapper) CanCorrelate() bool { return false }

// ChildrenAsSet is false — filter has one child.
func (w *physicalFilterWrapper) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the empty set — the seed
// doesn't surface predicate-side correlation through the wrapper.
func (w *physicalFilterWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares the wrapped plan's predicate list.
// Children equality is the caller's job (typically via SemanticEquals).
func (w *physicalFilterWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalFilterWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

// HashCodeWithoutChildren mixes class + plan's hash.
func (w *physicalFilterWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physfilterwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// WithChildren constructs a fresh wrapper using qs[0] as the new
// inner Quantifier. Returns an error if qs doesn't have exactly
// one entry.
func (w *physicalFilterWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalFilterWrapper.WithChildren: expected 1 child, got %d", len(qs))
	}
	return &physicalFilterWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost mirrors the LogicalFilter cost formula and applies the
// physical-wrapper discount so cost-driven extraction prefers the
// physical filter over the logical one.
func (w *physicalFilterWrapper) HintCost(child []properties.Cost) properties.Cost {
	if len(child) == 0 || w.plan == nil {
		return properties.Cost{}
	}
	in := child[0].Cardinality
	numPreds := len(w.plan.GetPredicates())
	if numPreds == 0 {
		numPreds = 1
	}
	sel := properties.FilterSelectivity
	for i := 1; i < numPreds; i++ {
		sel *= properties.FilterSelectivity
	}
	return properties.Cost{
		Cardinality: in * sel * physicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + in*properties.FilterCPU*float64(numPreds)) * physicalWrapperCostMultiplier,
	}
}

var _ expressions.RelationalExpression = (*physicalFilterWrapper)(nil)

// physicalSortWrapper adapts a `*plans.RecordQuerySortPlan` to the
// RelationalExpression interface. Same structure as
// physicalFilterWrapper — single inner Quantifier ranging over a
// wrapped inner physical plan.
type physicalSortWrapper struct {
	plan       *plans.RecordQuerySortPlan
	innerQuant expressions.Quantifier
}

// NewPhysicalSortWrapper constructs the wrapper.
func NewPhysicalSortWrapper(plan *plans.RecordQuerySortPlan, innerQuant expressions.Quantifier) *physicalSortWrapper {
	return &physicalSortWrapper{plan: plan, innerQuant: innerQuant}
}

// GetPlan exposes the wrapped physical plan.
func (w *physicalSortWrapper) GetPlan() *plans.RecordQuerySortPlan { return w.plan }

// GetResultValue returns the inner Quantifier's flowed object value
// — sort doesn't reshape rows.
func (w *physicalSortWrapper) GetResultValue() values.Value {
	return w.innerQuant.GetFlowedObjectValue()
}

// GetQuantifiers returns the inner Quantifier as the only child.
func (w *physicalSortWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

// CanCorrelate is false — sort doesn't anchor correlation.
func (w *physicalSortWrapper) CanCorrelate() bool { return false }

// ChildrenAsSet is false.
func (w *physicalSortWrapper) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the empty set.
func (w *physicalSortWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares the wrapped plan.
func (w *physicalSortWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalSortWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

// HashCodeWithoutChildren mixes class + plan's hash.
func (w *physicalSortWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physsortwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// WithChildren constructs a fresh wrapper using qs[0] as the new
// inner Quantifier.
func (w *physicalSortWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalSortWrapper.WithChildren: expected 1 child, got %d", len(qs))
	}
	return &physicalSortWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost mirrors LogicalSort with the physical-wrapper discount.
func (w *physicalSortWrapper) HintCost(child []properties.Cost) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	in := child[0].Cardinality
	logN := 1.0
	if in > 2 {
		// log2(in)
		l := in
		for l > 2 {
			l /= 2
			logN++
		}
	}
	return properties.Cost{
		Cardinality: in * physicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + in*properties.SortCPU*logN) * physicalWrapperCostMultiplier,
	}
}

var _ expressions.RelationalExpression = (*physicalSortWrapper)(nil)

// physicalDistinctWrapper adapts a `*plans.RecordQueryDistinctPlan` to
// the RelationalExpression interface.
type physicalDistinctWrapper struct {
	plan       *plans.RecordQueryDistinctPlan
	innerQuant expressions.Quantifier
}

// NewPhysicalDistinctWrapper constructs the wrapper.
func NewPhysicalDistinctWrapper(plan *plans.RecordQueryDistinctPlan, innerQuant expressions.Quantifier) *physicalDistinctWrapper {
	return &physicalDistinctWrapper{plan: plan, innerQuant: innerQuant}
}

// GetPlan exposes the wrapped physical plan.
func (w *physicalDistinctWrapper) GetPlan() *plans.RecordQueryDistinctPlan { return w.plan }

// GetResultValue returns the inner Quantifier's flowed object value.
func (w *physicalDistinctWrapper) GetResultValue() values.Value {
	return w.innerQuant.GetFlowedObjectValue()
}

// GetQuantifiers returns the inner Quantifier as the only child.
func (w *physicalDistinctWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

// CanCorrelate is false.
func (w *physicalDistinctWrapper) CanCorrelate() bool { return false }

// ChildrenAsSet is false.
func (w *physicalDistinctWrapper) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the empty set.
func (w *physicalDistinctWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares the wrapped plan.
func (w *physicalDistinctWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalDistinctWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

// HashCodeWithoutChildren mixes class + plan's hash.
func (w *physicalDistinctWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physdistwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// WithChildren constructs a fresh wrapper using qs[0] as the new
// inner Quantifier.
func (w *physicalDistinctWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalDistinctWrapper.WithChildren: expected 1 child, got %d", len(qs))
	}
	return &physicalDistinctWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost mirrors LogicalDistinct with the physical-wrapper discount.
func (w *physicalDistinctWrapper) HintCost(child []properties.Cost) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	in := child[0].Cardinality
	return properties.Cost{
		Cardinality: in * properties.DistinctSelectivity * physicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + in*properties.DistinctCPU) * physicalWrapperCostMultiplier,
	}
}

var _ expressions.RelationalExpression = (*physicalDistinctWrapper)(nil)

// physicalTypeFilterWrapper adapts a `*plans.RecordQueryTypeFilterPlan`
// to the RelationalExpression interface.
type physicalTypeFilterWrapper struct {
	plan       *plans.RecordQueryTypeFilterPlan
	innerQuant expressions.Quantifier
}

// NewPhysicalTypeFilterWrapper constructs the wrapper.
func NewPhysicalTypeFilterWrapper(plan *plans.RecordQueryTypeFilterPlan, innerQuant expressions.Quantifier) *physicalTypeFilterWrapper {
	return &physicalTypeFilterWrapper{plan: plan, innerQuant: innerQuant}
}

// GetPlan exposes the wrapped physical plan.
func (w *physicalTypeFilterWrapper) GetPlan() *plans.RecordQueryTypeFilterPlan { return w.plan }

// GetResultValue returns the inner Quantifier's flowed object value.
func (w *physicalTypeFilterWrapper) GetResultValue() values.Value {
	return w.innerQuant.GetFlowedObjectValue()
}

// GetQuantifiers returns the inner Quantifier as the only child.
func (w *physicalTypeFilterWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

// CanCorrelate is false.
func (w *physicalTypeFilterWrapper) CanCorrelate() bool { return false }

// ChildrenAsSet is false.
func (w *physicalTypeFilterWrapper) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the empty set.
func (w *physicalTypeFilterWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares the wrapped plan.
func (w *physicalTypeFilterWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalTypeFilterWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

// HashCodeWithoutChildren mixes class + plan's hash.
func (w *physicalTypeFilterWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("phystypefiltwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// WithChildren constructs a fresh wrapper.
func (w *physicalTypeFilterWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalTypeFilterWrapper.WithChildren: expected 1 child, got %d", len(qs))
	}
	return &physicalTypeFilterWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost mirrors LogicalTypeFilter with the physical-wrapper
// discount.
func (w *physicalTypeFilterWrapper) HintCost(child []properties.Cost) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	in := child[0].Cardinality
	return properties.Cost{
		Cardinality: in * properties.TypeFilterSelectivity * physicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + in*properties.TypeFilterCPU) * physicalWrapperCostMultiplier,
	}
}

var _ expressions.RelationalExpression = (*physicalTypeFilterWrapper)(nil)

// physicalInsertWrapper adapts a `*plans.RecordQueryInsertPlan` to
// the RelationalExpression interface — same shape as the other
// single-inner physical wrappers.
type physicalInsertWrapper struct {
	plan       *plans.RecordQueryInsertPlan
	innerQuant expressions.Quantifier
}

// NewPhysicalInsertWrapper constructs the wrapper.
func NewPhysicalInsertWrapper(plan *plans.RecordQueryInsertPlan, innerQuant expressions.Quantifier) *physicalInsertWrapper {
	return &physicalInsertWrapper{plan: plan, innerQuant: innerQuant}
}

// GetPlan exposes the wrapped physical plan.
func (w *physicalInsertWrapper) GetPlan() *plans.RecordQueryInsertPlan { return w.plan }

// GetResultValue returns the inner Quantifier's flowed object value.
func (w *physicalInsertWrapper) GetResultValue() values.Value {
	return w.innerQuant.GetFlowedObjectValue()
}

// GetQuantifiers returns the inner Quantifier as the only child.
func (w *physicalInsertWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

// CanCorrelate is false.
func (w *physicalInsertWrapper) CanCorrelate() bool { return false }

// ChildrenAsSet is false.
func (w *physicalInsertWrapper) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the empty set.
func (w *physicalInsertWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares wrapped plans.
func (w *physicalInsertWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalInsertWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

// HashCodeWithoutChildren mixes class + plan's hash.
func (w *physicalInsertWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physinsertwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// WithChildren constructs a fresh wrapper.
func (w *physicalInsertWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalInsertWrapper.WithChildren: expected 1 child, got %d", len(qs))
	}
	return &physicalInsertWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost: INSERT cost is dominated by the per-row write cost
// (Java's CascadesCostModel weights writes heavily). Mirrors the
// LogicalDML write cost — sumCPU + cardinality * WriteCPU.
func (w *physicalInsertWrapper) HintCost(child []properties.Cost) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	in := child[0].Cardinality
	return properties.Cost{
		Cardinality: in * physicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + in*properties.WriteCPU) * physicalWrapperCostMultiplier,
	}
}

var _ expressions.RelationalExpression = (*physicalInsertWrapper)(nil)
