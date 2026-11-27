// Portions derived from FoundationDB Record Layer (UnionCursor.java,
// MergeCursorState.java, UnorderedUnionCursor.java,
// UnionCursorContinuation.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"context"
	"fmt"
	"math"

	"fdb.dev/internal/recordlayer/query/plan/plans"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

func executeInJoin(
	ctx context.Context,
	p *plans.RecordQueryInJoinPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inValues := p.GetInValues()
	if comparand := p.GetInComparand(); inValues == nil && comparand != nil {
		v, err := comparand.Evaluate(evalCtx)
		if err != nil {
			return nil, err
		}
		list, err := inComparandList(v)
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			return recordlayer.Empty[QueryResult](), nil
		}
		inValues = list
	}
	if len(inValues) == 0 {
		return ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props)
	}

	// Java RecordQueryInJoinPlan.executePlan (:112-131):
	//   RecordCursor.flatMapPipelined(
	//       outerCont -> RecordCursor.fromList(getValues(context), outerCont),
	//       (outerValue, innerCont) -> inner.executePlan(context.withBinding(...),
	//                                       innerCont, props.clearSkipAndLimit()),
	//       checkFunc, continuation, pipelineSize)
	//     .skipThenLimit(skip, limit)
	// The continuation is the flatMap machinery's (outer list index, inner
	// continuation, check value) triple, so a resume lands on the SAME in-value
	// (validated by the check bytes) and continues its inner mid-stream. The
	// pre-A5 eager concat discarded the incoming continuation entirely — every
	// resumed page replayed the whole IN-join from value 0.
	bindingID := p.GetBindingAlias()
	outerFactory := func(cont []byte) recordlayer.RecordCursor[any] {
		return recordlayer.FromListWithContinuation(inValues, cont)
	}
	innerFactory := func(val any, innerCont []byte) recordlayer.RecordCursor[QueryResult] {
		boundCtx := evalCtx.WithBinding(bindingID, val)
		cursor, err := ExecutePlan(ctx, p.GetInner(), store, boundCtx, innerCont, props.ClearSkipAndLimit())
		if err != nil {
			return &errResultCursor{err: err}
		}
		return cursor
	}
	cursor := recordlayer.FlatMapPipelinedWithCheck(outerFactory, innerFactory, inValueCheckBytes, continuation, 1)
	return applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit), nil
}

// inComparandList is a runtime IN comparand's evaluated list. Java's
// InComparandSource.getValues casts the comparand to a List unchecked
// (InComparandSource.java:103-108) and its callers size and iterate it, so a
// NULL or non-list comparand fails there; only an actual (possibly empty)
// list is an IN source.
func inComparandList(v any) ([]any, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, &recordlayer.RecordCoreError{
			Message: fmt.Sprintf("IN comparand must evaluate to a list, got %T", v),
		}
	}
	return list, nil
}

// inValueCheckBytes is the IN-join flatMap check value — a deterministic byte
// form of the current in-value validating on resume that the outer list
// element at the saved index is still the same value. Java packs
// Tuple.from(ScanComparisons.toTupleItem(outerValue)) (RecordQueryInJoinPlan
// :123-129, DynamicMessage → toByteArray). Go in-values are plan-time-evaluated
// scalars; the tuple packer covers all of them after the UUID/int32 wire
// canonicalizations, gated by an explicit whitelist (the packer panics on
// anything else — RFC-134 forbids a recover boundary here). A value outside
// the whitelist yields NO check bytes (nil) — the resume then skips the
// outer-identity check, exactly the checker==null degradation Java's Cascades
// path runs with everywhere.
func inValueCheckBytes(val any) []byte {
	v := uuidToTupleElement(widenInt32(val))
	switch v.(type) {
	case nil, int, int64, uint, uint64, float32, float64, bool, string, []byte, tuple.UUID:
		return tuple.Tuple{v}.Pack()
	}
	return nil
}

// inUnionValuesSize is the number of child executions an IN-union makes: the
// product of its sources' sizes, saturating at MaxInt64. known is false when
// a source's values were unavailable at planning (the caller refuses that
// source on its own).
func inUnionValuesSize(sources [][]any) (size int64, known bool) {
	size = 1
	for _, source := range sources {
		if source == nil {
			return 0, false
		}
		n := int64(len(source))
		switch {
		case n == 0:
			return 0, true
		case size > math.MaxInt64/n:
			size = math.MaxInt64
		default:
			size *= n
		}
	}
	return size, true
}

