package plans

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryMultiIntersectionOnValuesPlan merges N input streams where
// all streams are ordered by the same comparison key (grouping columns).
// For each group of rows where the comparison key matches across ALL
// streams, it produces one output row combining:
//   - Common values (grouping columns) — taken from any stream (they're identical)
//   - Pick-up values (aggregates) — one from each stream
//
// Mirrors Java's RecordQueryMultiIntersectionOnValuesPlan which extends
// RecordQueryIntersectionPlan and adds a resultValue that constructs the
// merged output row from quantifier bindings.
//
// The children are stored ONCE, as Quantifiers over References — Java's shape
// (`RecordQuerySetPlan`'s `List<Quantifier.Physical> quantifiers`). The raw
// `children []RecordQueryPlan` slice they replace was a second storage
// location for the same edges. RFC-183 P5 step 2.
type RecordQueryMultiIntersectionOnValuesPlan struct {
	PlanExprBase
	childQs       []expressions.Quantifier // N input plans (one per aggregate index)
	comparisonKey []values.Value           // grouping columns to match on
	resultValue   values.Value             // result constructor (grouping + aggregates)

	// drivingAlias designates ONE stream as the group-existence source and
	// switches the merge from inner to OUTER (RFC-209 §5.3(b)). The zero value
	// means the plain inner intersection.
	//
	// Under outer semantics the driving stream decides the group set:
	//   - group in the driving stream, entry in another stream → its value;
	//   - group in the driving stream, NO entry in another stream → that stream
	//     contributes NULL, which the result value turns into the aggregate's
	//     empty-group identity (NULL for SUM, 0 for COUNT(col));
	//   - entry in another stream, group NOT in the driving stream → dropped.
	//
	// The first two repair the all-NULL group a SUM or COUNT(col) index never
	// wrote an entry for; the third repairs the phantom a vacated group left
	// behind. Inner semantics get the third right and the second wrong, which is
	// why this is not the existing operator with a filter bolted on top.
	//
	// Stored as an ALIAS, never as an index into childQs: WithQuantifiers
	// relinks the streams, and an index would silently designate a different
	// stream after a reorder — turning a correct plan into a wrong-rows one with
	// no structural change to notice.
	drivingAlias values.CorrelationIdentifier
}

// WithDrivingStream returns a copy of this plan whose merge is OUTER, driven by
// the stream carried by the given quantifier alias (RFC-209 §5.3(b)).
//
// The alias must belong to one of this plan's streams. A plan whose driving
// alias resolves to nothing is not executable as an outer merge — the executor
// refuses it rather than silently running an intersection, which is the
// fail-closed property §5.3 asks for.
func (p *RecordQueryMultiIntersectionOnValuesPlan) WithDrivingStream(
	alias values.CorrelationIdentifier,
) *RecordQueryMultiIntersectionOnValuesPlan {
	cp := *p
	cp.drivingAlias = alias
	return &cp
}

// GetDrivingAlias returns the group-existence stream's alias, or the zero value
// when this is a plain inner intersection.
func (p *RecordQueryMultiIntersectionOnValuesPlan) GetDrivingAlias() values.CorrelationIdentifier {
	return p.drivingAlias
}

// DrivingStreamIndex resolves the driving alias to a position in stream order,
// or -1 when this plan is an inner intersection.
//
// A non-zero alias that names no stream returns -1 too, and every caller treats
// -1 as "not an outer merge". That is deliberate: an unresolvable designation
// must not silently degrade to "drive from stream 0".
func (p *RecordQueryMultiIntersectionOnValuesPlan) DrivingStreamIndex() int {
	// Nil-safe: the cardinality and cost taxonomies probe plan methods on a nil
	// receiver to enumerate which shapes are covered, and library code must
	// answer rather than panic.
	if p == nil || p.drivingAlias.IsZero() {
		return -1
	}
	for i, q := range p.childQs {
		if q.GetAlias() == p.drivingAlias {
			return i
		}
	}
	return -1
}

// HasDrivingAlias reports whether a driving stream was DESIGNATED, regardless of
// whether it still resolves. The executor uses this to tell "inner intersection"
// (nothing designated) from "outer merge whose designation was lost" (designated
// but unresolvable) — the latter must fail loudly, never run as an intersection.
func (p *RecordQueryMultiIntersectionOnValuesPlan) HasDrivingAlias() bool {
	return p != nil && !p.drivingAlias.IsZero()
}

// IsOuter reports whether this merge has a resolvable driving stream.
func (p *RecordQueryMultiIntersectionOnValuesPlan) IsOuter() bool {
	return p != nil && p.DrivingStreamIndex() >= 0
}

// NewRecordQueryMultiIntersectionOnValuesPlan constructs an N-way
// multi-intersection. comparisonKey defines the row-equality key
// (grouping columns); resultValue is the Value expression that
// constructs the output row from quantifier bindings.
func NewRecordQueryMultiIntersectionOnValuesPlan(
	children []RecordQueryPlan,
	comparisonKey []values.Value,
	resultValue values.Value,
) (*RecordQueryMultiIntersectionOnValuesPlan, error) {
	return NewRecordQueryMultiIntersectionOnValuesPlanFromQuantifiers(
		QuantifiersOverPlans(children), comparisonKey, resultValue)
}

