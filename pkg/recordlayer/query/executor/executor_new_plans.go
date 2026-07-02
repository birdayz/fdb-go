package executor

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"slices"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// executeAggregateIndexScan scans an aggregate index (SUM, COUNT, etc.)
// and produces rows with grouping columns + aggregate value. The index
// maintainer (atomicMutationIndexMaintainer) returns entries where:
//   - Key = grouping column values (tuple-encoded)
//   - Value = aggregate result (little-endian int64 → tuple.Tuple{int64})
//
// No record fetch needed — the index entries ARE the aggregated result.
func executeAggregateIndexScan(
	ctx context.Context,
	p *plans.RecordQueryAggregateIndexPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	idxPlan := p.GetIndexPlan()
	idx := store.GetMetaData().GetIndex(idxPlan.GetIndexName())
	if idx == nil {
		return nil, fmt.Errorf("executor: aggregate index %q not found", idxPlan.GetIndexName())
	}
	maintainer, err := store.GetIndexMaintainer(idx)
	if err != nil {
		return nil, fmt.Errorf("executor: getting index maintainer for %q: %w", idxPlan.GetIndexName(), err)
	}

	scanRange, err := scanComparisonsToTupleRange(idxPlan.GetScanComparisons(), scanBindContext(evalCtx))
	if err != nil {
		return nil, fmt.Errorf("executor: building scan range for %q: %w", idxPlan.GetIndexName(), err)
	}

	scanProps := recordlayer.ScanProperties{
		ExecuteProperties:   props,
		Reverse:             idxPlan.IsReverse(),
		CursorStreamingMode: recordlayer.StreamingModeIterator,
	}

	indexCursor := maintainer.Scan(scanRange, continuation, scanProps)

	return &aggregateIndexCursor{
		inner:     indexCursor,
		groupCols: p.GetGroupCols(),
		// Single source for the aggregate column key: the plan's
		// CanonicalAggColumnName is also what planColumnNamesWithMD reports via
		// OutputColumnNames, so the cursor's row key and the reported name can't
		// drift (RFC-081).
		canonicalName: p.CanonicalAggColumnName(),
	}, nil
}

type aggregateIndexCursor struct {
	inner         recordlayer.RecordCursor[*recordlayer.IndexEntry]
	groupCols     []string
	canonicalName string
	closed        bool
}

func (c *aggregateIndexCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	result, err := c.inner.OnNext(ctx)
	if err != nil {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), err
	}
	if !result.HasNext() {
		return recordlayer.NewResultNoNext[QueryResult](result.GetNoNextReason(), result.GetContinuation()), nil
	}

	entry := result.GetValue()
	datum := make(map[string]any, len(c.groupCols)+1)

	for i, col := range c.groupCols {
		if i < len(entry.Key) {
			// Normalize a UUID group key (tuple.UUID off the aggregate index)
			// to the neutral [16]byte the value layer uses, matching the
			// covering cursor — otherwise a residual HAVING/filter compare in
			// cmpAny (which only knows [16]byte) would miss it.
			datum[col] = tupleElementToUUID(entry.Key[i])
		}
	}

	if len(entry.Value) > 0 {
		datum[c.canonicalName] = entry.Value[0]
	}

	return recordlayer.NewResultWithValue(QueryResult{Datum: datum}, result.GetContinuation()), nil
}

func (c *aggregateIndexCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *aggregateIndexCursor) IsClosed() bool { return c.closed }

var _ recordlayer.RecordCursor[QueryResult] = (*aggregateIndexCursor)(nil)

func executeMultiIntersection(
	ctx context.Context,
	p *plans.RecordQueryMultiIntersectionOnValuesPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	children := p.GetChildren()
	if len(children) == 0 {
		return recordlayer.Empty[QueryResult](), nil
	}

	// Decode the per-child IntersectionContinuation and resume each child from
	// its saved position (RFC-071) — shared with executeIntersection. Replaces
	// the prior loud-error guard on a non-nil continuation.
	cursors, resume, err := buildIntersectionChildCursors(ctx, children, store, evalCtx, continuation, props)
	if err != nil {
		return nil, err
	}

	keyVals := p.GetComparisonKey()
	compKeyFunc := multiIntersectionCompKeyFunc(keyVals)

	// IntersectionMulti returns, per matching comparison key, the list of
	// matching rows (one per child). Mirrors Java's IntersectionMultiCursor;
	// the regular intersection keeps only the first child, which would drop
	// every aggregate but the first.
	innerCursor := recordlayer.IntersectionMultiResume(cursors, compKeyFunc, false, resume)

	merged := &multiIntersectionMergeCursor{
		inner:       innerCursor,
		resultValue: p.GetResultValue(),
	}
	return applySkipLimit(merged, props.Skip, props.ReturnedRowLimit), nil
}

