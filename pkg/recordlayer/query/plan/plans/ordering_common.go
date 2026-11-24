// Portions derived from FoundationDB Record Layer (
// ValueIndexLikeMatchCandidate.java, OrderingProperty.java,
// AggregateIndexMatchCandidate.java, RecordQueryIndexPlan.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package plans

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// orderingSourceOf returns the reference a single-child plan's ordering flows
// from — its one child quantifier's group.
func orderingSourceOf(p RecordQueryPlan) *expressions.Reference {
	qs := p.GetQuantifiers()
	if len(qs) == 0 {
		return nil
	}
	return qs[0].GetRangesOver()
}

// inheritOrdering returns the first known ordering among a reference's
// members. After exploration the ordering-providing alternative may not be
// the first member, so every member is consulted.
//
// FINAL members first, then exploratory — the same discipline RFC-183 §10
// applied to findPhysicalExpr, for the same reason: Java consults FINAL
// expressions only, and AllMembers() concatenates exploratory members BEFORE
// final ones, so a bare AllMembers() scan answers from a promoted-but-
// dominated alternative when both sets hold a member. The exploratory
// fallback stays because a reference can be consulted before finalization.
//
// On the singleton references a plan's quantifier actually ranges over
// (see this file's header) the two orders coincide, so this is a no-op today
// — it is correct for the day the provenance changes, which is the same day
// the wrapper's version stops being the one that runs.
func inheritOrdering(ref *expressions.Reference) properties.Ordering {
	if ref == nil {
		return properties.Ordering{}
	}
	for _, m := range ref.FinalMembers() {
		if o := properties.EstimateOrdering(m); o.IsKnown {
			return o
		}
	}
	for _, m := range ref.Members() {
		if o := properties.EstimateOrdering(m); o.IsKnown {
			return o
		}
	}
	return properties.Ordering{}
}

// richOrderingOf returns the rich ordering a reference's members provide,
// FINAL members first — see inheritOrdering for why the order matters and why
// the exploratory fallback stays.
func richOrderingOf(ref *expressions.Reference) *properties.RichOrdering {
	if ref == nil {
		return properties.EmptyOrdering()
	}
	for _, m := range ref.FinalMembers() {
		if rh, ok := m.(properties.RichOrderingHinter); ok {
			return rh.HintRichOrdering()
		}
	}
	for _, m := range ref.Members() {
		if rh, ok := m.(properties.RichOrderingHinter); ok {
			return rh.HintRichOrdering()
		}
	}
	return properties.EmptyOrdering()
}

