// Portions derived from FoundationDB Record Layer (MessageHelpers.java,
// QueryResult.java, IndexOrphanBehavior.java, RecordCursor.java,
// UnionCursor.java, MergeCursorState.java, UnorderedUnionCursor.java,
// UnionCursorContinuation.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"context"
	"fmt"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func executeTypeFilter(
	ctx context.Context,
	p *plans.RecordQueryTypeFilterPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inner := p.GetInner()
	var innerCursor recordlayer.RecordCursor[QueryResult]
	var err error
	if scanPlan, directScan := inner.(*plans.RecordQueryScanPlan); directScan {
		// A TypeFilter over a primary scan is the physical boundary that makes a
		// single logical record type safe on an intermingled store. A PK range on
		// such a store can legitimately encounter another record descriptor before
		// this predicate discards it. Attaching the scan's single-type layout first
		// would reject that foreign row before the load-bearing TypeFilter can run.
		// Consume only this direct scan's raw cursor; the enclosing ExecutePlan call
		// publishes and validates the TypeFilter's exact layout after filtering.
		// Other child operators retain their ordinary boundary attachment.
		//
		// The rows come UNSTAMPED for the same reason. A stamp is that same
		// single-type assertion made at mint time, so leaving it on while skipping
		// the boundary would let a foreign row carry a layout that lies about it
		// through the one stage whose boundary is deliberately absent.
		innerCursor, err = executeScanUnstamped(
			ctx, scanPlan, store, evalCtx, continuation, props.ClearSkipAndLimit())
	} else {
		innerCursor, err = ExecutePlan(
			ctx, inner, store, evalCtx, continuation, props.ClearSkipAndLimit())
	}
	if err != nil {
		return nil, err
	}
	// Key the set on the RESOLVED name. The predicate below compares against
	// FDBStoredRecord.RecordType.Name, which is the STORED spelling, while
	// GetRecordTypes() is only as normalised as whoever built the plan made it:
	// RecordQueryScanPlan carries the SQL table name straight from the
	// translator, so the same accessor means different namespaces on two plan
	// types. A raw comparison then filters out every row and returns zero,
	// which is the silent shape -- no error, an empty result that reads as "no
	// records of this type". Resolving here is the same one-line rule
	// ScanRecordsByType applies, and it leaves an unresolvable name matching
	// nothing, exactly as before.
	allowed := make(map[string]bool, len(p.GetRecordTypes()))
	md := store.GetRecordMetaData()
	for _, rt := range p.GetRecordTypes() {
		if md != nil {
			if resolved := md.GetRecordType(rt); resolved != nil {
				allowed[resolved.Name] = true
				continue
			}
		}
		allowed[rt] = true
	}

	filtered := &filterResultCursor{
		inner: innerCursor,
		pred: func(qr QueryResult) (bool, error) {
			if qr.Record == nil || qr.Record.RecordType == nil {
				return false, nil
			}
			return allowed[qr.Record.RecordType.Name], nil
		},
	}
	return applySkipLimit(filtered, props.Skip, props.ReturnedRowLimit), nil
}

func executeFilter(
	ctx context.Context,
	p *plans.RecordQueryFilterPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}
	inputQOV, err := requireSoleInputQOV(p)
	if err != nil {
		return nil, err
	}

	preds := p.GetPredicates()
	// A CURRENT_TIMESTAMP-family reference needs the statement clock a
	// RowEvalContext carries — a bare frontier row would drift per row.
	needsRowCtx := hasBindingContext(evalCtx) || predicatesNeedStatementContext(preds)
	// When the input flows a 2-way ordinal join's merged
	// positional row, filter predicates evaluate under the LEG WINDOWS —
	// computed once, from the input plan's result value.
	legSpans, windowsOK := downstreamLegWindows(p.GetInner())
	filtered := &filterResultCursor{
		inner: innerCursor,
		pred: func(qr QueryResult) (bool, error) {
			var rowCtx any
			if qr.Positional != nil && qr.Positional.Layout != nil {
				rowCtx, err = frontierRowContext(qr.Positional, evalCtx, needsRowCtx, inputQOV)
				if err != nil {
					return false, err
				}
			} else if qr.Positional != nil && windowsOK {
				// The merged positional row of a gated 2-way
				// ordinal join — a leg reference QOV(leg).col needs
				// its leg window (unconditional: even with no binding context,
				// the leg bindings are required; the bare merged row misreads
				// leg-relative ordinals — a wrong-slot hazard).
				rowCtx = legWindowRowContext(qr.Positional, evalCtx, legSpans)
			} else if qr.Positional != nil {
				// The non-join frontier flows an authoritative
				// ordinal row — resolve filter predicates by ordinal (loud on a
				// miss, no name-map fallback).
				rowCtx, err = frontierRowContext(qr.Positional, evalCtx, needsRowCtx, inputQOV)
				if err != nil {
					return false, err
				}
			}
			for _, pred := range preds {
				res, err := pred.Eval(rowCtx)
				if err != nil {
					return false, recordlayer.TranslateTextCmpError(err)
				}
				if res != predicates.TriTrue {
					return false, nil
				}
			}
			return true, nil
		},
	}
	return applySkipLimit(filtered, props.Skip, props.ReturnedRowLimit), nil
}

