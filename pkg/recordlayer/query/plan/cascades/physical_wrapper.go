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

// physicalPlanExpression is implemented by all physical-plan wrapper
// types. Lets implement rules discover physical plans in a Reference
// with a single interface assertion instead of per-type switches.
type physicalPlanExpression interface {
	expressions.RelationalExpression
	GetRecordQueryPlan() plans.RecordQueryPlan
}

// IsPhysicalIndexScan reports whether the given RelationalExpression is
// a physicalIndexScanWrapper. Exported so external test packages can
// identify index scan plans without depending on the unexported type.
func IsPhysicalIndexScan(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalIndexScanWrapper)
	return ok
}

// IsPhysicalIntersection reports whether the given RelationalExpression
// is a physicalIntersectionWrapper.
func IsPhysicalIntersection(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalIntersectionWrapper)
	return ok
}

// IsPhysicalMultiIntersection reports whether the given
// RelationalExpression is a physicalMultiIntersectionWrapper.
func IsPhysicalMultiIntersection(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalMultiIntersectionWrapper)
	return ok
}

// GetPhysicalMultiIntersectionPlan returns the underlying
// RecordQueryMultiIntersectionOnValuesPlan if expr is a
// physicalMultiIntersectionWrapper, nil otherwise.
func GetPhysicalMultiIntersectionPlan(expr expressions.RelationalExpression) *plans.RecordQueryMultiIntersectionOnValuesPlan {
	w, ok := expr.(*physicalMultiIntersectionWrapper)
	if !ok {
		return nil
	}
	return w.plan
}

// IsPhysicalFilter reports whether the given RelationalExpression is
// a physical filter wrapper (either legacy or predicates-based).
func IsPhysicalFilter(expr expressions.RelationalExpression) bool {
	switch expr.(type) {
	case *physicalFilterWrapper, *physicalPredicatesFilterWrapper:
		return true
	}
	return false
}

// IsPhysicalInsert reports whether the given RelationalExpression is
// a physicalInsertWrapper.
func IsPhysicalInsert(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalInsertWrapper)
	return ok
}

// IsPhysicalDelete reports whether the given RelationalExpression is
// a physicalDeleteWrapper.
func IsPhysicalDelete(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalDeleteWrapper)
	return ok
}

// IsPhysicalUpdate reports whether the given RelationalExpression is
// a physicalUpdateWrapper.
func IsPhysicalUpdate(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalUpdateWrapper)
	return ok
}

// IsPhysicalPredicatesFilter reports whether the given expression is
// a physicalPredicatesFilterWrapper.
func IsPhysicalPredicatesFilter(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalPredicatesFilterWrapper)
	return ok
}

// IsPhysicalMap reports whether the given expression is a physicalMapWrapper.
func IsPhysicalMap(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalMapWrapper)
	return ok
}

// IsPhysicalFirstOrDefault reports whether the given expression is
// a physicalFirstOrDefaultWrapper.
func IsPhysicalFirstOrDefault(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalFirstOrDefaultWrapper)
	return ok
}

// IsPhysicalDefaultOnEmpty reports whether the given expression is
// a physicalDefaultOnEmptyWrapper.
func IsPhysicalDefaultOnEmpty(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalDefaultOnEmptyWrapper)
	return ok
}

// IsPhysicalUnorderedUnion reports whether the given expression is
// a physicalUnorderedUnionWrapper.
func IsPhysicalUnorderedUnion(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalUnorderedUnionWrapper)
	return ok
}

// IsPhysicalMergeSortUnion reports whether the given expression is
// a physicalMergeSortUnionWrapper.
func IsPhysicalMergeSortUnion(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalMergeSortUnionWrapper)
	return ok
}

// IsPhysicalInJoin reports whether the given expression is
// a physicalInJoinWrapper.
func IsPhysicalInJoin(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalInJoinWrapper)
	return ok
}

// IsPhysicalInUnion reports whether the given expression is
// a physicalInUnionWrapper.
func IsPhysicalInUnion(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalInUnionWrapper)
	return ok
}

// ExplainPhysicalPlan returns the Explain() string for a physical-plan
// expression, or empty string if the expression is not a physical plan.
func ExplainPhysicalPlan(expr expressions.RelationalExpression) string {
	ph, ok := expr.(physicalPlanExpression)
	if !ok {
		return ""
	}
	p := ph.GetRecordQueryPlan()
	if p == nil {
		return ""
	}
	return p.Explain()
}