// NewRecordQueryMultiIntersectionOnValuesPlanFromQuantifiers builds an N-way
// multi-intersection whose streams are LIVE memo quantifiers (the aggregate
// data-access rule passes PhysicalQuantifiers over the freshly-memoized leg
// plans) instead of snapshots over plans. This makes the multi-intersection its
// own cascades expression carrying its stream edges directly — the memo holds it
// without a physical wrapper (RFC-184 W2). comparisonKey and resultValue carry
// over verbatim.
func NewRecordQueryMultiIntersectionOnValuesPlanFromQuantifiers(
	qs []expressions.Quantifier,
	comparisonKey []values.Value,
	resultValue values.Value,
) (*RecordQueryMultiIntersectionOnValuesPlan, error) {
	base, err := newPlanExprBaseForValue("RecordQueryMultiIntersectionOnValuesPlan", resultValue)
	if err != nil {
		return nil, err
	}
	cpKeys := make([]values.Value, len(comparisonKey))
	copy(cpKeys, comparisonKey)
	return &RecordQueryMultiIntersectionOnValuesPlan{
		PlanExprBase:  base,
		childQs:       append([]expressions.Quantifier(nil), qs...),
		comparisonKey: cpKeys,
		resultValue:   resultValue,
	}, nil
}

// GetChildren returns the input plans, dereferenced through the quantifiers
// and in stream order — resultValue's pick-up columns are positional per
// stream.
func (p *RecordQueryMultiIntersectionOnValuesPlan) GetChildren() []RecordQueryPlan {
	return plansFromQuantifiers(p.childQs)
}

// GetComparisonKey returns the grouping-column values used to match
// rows across all input streams.
func (p *RecordQueryMultiIntersectionOnValuesPlan) GetComparisonKey() []values.Value {
	return p.comparisonKey
}

// GetResultValue returns the Value expression that constructs the merged
// output row, falling back to the FIRST stream's flowed object value when this
// plan carries none.
//
// The fallback is physicalMultiIntersectionWrapper's, adopted here now that
// the plan owns its child quantifiers: the wrapper answered
// innerQuants[0].GetFlowedObjectValue() because the plan had no quantifiers of
// its own to ask. It does now, so the wrapper holds no information the plan
// lacks and becomes deletable.
//
// The final arm defers to PlanExprBase rather than repeating its fresh
// stand-in, so a 0-stream plan answers exactly what every other plan answers.
func (p *RecordQueryMultiIntersectionOnValuesPlan) GetResultValue() values.Value {
	return p.resultValue
}

// GetResultType returns the result Value's type if a resultValue is
// set, or UnknownType otherwise.
func (p *RecordQueryMultiIntersectionOnValuesPlan) GetResultType() values.Type {
	return p.resultValue.Type()
}

// structuralKey lists the fields that distinguish this multi-intersection in
// the memo: the comparison key values and the resultValue, both by semantic
// Value identity (RFC-176 P2 — see semanticValueEquals). Children are excluded.
// The same key drives both EqualsPlanWithoutChildren and
// HashCodeWithoutChildren.
func (p *RecordQueryMultiIntersectionOnValuesPlan) structuralKey() *structuralKey {
	// The driving alias MUST be folded in. Without it the memo interns the OUTER
	// merge and the inner intersection over the same key and result value into a
	// single expression and serves whichever arrived first — and the two differ
	// on exactly the groups this operator exists to get right.
	return newStructuralKey().Values(p.comparisonKey).Value(p.resultValue).
		Str(p.drivingAlias.Name())
}

