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
	"sort"

	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/internal/recordlayer/query/plan/plans"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// MaterializationLimitExceededError is returned when an operator tries to
// buffer more rows in memory than the configured materialization limit.
type MaterializationLimitExceededError struct {
	Limit   int
	Context string
}

func (e *MaterializationLimitExceededError) Error() string {
	return fmt.Sprintf("materialization limit exceeded (%d rows): %s; consider adding an index or increasing the materialization limit", e.Limit, e.Context)
}

// CollectAll drains a cursor into a slice.
func CollectAll(ctx context.Context, cursor recordlayer.RecordCursor[QueryResult]) ([]QueryResult, error) {
	var results []QueryResult
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, err := cursor.OnNext(ctx)
		if err != nil {
			return nil, err
		}
		if !result.HasNext() {
			if lerr := errIfBufferTruncated(result); lerr != nil {
				return nil, lerr
			}
			break
		}
		results = append(results, result.GetValue())
	}
	return results, nil
}

// CollectAllBounded drains a cursor into a slice through an accounted
// boundedBuffer (RFC-130): every row is charged against the statement-wide
// memory byte budget (st) AND counted against the row-count materialization
// limit, so a missed accumulation site is impossible — the buffer cannot exist
// without the accountant. st is the always-present statement ExecuteState
// (props.State); a nil/zero-limit st makes the byte charge a no-op while the
// row-count cap still applies. Returns MaterializationLimitExceededError on the
// row cap and MemoryLimitExceededError (→ 54F01) on the byte budget.
// The second return is the total bytes charged against st — the owner of the
// returned rows releases exactly that at teardown (live-bytes model: the
// buffer is rebuilt per page against the statement-wide state, so an
// unreleased charge re-accumulates once per page).
func CollectAllBounded(ctx context.Context, cursor recordlayer.RecordCursor[QueryResult], st *recordlayer.ExecuteState, limit int, opName string) ([]QueryResult, int64, error) {
	buf := newBoundedBuffer[QueryResult](st, limit, opName, estimateQueryResultBytes)
	for {
		if err := ctx.Err(); err != nil {
			return nil, buf.Charged(), err
		}
		result, err := cursor.OnNext(ctx)
		if err != nil {
			return nil, buf.Charged(), err
		}
		if !result.HasNext() {
			if lerr := errIfBufferTruncated(result); lerr != nil {
				return nil, buf.Charged(), lerr
			}
			break
		}
		v := result.GetValue()
		if err := buf.Append(v); err != nil {
			return nil, buf.Charged(), err
		}
	}
	return buf.Items(), buf.Charged(), nil
}

// collectAllRowCapped drains a cursor into a slice enforcing the MaterializationLimit
// ROW cap but charging NO bytes against the statement memory budget — for cursors
// whose rows are already byte-charged upstream. The recursive-CTE initial/recursive
// level cursors have a TempTableInsertPlan at the top, which charges each row in
// tt.Add (monotonic, statement-wide, surviving the per-level Clear); re-charging the
// same shared records here would double-count and trip the budget at ~half its true
// value (RFC-130, code-review #328). Passing a nil ExecuteState makes the boundedBuffer
// skip both the estimate and the charge while keeping the row cap.
func collectAllRowCapped(ctx context.Context, cursor recordlayer.RecordCursor[QueryResult], limit int, opName string) ([]QueryResult, error) {
	rows, _, err := CollectAllBounded(ctx, cursor, nil, limit, opName)
	return rows, err
}