func multiIntersectionCompKeyFunc(keyVals []values.Value) recordlayer.ComparisonKeyFunc[QueryResult] {
	return func(qr QueryResult) tuple.Tuple {
		if len(keyVals) > 0 {
			t := make(tuple.Tuple, len(keyVals))
			for i, kv := range keyVals {
				// Comparison/merge keys are field extractions; the runtime
				// typed-error family is unreachable and ComparisonKeyFunc
				// has no error channel, so a stray error is a planner
				// invariant violation (panic, matching prior no-recover).
				v, err := kv.Evaluate(qr.Datum)
				if err != nil {
					panic(err)
				}
				// widenInt32 (RFC-092) + uuidToTupleElement: a UUID group/PK
				// comparison key arrives as a neutral [16]byte the tuple packer
				// can't encode; convert it to tuple.UUID so compareKeys' Pack
				// doesn't panic on a multi-aggregate intersection over a UUID
				// GROUP BY key (RFC-162). Mirrors mergeSortCursor.extractKey.
				t[i] = uuidToTupleElement(widenInt32(v))
			}
			return t
		}
		if qr.PrimaryKey != nil {
			return qr.PrimaryKey
		}
		return tuple.Tuple{fmt.Sprintf("%v", qr.Datum)}
	}
}

// multiIntersectionMergeCursor combines each set of matching child rows
// into a single output row. It merges every child's Datum map (grouping
// columns are identical across children; each child contributes its own
// aggregate column) and then evaluates the plan's result value against the
// merged row to produce the final record. Mirrors Java's
// RecordQueryMultiIntersectionOnValuesPlan.executePlan, which binds each
// child result to its quantifier and evaluates the resultValue.
type multiIntersectionMergeCursor struct {
	inner       recordlayer.RecordCursor[[]QueryResult]
	resultValue values.Value
	closed      bool
}

func (c *multiIntersectionMergeCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	result, err := c.inner.OnNext(ctx)
	if err != nil {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), err
	}
	if !result.HasNext() {
		return recordlayer.NewResultNoNext[QueryResult](result.GetNoNextReason(), result.GetContinuation()), nil
	}

	childResults := result.GetValue()
	merged := make(map[string]any)
	for _, cr := range childResults {
		if m, ok := cr.Datum.(map[string]any); ok {
			for k, v := range m {
				merged[k] = v
			}
		}
	}

	var datum any = merged
	if c.resultValue != nil {
		datum, err = c.resultValue.Evaluate(merged)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
	}
	return recordlayer.NewResultWithValue(QueryResult{Datum: datum, Complete: true}, result.GetContinuation()), nil
}

func (c *multiIntersectionMergeCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *multiIntersectionMergeCursor) IsClosed() bool { return c.closed }

var _ recordlayer.RecordCursor[QueryResult] = (*multiIntersectionMergeCursor)(nil)

func executeLoadByKeys(
	_ context.Context,
	p *plans.RecordQueryLoadByKeysPlan,
	store *recordlayer.FDBRecordStore,
	_ *EvaluationContext,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	keys := p.GetKeysSource().GetPrimaryKeys()
	if len(keys) == 0 {
		return recordlayer.Empty[QueryResult](), nil
	}

	// RFC-130: bounded by a plan-literal key count, but each element is a whole
	// stored record, so the resident bytes can grow. Charge each loaded record
	// against the statement memory budget via boundedBuffer.
	results := newBoundedBuffer[QueryResult](props.State, 0, "LoadByKeys", estimateQueryResultBytes)
	for _, pk := range keys {
		rec, err := store.LoadRecord(pk)
		if err != nil {
			return nil, fmt.Errorf("executor: LoadByKeys pk=%v: %w", pk, err)
		}
		if rec == nil {
			continue
		}
		qr := FromStoredRecord(rec)
		if err := results.Append(qr); err != nil {
			return nil, err
		}
	}
	return applySkipLimit(
		recordlayer.FromList(results.Items()),
		props.Skip, props.ReturnedRowLimit,
	), nil
}