// --- delegators -------------------------------------------------------------

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryFilterPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: a filter preserves its input's order.
func (p *RecordQueryFilterPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryPredicatesFilterPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: a filter preserves its input's order.
func (p *RecordQueryPredicatesFilterPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryTypeFilterPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: a type filter preserves its input's order.
func (p *RecordQueryTypeFilterPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryDistinctPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: duplicate elimination drops repeats without reordering the
// survivors.
func (p *RecordQueryDistinctPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: primary-key duplicate elimination drops rows without
// reordering the survivors.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryMapPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: a map reshapes rows without reordering them.
func (p *RecordQueryMapPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryLimitPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: a limit truncates a stream without reordering it.
func (p *RecordQueryLimitPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryDefaultOnEmptyPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: passing the child through preserves its order.
func (p *RecordQueryDefaultOnEmptyPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryFetchFromPartialRecordPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: fetching the full record per index entry preserves the index
// scan's order.
func (p *RecordQueryFetchFromPartialRecordPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// --- unordered --------------------------------------------------------------

// HintOrdering: an unordered union interleaves its legs arbitrarily.
func (p *RecordQueryUnorderedUnionPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{}
}

// HintOrdering: a K-NN probe returns its neighbours in an order the ordering
// property does not model.
func (p *RecordQueryVectorIndexPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{}
}

// HintOrdering: exploding a collection yields elements in no modeled order.
func (p *RecordQueryExplodePlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// HintOrdering: a dependent join's output order is not modeled.
func (p *RecordQueryFlatMapPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// HintOrdering: a nested-loop join's output order is not modeled.
func (p *RecordQueryNestedLoopJoinPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// HintOrdering: recursive traversal order is not modeled.
func (p *RecordQueryRecursiveDfsJoinPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// HintOrdering: recursive level order is not modeled.
func (p *RecordQueryRecursiveLevelUnionPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// HintOrdering: a table function's output order is opaque.
func (p *RecordQueryTableFunctionPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// HintOrdering: a temp-table insert's output order is not modeled.
func (p *RecordQueryTempTableInsertPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// HintOrdering: a temp-table scan reads an unordered buffer.
func (p *RecordQueryTempTableScanPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// --- rich orderings ---------------------------------------------------------
//
// HintRichOrdering is the binding-carrying form of HintOrdering: it keeps the
// distinction between a key that is EQUALITY-BOUND (FixedBinding — every row
// holds the same value, so the key satisfies a request in EITHER direction)
// and one that is merely SORTED in a given direction. That distinction is what
// lets `WHERE a = 1 ORDER BY a DESC` elide its sort over a forward scan, and
// plain Ordering cannot express it.
//
// Only the plans whose ordering carries bindings implement it. Everything else
// is served by the caller's fallback, which synthesizes a sorted-only binding
// map from HintOrdering.

// HintRichOrdering: an HNSW probe returns its neighbours in distance order,
// which is not a column ordering the planner models. Empty rather than a
// synthesized fallback, so no caller mistakes distance order for key order.
func (p *RecordQueryVectorIndexPlan) HintRichOrdering() *properties.RichOrdering {
	return properties.EmptyOrdering()
}

// HintRichOrdering: a predicate filter passes its input's rich ordering
// through, bindings and all — Java's OrderingProperty.visitPredicatesFilterPlan
// is orderingFromSingleChild. Without it the memo's computeWrapperRichOrdering
// would synthesise sorted-only bindings from the plain HintOrdering, so a
// filter MEMBER's PropRichOrdering — the property the in-union and in-join
// partition roll-ups read — would drop the FIXED bindings of an
// equality-prefixed scan beneath it. Sort elision does not depend on it:
// memberSatisfiesOrdering walks a filter as an orderingDelegator to its
// source's rich form. So it closes a latent divergence in the property (RFC-248)
// rather than changing a plan; the plans-level pin holds it. Same
// source-reference shape as the fetch below.
func (p *RecordQueryPredicatesFilterPlan) HintRichOrdering() *properties.RichOrdering {
	return richOrderingOf(p.OrderingSourceRef())
}

// HintRichOrdering: these drop or pass rows without reordering them, so their
// ordering, bindings and claims are the child's (Java's orderingFromSingleChild
// for the filter, type-filter, distinct and default-on-empty plans; a limit is
// Go's own and truncates the same way).
func (p *RecordQueryFilterPlan) HintRichOrdering() *properties.RichOrdering {
	return richOrderingOf(p.OrderingSourceRef())
}

func (p *RecordQueryTypeFilterPlan) HintRichOrdering() *properties.RichOrdering {
	return richOrderingOf(p.OrderingSourceRef())
}

func (p *RecordQueryLimitPlan) HintRichOrdering() *properties.RichOrdering {
	return richOrderingOf(p.OrderingSourceRef())
}

func (p *RecordQueryDistinctPlan) HintRichOrdering() *properties.RichOrdering {
	return richOrderingOf(p.OrderingSourceRef())
}

func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) HintRichOrdering() *properties.RichOrdering {
	return richOrderingOf(p.OrderingSourceRef())
}

func (p *RecordQueryDefaultOnEmptyPlan) HintRichOrdering() *properties.RichOrdering {
	return richOrderingOf(p.OrderingSourceRef())
}

// HintRichOrdering: Java's OrderingProperty.visitMapPlan pulls the child's
// ordering up through the result value, so the ordering states the map's own
// output columns; a child key the result does not carry ends the ordering.
func (p *RecordQueryMapPlan) HintRichOrdering() *properties.RichOrdering {
	child := richOrderingOf(p.OrderingSourceRef())
	if child == nil || len(child.GetKeys()) == 0 {
		return properties.EmptyOrdering()
	}
	alias := p.innerQ.GetAlias()
	local := map[values.CorrelationIdentifier]struct{}{alias: {}}
	if inner := p.GetInner(); inner != nil {
		if layout, err := inner.ProvidedOutputLayout(); err == nil && layout != nil && layout.Carrier() != nil {
			bound, bindErr := child.PullUpThroughValue(layout.Carrier(), alias, local)
			if bindErr != nil || bound == nil {
				return properties.EmptyOrdering()
			}
			child = bound
		}
	}
	pulled, err := child.PullUpThroughValue(p.resultValue, values.CurrentCorrelation(), local)
	if err != nil || pulled == nil {
		return properties.EmptyOrdering()
	}
	return pulled
}

// HintRichOrdering: fetching the full record per index entry preserves the
// index scan's rich ordering, so inherit it from the source.
//
// This DUPLICATES the physical wrapper's body rather than delegating to it,
// for the reason this file's header gives: same loop, but the wrapper walks a
// shared memo group while the plan walks the fresh singleton its own child
// quantifier ranges over. Two questions, two answers.
func (p *RecordQueryFetchFromPartialRecordPlan) HintRichOrdering() *properties.RichOrdering {
	return richOrderingOf(p.OrderingSourceRef())
}
