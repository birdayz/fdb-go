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

	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func executeFirstOrDefault(
	ctx context.Context,
	p *plans.RecordQueryFirstOrDefaultPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	resume, innerCont, decodeErr := decodeFirstOrDefaultContinuation(continuation)
	if decodeErr != nil {
		return nil, decodeErr
	}
	// A resume from the emitted row's own continuation: the single row
	// was already consumed — the cursor is exhausted, never re-executed
	// (re-execution would re-emit the row: a duplicate under any parent
	// that snapshots child positions).
	if resume == fodResumeConsumed {
		return recordlayer.Empty[QueryResult](), nil
	}
	// Java's non-strict FirstOrDefault applies the request BEFORE first/default,
	// unlike DefaultOnEmpty. The Go-only strict scalar barrier must instead
	// validate the complete input: request skip or cap must not hide row two.
	// Even a child cap of two is unsafe: a raw scan can spend that budget on
	// another record type and one matching row, then stop in-band before the
	// second match. Keep scan/time limits (which stop out-of-band), not row caps.
	innerProps := props
	if p.IsStrict() {
		innerProps = props.ClearSkipAndLimit()
	}
	inner, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, innerCont, innerProps)
	if err != nil {
		return nil, err
	}
	result, err := inner.OnNext(ctx)
	if err != nil {
		_ = inner.Close()
		return nil, err
	}
	if result.HasNext() {
		first := result.GetValue()
		if p.IsStrict() {
			// SQL scalar-subquery cardinality: a second row is a violation (21000),
			// not a silent truncation. Probe exactly one more row. This runs fresh
			// per outer row under the driving FlatMap, so at-most-one is enforced
			// per outer (mirrors the uncorrelated executor.EvaluateScalarSubquery).
			second, serr := inner.OnNext(ctx)
			if serr != nil {
				_ = inner.Close()
				return nil, serr
			}
			if second.HasNext() {
				_ = inner.Close()
				return nil, &ExecutionError{
					Kind:    ExecCardinalityViolation,
					Message: "scalar subquery returned more than one row",
				}
			}
			// An out-of-band (resource-limit) stop after the first row means the
			// input was TRUNCATED — a second matching row may exist beyond the
			// cap, so at-most-one is not yet proven. That is a PAGE BOUNDARY, not
			// a failure: hand the stop up so the parent pages.
			//
			// Whole-leg restart, NOT a checkpoint on the inner's position: the
			// operator is holding `first`, and a stop cursor carries no value, so
			// resuming the inner past `first` would lose it. Restarting re-derives
			// it. This is the one branch where the restart is the only correct
			// resume — see restartAfterOutOfBand.
			if second.GetNoNextReason().IsOutOfBand() {
				_ = inner.Close()
				return restartAfterOutOfBand(second.GetNoNextReason()), nil
			}
		}
		_ = inner.Close()
		first, err = normalizeDefaultResult(p, first)
		if err != nil {
			return nil, err
		}
		if p.IsStrict() {
			return applySkipLimit(newSingleResultCursor(first), props.Skip, props.ReturnedRowLimit), nil
		}
		return newSingleResultCursor(first), nil
	}
	_ = inner.Close()
	// An out-of-band (resource-limit) stop before the first row means the input
	// was TRUNCATED — we cannot tell whether a matching row would have followed,
	// so the default must NOT be fabricated (that is Java's
	// RecordQueryFirstOrDefaultPlan.java:100-106 / issue 3220, which treats the
	// truncated inner as an empty one and answers EXISTS wrongly). Hand the stop
	// up as a CHECKPOINT on the inner's own position (see
	// checkpointAfterOutOfBand): at this point the operator's entire state is
	// "zero rows seen", which the inner's continuation captures exactly, so the
	// resumed leg carries on from where it stopped instead of starting over.
	if result.GetNoNextReason().IsOutOfBand() {
		return checkpointAfterOutOfBand(result)
	}
	qr, err := defaultResultFromValue(p, p.GetDefaultValue(), evalCtx)
	if err != nil {
		return nil, err
	}
	if p.IsStrict() {
		return applySkipLimit(newSingleResultCursor(qr), props.Skip, props.ReturnedRowLimit), nil
	}
	return newSingleResultCursor(qr), nil
}