func executeUnorderedUnion(
	ctx context.Context,
	p *plans.RecordQueryUnorderedUnionPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inners := p.GetInners()
	if len(inners) == 0 {
		return recordlayer.Empty[QueryResult](), nil
	}
	var md *recordlayer.RecordMetaData
	if store != nil {
		md = store.GetRecordMetaData()
	}
	// SQL exposes a UNION's column names from the FIRST branch; later branches union
	// by POSITION. RecordQueryUnionPlan normalizes this (executeUnionStreaming), but
	// the unordered concat did NOT — so a branch whose output columns are named
	// differently from the first branch (e.g. mismatched aggregate aliases X vs Y)
	// flowed its rows under its OWN keys, and a downstream by-name read of the union's
	// (first-branch) column dropped them (TODO 7.6-union-remap / RFC-078). Remap each
	// later branch's keys to the first branch's, position-wise, exactly as the ordered
	// union does. A no-op when names already agree (the common case).
	firstBranchKeys := planColumnNamesWithMD(inners[0], md)
	childProps := props.ClearSkipAndLimit()
	cursors := make([]recordlayer.RecordCursor[QueryResult], 0, len(inners))
	for i, inner := range inners {
		c, err := ExecutePlan(ctx, inner, store, evalCtx, continuation, childProps)
		if err != nil {
			for _, prev := range cursors {
				_ = prev.Close()
			}
			return nil, err
		}
		if i > 0 && firstBranchKeys != nil {
			srcKeys := planColumnNamesWithMD(inner, md)
			if srcKeys != nil && !slices.Equal(srcKeys, firstBranchKeys) {
				target := firstBranchKeys
				c = recordlayer.MapCursor(c, func(qr QueryResult) QueryResult {
					return remapUnionColumnsByPosition(qr, srcKeys, target)
				})
			}
		}
		cursors = append(cursors, c)
	}
	return applySkipLimit(newConcatCursor[QueryResult](cursors), props.Skip, props.ReturnedRowLimit), nil
}

// producesMergedRows reports whether a plan emits merged join rows
// (multiple quantifiers' columns under qualified "ALIAS.COL" keys) rather
// than single-table rows. A filter over such a plan resolves QOV
// predicates through the qualified-key path, not an alias binding.
//
// This list must stay in sync with the code that WRITES qualified
// "ALIAS.COL" keys: mergeRows (executor.go) for the NLJ cursor, and
// qualifyOuterRow (used by the FlatMap cursor in flat_map_cursor.go).
// Those are the only two sites that emit merged rows today. A future
// merged-row operator (e.g. a hash/merge join) MUST be added here, or a
// filter over it would bind the merged row under one alias and bare-
// resolve qov(b).col to the wrong quantifier (see DIVERGENCES.md).
func producesMergedRows(p plans.RecordQueryPlan) bool {
	switch p.(type) {
	case *plans.RecordQueryNestedLoopJoinPlan, *plans.RecordQueryFlatMapPlan:
		return true
	}
	return false
}