func (p *RecordQueryMultiIntersectionOnValuesPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryMultiIntersectionOnValuesPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// HashCodeWithoutChildren folds the type discriminator, comparison key
// values, and result value (semantic Value hashes — see writeValueHash).
func (p *RecordQueryMultiIntersectionOnValuesPlan) HashCodeWithoutChildren() uint64 {
	if hash, ok := p.cachedStructuralHash(p); ok {
		return hash
	}
	hash := p.structuralKey().Hash("multiintersectiononvaluesplan|")
	p.storeStructuralHash(p, hash)
	return hash
}

// Explain renders MultiIntersection(child1, child2, ...; keys=[...]).
func (p *RecordQueryMultiIntersectionOnValuesPlan) Explain() string {
	children := p.GetChildren()
	parts := make([]string, len(children))
	for i, child := range children {
		if child == nil {
			parts[i] = "<nil>"
		} else {
			parts[i] = child.Explain()
		}
	}
	keys := values.ExplainPlanValues(p.comparisonKey)
	if idx := p.DrivingStreamIndex(); idx >= 0 {
		// Plan-visible because it changes the answer: a reader of EXPLAIN must be
		// able to tell the group-existence merge from an intersection, and which
		// stream supplies the group set.
		return fmt.Sprintf("GroupExistenceMerge(%s; keys=[%s], driving=%d)",
			strings.Join(parts, ", "), strings.Join(keys, ", "), idx)
	}
	return fmt.Sprintf("MultiIntersection(%s; keys=[%s])",
		strings.Join(parts, ", "), strings.Join(keys, ", "))
}

var (
	_ RecordQueryPlan                  = (*RecordQueryMultiIntersectionOnValuesPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryMultiIntersectionOnValuesPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryMultiIntersectionOnValuesPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// GetQuantifiers reports the real child quantifiers, overriding PlanExprBase's
// none. These are also what GetResultValue's nil-fallback reads.
func (p *RecordQueryMultiIntersectionOnValuesPlan) GetQuantifiers() []expressions.Quantifier {
	if len(p.childQs) == 0 {
		return nil
	}
	return p.childQs
}

// WithQuantifiers atomically rebuilds the merge over the replacement child
// edges. Comparison keys and the result constructor may retain any of those
// edge aliases, so all positional old→new pairs participate in one checked
// rebase before PlanExprBase is reconstructed.
//
// The arity check matters more here than for a plain set operation:
// resultValue picks up one aggregate per stream by position, so a
// different-length child list would not describe the same row.
func (p *RecordQueryMultiIntersectionOnValuesPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryMultiIntersectionOnValuesPlan", len(qs), len(p.childQs)); err != nil {
		return nil, err
	}
	pairs := make([]values.AliasPair, 0, len(qs))
	for i := range qs {
		oldInput, err := p.childQs[i].RequireFlowedObjectValue()
		if err != nil {
			return nil, fmt.Errorf("RecordQueryMultiIntersectionOnValuesPlan.WithQuantifiers old child %d: %w", i, err)
		}
		newInput, err := qs[i].RequireFlowedObjectValue()
		if err != nil {
			return nil, fmt.Errorf("RecordQueryMultiIntersectionOnValuesPlan.WithQuantifiers new child %d: %w", i, err)
		}
		if !oldInput.FlowedType().Equals(newInput.FlowedType()) {
			return nil, fmt.Errorf(
				"RecordQueryMultiIntersectionOnValuesPlan.WithQuantifiers child %d type changed from %s to %s",
				i, oldInput.FlowedType(), newInput.FlowedType())
		}
		if oldInput.Correlation() != newInput.Correlation() {
			pairs = append(pairs, values.AliasPair{
				Source: oldInput.Correlation(), Target: newInput.Correlation(),
			})
		}
	}
	aliasMap, err := values.NewAliasMap(pairs)
	if err != nil {
		return nil, fmt.Errorf("RecordQueryMultiIntersectionOnValuesPlan.WithQuantifiers alias map: %w", err)
	}
	rebasedKeys := make([]values.Value, len(p.comparisonKey))
	for i, key := range p.comparisonKey {
		rebasedKeys[i], err = values.RebaseValueChecked(key, aliasMap)
		if err != nil {
			return nil, fmt.Errorf("RecordQueryMultiIntersectionOnValuesPlan.WithQuantifiers comparison key %d: %w", i, err)
		}
	}
	rebasedResult, err := values.RebaseValueChecked(p.resultValue, aliasMap)
	if err != nil {
		return nil, fmt.Errorf("RecordQueryMultiIntersectionOnValuesPlan.WithQuantifiers result Value: %w", err)
	}
	rebuilt, err := NewRecordQueryMultiIntersectionOnValuesPlanFromQuantifiers(qs, rebasedKeys, rebasedResult)
	if err != nil {
		return nil, err
	}
	rebuilt.drivingAlias = p.drivingAlias
	// Carry the group-existence designation across the relink. WithQuantifiers
	// replaces the streams with fresh quantifiers over the finalized references,
	// and those carry NEW aliases — so an alias stored verbatim stops resolving
	// the moment the plan is finalized, and the outer merge silently degrades to
	// an intersection that drops every all-NULL group.
	//
	// The arity check above is what makes the positional carry-over sound: the
	// relink preserves stream order, so the driving stream is still at the same
	// position. The designation is re-derived from the OLD quantifiers and
	// re-expressed as the NEW stream's alias, so it stays an alias — a bare
	// index would still be wrong under any future reordering relink.
	if driving := p.DrivingStreamIndex(); driving >= 0 {
		rebuilt.drivingAlias = qs[driving].GetAlias()
	}
	return rebuilt, nil
}

// IsIntersection implements properties.IntersectionExpression — the marker
// ComparisonsProperty.EvaluateComparisons keys on to intersect (not union) its
// children's comparison sets. Adopted from the retired
// physicalMultiIntersectionWrapper (RFC-184 W2) so the memo member the property
// walks still reports it.
func (p *RecordQueryMultiIntersectionOnValuesPlan) IsIntersection() {}

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). The multi-intersection carries its streams as LIVE memo edges, so
// the relink rebuilds the streams and every retained Value program before
// GetChildren re-resolves through the new references (RFC-184 W2, replacing
// physicalMultiIntersectionWrapper.WithChildren).
func (p *RecordQueryMultiIntersectionOnValuesPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	return p.WithQuantifiers(qs)
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryMultiIntersectionOnValuesPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
