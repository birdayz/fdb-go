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

	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/internal/recordlayer/query/plan/plans"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

func executeIntersection(
	ctx context.Context,
	p *plans.RecordQueryIntersectionPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inners := p.GetInners()
	if len(inners) == 0 {
		return recordlayer.Empty[QueryResult](), nil
	}

	cursors, resume, err := buildIntersectionChildCursors(ctx, inners, store, evalCtx, continuation, props)
	if err != nil {
		return nil, err
	}

	keyVals := p.GetComparisonKeyValues()
	compKeyFunc := intersectionCompKeyFunc(keyVals)
	return applySkipLimit(
		recordlayer.IntersectionResume(cursors, compKeyFunc, p.IsReverse(), resume),
		props.Skip, props.ReturnedRowLimit,
	), nil
}

// buildIntersectionChildCursors decodes a parent IntersectionContinuation into
// per-child resume states (RFC-071) and creates one cursor per child:
//   - START (!Started): ExecutePlan with a nil continuation (begin fresh),
//   - MID (Started + bytes): ExecutePlan resuming from the per-child bytes,
//   - END (Started + empty): an empty cursor — the child is exhausted, which
//     ends the intersection immediately (any exhausted child ends it).
//
// The returned resume slice seeds each child's mergeChildState continuation
// (via IntersectionResume) so the next checkpoint re-encodes MID/END/START
// children correctly. With a nil/empty incoming continuation every child is
// START (unchanged first-page behavior). Shared by executeIntersection and
// executeMultiIntersection.
func buildIntersectionChildCursors(
	ctx context.Context,
	inners []plans.RecordQueryPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) ([]recordlayer.RecordCursor[QueryResult], []recordlayer.IntersectionChildResume, error) {
	resume, err := recordlayer.DecodeIntersectionContinuation(continuation, len(inners))
	if err != nil {
		return nil, nil, err
	}
	childProps := props.ClearSkipAndLimit()
	cursors := make([]recordlayer.RecordCursor[QueryResult], len(inners))
	for i, inner := range inners {
		if resume[i].Started && len(resume[i].Continuation) == 0 {
			cursors[i] = recordlayer.Empty[QueryResult]() // END: exhausted child
			continue
		}
		c, cerr := ExecutePlan(ctx, inner, store, evalCtx, resume[i].Continuation, childProps)
		if cerr != nil {
			for _, prev := range cursors[:i] {
				if prev != nil {
					prev.Close()
				}
			}
			return nil, nil, cerr
		}
		cursors[i] = c
	}
	return cursors, resume, nil
}

// compKeyEvalArg is the eval argument for an intersection/merge-sort COMPARISON
// KEY extraction. When the row carries an
// authoritative ordinal row (qr.Positional != nil — every merge/intersection
// branch is a plan whose output is positional), the comparison-key value resolves
// against that bare PositionalRow (FieldValue.Evaluate → evaluateOrdinal: by
// ordinal, loud on a miss — never a name-keyed read). A comparison key is
// a field extraction with no param / subquery / correlation binding, so the bare
// positional row is the whole context — frontierRowContext(pos, nil, false)
// collapses to exactly this. Both merge branches resolve the SAME field by
// ordinal, so merge/intersection order is preserved byte-for-byte.
func compKeyEvalArg(qr QueryResult, edges ...values.QuantifiedObjectValue) (any, error) {
	if qr.Positional == nil {
		return nil, nil
	}
	// Legacy/unadmitted cursor fixtures do not carry a physical layout. They
	// retain the historical bare-positional contract: a resolved FieldValue
	// reads its baked ordinal directly, without inventing an exact edge from a
	// names-only (UNKNOWN-typed) runtime row. Admitted production rows take the
	// strict branch below and must declare every non-current QOV explicitly.
	if qr.Positional.Layout == nil {
		return qr.Positional, nil
	}
	return frontierRowContext(qr.Positional, nil, false, edges...)
}