// isStringMap reports whether datum is a name-keyed row map. Small readability
// helper for the predicates-filter row-context dispatch.
func isStringMap(datum any) bool {
	_, ok := datum.(map[string]any)
	return ok
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
	preds := p.GetPredicates()
	innerAlias := p.GetInnerAlias()
	// Bind the current row under innerAlias only when the inner plan
	// produces single-table rows (bare-keyed scans/index scans). For such
	// rows a QOV predicate qov(alias).col must resolve via the binding
	// (bare lookup), since the row carries no "ALIAS.COL" qualified key.
	//
	// When the inner plan produces MERGED rows (NLJ / FlatMap join output),
	// the row already carries qualified "ALIAS.COL" keys, so the predicate
	// resolves through the RowEvalContext.Datum qualified path. We must NOT
	// bind the merged row under a single alias: a qov(b).col lookup would
	// then bare-resolve to whichever quantifier last wrote the bare key —
	// e.g. on a null-filled LEFT JOIN row (b absent), qov(b).id would wrongly
	// pick up the outer row's bare ID instead of NULL.
	bindAlias := innerAlias.Name() != "" && !producesMergedRows(p.GetInner())
	needsRowCtx := bindAlias || (evalCtx != nil && (len(evalCtx.params) > 0 || len(evalCtx.scalarSubqueries) > 0 || len(evalCtx.bindings) > 0))
	// RFC-173 Slice 1: on the positional frontier a QOV(innerAlias).col resolves
	// via the bare-positional fallback in evaluateCorrelated (Correlations miss →
	// Positional), so bindAlias is NOT a reason to wrap — only a genuine
	// param/subquery/outer-binding is. When none is present, flow the bare row.
	posNeedsCtx := hasBindingContext(evalCtx)
	filtered := &filterResultCursor{
		inner: inner,
		pred: func(qr QueryResult) (bool, error) {
			var rowCtx any = qr.Datum
			// RFC-048 W1: a HAVING/filter reference to a name absent from a
			// complete row (aggregate output) is a bug, not a NULL.
			strict := StrictReferenceCheck && qr.Complete
			switch {
			case qr.Positional != nil:
				// RFC-173 Slice 1: the non-join frontier flows an authoritative
				// ordinal row — resolve predicates by ordinal (loud on a miss, no
				// name-map fallback). A QOV(innerAlias).col resolves via the
				// bare-positional fallback in evaluateCorrelated, so no alias
				// binding is needed here.
				rowCtx = frontierRowContext(qr.Positional, evalCtx, posNeedsCtx)
			case isStringMap(qr.Datum) && (strict || needsRowCtx):
				m := qr.Datum.(map[string]any) //nolint:errcheck // guarded by isStringMap
				ec := evalCtx
				if ec == nil {
					ec = EmptyEvaluationContext()
				}
				if bindAlias {
					ec = ec.WithBinding(innerAlias, m)
				}
				if strict {
					rowCtx = ec.RowContextStrict(m)
				} else {
					rowCtx = ec.RowContext(m)
				}
			case !isStringMap(qr.Datum) && bindAlias:
				// A BARE SCALAR inner row (a non-ordinal lateral-array UNNEST's
				// Explode flows int64(101), not a map — RFC-142). A WHERE on the
				// element references the whole QuantifiedObjectValue(innerAlias)
				// (Java binds the primitive flowed value directly, not a
				// FieldValue — see generateCorrelatedFieldAccess), so bind the
				// scalar under innerAlias and evaluate through a RowEvalContext
				// whose Correlations resolves QOV(innerAlias) to it. Without this
				// binding QOV(innerAlias).eval returns NULL and the predicate
				// filters every element out.
				ec := evalCtx
				if ec == nil {
					ec = EmptyEvaluationContext()
				}
				ec = ec.WithBinding(innerAlias, qr.Datum)
				rowCtx = ec.RowContext(nil)
			}
			for _, pred := range preds {
				res, err := pred.Eval(rowCtx)
				if err != nil {
					return false, err
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
	inner, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}
	resultValue := p.GetResultValue()
	// RFC-173 Slice 1: on the positional frontier an outer correlation resolves via
	// the eval context's binder before the bare-positional frontier fallback.
	posNeedsCtx := hasBindingContext(evalCtx)
	// RFC-173 Slice 1: the Map output schema is row-invariant — derive the emitted
	// positional row's OUTPUT names once from the result value's record type (nil
	// when the result isn't a record; then no positional row is emitted).
	var mapPosNames []string
	var mapPosType *values.RecordType
	if rt, ok := resultValue.Type().(*values.RecordType); ok {
		mapPosNames = make([]string, len(rt.Fields))
		for i, fld := range rt.Fields {
			mapPosNames[i] = fld.Name
		}
		mapPosType = positionalTypeFromNames(mapPosNames)
	}
	var evalErr error
	mapped := recordlayer.MapCursor(inner, func(qr QueryResult) QueryResult {
		if evalErr != nil {
			return qr
		}
		var rowCtx any = qr.Datum
		switch {
		case qr.Positional != nil:
			// RFC-173 Slice 1: the non-join frontier flows an authoritative ordinal
			// row — resolve the result value by ordinal (loud on a miss, no
			// name-map fallback), taking precedence over the name-keyed Datum.
			rowCtx = frontierRowContext(qr.Positional, evalCtx, posNeedsCtx)
		case StrictReferenceCheck && qr.Complete:
			// RFC-048 W1: a projection reading a name absent from a complete row
			// (aggregate output) is a bug, not a NULL. Production passes the raw
			// Datum map here (no parameter binder / scalar-subquery resolver), so
			// the strict context must carry ONLY Datum + Strict — adding a Binder or
			// ScalarSubqueries would let a param/subquery resolve in the test binary
			// while it returns NULL in production, i.e. strict mode would change
			// results. Bare strict context = identical resolution + miss reporting.
			if m, ok := qr.Datum.(map[string]any); ok {
				rowCtx = &values.RowEvalContext{Datum: m, Strict: true}
			}
		}
		m, err := resultValue.Evaluate(rowCtx)
		if err != nil {
			evalErr = err
			return qr
		}
		// RFC-173 Slice 1: dual-emit the ordinal positional row (OUTPUT-named),
		// built from the evaluated result so it mirrors the name-keyed Datum
		// field-for-field — but ONLY when the input is itself on the non-join
		// frontier (carried a Positional). A Map OVER A JOIN re-emits join-qualified
		// columns resolved by name; emitting a Positional there would wrongly flip
		// the consumer onto the ordinal path. Positional propagates along the
		// frontier and stops at the join/aggregate boundary.
		var pos *PositionalRow
		if qr.Positional != nil {
			if mm, ok := m.(map[string]any); ok && mapPosNames != nil {
				slots := make([]any, len(mapPosNames))
				for i, name := range mapPosNames {
					slots[i] = mm[name]
				}
				pos = &PositionalRow{Type: mapPosType, Slots: slots}
			}
		}
		return QueryResult{Datum: m, Positional: pos, Record: qr.Record, PrimaryKey: qr.PrimaryKey}
	})
	return &errCheckCursor{inner: applySkipLimit(mapped, props.Skip, props.ReturnedRowLimit), err: &evalErr}, nil
}

func executeFirstOrDefault(
	ctx context.Context,
	p *plans.RecordQueryFirstOrDefaultPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inner, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props)
	if err != nil {
		return nil, err
	}
	result, err := inner.OnNext(ctx)
	_ = inner.Close()
	if err != nil {
		return nil, err
	}
	if result.HasNext() {
		return newSingleResultCursor(result.GetValue()), nil
	}
	// An out-of-band (resource-limit) stop before the first row means the input was
	// TRUNCATED — we can't tell whether a matching row would have followed, so error
	// (→ 54F01) instead of fabricating the default and returning a wrong EXISTS/scalar
	// answer from a partial scan (RFC-106a).
	if lerr := errIfBufferTruncated(result); lerr != nil {
		return nil, lerr
	}
	defaultVal := p.GetDefaultValue()
	var datum any
	if defaultVal != nil {
		datum, err = defaultVal.Evaluate(nil)
		if err != nil {
			return nil, err
		}
	}
	return newSingleResultCursor(QueryResult{Datum: datum}), nil
}

func executeDefaultOnEmpty(
	ctx context.Context,
	p *plans.RecordQueryDefaultOnEmptyPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inner, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props)
	if err != nil {
		return nil, err
	}
	firstResult, err := inner.OnNext(ctx)
	if err != nil {
		_ = inner.Close()
		return nil, err
	}
	if firstResult.HasNext() {
		return newPrependResultCursor(firstResult.GetValue(), inner), nil
	}
	_ = inner.Close()
	// Out-of-band (resource-limit) stop before the first row → truncated input;
	// error rather than fabricate the default (RFC-106a; see FirstOrDefault).
	if lerr := errIfBufferTruncated(firstResult); lerr != nil {
		return nil, lerr
	}
	defaultVal := p.GetDefaultValue()
	var datum any
	if defaultVal != nil {
		datum, err = defaultVal.Evaluate(nil)
		if err != nil {
			return nil, err
		}
	}
	return newSingleResultCursor(QueryResult{Datum: datum}), nil
}

func executeInJoin(
	ctx context.Context,
	p *plans.RecordQueryInJoinPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inValues := p.GetInValues()
	if len(inValues) == 0 {
		return ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props)
	}

	bindingID := values.NamedCorrelationIdentifier(p.GetBindingName())
	var cursors []recordlayer.RecordCursor[QueryResult]
	for _, val := range inValues {
		boundCtx := evalCtx.WithBinding(bindingID, val)
		cursor, err := ExecutePlan(ctx, p.GetInner(), store, boundCtx, nil, props)
		if err != nil {
			for _, c := range cursors {
				c.Close()
			}
			return nil, err
		}
		cursors = append(cursors, cursor)
	}

	if len(cursors) == 1 {
		return cursors[0], nil
	}

	return newConcatCursor(cursors), nil
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
	bindingNames := p.GetBindingNames()
	if len(inSources) == 0 || len(bindingNames) == 0 {
		return ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props)
	}

	// Single binding dimension: execute inner once per IN value,
	// merge-sort if comparison keys exist, otherwise concat.
	if len(bindingNames) == 1 && len(inSources[0]) > 0 {
		bindingID := values.NamedCorrelationIdentifier(bindingNames[0])
		var cursors []recordlayer.RecordCursor[QueryResult]
		for _, val := range inSources[0] {
			boundCtx := evalCtx.WithBinding(bindingID, val)
			cursor, err := ExecutePlan(ctx, p.GetInner(), store, boundCtx, nil, props.ClearSkipAndLimit())
			if err != nil {
				for _, c := range cursors {
					c.Close()
				}
				return nil, err
			}
			cursors = append(cursors, cursor)
		}
		if len(cursors) == 1 {
			return applySkipLimit(cursors[0], props.Skip, props.ReturnedRowLimit), nil
		}
		compKeys := p.GetComparisonKeys()
		if len(compKeys) > 0 {
			merged := &mergeSortCursor{
				cursors:   cursors,
				compKeys:  compKeys,
				reverse:   p.IsReverse(),
				dedup:     true,
				peeked:    make([]QueryResult, len(cursors)),
				hasPeeked: make([]bool, len(cursors)),
				exhausted: make([]bool, len(cursors)),
			}
			return applySkipLimit(merged, props.Skip, props.ReturnedRowLimit), nil
		}
		return applySkipLimit(newConcatCursor(cursors), props.Skip, props.ReturnedRowLimit), nil
	}

	return nil, fmt.Errorf("executeInUnion: multi-binding IN union (%d bindings) not yet implemented", len(bindingNames))
}