// PhysicalIndexScanName returns the index name if expr is a
// physicalIndexScanWrapper or a physicalFetchFromPartialRecordWrapper
// whose inner plan is an index plan. Returns empty string otherwise.
func PhysicalIndexScanName(expr expressions.RelationalExpression) string {
	if w, ok := expr.(*physicalIndexScanWrapper); ok {
		return w.plan.GetIndexName()
	}
	if fw, ok := expr.(*physicalFetchFromPartialRecordWrapper); ok {
		if ip, ok := fw.plan.GetInner().(*plans.RecordQueryIndexPlan); ok {
			return ip.GetIndexName()
		}
	}
	return ""
}

// extractChildPlanFromQuantifier gets the RecordQueryPlan from a
// quantifier's Reference. Used by WithChildren implementations to
// rebuild the plan with the freshly-extracted child plan during plan
// extraction. Returns nil if the quantifier has no physical plan.
func extractChildPlanFromQuantifier(q expressions.Quantifier) plans.RecordQueryPlan {
	ref := q.GetRangesOver()
	if ref == nil {
		return nil
	}
	return findPhysicalPlan(ref)
}

// findPhysicalPlan scans ref's members for the first physical-plan
// expression and returns its underlying RecordQueryPlan. Returns nil
// if no physical plan has been yielded into ref yet.
func findPhysicalPlan(ref *expressions.Reference) plans.RecordQueryPlan {
	if ref == nil {
		return nil
	}
	for _, m := range ref.AllMembers() {
		if ph, ok := m.(physicalPlanExpression); ok {
			return ph.GetRecordQueryPlan()
		}
	}
	return nil
}

// findPhysicalExpr scans ref's members for the first physical-plan
// expression and returns it as a RelationalExpression. Used by
// implement rules to obtain the existing wrapper (already memoized
// in the inner Reference by a prior implement-rule fire) without
// re-wrapping from scratch.
func findPhysicalExpr(ref *expressions.Reference) expressions.RelationalExpression {
	if ref == nil {
		return nil
	}
	for _, m := range ref.AllMembers() {
		if _, ok := m.(physicalPlanExpression); ok {
			return m
		}
	}
	return nil
}

// findBestPhysicalExpr returns the cheapest physical member of ref
// under the given cost comparator. Returns nil if no physical member
// exists.
func findBestPhysicalExpr(ref *expressions.Reference, less func(a, b expressions.RelationalExpression) bool) expressions.RelationalExpression {
	if ref == nil {
		return nil
	}
	var best expressions.RelationalExpression
	for _, m := range ref.AllMembers() {
		if _, ok := m.(physicalPlanExpression); !ok {
			continue
		}
		if best == nil || less(m, best) {
			best = m
		}
	}
	return best
}

// isLeafReplaceable reports whether a plan is safe to substitute as the
// inner of a projection without altering the output schema or predicate
// semantics. Only leaf-adjacent plans (scans, filters over scans, index
// scans, streaming agg, distinct, etc.) qualify. Compound join plans
// (NLJ, FlatMap, InJoin) encode predicate semantics in their structure
// and must NOT be swapped — extraction already picks the right join plan
// via quantifier traversal.
func isLeafReplaceable(p plans.RecordQueryPlan) bool {
	switch p.(type) {
	case *plans.RecordQueryScanPlan,
		*plans.RecordQueryIndexPlan,
		*plans.RecordQueryFilterPlan,
		*plans.RecordQueryTypeFilterPlan,
		*plans.RecordQueryFetchFromPartialRecordPlan,
		*plans.RecordQueryInMemorySortPlan,
		*plans.RecordQueryStreamingAggregationPlan,
		*plans.RecordQueryDistinctPlan,
		*plans.RecordQueryLimitPlan,
		*plans.RecordQuerySortPlan,
		*plans.RecordQueryPredicatesFilterPlan,
		*plans.RecordQueryAggregateIndexPlan:
		return true
	}
	return false
}

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