// defaultResultFromValue evaluates an empty arm using bindings, never a consumed
// child frontier. The plan owns the exact output carrier; SQL NULL records keep
// an absent-current marker distinct from present records with all-NULL fields.
func defaultResultFromValue(
	p plans.RecordQueryPlan,
	defaultValue values.Value,
	evalCtx *EvaluationContext,
) (QueryResult, error) {
	if evalCtx == nil {
		evalCtx = EmptyEvaluationContext()
	}
	rowCtx := evalCtx.RowContext()
	resultType := p.GetResultType()
	recordType, isRecord := resultType.(*values.RecordType)
	if !isRecord {
		var value any
		if defaultValue != nil {
			var err error
			value, err = defaultValue.Evaluate(rowCtx)
			if err != nil {
				return QueryResult{}, err
			}
		}
		return QueryResult{Positional: scalarPositionalRowOfType(value, resultType)}, nil
	}

	var datum any
	if constructor, ok := defaultValue.(*values.RecordConstructorValue); ok {
		result, err := resultFromValue(constructor, rowCtx)
		if err != nil {
			return QueryResult{}, err
		}
		datum = result.Positional
	} else if defaultValue != nil {
		var err error
		datum, err = defaultValue.Evaluate(rowCtx)
		if err != nil {
			return QueryResult{}, err
		}
	}
	// A typed nil in an interface is still a NULL record, not a present shell.
	if row, ok := datum.(*PositionalRow); ok && row == nil {
		datum = nil
	}
	if message, ok := datum.(proto.Message); ok && isNilProtoMessage(message) {
		datum = nil
	}
	layout, err := p.ProvidedOutputLayout()
	if err != nil {
		return QueryResult{}, err
	}
	row, err := NewLayoutPositionalRow(recordType, layout)
	if err != nil {
		return QueryResult{}, err
	}
	if datum != nil {
		if message, ok := datum.(proto.Message); ok {
			// Use the field reader's storage-shape authority. Logical field
			// nullability is not protobuf presence, and nullable arrays may
			// arrive as ordinary repeated fields rather than wrapper messages.
			if !values.ProtoRecordDescriptorCompatible(message.ProtoReflect().Descriptor(), values.FlowedExactType(layout.Carrier())) {
				return QueryResult{}, layoutBindingError(values.LayoutRuntimeShape,
					"default protobuf descriptor disagrees with declared record type")
			}
			converted := protoToPositional(message)
			converted.Type = recordType
			datum = converted
		}
		if positional, ok := datum.(*PositionalRow); ok {
			if positional.Type == nil || !values.WithNullability(positional.Type, true).Equals(values.WithNullability(recordType, true)) {
				return QueryResult{}, layoutBindingError(values.LayoutRuntimeShape,
					"default positional record disagrees with declared record type")
			}
		}
		source, ok := datum.(values.OrdinalRow)
		if !ok {
			return QueryResult{}, layoutBindingError(values.LayoutRuntimeShape,
				fmt.Sprintf("default record has non-ordinal runtime type %T", datum))
		}
		for i := range row.Slots {
			field, exists := source.Get(i)
			if !exists {
				return QueryResult{}, layoutBindingError(values.LayoutRuntimeShape, "default record is shorter than declared type")
			}
			row.Slots[i] = field
		}
		if _, exists := source.Get(len(row.Slots)); exists {
			return QueryResult{}, layoutBindingError(values.LayoutRuntimeShape, "default record is wider than declared type")
		}
	}
	presence, err := values.NewOrdinalCarrierMatchPresence(layout, datum != nil)
	if err != nil {
		return QueryResult{}, err
	}
	row.LayoutPresence = presence
	return QueryResult{Positional: row}, nil
}