// intersectionCompKeyFunc uses the plan's comparison-key values, then PrimaryKey,
// then losslessly encoded positional content. A row with none of these is an error.
func intersectionCompKeyFunc(keyVals []values.Value) recordlayer.ComparisonKeyFunc[QueryResult] {
	var programs comparisonKeyPrograms
	return func(qr QueryResult) (tuple.Tuple, error) {
		if len(keyVals) > 0 {
			evalKeys, inputEdges, err := programs.forRow(keyVals, qr.Positional)
			if err != nil {
				return nil, fmt.Errorf("intersection comparison key program: %w", err)
			}
			arg, err := compKeyEvalArg(qr, inputEdges...)
			if err != nil {
				return nil, fmt.Errorf("intersection comparison key context: %w", err)
			}
			t := make(tuple.Tuple, len(keyVals))
			for i, kv := range evalKeys {
				// Comparison/merge keys are field extractions over the
				// row; an eval failure means the plan and the row
				// disagree (a planner bug), which must fail the QUERY,
				// not the process — Java surfaces it as a
				// RecordCoreException through the cursor future chain.
				// Resolves against the ordinal row via compKeyEvalArg.
				v, err := kv.Evaluate(arg)
				if err != nil {
					return nil, fmt.Errorf("intersection comparison key: %w", err)
				}
				// widenInt32: tuple has no int32 (RFC-092). uuidToTupleElement:
				// a UUID comparison/PK key is a neutral [16]byte, which the tuple
				// packer cannot encode — convert it to tuple.UUID, exactly as
				// packedDedupKey does, so compareKeys' Pack doesn't
				// panic on an intersection over a UUID key (RFC-162).
				t[i] = uuidToTupleElement(widenInt32(v))
			}
			return t, nil
		}
		if qr.PrimaryKey != nil {
			return qr.PrimaryKey, nil
		}
		// No comparison keys and no PK: match rows by their FULL positional
		// content, encoded LOSSLESSLY via the continuation codec — the
		// retired fmt %v rendering collapsed distinct composite rows and
		// collapsed every nil-layout row into one.
		if qr.Positional == nil {
			// A row with no keys, no PK and no positional content cannot be
			// matched against anything; a constant key would intersect ALL
			// such rows.
			return nil, fmt.Errorf("intersection row carries no comparison keys, primary key, or positional content — malformed plan")
		}
		b, err := appendContValue(nil, qr.Positional.Slots)
		if err != nil {
			return nil, fmt.Errorf("intersection positional comparison key: %w", err)
		}
		return tuple.Tuple{b}, nil
	}
}

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
	if len(keyVals) == 0 {
		// The comparison key is the grouping values, Java's
		// ComparisonKeyFunction.OnValues. An ungrouped merge has none, and the
		// one row each aggregate stream yields matches on the empty key, as
		// Java's empty value list does.
		compKeyFunc = func(QueryResult) (tuple.Tuple, error) { return tuple.Tuple{}, nil }
	}
	outputType, ok := p.GetResultType().(*values.RecordType)
	if !ok || outputType == nil {
		return nil, fmt.Errorf("multi-intersection result type is %T, want exact record", p.GetResultType())
	}

	// IntersectionMulti returns, per matching comparison key, the list of
	// matching rows (one per child). Mirrors Java's IntersectionMultiCursor;
	// the regular intersection keeps only the first child, which would drop
	// every aggregate but the first.
	innerCursor := recordlayer.IntersectionMultiResume(cursors, compKeyFunc, p.IsReverse(), resume)

	merged := &multiIntersectionMergeCursor{
		inner:       innerCursor,
		resultValue: p.GetResultValue(),
		outputType:  outputType,
		// Every child is an aggregate-index scan whose row is
		// [groupCols..., aggregate]; the result value's ordinals are baked
		// against that width.
		childWidth: len(keyVals) + 1,
	}
	return applySkipLimit(merged, props.Skip, props.ReturnedRowLimit), nil
}