// GetRecordQueryPlan implements physicalPlanExpression.
func (w *physicalScanWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

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
func (w *physicalScanWrapper) HintCost(_ []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	if w.plan == nil {
		card := stats.RecordTypeCardinality("")
		return properties.Cost{Cardinality: card, CPU: card * properties.ScanCPU}
	}
	comps := w.plan.GetScanComparisons()
	numBound := 0
	allEquality := true
	for _, cr := range comps {
		if !cr.IsEmpty() {
			numBound++
			if !cr.IsEquality() {
				allEquality = false
			}
		}
	}
	if numBound > 0 && allEquality && numBound == len(comps) {
		return properties.Cost{Cardinality: 1, CPU: properties.ScanCPU}
	}
	types := w.plan.GetRecordTypes()
	total := 0.0
	if len(types) == 0 {
		total = stats.RecordTypeCardinality("")
	} else {
		for _, t := range types {
			total += stats.RecordTypeCardinality(t)
		}
	}
	sel := 1.0
	for i := 0; i < numBound; i++ {
		sel *= properties.FilterSelectivity
	}
	card := total * sel * physicalWrapperCostMultiplier
	return properties.Cost{Cardinality: card, CPU: card * properties.ScanCPU}
}

// HintOrdering: a scan produces rows in PK order when the scan
// carries PK values (from WithPrimaryKey). Otherwise unknown.
func (w *physicalScanWrapper) HintOrdering() properties.Ordering {
	if w.plan == nil {
		return properties.Ordering{}
	}
	pk := w.plan.GetPrimaryKeyValues()
	if len(pk) == 0 {
		return properties.Ordering{}
	}
	desc := make([]bool, len(pk))
	if w.plan.IsReverse() {
		for i := range desc {
			desc[i] = true
		}
	}
	return properties.Ordering{IsKnown: true, Keys: pk, Descending: desc}
}

func (w *physicalScanWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalScanWrapper)(nil)

// physicalIndexScanWrapper adapts a `*plans.RecordQueryIndexPlan` to
// the RelationalExpression interface. Same leaf shape as
// physicalScanWrapper — index scans have no children in the Memo.
type physicalIndexScanWrapper struct {
	plan        *plans.RecordQueryIndexPlan
	columnNames []string // index column names for ordering property
	unique      bool
	covering    bool // true when the index provides all needed columns (MergeFetch can eliminate the fetch)
}

func (w *physicalIndexScanWrapper) GetPlan() *plans.RecordQueryIndexPlan      { return w.plan }
func (w *physicalIndexScanWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalIndexScanWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalIndexScanWrapper) GetQuantifiers() []expressions.Quantifier { return nil }
func (w *physicalIndexScanWrapper) CanCorrelate() bool                       { return false }
func (w *physicalIndexScanWrapper) ChildrenAsSet() bool                      { return false }

func (w *physicalIndexScanWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalIndexScanWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalIndexScanWrapper)
	if !ok {
		return false
	}
	return plans.Equals(w.plan, o.plan)
}

func (w *physicalIndexScanWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physindexscanwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalIndexScanWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 0 {
		return nil, fmt.Errorf("physicalIndexScanWrapper.WithChildren: expected 0 children, got %d", len(qs))
	}
	return w, nil
}

// HintOrdering: an index scan produces rows in index-key order for
// the non-equality-bound suffix columns. E.g. index(a, b, c) with
// a = 1 produces output sorted by (b, c).
func (w *physicalIndexScanWrapper) HintOrdering() properties.Ordering {
	if w.plan == nil || len(w.columnNames) == 0 {
		return properties.Ordering{}
	}
	comps := w.plan.GetScanComparisons()
	firstNonEq := 0
	for i, cr := range comps {
		if cr.IsEquality() {
			firstNonEq = i + 1
		} else {
			break
		}
	}
	if firstNonEq >= len(w.columnNames) {
		return properties.Ordering{IsKnown: true}
	}
	rev := w.plan.IsReverse()
	keys := make([]values.Value, 0, len(w.columnNames)-firstNonEq)
	desc := make([]bool, 0, len(w.columnNames)-firstNonEq)
	for i := firstNonEq; i < len(w.columnNames); i++ {
		keys = append(keys, &values.FieldValue{Field: w.columnNames[i], Typ: values.UnknownType})
		desc = append(desc, rev)
	}
	return properties.Ordering{IsKnown: true, Keys: keys, Descending: desc}
}

// HintRichOrdering returns the full ordering with bindings: equality-bound
// prefix columns become FixedBinding entries (with comparison reference),
// non-equality suffix columns become SortedBinding entries. This enables
// ordering-aware InJoin source matching.
func (w *physicalIndexScanWrapper) HintRichOrdering() *RichOrdering {
	if w.plan == nil || len(w.columnNames) == 0 {
		return EmptyOrdering()
	}
	comps := w.plan.GetScanComparisons()
	bm := make(map[values.Value][]OrderingBinding)
	keys := make([]values.Value, 0, len(w.columnNames))

	rev := w.plan.IsReverse()
	for i, col := range w.columnNames {
		key := &values.FieldValue{Field: col, Typ: values.UnknownType}
		keys = append(keys, key)
		if i < len(comps) && comps[i].IsEquality() {
			bm[key] = []OrderingBinding{FixedBinding(comps[i])}
		} else {
			dir := ProvidedSortOrderAscending
			if rev {
				dir = ProvidedSortOrderDescending
			}
			bm[key] = []OrderingBinding{SortedBinding(dir)}
		}
	}
	return NewRichOrdering(bm, keys, w.unique)
}