// An out-of-band stop underneath a first-or-default is a resumable PAGE
// BOUNDARY, not a failure. The per-page time budget exists precisely so a page
// ends before FDB's 5s transaction wall, and every Java operator propagates such
// a stop upward with a continuation rather than throwing
// (FlatMapPipelinedCursor.java:325-367 forwards the inner's reason verbatim;
// MemorySortCursor.java:78-93 checkpoints its sorter; CursorLimitManager.java:144
// is the ONLY place in the Java engine that turns a limit into an exception, and
// only under failOnScanLimitReached). Collapsing it into an error here made a
// slow-but-healthy cluster fail a legitimate query with 54F01 — the cluster was
// not out of budget, the PAGE was.
//
// This operator is EAGER: unlike a streaming cursor it must drain its inner leg
// to decide what to flow, so it meets the stop itself instead of passing a
// no-next result through. It therefore has to mint the resume position, and
// which position is correct depends on what it was holding when the stop
// arrived — hence two functions below, not one.
//
// The asymmetry is LOAD-BEARING, not stylistic, and it is the thing to read
// before collapsing the two branches into one. Point the empty branch's
// checkpoint at the strict branch and the operator silently drops the row it was
// holding: the resumed inner starts PAST that row, finds nothing, and the
// default is fabricated in its place. A correlated scalar subquery then answers
// NULL where it had a value — the observable is a caller failing to scan NULL
// into a non-nullable column, and TestFDB_NotExistsOutOfBandStop_PaginatesNotErrors
// /ScannedRowsLimitStrictScalarSubquery is what catches it. Point the strict
// branch's restart at the empty branch and nothing is wrong, only slow —
// unboundedly so, which the livelock backstop turns into a loud failure.
//
// Neither is what Java does here. RecordQueryFirstOrDefaultPlan.java:100-106
// hands its child a null continuation and reads a truncated inner as an EMPTY
// one, silently answering NOT EXISTS = true from a partial scan — a wrong answer
// its own comment flags as unfixed (FoundationDB/fdb-record-layer#3220), and
// RecordCursor.java:920-925 names the fix in the abstract: when the future is
// backed by a cursor that may stop out-of-band, "it may be desirable to include
// the progress included in the underlying cursor in the final continuation …
// something more bespoke may be required". checkpointAfterOutOfBand is that
// bespoke thing.
//
// Consumers that cannot page still fail loud: any eager drain routes through
// errIfBufferTruncated / errIfDrainTruncated, which turn this same out-of-band
// stop into 54F01. Only a parent with a real resume point — the nested-loop
// flat map, or the paginating SQL driver — converts it into a page boundary.

// checkpointAfterOutOfBand handles the stop that arrives BEFORE any inner row.
// The operator's entire state at that instant is "zero rows seen", and the inner
// leg's own continuation captures exactly where it got to — so the two together
// are a complete resume position. The resumed leg carries on from the checkpoint
// and the first row it then finds is still the leg's first row.
//
// This is what makes the fix work on a genuinely slow cluster rather than only
// on a slightly slow one: progress becomes per INNER ROW instead of per outer
// row. A restart here would re-scan the same prefix every page, so a leg that
// cannot fit one page's budget would never finish — the driver's no-progress
// tripwire would fire and the query would fail with 54F01 anyway, just with a
// different message.
//
// Falls back to a whole-leg restart only when the inner has no resumable
// position to checkpoint (a start/end continuation), which is not a state any
// current leaf produces after an out-of-band stop but is cheap to be right about.
func checkpointAfterOutOfBand(result recordlayer.RecordCursorResult[QueryResult]) (recordlayer.RecordCursor[QueryResult], error) {
	reason := result.GetNoNextReason()
	cont := result.GetContinuation()
	if cont == nil || cont.IsEnd() {
		return restartAfterOutOfBand(reason), nil
	}
	inner, err := cont.ToBytes()
	if err != nil {
		return nil, err
	}
	if len(inner) == 0 {
		return restartAfterOutOfBand(reason), nil
	}
	return recordlayer.Stopped[QueryResult](reason,
		recordlayer.NewBytesContinuation(encodeFirstOrDefaultCheckpoint(inner))), nil
}