func executeMergeSortUnion(
	ctx context.Context,
	p *plans.RecordQueryMergeSortUnionPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inners := p.GetInners()
	if len(inners) == 0 {
		return newEmptyCursor[QueryResult](), nil
	}

	cursors := make([]recordlayer.RecordCursor[QueryResult], len(inners))
	for i, inner := range inners {
		c, err := ExecutePlan(ctx, inner, store, evalCtx, nil, props.ClearSkipAndLimit())
		if err != nil {
			for _, prev := range cursors[:i] {
				prev.Close()
			}
			return nil, err
		}
		cursors[i] = c
	}

	return &mergeSortCursor{
		cursors:   cursors,
		compKeys:  p.GetComparisonKeys(),
		reverse:   p.IsReverse(),
		dedup:     p.RemovesDuplicates(),
		peeked:    make([]QueryResult, len(cursors)),
		hasPeeked: make([]bool, len(cursors)),
		exhausted: make([]bool, len(cursors)),
	}, nil
}

type mergeSortCursor struct {
	cursors   []recordlayer.RecordCursor[QueryResult]
	compKeys  []values.Value
	reverse   bool
	dedup     bool
	peeked    []QueryResult
	hasPeeked []bool
	exhausted []bool
	lastKey   string
	hasLast   bool
	closed    bool
}