// HintCost: index scans are cheaper than full table scans because
// they read a subset of records. Apply a selectivity multiplier on
// top of the physical-wrapper discount. Unique indexes with all
// columns equality-bound return cardinality=1 (point lookup).
//
// Fetch I/O cost (FetchCPU per row) is NOT included here — it
// belongs on the Fetch enforcer wrapper, which is eliminated for
// covering scans.
func (w *physicalIndexScanWrapper) HintCost(_ []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	base := indexBaseCardinality(w.plan, stats) * physicalWrapperCostMultiplier
	numBound := 0
	allEquality := true
	if w.plan != nil {
		for _, cr := range w.plan.GetScanComparisons() {
			if !cr.IsEmpty() {
				numBound++
				if !cr.IsEquality() {
					allEquality = false
				}
			}
		}
		if w.unique && allEquality && numBound == len(w.columnNames) {
			return properties.Cost{Cardinality: physicalWrapperCostMultiplier, CPU: 0}
		}
		sel := 1.0
		for i := 0; i < numBound; i++ {
			sel *= properties.FilterSelectivity
		}
		base *= sel
	}
	cpu := base * properties.ScanCPU
	return properties.Cost{Cardinality: base, CPU: cpu}
}

func indexBaseCardinality(plan *plans.RecordQueryIndexPlan, stats properties.StatisticsProvider) float64 {
	if plan != nil {
		if types := plan.GetRecordTypes(); len(types) > 0 {
			total := 0.0
			for _, t := range types {
				total += stats.RecordTypeCardinality(t)
			}
			return total
		}
	}
	return stats.RecordTypeCardinality("")
}

func (w *physicalIndexScanWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalIndexScanWrapper)(nil)

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

// GetRecordQueryPlan implements physicalPlanExpression.
func (w *physicalFilterWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

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
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		newPlan := plans.NewRecordQueryFilterPlan(w.plan.GetPredicates(), innerPlan)
		return &physicalFilterWrapper{plan: newPlan, innerQuant: qs[0]}, nil
	}
	return &physicalFilterWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost mirrors the LogicalFilter cost formula and applies the
// physical-wrapper discount so cost-driven extraction prefers the
// physical filter over the logical one.
func (w *physicalFilterWrapper) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
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

func (w *physicalFilterWrapper) HintOrdering() properties.Ordering {
	ref := w.innerQuant.GetRangesOver()
	if ref == nil {
		return properties.Ordering{}
	}
	for _, m := range ref.AllMembers() {
		o := properties.EstimateOrdering(m)
		if o.IsKnown {
			return o
		}
	}
	return properties.Ordering{}
}

func (w *physicalFilterWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalFilterWrapper)(nil)

// IsPhysicalDistinct reports whether the given RelationalExpression is
// a physicalDistinctWrapper.
func IsPhysicalDistinct(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalDistinctWrapper)
	return ok
}

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

// GetRecordQueryPlan implements physicalPlanExpression.
func (w *physicalDistinctWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

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
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		return &physicalDistinctWrapper{plan: plans.NewRecordQueryDistinctPlan(innerPlan), innerQuant: qs[0]}, nil
	}
	return &physicalDistinctWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost mirrors LogicalDistinct with the physical-wrapper discount.
func (w *physicalDistinctWrapper) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	in := child[0].Cardinality
	return properties.Cost{
		Cardinality: in * properties.DistinctSelectivity * physicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + in*properties.DistinctCPU) * physicalWrapperCostMultiplier,
	}
}

func (w *physicalDistinctWrapper) HintOrdering() properties.Ordering {
	ref := w.innerQuant.GetRangesOver()
	if ref == nil {
		return properties.Ordering{}
	}
	for _, m := range ref.AllMembers() {
		o := properties.EstimateOrdering(m)
		if o.IsKnown {
			return o
		}
	}
	return properties.Ordering{}
}

func (w *physicalDistinctWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
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

// GetRecordQueryPlan implements physicalPlanExpression.
func (w *physicalTypeFilterWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

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
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		return &physicalTypeFilterWrapper{plan: plans.NewRecordQueryTypeFilterPlan(w.plan.GetRecordTypes(), innerPlan), innerQuant: qs[0]}, nil
	}
	return &physicalTypeFilterWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost mirrors LogicalTypeFilter with the physical-wrapper
// discount.
func (w *physicalTypeFilterWrapper) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	in := child[0].Cardinality
	return properties.Cost{
		Cardinality: in * properties.TypeFilterSelectivity * physicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + in*properties.TypeFilterCPU) * physicalWrapperCostMultiplier,
	}
}