// errIfBufferTruncated returns a 54F01-mapped error when an eager/buffered
// collect's source cursor stopped OUT-OF-BAND — i.e. a scan/byte/time resource
// limit (RFC-106a) cut it off, not true exhaustion or a legitimate
// ReturnedRowLimit. A buffered operator (union/NLJ-inner/INSERT/recursive-CTE,
// scalar subquery, DML drain) materializes its source in one shot and cannot
// paginate a continuation, so an out-of-band stop means the buffer is INCOMPLETE.
// Erroring (→ 54F01) is correct; silently returning the partial buffer would be a
// silent truncation (CLAUDE.md: no silent caps). Mirrors Java's
// RecordCursor.NoNextReason.isOutOfBand() — the streaming operators (sort/group)
// instead capture the partial state in a continuation and paginate, which a
// one-shot buffer cannot.
func errIfBufferTruncated(result recordlayer.RecordCursorResult[QueryResult]) error {
	if result.GetNoNextReason().IsOutOfBand() {
		return &recordlayer.ScanLimitReachedError{Reason: result.GetNoNextReason()}
	}
	return nil
}

// comparePKTuples compares two primary key tuples using their packed
// byte representation, which preserves FDB tuple ordering. Returns
// -1, 0, or 1.
func comparePKTuples(a, b tuple.Tuple) int {
	ap := a.Pack()
	bp := b.Pack()
	for i := 0; i < len(ap) && i < len(bp); i++ {
		if ap[i] < bp[i] {
			return -1
		}
		if ap[i] > bp[i] {
			return 1
		}
	}
	if len(ap) < len(bp) {
		return -1
	}
	if len(ap) > len(bp) {
		return 1
	}
	return 0
}

// sortEvalRow returns the row a sort-key Value expression should be evaluated
// against: the authoritative ordinal positional row (Value.Evaluate then
// resolves by ordinal, loud on a miss), chained to the evaluation context. A
// sort under a correlated FlatMap inner — a grouped derived body ordering by
// an enclosing row's value (`ORDER BY COUNT(*) - w.f`) — reads that value
// from the context's bindings, exactly as the projection and filter above it
// do; without the context the enclosing QOV is unbound.
func sortEvalRow(qr QueryResult, evalCtx *EvaluationContext, edges ...values.QuantifiedObjectValue) (any, error) {
	if qr.Positional == nil {
		return nil, nil
	}
	return frontierRowContext(qr.Positional, evalCtx, evalCtx != nil, edges...)
}

// valueReadsOnlyExactCarrier reports whether every QOV leaf in value is the
// pointer-exact selected input carrier. A joined row can expose both that
// carrier and retained leg windows with overlapping field names. The carrier
// is authoritative only for a Value already rewritten onto its exact handle;
// an independently minted current QOV, a named leg, or any mixed-owner tree
// must keep the ordinary leg-window path.
func valueReadsOnlyExactCarrier(value values.Value, carrier values.QuantifiedObjectValue) bool {
	if value == nil || carrier == nil {
		return false
	}
	found := false
	exact := true
	values.WalkValue(value, func(node values.Value) bool {
		qov, ok := values.AsQuantifiedObjectValue(node)
		if !ok {
			return true
		}
		found = true
		if qov != carrier {
			exact = false
		}
		return false
	})
	return found && exact
}

// --- Go extensions (no Java equivalent) ---

// executeInMemorySort materializes the inner plan's output and sorts it.
// Go extension — Java's Cascades has no physical sort operator.
func executeInMemorySort(
	ctx context.Context,
	p *plans.RecordQueryInMemorySortPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inner, inputLayout, err := validateInMemorySortLayouts(p)
	if err != nil {
		return nil, err
	}
	innerContinuation, priorBuf, emitPhase, err := resumeInMemorySortContinuation(store, inner, inputLayout, continuation)
	if err != nil {
		return nil, err
	}

	innerCursor, err := ExecutePlan(ctx, inner, store, evalCtx, innerContinuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}
	inputQOV, err := requireSoleInputQOV(p)
	if err != nil {
		return nil, err
	}

	sortFn := buildInMemorySortFn(p, inputLayout, inputQOV, evalCtx)

	cursor := newCustomSortCursor(innerCursor, sortFn, props.State)
	// RFC-180 H4: a RESUMED buffer is memory like any other — the fill loop
	// charges every fresh row against the RFC-130 statement budget
	// (customSortCursor.OnNext), so restored rows must charge identically at
	// injection or a resume smuggles an arbitrarily large buffer past the
	// limit. The cursor tracks its charges and releases them on Close, so a
	// statement-wide ExecuteState reused across pages accounts the buffer's
	// LIVE bytes once, not once per page boundary.
	if len(priorBuf) > 0 {
		if cerr := cursor.chargeResumedBuffer(priorBuf); cerr != nil {
			_ = cursor.Close()
			return nil, cerr
		}
	}
	switch {
	case emitPhase:
		// Emit-phase resume (sortEmitContinuation): the inner was EXHAUSTED when
		// the continuation was taken and priorBuf is the remaining SORTED output —
		// go straight to emit. Re-running the fill loop would re-scan the inner
		// from scratch and duplicate every restored row (the buffer is a slice,
		// not Java MemorySortCursor's key-deduped scratchpad map). An EMPTY
		// remaining buffer (resume taken at the last row) still sets loaded so the
		// cursor reports SourceExhausted instead of re-filling.
		cursor.buf = priorBuf
		cursor.loaded = true
	case len(priorBuf) > 0:
		// Fill-phase resume: the buffer so far, inner resumes from its position;
		// sortFn runs over the COMBINED buffer at exhaustion.
		cursor.buf = priorBuf
	}
	return applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit), nil
}