func (m *mergeSortCursor) IsClosed() bool { return m.closed }

func (m *mergeSortCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	for {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		if err := m.fillPeekBuffers(ctx); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}

		bestIdx := -1
		for i := range m.cursors {
			if !m.hasPeeked[i] {
				continue
			}
			if bestIdx < 0 || m.isBetter(m.peeked[i], m.peeked[bestIdx]) {
				bestIdx = i
			}
		}

		if bestIdx < 0 {
			return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
		}

		result := m.peeked[bestIdx]
		m.hasPeeked[bestIdx] = false

		if m.dedup {
			key := m.extractKey(result)
			if m.hasLast && key == m.lastKey {
				continue
			}
			m.lastKey = key
			m.hasLast = true
		}

		return recordlayer.NewResultWithValue(result, &recordlayer.StartContinuation{}), nil
	}
}

func (m *mergeSortCursor) fillPeekBuffers(ctx context.Context) error {
	for i := range m.cursors {
		if m.hasPeeked[i] || m.exhausted[i] {
			continue
		}
		result, err := m.cursors[i].OnNext(ctx)
		if err != nil {
			return err
		}
		if result.HasNext() {
			m.peeked[i] = result.GetValue()
			m.hasPeeked[i] = true
		} else if result.GetNoNextReason().IsOutOfBand() {
			// A branch cut off OUT-OF-BAND (resource limit, paginate mode) cannot be
			// merged correctly — treating it as exhausted would drop the rest of that
			// sorted run and emit a wrong merge order. Error instead (RFC-106a).
			return &recordlayer.ScanLimitReachedError{Reason: result.GetNoNextReason()}
		} else {
			m.exhausted[i] = true
		}
	}
	return nil
}

