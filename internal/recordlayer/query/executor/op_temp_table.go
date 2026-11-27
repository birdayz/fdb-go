// Portions derived from FoundationDB Record Layer (MessageHelpers.java,
// QueryResult.java, IndexOrphanBehavior.java, RecordCursor.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	cursorpb "fdb.dev/internal/pb/cursor"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func executeTempTableScan(
	p *plans.RecordQueryTempTableScanPlan,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	// Java TempTableScanPlan.executePlan: ListCursor(table.getList(),
	// continuation) — a mid-list resume is a 4-byte position.
	tt := evalCtx.GetOrCreateTempTable(p.GetTempTableAlias(), props.State)
	items := tt.GetList()
	cursor := applySkipLimit(
		recordlayer.FromListWithContinuation(items, continuation), props.Skip, props.ReturnedRowLimit)

	// Recursive-continuation tokens intentionally retain only positional field
	// names and slots. decodeRecursiveRow therefore reconstructs an UNKNOWN-only
	// schema; the selected TempTableScan is the first operator that again owns an
	// exact row declaration. Reattach that declaration here, before the generic
	// provided-layout adapter checks the emitted row. This is not a general type
	// inference path: exact rows pass through, and only an anonymous, layout-free,
	// UNKNOWN-only row with the identical field names, ordinals, and width may be
	// restamped.
	layout, err := p.ProvidedOutputLayout()
	if err != nil {
		return nil, fmt.Errorf("executor: temp-table scan provided output layout: %w", err)
	}
	if layout.CarrierKind() == values.OrdinalCarrierRecord {
		expected, ok := values.PhysicalCarrierType(layout).(*values.RecordType)
		if !ok || expected == nil {
			return nil, layoutBindingError(values.LayoutTypeMismatch,
				"temp-table scan record layout has no exact record carrier")
		}
		cursor = recordlayer.MapErrCursor(cursor, func(result QueryResult) (QueryResult, error) {
			return restampDecodedTempTableRow(result, expected)
		})
	}
	return cursor, nil
}

// restampDecodedTempTableRow restores only schema metadata deliberately absent
// from the recursive continuation wire payload. It is copy-on-write: the temp
// table snapshot and the decoded continuation row remain untouched.
func restampDecodedTempTableRow(result QueryResult, expected *values.RecordType) (QueryResult, error) {
	row := result.Positional
	if row == nil || row.Type == nil || expected == nil {
		return QueryResult{}, layoutBindingError(values.LayoutRuntimeShape,
			"temp-table scan requires a typed positional row")
	}
	if row.Type.Equals(expected) {
		return result, nil
	}
	if row.Layout != nil || row.LayoutPresence != nil {
		return QueryResult{}, layoutBindingError(values.LayoutCarrierMismatch,
			"temp-table scan cannot restamp a row that already carries layout metadata")
	}
	// This is decodeRecursiveRow's canonical envelope. Record-level name and
	// nullability are not present in the token and are therefore restored from
	// expected rather than compared with placeholder zero values.
	if row.Type.RecordName != "" || row.Type.Nullable || len(row.Type.Legs) != 0 {
		return QueryResult{}, layoutBindingError(values.LayoutCarrierMismatch,
			"temp-table scan row is not a continuation-decoded positional schema")
	}
	if len(row.Type.Fields) != len(expected.Fields) || len(row.Slots) != len(expected.Fields) {
		return QueryResult{}, layoutBindingError(values.LayoutCarrierMismatch,
			"temp-table scan row width disagrees with the selected carrier")
	}
	for i := range expected.Fields {
		actualField := row.Type.Fields[i]
		expectedField := expected.Fields[i]
		if actualField.Name != expectedField.Name || actualField.Ordinal != expectedField.Ordinal {
			return QueryResult{}, layoutBindingError(values.LayoutCarrierMismatch,
				fmt.Sprintf("temp-table scan row field %d identity disagrees with the selected carrier", i))
		}
		if actualField.FieldType == nil || actualField.FieldType.Code() != values.TypeCodeUnknown {
			return QueryResult{}, layoutBindingError(values.LayoutTypeMismatch,
				fmt.Sprintf("temp-table scan row field %d has a concrete type that disagrees with the selected carrier", i))
		}
	}

	copyRow := *row
	copyRow.Type = expected
	copyRow.Slots = append([]any(nil), row.Slots...)
	result.Positional = &copyRow
	return result, nil
}