// restartAfterOutOfBand handles the stop that arrives while the operator is
// already holding its first row — the strict at-most-one probe. A stop cursor
// carries no value, so resuming the inner PAST that row would lose it; the whole
// leg has to run again. Nothing is lost by that: no row of this operator was
// emitted, and the re-run re-derives both the row and the cardinality proof.
//
// The livelock this can produce is real and deliberate: if one page's budget
// cannot reach the first row and probe past it, every page restarts at the same
// position and the driver's no-progress tripwire fires. That is the correct-or-
// loud outcome for a budget too small to answer the query at all, and it is the
// backstop the empty-branch checkpoint above no longer needs.
func restartAfterOutOfBand(reason recordlayer.NoNextReason) recordlayer.RecordCursor[QueryResult] {
	return recordlayer.Stopped[QueryResult](reason,
		recordlayer.NewBytesContinuation([]byte{fodTagRestart}))
}

func executeDefaultOnEmpty(
	ctx context.Context,
	p *plans.RecordQueryDefaultOnEmptyPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	// Java RecordQueryDefaultOnEmptyPlan.executePlan delegates to
	//   RecordCursor.orElse(cont -> child.executePlan(store, ctx, cont, clearSkipAndLimit),
	//                        (executor, cont) -> RecordCursor.fromList(executor, [default], cont),
	//                        continuation)
	//     .skipThenLimit(skip, limit)
	// OrElseCursor serializes state UNDECIDED/USE_INNER/USE_OTHER, so once the
	// inner has produced a row the resumed cursor stays on USE_INNER (never
	// fabricating the default), and EVERY emitted row's continuation carries the
	// inner's real position — which fixes both a paging loop that never advanced
	// (a single-result default cursor emitted a nil StartContinuation forever) and
	// a spurious null-extension on a mid-stream resume past the last inner row.
	//
	// The inner (primary) clears skip/limit; the default (alternative) is a
	// single-row list cursor that respects its continuation; skip/limit apply to
	// the whole OrElse — mirroring Java's clearSkipAndLimit-on-child +
	// skipThenLimit-on-orElse split. An out-of-band inner stop before the first
	// row flows through OrElse's UNDECIDED no-next (Java's behaviour): the caller
	// resumes and the default is NEVER fabricated on truncation. FirstOrDefault
	// reaches the same outcome by a different route (restartAfterOutOfBand) —
	// being eager, it has to convert the stop into one explicitly. The
	// uncorrelated scalar-subquery path has no parent that can page and so keeps
	// its RFC-106a truncation error.
	nestedProps := props.ClearSkipAndLimit()
	primaryFactory := func(cont []byte) recordlayer.RecordCursor[QueryResult] {
		inner, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, cont, nestedProps)
		if err != nil {
			return &errResultCursor{err: err}
		}
		return recordlayer.MapErrCursor(inner, func(result QueryResult) (QueryResult, error) {
			return normalizeDefaultResult(p, result)
		})
	}
	alternativeFactory := func(cont []byte) recordlayer.RecordCursor[QueryResult] {
		// Evaluate the default lazily (only when the inner is empty), matching
		// Java's else-lambda: onEmptyResultValue.eval runs inside fromList's
		// supplier. A nil default flows a scalar NULL row.
		var defaultRow QueryResult
		if defaultVal := p.GetDefaultValue(); defaultVal != nil {
			qr, err := defaultResultFromValue(p, defaultVal, evalCtx)
			if err != nil {
				return &errResultCursor{err: err}
			}
			defaultRow = qr
		} else {
			defaultRow = QueryResult{Positional: scalarPositionalRow(nil)}
		}
		return recordlayer.FromListWithContinuation([]QueryResult{defaultRow}, cont)
	}
	orElse := recordlayer.OrElseWithContinuation(primaryFactory, alternativeFactory, continuation)
	return applySkipLimit(orElse, props.Skip, props.ReturnedRowLimit), nil
}