func (m *mergeSortCursor) isBetter(a, b QueryResult) bool {
	for _, key := range m.compKeys {
		// Merge-order keys are field extractions; the runtime typed-error
		// family is unreachable and the comparator has no error channel, so
		// a stray error is a planner invariant violation (panic, matching
		// the prior no-recover behaviour).
		va, err := key.Evaluate(a.Datum)
		if err != nil {
			panic(err)
		}
		vb, err := key.Evaluate(b.Datum)
		if err != nil {
			panic(err)
		}
		cmp := compareValues(va, vb)
		if cmp == 0 {
			continue
		}
		if m.reverse {
			return cmp > 0
		}
		return cmp < 0
	}
	return false
}

func (m *mergeSortCursor) extractKey(qr QueryResult) string {
	if len(m.compKeys) == 0 {
		return ""
	}
	t := make(tuple.Tuple, len(m.compKeys))
	for i, key := range m.compKeys {
		// Merge-dedup keys are field extractions; the runtime typed-error
		// family is unreachable and extractKey has no error channel, so a
		// stray error is a planner invariant violation (panic, matching the
		// prior no-recover behaviour).
		v, err := key.Evaluate(qr.Datum)
		if err != nil {
			panic(err)
		}
		switch tv := v.(type) {
		case nil, int64, int, uint, uint64, float32, float64, string, []byte, bool:
			t[i] = tv
		case int32:
			t[i] = int64(tv)
		case [16]byte:
			// A UUID merge key must pack as a tuple.UUID (0x30 + 16 bytes) so the
			// packed-tuple ordering the merge relies on matches the unsigned
			// big-endian UUID order — the fmt.Sprintf default below would pack a
			// decimal-list string ("[16]uint8:[85 14 …]") that sorts lexically.
			t[i] = tuple.UUID(tv)
		default:
			t[i] = fmt.Sprintf("%T:%v", v, v)
		}
	}
	return string(t.Pack())
}

func (m *mergeSortCursor) Close() error {
	if m.closed {
		return nil
	}
	m.closed = true
	var firstErr error
	for _, c := range m.cursors {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func compareValues(a, b any) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}
	switch av := a.(type) {
	case int64:
		switch bv := b.(type) {
		case int64:
			if av < bv {
				return -1
			}
			if av > bv {
				return 1
			}
			return 0
		case int32:
			bv64 := int64(bv)
			if av < bv64 {
				return -1
			}
			if av > bv64 {
				return 1
			}
			return 0
		default:
			bf := toFloat64(b)
			if !math.IsNaN(bf) {
				af := float64(av)
				if af < bf {
					return -1
				}
				if af > bf {
					return 1
				}
				return 0
			}
		}
	case int32:
		av64 := int64(av)
		switch bv := b.(type) {
		case int64:
			if av64 < bv {
				return -1
			}
			if av64 > bv {
				return 1
			}
			return 0
		case int32:
			if av < bv {
				return -1
			}
			if av > bv {
				return 1
			}
			return 0
		default:
			bf := toFloat64(b)
			if !math.IsNaN(bf) {
				af := float64(av)
				if af < bf {
					return -1
				}
				if af > bf {
					return 1
				}
				return 0
			}
		}
	case float64:
		if math.IsNaN(av) {
			break
		}
		bv := toFloat64(b)
		if !math.IsNaN(bv) {
			if av < bv {
				return -1
			}
			if av > bv {
				return 1
			}
			return 0
		}
	case string:
		if bv, ok := b.(string); ok {
			if av < bv {
				return -1
			}
			if av > bv {
				return 1
			}
			return 0
		}
	case [16]byte:
		// UUID sorts by unsigned big-endian bytes — the same order the tuple.UUID
		// wire encoding and the filter-path predicates.cmpAny use, so an
		// in-memory sort of a non-indexed UUID column agrees with an ordered
		// index scan. Without this arm the fmt.Sprintf("%v") fallback below would
		// compare decimal-list strings ("[85 14 …]") in lexical, not byte, order.
		if bv, ok := b.([16]byte); ok {
			return bytes.Compare(av[:], bv[:])
		}
	}
	as := fmt.Sprintf("%v", a)
	bs := fmt.Sprintf("%v", b)
	if as < bs {
		return -1
	}
	if as > bs {
		return 1
	}
	return 0
}

