package plans

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryStreamingAggregationPlan groups input rows by grouping
// keys and computes aggregates over each group in a streaming fashion.
// The plan requires that the inner plan produces rows already sorted
// by the grouping keys — no materialisation needed.
//
// Mirrors Java's RecordQueryStreamingAggregationPlan: the streaming
// operator reads sorted input and emits one output row per change in
// the grouping-key combination. When the inner is NOT ordered by
// grouping keys, ImplementStreamingAggregationRule does not fire —
// a sort is needed first, or the hash-aggregate path (future) is
// used instead.
type RecordQueryStreamingAggregationPlan struct {
	PlanExprBase
	innerQ       expressions.Quantifier
	groupingKeys []values.Value
	aggregates   []expressions.AggregateSpec
}

func NewRecordQueryStreamingAggregationPlan(
	inner RecordQueryPlan,
	groupingKeys []values.Value,
	aggregates []expressions.AggregateSpec,
) *RecordQueryStreamingAggregationPlan {
	return &RecordQueryStreamingAggregationPlan{
		innerQ:       QuantifierOverPlan(inner),
		groupingKeys: groupingKeys,
		aggregates:   aggregates,
	}
}

// NewRecordQueryStreamingAggregationPlanFromQuantifier builds a streaming
// aggregation whose child is a supplied memo quantifier instead of a snapshot
// over a single plan. This makes the plan its own cascades expression carrying
// its child edge directly — the memo holds it without a physicalStreamingAggWrapper
// (RFC-184 W2).
//
// Streaming aggregation is a PRODUCER, not an ordering-delegator: it reshapes
// rows (one output row per grouping-key change) and provides its OWN output
// ordering. But it has a CORRECTNESS PRECONDITION — the inner must be ordered by
// the grouping keys — so the emitter chooses the child edge per arm: a plain
// count-only aggregation (no grouping keys) or a self-contained ordered producer
// (an InMemorySort it builds, a covering index scan) carries the LIVE
// shared-group edge, while a DELEGATING ordered inner (an existing Fetch/Filter
// spine) is frozen deep by pinOrderedSpine + FinalOf so it cannot float to an
// unordered sibling and split groups. The grouping keys and aggregate specs are
// preserved so OutputRecordType / GetResultValue stay stable.
func NewRecordQueryStreamingAggregationPlanFromQuantifier(
	innerQ expressions.Quantifier,
	groupingKeys []values.Value,
	aggregates []expressions.AggregateSpec,
) *RecordQueryStreamingAggregationPlan {
	return &RecordQueryStreamingAggregationPlan{
		innerQ:       innerQ,
		groupingKeys: groupingKeys,
		aggregates:   aggregates,
	}
}

func (p *RecordQueryStreamingAggregationPlan) GetInner() RecordQueryPlan {
	return planFromQuantifier(p.innerQ)
}

// GetInnerQuantifier returns the live child quantifier — the single memo edge the
// aggregation ranges over. Since RFC-184 W2 the memo holds the bare plan (no
// physicalStreamingAggWrapper whose innerQuant field was read), this exposes the
// same edge for derivations and extraction.
func (p *RecordQueryStreamingAggregationPlan) GetInnerQuantifier() expressions.Quantifier {
	return p.innerQ
}

// GetResultValue flows a TYPED QOV whose RecordType is the aggregate's output
// schema ([groupKeys, aggregates], the plan's single naming authority), so the
// resolver BAKES downstream references to ordinals at plan time (Java's
// getFieldNameToOrdinalMap). A downstream ref then reads the aggregateCursor's
// PositionalRow by Get(ordinal) — order, not spelling — robust to redundant
// spellings of the same column. A streaming aggregation is a PRODUCER: it does
// NOT flow its inner's rows through, so this must NOT delegate to the child's
// flowed value (unlike the filter/distinct passthroughs). This is the identity
// physicalStreamingAggWrapper.GetResultValue supplied (RFC-184 W2).
func (p *RecordQueryStreamingAggregationPlan) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValueOfType(values.UniqueCorrelationIdentifier(), p.OutputRecordType())
}

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryStreamingAggregationPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

