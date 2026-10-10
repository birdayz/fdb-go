// Portions derived from FoundationDB Record Layer (
// RecordQueryMultiIntersectionOnValuesPlan.java,
// RecordQueryIntersectionPlan.java, RecordQuerySetPlan.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2020 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

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

	// reverse merges streams that run in descending key order (every child
	// scans in reverse), as Java's plan carries it.
	reverse bool
}

// WithReverse returns a copy of this plan merging descending streams.
func (p *RecordQueryMultiIntersectionOnValuesPlan) WithReverse(reverse bool) *RecordQueryMultiIntersectionOnValuesPlan {
	cp := *p
	cp.reverse = reverse
	return &cp
}

// IsReverse reports whether the merge compares descending keys.
func (p *RecordQueryMultiIntersectionOnValuesPlan) IsReverse() bool { return p != nil && p.reverse }

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
	return newStructuralKey().Values(p.comparisonKey).Value(p.resultValue).Bool(p.reverse)
}

func (p *RecordQueryMultiIntersectionOnValuesPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryMultiIntersectionOnValuesPlan)
	return ok && p.keyFor(p).Equal(o.keyFor(o))
}

// HashCodeWithoutChildren folds the type discriminator, comparison key
// values, and result value (semantic Value hashes — see writeValueHash).
func (p *RecordQueryMultiIntersectionOnValuesPlan) HashCodeWithoutChildren() uint64 {
	if hash, ok := p.cachedStructuralHash(p); ok {
		return hash
	}
	hash := p.keyFor(p).Hash("multiintersectiononvaluesplan|")
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
	direction := ""
	if p.reverse {
		direction = ", reverse"
	}
	return fmt.Sprintf("MultiIntersection(%s; keys=[%s]%s)",
		strings.Join(parts, ", "), strings.Join(keys, ", "), direction)
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
		if !values.FlowedTypesEqual(oldInput, newInput) {
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
	rebuilt.reverse = p.reverse
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