func (w *physicalTypeFilterWrapper) HintOrdering() properties.Ordering {
	ref := w.innerQuant.GetRangesOver()
	if ref == nil {
		return properties.Ordering{}
	}
	for _, m := range ref.AllMembers() {
		o := properties.EstimateOrdering(m)
		if o.IsKnown {
			return o
		}
	}
	return properties.Ordering{}
}

func (w *physicalTypeFilterWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
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

// GetRecordQueryPlan implements physicalPlanExpression.
func (w *physicalInsertWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

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
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		return &physicalInsertWrapper{plan: plans.NewRecordQueryInsertPlan(innerPlan, w.plan.GetTargetRecordType(), w.plan.GetTargetType()), innerQuant: qs[0]}, nil
	}
	return &physicalInsertWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost: INSERT cost is dominated by the per-row write cost
// (Java's CascadesCostModel weights writes heavily). Mirrors the
// LogicalDML write cost — sumCPU + cardinality * WriteCPU.
func (w *physicalInsertWrapper) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	in := child[0].Cardinality
	return properties.Cost{
		Cardinality: in * physicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + in*properties.WriteCPU) * physicalWrapperCostMultiplier,
	}
}

func (w *physicalInsertWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalInsertWrapper)(nil)

// physicalDeleteWrapper adapts `*plans.RecordQueryDeletePlan` to
// the RelationalExpression interface.
type physicalDeleteWrapper struct {
	plan       *plans.RecordQueryDeletePlan
	innerQuant expressions.Quantifier
}

// NewPhysicalDeleteWrapper constructs the wrapper.
func NewPhysicalDeleteWrapper(plan *plans.RecordQueryDeletePlan, innerQuant expressions.Quantifier) *physicalDeleteWrapper {
	return &physicalDeleteWrapper{plan: plan, innerQuant: innerQuant}
}

// GetPlan exposes the wrapped physical plan.
func (w *physicalDeleteWrapper) GetPlan() *plans.RecordQueryDeletePlan { return w.plan }