// normalizeDefaultResult publishes the operator's reconciled output type on an
// inner row. FirstOrDefault and DefaultOnEmpty combine two result alternatives:
// a non-null child and a nullable default produce a nullable scalar or record.
// Forwarding the child's narrower runtime type unchanged makes the row disagree
// with the plan's exact provided layout even though its values are valid for
// that layout. Retag only the root-nullability widening admitted by the plan
// constructor; every field, ordinal, and slot count must still agree exactly.
func normalizeDefaultResult(
	p plans.RecordQueryPlan,
	result QueryResult,
) (QueryResult, error) {
	declared := p.GetResultType()
	resultType, isRecord := declared.(*values.RecordType)
	if !isRecord {
		row := result.Positional
		scalar, err := isBareScalarRow(row)
		if err != nil {
			return QueryResult{}, err
		}
		if !scalar || row.Type == nil || len(row.Type.Fields) != 1 || len(row.Slots) != 1 ||
			!values.WithNullability(row.Type.Fields[0].FieldType, true).Equals(values.WithNullability(declared, true)) ||
			(row.Type.Fields[0].FieldType.IsNullable() && !declared.IsNullable()) {
			return QueryResult{}, layoutBindingError(values.LayoutTypeMismatch, fmt.Sprintf("%T scalar child disagrees with declared output type %s", p, declared))
		}
		if row.Slots[0] == nil && !declared.IsNullable() {
			return QueryResult{}, layoutBindingError(values.LayoutNullabilityMismatch, fmt.Sprintf("%T non-nullable scalar child is SQL NULL", p))
		}
		result.Positional = scalarPositionalRowOfType(row.Slots[0], declared)
		return result, nil
	}
	if result.Positional == nil || result.Positional.Type == nil {
		return QueryResult{}, fmt.Errorf("%T record result has no typed positional row", p)
	}
	actualType := result.Positional.Type
	if !values.WithNullability(actualType, true).Equals(values.WithNullability(resultType, true)) ||
		len(result.Positional.Slots) != len(resultType.Fields) ||
		(actualType.IsNullable() && !resultType.IsNullable()) {
		return QueryResult{}, fmt.Errorf(
			"%T runtime row type %s is incompatible with result type %s",
			p, actualType, resultType)
	}
	if actualType.Equals(resultType) && result.Positional.Layout == nil {
		return result, nil
	}
	row := *result.Positional
	row.Type = resultType
	row.Slots = append([]any(nil), result.Positional.Slots...)
	// The child's layout describes the child type. The default-producing plan
	// publishes a fresh identity layout for the reconciled union type;
	// ExecutePlan attaches that parent layout after the chosen branch.
	row.Layout = nil
	row.LayoutPresence = nil
	// Replacing the child's address layout must not turn a whole-record NULL
	// into a present record whose fields happen to be NULL. Rebase only that
	// carrier-level absence; the child's source-window identities do not flow.
	_, absent, err := result.Positional.wholeObjectBinding()
	if err != nil {
		return QueryResult{}, err
	}
	if absent {
		layout, layoutErr := p.ProvidedOutputLayout()
		if layoutErr != nil {
			return QueryResult{}, layoutErr
		}
		presence, presenceErr := values.NewOrdinalCarrierMatchPresence(layout, false)
		if presenceErr != nil {
			return QueryResult{}, presenceErr
		}
		row.Layout, row.LayoutPresence = layout, presence
	}
	result.Positional = &row
	return result, nil
}