func executeInUnion(
	ctx context.Context,
	p *plans.RecordQueryInUnionPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inSources := p.GetInSources()
	bindingAliases := p.GetBindingAliases()
	if len(inSources) == 0 {
		return ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props)
	}
	var err error
	inSources, err = resolveInUnionSources(p, evalCtx, inSources)
	if err != nil {
		return nil, err
	}
	if len(bindingAliases) == 0 || len(inSources) != len(bindingAliases) {
		return nil, fmt.Errorf(
			"executeInUnion: binding/source dimension mismatch (%d bindings, %d sources)",
			len(bindingAliases),
			len(inSources),
		)
	}
	// Java checks the number of child executions FIRST, before any leg opens
	// (RecordQueryInUnionPlan.java:151-153): the PRODUCT of the sources'
	// sizes against the plan's maximum (the planner configuration's
	// attemptFailedInJoinAsUnionMaxSize, 24 in the relational layer), a
	// RecordCoreException "too many IN values" above it. Java's product is an
	// int that wraps (65536 x 65536 is 0, answered as empty); Go's saturates
	// and refuses (declared in DIVERGENCES.md).
	if size, known := inUnionValuesSize(inSources); known && size > int64(p.GetMaxSize()) {
		// The size is Java's log info, not part of its message.
		return nil, &recordlayer.RecordCoreError{Message: "too many IN values"}
	}
	for _, source := range inSources {
		if source != nil && len(source) == 0 {
			// The Cartesian product of the binding dimensions is empty, so the
			// inner must not execute. A nil source is different: its values were
			// unavailable at planning time and remain an unsupported runtime
			// binding, not a known-empty list.
			return recordlayer.Empty[QueryResult](), nil
		}
	}
	if fanout, known := inUnionValuesSize(inSources); known && fanout == 1 {
		// Java binds the complete Cartesian context before its size==1
		// fast path. This matters for multiple singleton dimensions: there is
		// one child execution, but every binding must be present.
		childContext := evalCtx
		for i, source := range inSources {
			childContext = childContext.WithBinding(
				bindingAliases[i],
				source[0],
			)
		}
		return ExecutePlan(ctx, p.GetInner(), store, childContext, continuation, props)
	}

	// One child execution per combination of IN values. Mirrors Java
	// RecordQueryInUnionPlan.executePlan (:150-175) over getValuesContexts
	// (:340-353): the Cartesian product of the sources, the first source most
	// significant; size==1 hands the continuation straight to the sole child;
	// otherwise the children are CURSOR FACTORIES and the continuation is the
	// UnionCursor's per-child UnionContinuation.
	combinations := [][]any{nil}
	for i, source := range inSources {
		if source == nil {
			return nil, fmt.Errorf("executeInUnion: IN source %d has no planning-time values", i)
		}
		next := make([][]any, 0, len(combinations)*len(source))
		for _, parent := range combinations {
			for _, val := range source {
				next = append(next, append(append(make([]any, 0, len(inSources)), parent...), val))
			}
		}
		combinations = next
	}
	// childFactory tags each combination's execution context with its own
	// index (withRecursionInvocationBranch): when compKeys is non-empty
	// below, newMergeSortCursorFromFactories constructs and drives ALL
	// factories CONCURRENTLY (every leg pulled from in lockstep, not one
	// after another), so if p.GetInner() is itself a recursive-union plan,
	// its statement-scoped depth guard (recordlayer.ExecuteState.recursionLevels)
	// would otherwise key every one of these concurrently-live invocations by
	// the SAME plan pointer — see recursionInvocationKey's doc comment for why
	// that under- and over-counts. The index-tagged ctx gives each
	// combination's invocation of the same inner plan a distinct, resume-
	// stable identity.
	childFactory := func(idx int, combination []any) recordlayer.CursorFactory[QueryResult] {
		return func(cont []byte) recordlayer.RecordCursor[QueryResult] {
			boundCtx := evalCtx
			for i, val := range combination {
				boundCtx = boundCtx.WithBinding(bindingAliases[i], val)
			}
			childCtx := withRecursionInvocationBranch(ctx, idx)
			cursor, err := ExecutePlan(childCtx, p.GetInner(), store, boundCtx, cont, props.ClearSkipAndAdjustLimit())
			if err != nil {
				return &errResultCursor{err: err}
			}
			return cursor
		}
	}
	factories := make([]recordlayer.CursorFactory[QueryResult], len(combinations))
	for i, combination := range combinations {
		factories[i] = childFactory(i, combination)
	}
	if compKeys := p.GetComparisonKeys(); len(compKeys) > 0 {
		merged, err := newMergeSortCursorFromFactories(factories, compKeys, p.IsReverse(), true, continuation)
		if err != nil {
			return nil, err
		}
		return applySkipLimit(merged, props.Skip, props.ReturnedRowLimit), nil
	}
	// No comparison keys (order-free IN union): a resumable branch-tagged
	// concat chain (concatFactories, shared with executeUnion)
	// instead of the pre-A5 eager concat that discarded the continuation.
	return applySkipLimit(concatFactories(factories, continuation), props.Skip, props.ReturnedRowLimit), nil
}

func resolveInUnionSources(
	p *plans.RecordQueryInUnionPlan,
	evalCtx *EvaluationContext,
	inSources [][]any,
) ([][]any, error) {
	// A source planning could not evaluate is evaluated now, under the
	// plan's evaluation context: Java's InComparandSource.getValues (the
	// comparand is the planner's ArrayDistinctValue, so it deduplicates). A
	// resume re-evaluates it, as Java's does.
	comparands := p.GetInComparands()
	if len(comparands) == 0 {
		return inSources, nil
	}
	evaluated := make([][]any, len(inSources))
	copy(evaluated, inSources)
	for i, c := range comparands {
		if c == nil || i >= len(evaluated) || evaluated[i] != nil {
			continue
		}
		v, err := c.Evaluate(evalCtx)
		if err != nil {
			return nil, err
		}
		list, err := inComparandList(v)
		if err != nil {
			return nil, err
		}
		if list == nil {
			list = []any{}
		}
		evaluated[i] = list
	}
	return evaluated, nil
}