func executeTempTableInsert(
	ctx context.Context,
	p *plans.RecordQueryTempTableInsertPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	if p.IsOwning() {
		// Java TempTableInsertPlan.executePlan's owning arm
		// (TempTableInsertCursor.from): the cursor's continuation
		// snapshots the table alongside the child position, so a
		// mid-stream resume (the recursive union's) restores the rows
		// inserted so far into the freshly-bound table before resuming
		// the child mid-way.
		tt := evalCtx.GetOrCreateTempTable(p.GetTempTableAlias(), props.State)
		return newTempTableInsertCursor(continuation, tt, storeDescriptorResolver(store),
			func(childCont []byte) (recordlayer.RecordCursor[QueryResult], error) {
				return ExecutePlan(ctx, p.GetInner(), store, evalCtx, childCont, props.ClearSkipAndLimit())
			})
	}
	innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}

	tt := evalCtx.GetOrCreateTempTable(p.GetTempTableAlias(), props.State)

	// RFC-130: charge the temp-table working set; tt.Add returns the budget
	// error, propagated via MapErrCursor (MapCursor cannot return an error).
	// The table outlives this cursor — it is the recursion's shared working
	// set (BFS ping-pong locals, or the DFS accumulator GetOrCreate mints
	// into the shared bindings map) — so its charges are released by the
	// recursion's teardown, never here.
	mapped := recordlayer.MapErrCursor(innerCursor, func(qr QueryResult) (QueryResult, error) {
		if err := tt.Add(qr); err != nil {
			return QueryResult{}, err
		}
		return qr, nil
	})
	return mapped, nil
}

func executeTableFunction(
	p *plans.RecordQueryTableFunctionPlan,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	sv := p.GetStreamValue()
	if sv == nil {
		return applySkipLimit(recordlayer.Empty[QueryResult](), props.Skip, props.ReturnedRowLimit), nil
	}
	if rv, ok := sv.(*values.RangeValue); ok {
		cursor, err := newRangeCursor(rv, evalCtx, continuation)
		if err != nil {
			return nil, err
		}
		return applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit), nil
	}
	result, err := sv.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return applySkipLimit(recordlayer.Empty[QueryResult](), props.Skip, props.ReturnedRowLimit), nil
	}
	var elementHandle values.ExactTypeHandle
	if elementType := arrayElementType(sv); elementType != nil {
		elementHandle, err = values.SnapshotExactType(elementType)
		if err != nil {
			return nil, err
		}
	}
	list, ok := result.([]any)
	if !ok {
		return applySkipLimit(
			recordlayer.FromListWithContinuation([]QueryResult{{Positional: explodeElementRow(result, elementHandle)}}, continuation),
			props.Skip, props.ReturnedRowLimit,
		), nil
	}
	items := make([]QueryResult, len(list))
	for i, elem := range list {
		items[i] = QueryResult{Positional: explodeElementRow(elem, elementHandle)}
	}
	return applySkipLimit(recordlayer.FromListWithContinuation(items, continuation), props.Skip, props.ReturnedRowLimit), nil
}

// rangeCursor is Java's RangeValue.Cursor: rows (ID) from nextPosition by step,
// each continuation a RangeCursorContinuation carrying the following position.
type rangeCursor struct {
	rowType   *values.RecordType
	next, end int64
	step      int64
	closed    bool
}

func newRangeCursor(rv *values.RangeValue, evalCtx *EvaluationContext, continuation []byte) (*rangeCursor, error) {
	begin, end, step, err := rv.Bounds(evalCtx)
	if err != nil {
		return nil, err
	}
	if continuation != nil {
		var c cursorpb.RangeCursorContinuation
		if err := proto.Unmarshal(continuation, &c); err != nil {
			return nil, fmt.Errorf("invalid range continuation: %w", err)
		}
		begin = c.GetNextPosition()
		if err := values.CheckRangeBounds(begin, end, step); err != nil {
			return nil, err
		}
	}
	return &rangeCursor{rowType: rv.Type().(*values.RecordType), next: begin, end: end, step: step}, nil
}

func (c *rangeCursor) OnNext(context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.closed || c.next >= c.end {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
	}
	row := QueryResult{Positional: &PositionalRow{Type: c.rowType, Slots: []any{c.next}}}
	c.next += c.step
	var cont recordlayer.RecordCursorContinuation = &recordlayer.EndContinuation{}
	// Java's Continuation.isEnd: nextPosition >= endExclusive + step.
	if c.next < c.end+c.step {
		b, err := proto.Marshal(&cursorpb.RangeCursorContinuation{NextPosition: proto.Int64(c.next)})
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		cont = recordlayer.NewBytesContinuation(b)
	}
	return recordlayer.NewResultWithValue(row, cont), nil
}

func (c *rangeCursor) Close() error { c.closed = true; return nil }

func (c *rangeCursor) IsClosed() bool { return c.closed }