// A first-or-default's continuation namespace is TAGGED. The operator is eager —
// it drains its inner leg to decide whether to flow the first row or the default
// — so the byte string it hands its parent has to say WHICH of three states it
// stopped in, and one of those states carries the inner leg's own continuation
// bytes as a payload.
//
// An untagged namespace cannot express that safely. The "one row was consumed"
// marker is the single byte 0x01, and the operator used to pass anything else
// straight through to its inner as that inner's continuation — so an inner
// continuation that happened to BE 0x01 would have resumed as already-consumed.
// That is a silent wrong answer, not a crash: the operator flows nothing where
// the inner still had rows, and an EXISTS built on it inverts. The collision was
// unreachable only because nothing ever fed this operator a real inner
// continuation; checkpointing (checkpointAfterOutOfBand) is exactly what starts
// doing so, which is why the namespace gets tagged in the same change.
//
// The tag byte makes the three states disjoint by construction. 0x01 keeps its
// meaning so continuations issued before the tagging still decode correctly.
const (
	fodTagConsumed   byte = 0x01 // the single row was emitted and consumed
	fodTagCheckpoint byte = 0x02 // zero rows seen; payload is the inner leg's position
	fodTagRestart    byte = 0x03 // re-run the whole leg from the beginning
)

// fodResume is what a decoded first-or-default continuation says to do.
type fodResume int

const (
	fodResumeStart    fodResume = iota // run the inner leg from the beginning
	fodResumeConsumed                  // the row is gone; the cursor is exhausted
	fodResumeInner                     // run the inner leg from the decoded position
)

// singleResultConsumedToken is the continuation a singleResultCursor's
// emitted row carries: "the one row was consumed". A resume from it is
// EXHAUSTED (executeFirstOrDefault returns Empty). The retired
// value+nil-continuation shape was impossible in Java (every value
// result carries a resumable continuation) and made every parent
// snapshot the child as START — safe only while the shape stayed ≤1 row
// under a per-outer-row FlatMap; any other parent would have silently
// replayed the row.
var singleResultConsumedToken = []byte{fodTagConsumed}

// encodeFirstOrDefaultCheckpoint wraps an inner leg's continuation bytes so they
// cannot be mistaken for any other state in this operator's namespace.
func encodeFirstOrDefaultCheckpoint(inner []byte) []byte {
	out := make([]byte, 0, len(inner)+1)
	out = append(out, fodTagCheckpoint)
	return append(out, inner...)
}

// decodeFirstOrDefaultContinuation reads this operator's namespace. An
// unrecognised tag is a corrupt continuation and must surface as one — silently
// treating it as a fresh start would re-emit rows the caller already consumed
// (the reason executeFlatMap raises ContinuationParseError on a bad envelope).
func decodeFirstOrDefaultContinuation(b []byte) (fodResume, []byte, error) {
	if len(b) == 0 {
		return fodResumeStart, nil, nil
	}
	switch b[0] {
	case fodTagConsumed:
		if len(b) != 1 {
			return fodResumeStart, nil, &recordlayer.ContinuationParseError{
				Message:  "first-or-default consumed token carries a payload",
				RawBytes: b,
			}
		}
		return fodResumeConsumed, nil, nil
	case fodTagRestart:
		if len(b) != 1 {
			return fodResumeStart, nil, &recordlayer.ContinuationParseError{
				Message:  "first-or-default restart token carries a payload",
				RawBytes: b,
			}
		}
		return fodResumeStart, nil, nil
	case fodTagCheckpoint:
		// An empty payload is a checkpoint at the very beginning — the same
		// thing as a restart, and handled as one rather than rejected.
		if len(b) == 1 {
			return fodResumeStart, nil, nil
		}
		return fodResumeInner, b[1:], nil
	default:
		return fodResumeStart, nil, &recordlayer.ContinuationParseError{
			Message:  "unknown first-or-default continuation tag",
			RawBytes: b,
		}
	}
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
	return recordlayer.NewResultWithValue(
		c.value, recordlayer.NewBytesContinuation(singleResultConsumedToken),
	), nil
}

func (c *singleResultCursor) Close() error { c.closed = true; return nil }

func (c *singleResultCursor) IsClosed() bool { return c.closed }