func multiIntersectionCompKeyFunc(keyVals []values.Value) recordlayer.ComparisonKeyFunc[QueryResult] {
	var programs comparisonKeyPrograms
	return func(qr QueryResult) (tuple.Tuple, error) {
		if len(keyVals) > 0 {
			evalKeys, inputEdges, err := programs.forRow(keyVals, qr.Positional)
			if err != nil {
				return nil, fmt.Errorf("multi-intersection comparison key program: %w", err)
			}
			arg, err := compKeyEvalArg(qr, inputEdges...)
			if err != nil {
				return nil, err
			}
			t := make(tuple.Tuple, len(keyVals))
			for i, kv := range evalKeys {
				// Comparison/merge keys are field extractions; an eval
				// failure means the plan and the row disagree (a planner
				// bug), which must fail the QUERY, not the process — Java
				// surfaces it as a RecordCoreException through the cursor
				// future chain. Resolves against the ordinal row via
				// compKeyEvalArg.
				v, err := kv.Evaluate(arg)
				if err != nil {
					return nil, fmt.Errorf("multi-intersection comparison key: %w", err)
				}
				// widenInt32 (RFC-092) + uuidToTupleElement: a UUID group/PK
				// comparison key arrives as a neutral [16]byte the tuple packer
				// can't encode; convert it to tuple.UUID so compareKeys' Pack
				// doesn't panic on a multi-aggregate intersection over a UUID
				// GROUP BY key (RFC-162). Mirrors packedDedupKey's UUID arm.
				t[i] = uuidToTupleElement(widenInt32(v))
			}
			return t, nil
		}
		if qr.PrimaryKey != nil {
			return qr.PrimaryKey, nil
		}
		// No comparison keys and no PK: match rows by their FULL positional
		// content, encoded LOSSLESSLY via the continuation codec — the
		// retired fmt %v rendering collapsed distinct composite rows and
		// collapsed every nil-layout row into one.
		if qr.Positional == nil {
			// A row with no keys, no PK and no positional content cannot be
			// matched against anything; a constant key would intersect ALL
			// such rows.
			return nil, fmt.Errorf("intersection row carries no comparison keys, primary key, or positional content — malformed plan")
		}
		b, err := appendContValue(nil, qr.Positional.Slots)
		if err != nil {
			return nil, fmt.Errorf("multi-intersection positional comparison key: %w", err)
		}
		return tuple.Tuple{b}, nil
	}
}

// multiIntersectionMergeCursor combines each set of matching child rows
// into a single output row. It concatenates every child's positional row
// (grouping columns are identical across children; each child contributes its
// own aggregate column) and evaluates the plan's result value against the
// concatenation to produce the final record. Mirrors Java's
// RecordQueryMultiIntersectionOnValuesPlan.executePlan, which binds each
// child result to its quantifier and evaluates the resultValue.
type multiIntersectionMergeCursor struct {
	inner       recordlayer.RecordCursor[[]QueryResult]
	resultValue values.Value
	// outputType is the plan-admitted result row. The result constructor owns
	// exact field types; deriving a type from display names here would erase
	// them to Unknown and make layout attachment reject an otherwise-correct
	// row after it had already been computed.
	outputType *values.RecordType
	// childWidth is the per-child span the resultValue's ordinals were baked
	// against. It has NO usable zero value: leaving it unset is an error at
	// evaluation time, not a silently disabled check. Set
	// mergeChildWidthUnchecked to opt out deliberately. See mergeChildEvalArg.
	childWidth int
	closed     bool
	// lastNoNext replays the terminal result on a contract-violating re-call
	// (Java's cached no-next result on the map/merge stage) — never re-pulls
	// the inner intersection.
	lastNoNext *recordlayer.RecordCursorResult[QueryResult]
}

func (c *multiIntersectionMergeCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}
	result, err := c.inner.OnNext(ctx)
	if err != nil {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), err
	}
	if !result.HasNext() {
		res := recordlayer.NewResultNoNext[QueryResult](result.GetNoNextReason(), result.GetContinuation())
		c.lastNoNext = &res
		return res, nil
	}

	childResults := result.GetValue()

	// Resolve the resultValue against the CONCATENATED ordinal rows of
	// the matched children — the aggregate-index producer builds a Positional per
	// child; the grouping columns are identical across children and the aggregate
	// columns are distinct, so a plan-time bake against the flat concatenation
	// resolves each resultValue column to the correct slot.
	evalArg, err := mergeChildEvalArg(childResults, c.childWidth)
	if err != nil {
		return recordlayer.RecordCursorResult[QueryResult]{}, err
	}

	qr := QueryResult{}
	// Emit the authoritative ordinal OUTPUT row. The resultValue is a
	// RecordConstructorValue whose Fields ARE the output columns in output order
	// (the plan's result row type, which names the result-set columns), so
	// evaluating each field against the concatenated child positional row
	// produces a per-slot output row whose names/order match the result-set columns.
	if rc, ok := c.resultValue.(*values.RecordConstructorValue); ok {
		if c.outputType == nil || len(c.outputType.Fields) != len(rc.Fields) {
			return recordlayer.RecordCursorResult[QueryResult]{}, fmt.Errorf(
				"multi-intersection result type width does not match result constructor")
		}
		posSlots := make([]any, len(rc.Fields))
		for i, f := range rc.Fields {
			fv, ferr := f.Value.Evaluate(evalArg)
			if ferr != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, ferr
			}
			posSlots[i] = fv
		}
		qr.Positional = &PositionalRow{Type: c.outputType, Slots: posSlots}
	} else if c.resultValue != nil {
		// Non-RC resultValue: a scalar output row.
		datum, derr := c.resultValue.Evaluate(evalArg)
		if derr != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, derr
		}
		qr.Positional = scalarPositionalRow(datum)
	} else if p, ok := evalArg.(*PositionalRow); ok {
		// No resultValue: the concatenated child row IS the output.
		qr.Positional = p
	}
	return recordlayer.NewResultWithValue(qr, result.GetContinuation()), nil
}