func newEmptyCursor[T any]() recordlayer.RecordCursor[T] {
	return &emptyCursor[T]{}
}

type emptyCursor[T any] struct{ closed bool }

func (c *emptyCursor[T]) OnNext(context.Context) (recordlayer.RecordCursorResult[T], error) {
	return recordlayer.NewResultNoNext[T](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
}
func (c *emptyCursor[T]) IsClosed() bool { return c.closed }
func (c *emptyCursor[T]) Close() error   { c.closed = true; return nil }

type concatCursor[T any] struct {
	cursors []recordlayer.RecordCursor[T]
	idx     int
	closed  bool
}

func newConcatCursor[T any](cursors []recordlayer.RecordCursor[T]) *concatCursor[T] {
	return &concatCursor[T]{cursors: cursors}
}

func (c *concatCursor[T]) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[T], error) {
	for c.idx < len(c.cursors) {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[T]{}, err
		}
		result, err := c.cursors[c.idx].OnNext(ctx)
		if err != nil {
			return result, err
		}
		if result.HasNext() {
			return result, nil
		}
		// A branch that stopped OUT-OF-BAND (a scan/byte/time resource limit in
		// paginate mode) cannot be resumed across the concat boundary — concat
		// carries no per-branch continuation state, so advancing to the next branch
		// would silently drop the rest of this one. Error instead (RFC-106a;
		// same reasoning as the multidim skip-scan). SourceExhausted → next branch.
		// Without a scan limit set, out-of-band never fires, so UNION ALL is unchanged.
		if result.GetNoNextReason().IsOutOfBand() {
			return recordlayer.RecordCursorResult[T]{}, &recordlayer.ScanLimitReachedError{Reason: result.GetNoNextReason()}
		}
		c.idx++
	}
	return recordlayer.NewResultNoNext[T](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
}

func (c *concatCursor[T]) IsClosed() bool { return c.closed }

func (c *concatCursor[T]) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	var firstErr error
	for _, cur := range c.cursors {
		if err := cur.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// singleResultCursor yields one result then ends.
type singleResultCursor struct {
	value  QueryResult
	done   bool
	closed bool
}

func newSingleResultCursor(v QueryResult) *singleResultCursor {
	return &singleResultCursor{value: v}
}

func (c *singleResultCursor) OnNext(_ context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.done || c.closed {
		return recordlayer.NewResultNoNext[QueryResult](
			recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
		), nil
	}
	c.done = true
	// Use nil continuation — a single-result cursor doesn't support
	// resumption. EndContinuation is rejected by NewResultWithValue
	// (a value result must have a resumable continuation).
	return recordlayer.NewResultWithValue(c.value, nil), nil
}

func (c *singleResultCursor) Close() error   { c.closed = true; return nil }
func (c *singleResultCursor) IsClosed() bool { return c.closed }

// prependResultCursor yields one value then delegates to inner.
type prependResultCursor struct {
	first   QueryResult
	inner   recordlayer.RecordCursor[QueryResult]
	yielded bool
	closed  bool
}

func newPrependResultCursor(first QueryResult, inner recordlayer.RecordCursor[QueryResult]) *prependResultCursor {
	return &prependResultCursor{first: first, inner: inner}
}

func (c *prependResultCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if !c.yielded {
		c.yielded = true
		return recordlayer.NewResultWithValue(c.first, &recordlayer.StartContinuation{}), nil
	}
	return c.inner.OnNext(ctx)
}

func (c *prependResultCursor) Close() error   { c.closed = true; return c.inner.Close() }
func (c *prependResultCursor) IsClosed() bool { return c.closed }