// producesMergedRows reports whether a plan emits merged join rows
// (multiple quantifiers' columns concatenated into one positional row with
// per-leg windows) rather than single-table rows. A filter over such a plan
// resolves QOV predicates leg-locally through the windows, not via an alias
// binding.
//
// This list must stay in sync with the merged-row producers: mergeRows
// (executor.go) for the NLJ cursor, and the FlatMap cursor's merge output
// (flat_map_cursor.go). Those are the only two sites that emit merged rows.
// A future merged-row operator (e.g. a hash/merge join) MUST be added here,
// or a filter over it would bind the merged row under one alias and bare-
// resolve qov(b).col to the wrong quantifier (see DIVERGENCES.md).
func producesMergedRows(p plans.RecordQueryPlan) bool {
	switch p.(type) {
	case *plans.RecordQueryNestedLoopJoinPlan, *plans.RecordQueryFlatMapPlan:
		return true
	}
	return false
}

func executePredicatesFilter(
	ctx context.Context,
	p *plans.RecordQueryPredicatesFilterPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inner, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}
	inputQOV, err := requireSoleInputQOV(p)
	if err != nil {
		return nil, err
	}
	preds := p.GetPredicates()
	innerAlias := p.GetInnerAlias()
	// Bind the current row under innerAlias only when the inner plan
	// produces single-table rows (bare-keyed scans/index scans). For such
	// rows a QOV predicate qov(alias).col must resolve via the binding
	// (bare lookup), since the row carries no "ALIAS.COL" qualified key.
	//
	// When the inner plan produces MERGED rows (NLJ / FlatMap join output),
	// the row is an ordinal merged row whose per-leg windows resolve a
	// qov(alias).col reference leg-locally. We must NOT bind the merged row
	// under a single alias: a qov(b).col lookup would
	// then bare-resolve to whichever quantifier last wrote the bare key —
	// e.g. on a null-filled LEFT JOIN row (b absent), qov(b).id would wrongly
	// pick up the outer row's bare ID instead of NULL.
	bindAlias := innerAlias.Name() != "" && !producesMergedRows(p.GetInner())
	inputEdges := []values.QuantifiedObjectValue{inputQOV}
	if bindAlias && innerAlias != inputQOV.Correlation() {
		aliasQOV, aliasErr := values.NewQuantifiedObjectValue(innerAlias, inputQOV.FlowedType())
		if aliasErr != nil {
			return nil, fmt.Errorf("predicates filter input alias %q: %w", innerAlias.Name(), aliasErr)
		}
		inputEdges = append(inputEdges, aliasQOV)
	}
	// On the positional frontier a QOV(innerAlias).col resolves
	// via the bare-positional fallback in evaluateCorrelated (Correlations miss →
	// Positional), so bindAlias is NOT a reason to wrap — only a genuine
	// param/subquery/outer-binding is, or a CURRENT_TIMESTAMP-family
	// reference that needs the statement clock a RowEvalContext carries.
	// When neither is present, flow the bare row.
	posNeedsCtx := hasBindingContext(evalCtx) || predicatesNeedStatementContext(preds)
	// When the input flows a 2-way ordinal join's merged
	// positional row, predicates evaluate under the LEG WINDOWS — computed
	// once, from the input plan's result value.
	legSpans, windowsOK := downstreamLegWindows(p.GetInner())
	filtered := &filterResultCursor{
		inner: inner,
		pred: func(qr QueryResult) (bool, error) {
			scalar, kindErr := isBareScalarRow(qr.Positional)
			if kindErr != nil {
				return false, kindErr
			}
			var rowCtx any
			switch {
			case qr.Positional != nil && qr.Positional.Layout != nil:
				rowCtx, err = frontierRowContext(qr.Positional, evalCtx, posNeedsCtx, inputEdges...)
				if err != nil {
					return false, err
				}
			case qr.Positional != nil && windowsOK:
				// The merged positional row of a gated 2-way
				// ordinal join — a leg reference QOV(leg).col needs
				// its leg window (unconditional: even with no binding context,
				// the leg bindings are required; the bare merged row misreads
				// leg-relative ordinals — a wrong-slot hazard).
				rowCtx = legWindowRowContext(qr.Positional, evalCtx, legSpans)
			case qr.Positional != nil && scalar:
				// A BARE SCALAR inner row (a non-ordinal lateral-array UNNEST's
				// Explode flows a raw int64, wrapped by scalarPositionalRow into a
				// 1-slot `_0` row — RFC-142). A WHERE on the element references the
				// whole QuantifiedObjectValue(innerAlias) (Java binds the primitive
				// flowed value directly, not a FieldValue — see
				// generateCorrelatedFieldAccess), so bind the UNWRAPPED scalar under
				// innerAlias so QOV(innerAlias) resolves to it. Without this the QOV
				// whole-row fallback returns the 1-slot row, not the scalar.
				// The exact current and input edge need this binding even when
				// there is no optional extra innerAlias.
				layout, layoutErr := p.GetInner().ProvidedOutputLayout()
				if layoutErr != nil {
					return false, fmt.Errorf("predicates filter scalar input layout: %w", layoutErr)
				}
				rowCtx, err = scalarLayoutRowContext(layout, qr.Positional, evalCtx, inputEdges...)
				if err != nil {
					return false, err
				}
			case qr.Positional != nil:
				// The non-join frontier flows an authoritative
				// ordinal row — resolve predicates by ordinal (loud on a miss, no
				// name-map fallback). A QOV(innerAlias).col resolves via the
				// bare-positional fallback in evaluateCorrelated, so no alias
				// binding is needed here.
				rowCtx, err = frontierRowContext(qr.Positional, evalCtx, posNeedsCtx, inputEdges...)
				if err != nil {
					return false, err
				}
			}
			for _, pred := range preds {
				res, err := pred.Eval(rowCtx)
				if err != nil {
					return false, recordlayer.TranslateTextCmpError(err)
				}
				if res != predicates.TriTrue {
					return false, nil
				}
			}
			return true, nil
		},
	}
	return applySkipLimit(filtered, props.Skip, props.ReturnedRowLimit), nil
}