func validateInMemorySortLayouts(p *plans.RecordQueryInMemorySortPlan) (plans.RecordQueryPlan, values.OrdinalLayout, error) {
	expectedLayout, layoutErr := p.ProvidedOutputLayout()
	if layoutErr != nil {
		return nil, nil, fmt.Errorf("in-memory sort provided output layout: %w", layoutErr)
	}
	if expectedLayout == nil || expectedLayout.CarrierKind() != values.OrdinalCarrierRecord {
		return nil, nil, layoutBindingError(values.LayoutCarrierMismatch, "in-memory sort requires a concrete record output layout")
	}
	canonical, canonicalErr := values.IsCanonicalCurrentOnlyOrdinalLayout(expectedLayout)
	if canonicalErr != nil {
		return nil, nil, fmt.Errorf("in-memory sort provided output layout: %w", canonicalErr)
	}
	if !canonical {
		return nil, nil, layoutBindingError(
			values.LayoutNormalizationTypeMismatch,
			"in-memory sort cannot buffer a source-window layout before ordinal carrier normalization",
		)
	}
	inner := p.GetInner()
	if inner == nil {
		return nil, nil, layoutBindingError(values.LayoutCarrierMismatch, "in-memory sort requires a selected inner plan")
	}
	inputLayout, inputLayoutErr := inner.ProvidedOutputLayout()
	if inputLayoutErr != nil {
		return nil, nil, fmt.Errorf("in-memory sort required input layout: %w", inputLayoutErr)
	}
	if inputLayout == nil || inputLayout.CarrierKind() != values.OrdinalCarrierRecord {
		return nil, nil, layoutBindingError(values.LayoutCarrierMismatch, "in-memory sort requires a concrete record input layout")
	}
	if !values.PhysicalCarrierType(inputLayout).Equals(values.PhysicalCarrierType(expectedLayout)) {
		return nil, nil, layoutBindingError(values.LayoutTypeMismatch, "in-memory sort input and output carrier types disagree")
	}
	return inner, inputLayout, nil
}

func resumeInMemorySortContinuation(
	store *recordlayer.FDBRecordStore,
	inner plans.RecordQueryPlan,
	inputLayout values.OrdinalLayout,
	continuation []byte,
) (innerContinuation []byte, priorBuf []QueryResult, emitPhase bool, err error) {
	if continuation == nil {
		return nil, nil, false, nil
	}
	// Buffered STRUCT column slots rebuild as their ENCODED representation
	// (concrete-type identity with fresh rows, which the %T-keyed
	// group/dedup paths depend on), over the stored metadata's or the
	// selected plan's descriptors: a generated message (flag 0) as the
	// compiled type whose descriptor that is, a *dynamicpb.Message (flag 1)
	// as dynamicpb — never across representations. An unknown descriptor fails loudly
	// rather than leaking a descriptor-less placeholder into the row domain.
	var md *recordlayer.RecordMetaData
	if store != nil {
		md = store.GetRecordMetaData()
	}
	resolve := continuationMessageResolver(md, inner)
	// The continuation buffer contains CHILD rows: sort keys are evaluated
	// before the materialization boundary and therefore use the child's exact
	// layout/current handle. Attach the fresh sort output layout only when the
	// outer ExecutePlan adapter publishes an emitted row.
	ic, buf, exhausted, decErr := decodeSortContinuation(continuation, resolve, inputLayout)
	if decErr != nil {
		return nil, nil, false, fmt.Errorf("invalid sort continuation: %w", decErr)
	}
	return ic, buf, exhausted, nil
}