// mergeChildWidthUnchecked is the explicit opt-out from the per-child width
// assertion, for callers that genuinely cannot state a width. It is a NEGATIVE
// sentinel rather than zero so that forgetting to set the width can never be
// mistaken for choosing to skip the check, and it is matched by EQUALITY so a
// computed negative cannot opt out by accident.
const mergeChildWidthUnchecked = -1

// mergeChildEvalArg builds the eval argument the MultiIntersection resultValue is
// evaluated against: the CONCATENATED PositionalRow of the matched child rows
// (a bare OrdinalRow whose plan-produced concatenated type the baked
// references read). Fields are concatenated in child order; grouping
// columns repeat across children with the same value, so a first-match bind is
// order-safe. Returns nil if any child lacks a Positional (should not happen — the
// aggregate-index producer builds one per child).
// expectedWidth is the per-child span the result value's ordinals were BAKED
// against at plan time (len(groupCols)+1). It is verified here rather than
// assumed because the assumption is derived independently in three places — the
// planning rule that bakes the ordinals, the cursor that sizes the absent-child
// filler, and this concatenation — and nothing made the three agree. A child
// arriving at a different width does not fail: it shifts every LATER child's
// aggregate by the difference and hands each group another group's value, with
// every row well-formed and the row count unchanged. That is the silent
// wrong-rows class, so it is an error, not a best-effort nil.
//
// The contract is INVERTED from the obvious one, deliberately: any width that
// is not a real width fails CLOSED. An unset (or otherwise nonsensical) width
// is an error, and opting out takes the exact mergeChildWidthUnchecked
// sentinel -- not merely "some negative number". A -2 arriving from arithmetic
// on an empty grouping key is a bug and must be loud; only the constant means
// "I have considered this and there is no width to state".
//
// The obvious spelling — "0 disables the check" — made the assertion fail OPEN,
// and it did so invisibly. Deleting the caller's `childWidth:` line left the
// struct field at its zero value, the check silently disabled, and the entire
// gate green: the tests below call this function with an explicit width, so
// they prove the function CHECKS but never that the caller ASKS. An assertion
// whose disarmed state is also its default is not an assertion; it is a
// suggestion that happens to be followed.
func mergeChildEvalArg(childResults []QueryResult, expectedWidth int) (any, error) {
	if expectedWidth != mergeChildWidthUnchecked && expectedWidth <= 0 {
		return nil, fmt.Errorf(
			"group-existence merge: no per-child width was stated, so the plan's baked " +
				"result-value ordinals cannot be validated against the rows actually " +
				"flowed. This is a wiring bug in the cursor's construction, not a " +
				"property of the data: set childWidth from the plan, or " +
				"mergeChildWidthUnchecked to opt out on purpose")
	}
	var fields []values.Field
	var slots []any
	ord := 0
	for ci, cr := range childResults {
		p := cr.Positional
		if p == nil || p.Type == nil {
			return nil, nil
		}
		if expectedWidth != mergeChildWidthUnchecked && len(p.Type.Fields) != expectedWidth {
			return nil, fmt.Errorf(
				"group-existence merge: child %d flowed %d columns, but the plan's result "+
					"value was baked against %d per child; every later child's aggregate "+
					"would be read from the wrong slot and each group would report another "+
					"group's value",
				ci, len(p.Type.Fields), expectedWidth)
		}
		for i, f := range p.Type.Fields {
			nf := f
			nf.Ordinal = ord
			fields = append(fields, nf)
			var v any
			if i < len(p.Slots) {
				v = p.Slots[i]
			}
			slots = append(slots, v)
			ord++
		}
	}
	if len(fields) == 0 {
		return nil, nil
	}
	return &PositionalRow{Type: &values.RecordType{Fields: fields}, Slots: slots}, nil
}

func (c *multiIntersectionMergeCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *multiIntersectionMergeCursor) IsClosed() bool { return c.closed }

var _ recordlayer.RecordCursor[QueryResult] = (*multiIntersectionMergeCursor)(nil)