// GetRecordQueryPlan implements physicalPlanExpression.
func (w *physicalDeleteWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

// GetResultValue returns the inner Quantifier's flowed object value.
func (w *physicalDeleteWrapper) GetResultValue() values.Value {
	return w.innerQuant.GetFlowedObjectValue()
}

// GetQuantifiers returns the inner Quantifier as the only child.
func (w *physicalDeleteWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

// CanCorrelate is false.
func (w *physicalDeleteWrapper) CanCorrelate() bool { return false }

// ChildrenAsSet is false.
func (w *physicalDeleteWrapper) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the empty set.
func (w *physicalDeleteWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares wrapped plans.
func (w *physicalDeleteWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalDeleteWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

// HashCodeWithoutChildren mixes class + plan's hash.
func (w *physicalDeleteWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physdeletewrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// WithChildren constructs a fresh wrapper.
func (w *physicalDeleteWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalDeleteWrapper.WithChildren: expected 1 child, got %d", len(qs))
	}
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		return &physicalDeleteWrapper{plan: plans.NewRecordQueryDeletePlan(innerPlan, w.plan.GetTargetRecordType()), innerQuant: qs[0]}, nil
	}
	return &physicalDeleteWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost: DELETE write-heavy cost like INSERT.
func (w *physicalDeleteWrapper) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	in := child[0].Cardinality
	return properties.Cost{
		Cardinality: in * physicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + in*properties.WriteCPU) * physicalWrapperCostMultiplier,
	}
}

func (w *physicalDeleteWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalDeleteWrapper)(nil)

// physicalUpdateWrapper adapts `*plans.RecordQueryUpdatePlan` to
// the RelationalExpression interface.
type physicalUpdateWrapper struct {
	plan       *plans.RecordQueryUpdatePlan
	innerQuant expressions.Quantifier
}

// NewPhysicalUpdateWrapper constructs the wrapper.
func NewPhysicalUpdateWrapper(plan *plans.RecordQueryUpdatePlan, innerQuant expressions.Quantifier) *physicalUpdateWrapper {
	return &physicalUpdateWrapper{plan: plan, innerQuant: innerQuant}
}

// GetPlan exposes the wrapped physical plan.
func (w *physicalUpdateWrapper) GetPlan() *plans.RecordQueryUpdatePlan { return w.plan }

// GetRecordQueryPlan implements physicalPlanExpression.
func (w *physicalUpdateWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

// GetResultValue returns the inner Quantifier's flowed object value.
func (w *physicalUpdateWrapper) GetResultValue() values.Value {
	return w.innerQuant.GetFlowedObjectValue()
}

// GetQuantifiers returns the inner Quantifier as the only child.
func (w *physicalUpdateWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

// CanCorrelate is false.
func (w *physicalUpdateWrapper) CanCorrelate() bool { return false }

// ChildrenAsSet is false.
func (w *physicalUpdateWrapper) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the empty set.
func (w *physicalUpdateWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares wrapped plans.
func (w *physicalUpdateWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalUpdateWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

// HashCodeWithoutChildren mixes class + plan's hash.
func (w *physicalUpdateWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physupdatewrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// WithChildren constructs a fresh wrapper.
func (w *physicalUpdateWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalUpdateWrapper.WithChildren: expected 1 child, got %d", len(qs))
	}
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		return &physicalUpdateWrapper{plan: plans.NewRecordQueryUpdatePlan(innerPlan, w.plan.GetTargetRecordType(), w.plan.GetTransforms()), innerQuant: qs[0]}, nil
	}
	return &physicalUpdateWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost: UPDATE write-heavy cost like INSERT/DELETE.
func (w *physicalUpdateWrapper) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	in := child[0].Cardinality
	return properties.Cost{
		Cardinality: in * physicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + in*properties.WriteCPU) * physicalWrapperCostMultiplier,
	}
}

func (w *physicalUpdateWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalUpdateWrapper)(nil)

// physicalUnionWrapper adapts `*plans.RecordQueryUnionPlan` to the
// RelationalExpression interface. Unlike the single-inner wrappers,
// Union exposes N inner Quantifiers (one per child plan).
type physicalUnionWrapper struct {
	plan        *plans.RecordQueryUnionPlan
	innerQuants []expressions.Quantifier
}

// NewPhysicalUnionWrapper constructs the wrapper.
func NewPhysicalUnionWrapper(plan *plans.RecordQueryUnionPlan, innerQuants []expressions.Quantifier) *physicalUnionWrapper {
	copied := make([]expressions.Quantifier, len(innerQuants))
	copy(copied, innerQuants)
	return &physicalUnionWrapper{plan: plan, innerQuants: copied}
}

// GetPlan exposes the wrapped physical plan.
func (w *physicalUnionWrapper) GetPlan() *plans.RecordQueryUnionPlan { return w.plan }

// GetRecordQueryPlan implements physicalPlanExpression.
func (w *physicalUnionWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

// GetResultValue returns the first inner's flowed object value.
// Java's RecordQueryUnionPlan emits rows compatible with all
// children; the seed picks the first child's row shape.
func (w *physicalUnionWrapper) GetResultValue() values.Value {
	if len(w.innerQuants) == 0 {
		return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
	}
	return w.innerQuants[0].GetFlowedObjectValue()
}

// GetQuantifiers returns the inner Quantifiers (children).
func (w *physicalUnionWrapper) GetQuantifiers() []expressions.Quantifier { return w.innerQuants }

// CanCorrelate is false.
func (w *physicalUnionWrapper) CanCorrelate() bool { return false }

// ChildrenAsSet is true — UNION children are bag-equivalent.
func (w *physicalUnionWrapper) ChildrenAsSet() bool { return true }

// GetCorrelatedToWithoutChildren returns the empty set.
func (w *physicalUnionWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares the wrapped plan.
func (w *physicalUnionWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalUnionWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

// HashCodeWithoutChildren mixes class + plan's hash.
func (w *physicalUnionWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physunionwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// WithChildren constructs a fresh wrapper with the new quantifiers.
func (w *physicalUnionWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	copied := make([]expressions.Quantifier, len(qs))
	copy(copied, qs)
	return &physicalUnionWrapper{plan: w.plan, innerQuants: copied}, nil
}

// HintCost: UNION cardinality is sum of children, CPU is cumulative
// + per-output-row merge work. Mirrors LogicalUnion.
func (w *physicalUnionWrapper) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	sumCard := 0.0
	sumCPU := 0.0
	for _, c := range child {
		sumCard += c.Cardinality
		sumCPU += c.CPU
	}
	return properties.Cost{
		Cardinality: sumCard * physicalWrapperCostMultiplier,
		CPU:         (sumCPU + sumCard*properties.UnionCPU) * physicalWrapperCostMultiplier,
	}
}

func (w *physicalUnionWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalUnionWrapper)(nil)

// physicalIntersectionWrapper adapts `*plans.RecordQueryIntersectionPlan`
// to the RelationalExpression interface. Same N-child shape as the
// Union wrapper; cost differs (Intersection bounded by min child
// cardinality, while Union sums).
type physicalIntersectionWrapper struct {
	plan        *plans.RecordQueryIntersectionPlan
	innerQuants []expressions.Quantifier
}

// NewPhysicalIntersectionWrapper constructs the wrapper.
func NewPhysicalIntersectionWrapper(plan *plans.RecordQueryIntersectionPlan, innerQuants []expressions.Quantifier) *physicalIntersectionWrapper {
	copied := make([]expressions.Quantifier, len(innerQuants))
	copy(copied, innerQuants)
	return &physicalIntersectionWrapper{plan: plan, innerQuants: copied}
}

// GetPlan exposes the wrapped physical plan.
func (w *physicalIntersectionWrapper) GetPlan() *plans.RecordQueryIntersectionPlan {
	return w.plan
}

// GetRecordQueryPlan implements physicalPlanExpression.
func (w *physicalIntersectionWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

// GetResultValue returns the first inner's flowed object value —
// intersection emits rows compatible with all children.
func (w *physicalIntersectionWrapper) GetResultValue() values.Value {
	if len(w.innerQuants) == 0 {
		return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
	}
	return w.innerQuants[0].GetFlowedObjectValue()
}

// GetQuantifiers returns the inner Quantifiers (children).
func (w *physicalIntersectionWrapper) GetQuantifiers() []expressions.Quantifier {
	return w.innerQuants
}

// IsIntersection implements properties.IntersectionExpression.
func (w *physicalIntersectionWrapper) IsIntersection() {}

// CanCorrelate is false.
func (w *physicalIntersectionWrapper) CanCorrelate() bool { return false }

// ChildrenAsSet is true — INTERSECTION children are bag-equivalent.
func (w *physicalIntersectionWrapper) ChildrenAsSet() bool { return true }

// GetCorrelatedToWithoutChildren returns the empty set.
func (w *physicalIntersectionWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares the wrapped plan.
func (w *physicalIntersectionWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalIntersectionWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

// HashCodeWithoutChildren mixes class + plan's hash.
func (w *physicalIntersectionWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physintersectionwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// WithChildren constructs a fresh wrapper with the new quantifiers.
func (w *physicalIntersectionWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	copied := make([]expressions.Quantifier, len(qs))
	copy(copied, qs)
	return &physicalIntersectionWrapper{plan: w.plan, innerQuants: copied}, nil
}

// HintCost: Intersection cardinality is bounded by the SMALLEST
// child (the intersection can't be larger than its smallest
// participant). CPU sums children + per-output-row merge work
// (more expensive than Union — comparison-key-driven matching).
// Mirrors LogicalIntersection.
func (w *physicalIntersectionWrapper) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	minCard := child[0].Cardinality
	sumCard := 0.0
	sumCPU := 0.0
	for _, c := range child {
		if c.Cardinality < minCard {
			minCard = c.Cardinality
		}
		sumCard += c.Cardinality
		sumCPU += c.CPU
	}
	return properties.Cost{
		Cardinality: minCard * physicalWrapperCostMultiplier,
		CPU:         (sumCPU + sumCard*properties.IntersectionCPU) * physicalWrapperCostMultiplier,
	}
}

func (w *physicalIntersectionWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalIntersectionWrapper)(nil)

// --- Projection wrapper -----------------------------------------------

type physicalProjectionWrapper struct {
	plan       *plans.RecordQueryProjectionPlan
	innerQuant expressions.Quantifier
}

func NewPhysicalProjectionWrapper(plan *plans.RecordQueryProjectionPlan, innerQuant expressions.Quantifier) *physicalProjectionWrapper {
	return &physicalProjectionWrapper{plan: plan, innerQuant: innerQuant}
}

func (w *physicalProjectionWrapper) GetPlan() *plans.RecordQueryProjectionPlan { return w.plan }

func (w *physicalProjectionWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalProjectionWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalProjectionWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

func (w *physicalProjectionWrapper) CanCorrelate() bool { return false }

func (w *physicalProjectionWrapper) ChildrenAsSet() bool { return false }

func (w *physicalProjectionWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalProjectionWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalProjectionWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalProjectionWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physprojwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalProjectionWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalProjectionWrapper.WithChildren: expected 1 child, got %d", len(qs))
	}
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		newPlan := plans.NewRecordQueryProjectionPlanWithAliases(w.plan.GetProjections(), w.plan.GetAliases(), innerPlan)
		return &physicalProjectionWrapper{plan: newPlan, innerQuant: qs[0]}, nil
	}
	return &physicalProjectionWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

func (w *physicalProjectionWrapper) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	return properties.Cost{
		Cardinality: child[0].Cardinality * physicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + child[0].Cardinality*properties.ProjectionCPU) * physicalWrapperCostMultiplier,
	}
}

func (w *physicalProjectionWrapper) HintOrdering() properties.Ordering {
	ref := w.innerQuant.GetRangesOver()
	if ref == nil {
		return properties.Ordering{}
	}
	for _, m := range ref.AllMembers() {
		o := properties.EstimateOrdering(m)
		if o.IsKnown {
			return o
		}
	}
	return properties.Ordering{}
}

func (w *physicalProjectionWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalProjectionWrapper)(nil)

// --- Values wrapper ---------------------------------------------------

type physicalValuesWrapper struct {
	plan *plans.RecordQueryValuesPlan
}

func NewPhysicalValuesWrapper(plan *plans.RecordQueryValuesPlan) *physicalValuesWrapper {
	return &physicalValuesWrapper{plan: plan}
}

func (w *physicalValuesWrapper) GetPlan() *plans.RecordQueryValuesPlan { return w.plan }

func (w *physicalValuesWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalValuesWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalValuesWrapper) GetQuantifiers() []expressions.Quantifier { return nil }

func (w *physicalValuesWrapper) CanCorrelate() bool { return false }

func (w *physicalValuesWrapper) ChildrenAsSet() bool { return false }

func (w *physicalValuesWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalValuesWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalValuesWrapper)
	if !ok {
		return false
	}
	return plans.Equals(w.plan, o.plan)
}

func (w *physicalValuesWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physvalueswrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalValuesWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 0 {
		return nil, fmt.Errorf("physicalValuesWrapper.WithChildren: expected 0 children, got %d", len(qs))
	}
	return w, nil
}

func (w *physicalValuesWrapper) HintCost(_ []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	return properties.Cost{Cardinality: 1, CPU: 0}
}

func (w *physicalValuesWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalValuesWrapper)(nil)

// physicalAggregateIndexWrapper wraps a RecordQueryAggregateIndexPlan
// as a leaf physical expression. Mirrors the aggregate index scan path
// in Java's Cascades planner.
type physicalAggregateIndexWrapper struct {
	plan *plans.RecordQueryAggregateIndexPlan
}

func (w *physicalAggregateIndexWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalAggregateIndexWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalAggregateIndexWrapper) GetQuantifiers() []expressions.Quantifier { return nil }
func (w *physicalAggregateIndexWrapper) CanCorrelate() bool                       { return false }
func (w *physicalAggregateIndexWrapper) ChildrenAsSet() bool                      { return false }

func (w *physicalAggregateIndexWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalAggregateIndexWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalAggregateIndexWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalAggregateIndexWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physaggidxwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalAggregateIndexWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 0 {
		return nil, fmt.Errorf("physicalAggregateIndexWrapper.WithChildren: expected 0 children, got %d", len(qs))
	}
	return w, nil
}

func (w *physicalAggregateIndexWrapper) HintCost(_ []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	tableCard := properties.LeafScanCardinality
	if stats != nil {
		tableCard = stats.RecordTypeCardinality(w.plan.GetRecordTypeName())
	}
	cardinality := tableCard * properties.DistinctSelectivity * physicalWrapperCostMultiplier
	if cardinality < 1 {
		cardinality = 1
	}
	return properties.Cost{
		Cardinality: cardinality,
		CPU:         cardinality * properties.ScanCPU,
	}
}

func (w *physicalAggregateIndexWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

func (w *physicalAggregateIndexWrapper) HintOrdering() properties.Ordering {
	groupCols := w.plan.GetGroupCols()
	if len(groupCols) == 0 {
		return properties.Ordering{IsKnown: true}
	}
	keys := make([]values.Value, len(groupCols))
	desc := make([]bool, len(groupCols))
	for i, col := range groupCols {
		keys[i] = &values.FieldValue{Field: col, Typ: values.UnknownType}
		desc[i] = w.plan.IsReverse()
	}
	return properties.Ordering{IsKnown: true, Keys: keys, Descending: desc}
}

// IsPhysicalAggregateIndex reports whether the expression is an aggregate
// index scan wrapper.
func IsPhysicalAggregateIndex(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalAggregateIndexWrapper)
	return ok
}

var _ physicalPlanExpression = (*physicalAggregateIndexWrapper)(nil)