func (p *RecordQueryStreamingAggregationPlan) GetGroupingKeys() []values.Value { return p.groupingKeys }
func (p *RecordQueryStreamingAggregationPlan) GetAggregates() []expressions.AggregateSpec {
	return p.aggregates
}

func (p *RecordQueryStreamingAggregationPlan) GetResultType() values.Type {
	return values.UnknownType
}

// OutputColumnNames is the SINGLE naming authority for this plan's output row:
// grouping keys (in GROUP BY order) then aggregates (in aggregate order), each
// alias-preferring. The ordinal model bakes downstream references against
// this order, and the executor's aggregateCursor emits its PositionalRow with these
// exact names — so a reference over the aggregate resolves by Get(ordinal) (Java's
// getFieldValueForFieldOrdinals) instead of a spelling-sensitive name lookup.
func (p *RecordQueryStreamingAggregationPlan) OutputColumnNames() []string {
	return expressions.GroupByOutputColumnNames(p.groupingKeys, p.aggregates)
}

// OutputRecordType is OutputColumnNames as a RAW RecordType (ordinal == slice
// position; dup-name-safe). Flowed as the aggregate's result-value QOV type so the
// resolver BAKES downstream references to ordinals at plan time.
func (p *RecordQueryStreamingAggregationPlan) OutputRecordType() *values.RecordType {
	names := p.OutputColumnNames()
	fields := make([]values.Field, len(names))
	for i, n := range names {
		fields[i] = values.Field{Name: n, FieldType: values.UnknownType, Ordinal: i}
	}
	return &values.RecordType{Fields: fields}
}

func (p *RecordQueryStreamingAggregationPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// structuralKey folds the grouping keys (by value) then per aggregate the
// Function discriminator + Operand (by value), pairing equality and hashing so
// they can never disagree on which fields matter.
func (p *RecordQueryStreamingAggregationPlan) structuralKey() *structuralKey {
	k := newStructuralKey().Values(p.groupingKeys)
	for _, a := range p.aggregates {
		k.Int(int(a.Function)).Value(a.Operand)
	}
	return k
}

func (p *RecordQueryStreamingAggregationPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryStreamingAggregationPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryStreamingAggregationPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("streamagg|")
}

func (p *RecordQueryStreamingAggregationPlan) Explain() string {
	keys := make([]string, len(p.groupingKeys))
	for i, k := range p.groupingKeys {
		keys[i] = values.ExplainValue(k)
	}
	innerLabel := "<nil>"
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	return fmt.Sprintf("StreamingAgg(keys=[%s], %s)", strings.Join(keys, ", "), innerLabel)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryStreamingAggregationPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryStreamingAggregationPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryStreamingAggregationPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryStreamingAggregationPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 1 {
		return p
	}
	cp := *p
	cp.innerQ = qs[0]
	return &cp
}

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). The aggregation carries its child as a single memo edge, so the
// relink is a quantifier swap: WithQuantifiers copies the receiver (preserving
// the grouping keys and aggregate specs) and re-resolves GetInner through the
// new singleton reference. This replaces physicalStreamingAggWrapper.WithChildren
// (RFC-184 W2), whose separate snapshot plan field forced a constructor rebuild
// gated on isLeafReplaceable. Streaming aggregation is a PRODUCER, not on the
// ordering-delegation spine, so the emitter has already frozen (or kept live)
// the ordering-correct inner per arm — extraction recurses through that edge
// faithfully.
func (p *RecordQueryStreamingAggregationPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("RecordQueryStreamingAggregationPlan.WithChildren: expected 1 child, got %d", len(qs))
	}
	return p.WithQuantifiers(qs), nil
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryStreamingAggregationPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