func buildInMemorySortFn(
	p *plans.RecordQueryInMemorySortPlan,
	inputLayout values.OrdinalLayout,
	inputQOV values.QuantifiedObjectValue,
	evalCtx *EvaluationContext,
) func([]QueryResult) error {
	keys := p.GetSortKeys()
	// When the sort input flows a merged ordinal-join row, evaluate the
	// sort keys through the SAME leg-window context the downstream projection/
	// filter use (legWindowRowContext -> spanAwareRow), so a QUALIFIED key
	// (`p.name`) over a multi-way join resolves to its BURIED leg — the box span's
	// buried-leg windows — instead of the naked merged row's first bare match.
	legSpans, joinWindowsOK := downstreamLegWindows(p.GetInner())
	sortKeyValue := func(qr QueryResult, k plans.SortKey) (any, error) {
		kv := k.ValueExpr
		if kv == nil {
			// Every sort key carries its Value (the planner rules set
			// ValueExpr unconditionally; the ordinal is baked at plan time). A
			// nil ValueExpr is a malformed plan — loud, never a name read.
			return nil, fmt.Errorf("in-memory sort: sort key %q carries no value expression — malformed plan", k.Field)
		}
		if joinWindowsOK && qr.Positional != nil {
			if valueReadsOnlyExactCarrier(kv, inputLayout.Carrier()) {
				arg, err := frontierRowContext(qr.Positional, evalCtx, true, inputQOV)
				if err != nil {
					return nil, err
				}
				return kv.Evaluate(arg)
			}
			return kv.Evaluate(legWindowRowContext(qr.Positional, evalCtx, legSpans))
		}
		// The authoritative ordinal row on the non-join frontier (loud on a
		// miss via FieldValue.evaluateOrdinal).
		arg, err := sortEvalRow(qr, evalCtx, inputQOV)
		if err != nil {
			return nil, err
		}
		return kv.Evaluate(arg)
	}
	return func(results []QueryResult) error {
		pkDesc := false
		if len(keys) > 0 {
			pkDesc = keys[len(keys)-1].Desc
		}
		var sortErr error
		sort.SliceStable(results, func(i, j int) bool {
			if sortErr != nil {
				return false
			}
			for _, k := range keys {
				var ci, cj any
				var err error
				if ci, err = sortKeyValue(results[i], k); err != nil {
					sortErr = err
					return false
				}
				if cj, err = sortKeyValue(results[j], k); err != nil {
					sortErr = err
					return false
				}
				iNil := ci == nil
				jNil := cj == nil
				if iNil && jNil {
					continue
				}
				if iNil || jNil {
					if k.NullsFirst {
						return iNil
					}
					return jNil
				}
				cmp, cmpErr := compareValues(ci, cj)
				if cmpErr != nil {
					if sortErr == nil {
						sortErr = cmpErr
					}
					return false
				}
				if cmp == 0 {
					continue
				}
				if k.Desc {
					return cmp > 0
				}
				return cmp < 0
			}
			if results[i].PrimaryKey != nil && results[j].PrimaryKey != nil {
				cmp := comparePKTuples(results[i].PrimaryKey, results[j].PrimaryKey)
				if cmp != 0 {
					if pkDesc {
						return cmp > 0
					}
					return cmp < 0
				}
			}
			return false
		})
		return sortErr
	}
}