func executeMap(
	ctx context.Context,
	p *plans.RecordQueryMapPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	// As in Java's RecordQueryMapPlan, the child owns the original request
	// and continuation. Mapping only returned rows also avoids evaluating
	// expressions on rows the child discarded for request skip.
	inner, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props)
	if err != nil {
		return nil, err
	}
	inputQOV, err := requireSoleInputQOV(p)
	if err != nil {
		return nil, err
	}
	resultValue := p.GetResultValue()
	// On the positional frontier an outer correlation resolves via
	// the eval context's binder before the bare-positional frontier fallback.
	// A CURRENT_TIMESTAMP-family result value needs the statement clock a
	// RowEvalContext carries — a bare frontier row would drift per row.
	posNeedsCtx := hasBindingContext(evalCtx) || valuesNeedStatementContext(resultValue)
	// The Map output schema is row-invariant — derive the emitted
	// positional row's OUTPUT names once from the result value's record type. When
	// the result is a RecordConstructorValue, evaluate its Fields INDIVIDUALLY into
	// dense slots (never through the collapsing name map — a duplicate output name
	// keeps both slots by ordinal).
	var mapPosType *values.RecordType
	mapRC, _ := resultValue.(*values.RecordConstructorValue)
	if rt, ok := resultValue.Type().(*values.RecordType); ok {
		mapPosType = rt
	}
	// When the input flows a 2-way ordinal join's merged
	// positional row, the result value evaluates under the LEG WINDOWS —
	// computed once, from the input plan's result value.
	legSpans, windowsOK := downstreamLegWindows(p.GetInner())
	// The Map mints its rows and is their one owner, so they carry the layout
	// the output boundary holds them to instead of being copied for it.
	mintLayout := mintedRowLayout(p)
	var evalErr error
	mapped := recordlayer.MapCursor(inner, func(qr QueryResult) QueryResult {
		if evalErr != nil {
			return qr
		}
		var rowCtx any
		if qr.Positional != nil && qr.Positional.Layout != nil {
			rowCtx, evalErr = frontierRowContext(qr.Positional, evalCtx, posNeedsCtx, inputQOV)
			if evalErr != nil {
				return qr
			}
		} else if qr.Positional != nil && windowsOK {
			// The merged positional row of a gated 2-way
			// ordinal join — a leg reference QOV(leg).col needs its
			// leg window (unconditional; see executePredicatesFilter).
			rowCtx = legWindowRowContext(qr.Positional, evalCtx, legSpans)
		} else if qr.Positional != nil {
			// The non-join frontier flows an authoritative ordinal
			// row — resolve the result value by ordinal (loud on a miss).
			rowCtx, evalErr = frontierRowContext(qr.Positional, evalCtx, posNeedsCtx, inputQOV)
			if evalErr != nil {
				return qr
			}
		}
		// A Map's output IS a PositionalRow. When the result value is a
		// RecordConstructor, evaluate each field individually into dense slots; a
		// scalar result wraps in a 1-slot row.
		var pos *PositionalRow
		if mapRC != nil && mapPosType != nil {
			slots := make([]any, len(mapRC.Fields))
			for i, f := range mapRC.Fields {
				fv, ferr := f.Value.Evaluate(rowCtx)
				if ferr != nil {
					evalErr = recordlayer.TranslateTextCmpError(ferr)
					return qr
				}
				slots[i] = fv
			}
			pos = &PositionalRow{Type: mapPosType, Slots: slots, Layout: mintLayout}
		} else {
			m, err := resultValue.Evaluate(rowCtx)
			if err != nil {
				evalErr = recordlayer.TranslateTextCmpError(err)
				return qr
			}
			pos = scalarPositionalRowOfType(m, resultValue.Type())
		}
		return QueryResult{Positional: pos, Record: qr.Record, PrimaryKey: qr.PrimaryKey}
	})
	return &errCheckCursor{inner: mapped, err: &evalErr}, nil
}
