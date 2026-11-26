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

	"fdb.dev/gen"
	"fdb.dev/internal/unicode15"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func executeFlatMap(
	ctx context.Context,
	p *plans.RecordQueryFlatMapPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	nestedProps := props.ClearSkipAndLimit()

	// Parse FlatMapContinuation if resuming.
	var outerCont, innerCont, checkValue []byte
	if len(continuation) > 0 {
		var fmc gen.FlatMapContinuation
		if err := recordlayer.UnmarshalAsJava(continuation, &fmc); err != nil {
			// Java: RecordCursor.flatMapPipelined —
			//   throw new RecordCoreException("error parsing continuation", ex).
			// A corrupt continuation must fail, not silently restart from
			// scratch (which re-emits rows the caller already consumed).
			return nil, &recordlayer.ContinuationParseError{RawBytes: continuation, Cause: err}
		}
		outerCont = fmc.OuterContinuation
		innerCont = fmc.InnerContinuation
		checkValue = fmc.CheckValue
	}

	outerCursor, err := ExecutePlan(ctx, p.GetOuter(), store, evalCtx, outerCont, nestedProps)
	if err != nil {
		return nil, err
	}

	cursor, err := newFlatMapCursorForPlan(
		outerCursor, p.GetOuter(), p.GetInner(), store, evalCtx,
		p.GetOuterAlias(), p.GetInnerAlias(),
		p.GetResultValue(),
		nestedProps,
		p.InheritOuterRecordProperties(),
		p.NullSupplyingOuter(),
	)
	if err != nil {
		outerCursor.Close()
		return nil, err
	}
	cursor.initialInnerCont = innerCont
	cursor.hasPendingInner = innerCont != nil
	cursor.pendingCheckValue = checkValue
	// Seed lastOuterContinuation from the saved OuterContinuation whenever one is
	// present — NOT only when an inner continuation is also present. The very next
	// outer advance copies it into priorOuterContinuation (the position AT the
	// resumed outer row), which is what a subsequent mid-inner buildContinuation
	// pairs with the check_value. If a page ended via wrapOuterContinuation (outer
	// hit an out-of-band limit → innerCont nil but outerCont set), gating on
	// innerCont != nil left priorOuterContinuation nil on the resumed page, so the
	// next mid-inner continuation encoded an EMPTY OuterContinuation (resume from
	// the start) while carrying a non-start check_value → check_value mismatch on
	// the following resume.
	if outerCont != nil {
		cursor.lastOuterContinuation = recordlayer.NewBytesContinuation(outerCont)
	}
	return applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit), nil
}

func executeNestedLoopJoin(
	ctx context.Context,
	p *plans.RecordQueryNestedLoopJoinPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	outputLayout, err := p.ProvidedOutputLayout()
	if err != nil {
		return nil, fmt.Errorf("nested loop join output layout: %w", err)
	}
	outputSourceOrigins, err := nestedLoopJoinOutputSourceOrigins(p, outputLayout)
	if err != nil {
		return nil, err
	}
	// Materialize the inner side once (typically the smaller table).
	// Clear TimeLimit for inner — the inner must be fully materialized
	// within this transaction. Java's FlatMapPipelinedCursor also
	// materializes the inner fully per outer row.
	innerProps := props.ClearSkipAndLimit().ClearRowAndTimeLimits()
	innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, nil, innerProps)
	if err != nil {
		return nil, err
	}
	innerRows, innerCharged, err := CollectAllBounded(ctx, innerCursor, props.State, props.GetMaterializationLimit(), "nested loop join inner side")
	innerCursor.Close()
	if err != nil {
		props.State.ReleaseMemory(innerCharged)
		return nil, err
	}

	// Decode the NLJ page continuation (nljContinuation: a
	// FlatMapContinuation envelope wrapping the outer position, the current
	// outer's check value, and the tuple-packed inner position). Anything
	// else — including the retired pre-continuation binary's one-byte fake
	// marker — declines loudly: forwarding unrecognized bytes to the outer
	// child risks a key-value cursor's raw-suffix fallback silently
	// accepting them as a scan position (wrong rows).
	if p.GetJoinType() == plans.JoinFullOuter && len(continuation) > 0 {
		// No FULL OUTER continuation resumes — the cross-outer matchedInner
		// bitmap has no serialized form. New tokens carry the declining FULL
		// marker, but a token minted by a PREVIOUS binary version packs
		// ordinary mid-inner state; decoding it here would rebuild the
		// bitmap zeroed and re-pad already-matched inner rows. Join-type
		// context is the authority, not the token's own claim.
		props.State.ReleaseMemory(innerCharged)
		return nil, &UnsupportedContinuationError{Shape: "nested loop join FULL OUTER (does not resume)"}
	}
	outerContinuation, nljResume, decodeErr := decodeNLJContinuation(continuation)
	if decodeErr != nil {
		props.State.ReleaseMemory(innerCharged)
		return nil, decodeErr
	}

	// Stream the outer side one row at a time via nljCursor.
	outerProps := props.ClearSkipAndLimit()
	if p.GetJoinType() == plans.JoinFullOuter {
		// FULL OUTER accumulates cross-outer match state (the matchedInner
		// bitmap) that drives the post-outer drain phase, and that state is
		// NOT serialized into the continuation. The driver rebuilds the
		// cursor from scratch on each transaction page, which would reset
		// the bitmap mid-scan and produce wrong drain results. Clear the
		// outer's time/row limits so the whole FULL join completes within a
		// single transaction (one cursor, one bitmap). Very large FULL joins
		// then fail loudly at FDB's 5s transaction limit rather than
		// returning silently-wrong rows — the same limitation class as the
		// materialized inner side above. INNER/LEFT/RIGHT are unaffected:
		// they carry no cross-outer state and resume correctly per outer row.
		//
		// Consequence: with limits cleared the outer always runs to
		// SourceExhausted in one transaction and never emits a partial
		// continuation, so a fresh FULL query passes continuation=nil here
		// and the driver can never hand a FULL OUTER continuation back. The
		// `continuation` arg below is thus effectively always nil for FULL;
		// it is passed through unconditionally only for code uniformity.
		outerProps = outerProps.ClearRowAndTimeLimits()
	}
	outerCursor, err := ExecutePlan(ctx, p.GetOuter(), store, evalCtx, outerContinuation, outerProps)
	if err != nil {
		props.State.ReleaseMemory(innerCharged)
		return nil, err
	}

	cursor, err := newNLJCursor(
		outerCursor, innerRows,
		p.GetJoinType(), p.GetOuterAlias(), p.GetInnerAlias(),
		p.GetPredicates(), p.GetResultValue(), outputLayout, outputSourceOrigins,
		evalCtx, props.State,
	)
	if err != nil {
		outerCursor.Close()
		props.State.ReleaseMemory(innerCharged)
		return nil, err
	}
	// Live-bytes model: the inner side is re-materialized (and re-charged)
	// on every page against the statement-wide state; the cursor releases
	// its charges — inner rows plus any hash index — at page teardown. ADD
	// to the tally: newNLJCursor already accumulated the hash-index charges.
	cursor.chargedBytes += innerCharged
	cursor.armResume(outerContinuation, nljResume)
	return applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit), nil
}

// nestedLoopJoinOutputSourceOrigins proves which selected child supplies each
// null-supplying output-layout window, record or scalar. The proof preserves
// correlation kind and complete row shape; it permits only the top-level
// nullability widening licensed by a null-supplying join edge and the existing
// checked logical/physical record-name normalization. Exactly one child may
// claim a source. No display name participates.
//
// The resulting mapping is not used to fetch a value; the source window in
// outputLayout remains the sole value authority. It answers only whether that
// source was absent because its physical leg (or a nested child leg) did not
// match.
func nestedLoopJoinOutputSourceOrigins(
	p *plans.RecordQueryNestedLoopJoinPlan,
	outputLayout values.OrdinalLayout,
) (map[values.CorrelationIdentifier]outputSourceOrigin, error) {
	if p == nil || outputLayout == nil {
		return nil, layoutBindingError(values.LayoutCarrierMismatch,
			"nested-loop join source-origin proof has no selected output layout")
	}
	if err := values.ValidateOrdinalLayoutAdmission(outputLayout); err != nil {
		return nil, fmt.Errorf("nested loop join source-origin layout: %w", err)
	}
	legs := []nljChildLeg{
		{
			plan: p.GetOuter(), alias: p.GetOuterAlias(),
			nullSupplying: p.GetJoinType() == plans.JoinFullOuter,
		},
		{
			plan: p.GetInner(), alias: p.GetInnerAlias(),
			nullSupplying: p.GetJoinType() == plans.JoinLeftOuter ||
				p.GetJoinType() == plans.JoinFullOuter,
		},
	}
	childLayouts := make([]values.OrdinalLayout, len(legs))
	for i := range legs {
		if legs[i].plan == nil || legs[i].alias.IsZero() {
			continue
		}
		layout, err := legs[i].plan.ProvidedOutputLayout()
		if err != nil {
			return nil, fmt.Errorf("nested loop join child %s output layout: %w", legs[i].alias, err)
		}
		childLayouts[i] = layout
	}

	origins := make(map[values.CorrelationIdentifier]outputSourceOrigin)
	for _, outputSource := range outputLayout.WindowSources() {
		if values.FlowedExactType(outputSource) == nil {
			continue
		}
		outputNullSupplying, outputNullErr := values.LayoutWindowNullSupplying(
			outputLayout, outputSource)
		if outputNullErr != nil {
			return nil, fmt.Errorf("nested-loop join output source: %w", outputNullErr)
		}
		if !outputNullSupplying {
			// Presence origins exist solely for null-supplying output windows.
			// An ordinary retained source is bound directly from the emitted row.
			continue
		}
		origin, claimed, err := findClaimedNLJChildOrigin(outputSource, legs, childLayouts)
		if err != nil {
			return nil, err
		}
		if claimed {
			origins[outputSource.Correlation()] = origin
			continue
		}
		origin, buried, err := findBuriedNLJLegOrigin(outputLayout, outputSource, legs)
		if err != nil {
			return nil, err
		}
		if buried {
			origins[outputSource.Correlation()] = origin
		}
	}
	if len(origins) == 0 {
		return nil, nil
	}
	return origins, nil
}

type nljChildLeg struct {
	plan          plans.RecordQueryPlan
	alias         values.CorrelationIdentifier
	nullSupplying bool
}

func findClaimedNLJChildOrigin(
	outputSource values.QuantifiedObjectValue,
	legs []nljChildLeg,
	childLayouts []values.OrdinalLayout,
) (outputSourceOrigin, bool, error) {
	claim := -1
	var claimedSource values.QuantifiedObjectValue
	claimedNullSupplying := false
	for legIndex, childLayout := range childLayouts {
		if childLayout == nil {
			continue
		}
		for _, childSource := range childLayout.WindowSources() {
			if childSource == nil || childSource.Correlation() != outputSource.Correlation() {
				continue
			}
			expectedSource := childSource
			if legs[legIndex].nullSupplying {
				expectedType := values.WithNullability(childSource.FlowedType(), true)
				if expectedType == nil {
					return outputSourceOrigin{}, false, layoutBindingError(values.CorrelationTypeConflict,
						"nested-loop join retained source has no nullable selected-child type")
				}
				if !values.FlowedTypeEquals(childSource, expectedType) {
					widenedSource, widenErr := values.NewQuantifiedObjectValue(
						childSource.Correlation(), expectedType)
					if widenErr != nil {
						return outputSourceOrigin{}, false, fmt.Errorf("nested-loop join retained source widening: %w", widenErr)
					}
					expectedSource = widenedSource
				}
			}
			compatible := values.FlowedTypesEqual(expectedSource, outputSource)
			if !compatible {
				normalized, normalizeErr := values.TranslateLogicalSourceNameNormalization(
					expectedSource, expectedSource.Correlation(), outputSource)
				if normalizeErr != nil {
					return outputSourceOrigin{}, false, fmt.Errorf("nested-loop join retained source normalization: %w", normalizeErr)
				}
				compatible = normalized == outputSource
			}
			if !compatible {
				return outputSourceOrigin{}, false, layoutBindingError(values.CorrelationTypeConflict,
					"nested-loop join retained source disagrees with its selected child")
			}
			if claim >= 0 && claim != legIndex {
				return outputSourceOrigin{}, false, layoutBindingError(values.LayoutInvalidWindow,
					"two nested-loop join children claim one retained output source")
			}
			childNullSupplying, nullErr := values.LayoutWindowNullSupplying(childLayout, childSource)
			if nullErr != nil {
				return outputSourceOrigin{}, false, fmt.Errorf("nested-loop join retained child source: %w", nullErr)
			}
			claim = legIndex
			claimedSource = childSource
			claimedNullSupplying = childNullSupplying
		}
	}
	if claim < 0 {
		return outputSourceOrigin{}, false, nil
	}
	return outputSourceOrigin{
		topLegAlias:        legs[claim].alias,
		childSource:        claimedSource,
		childNullSupplying: claimedNullSupplying,
	}, true, nil
}

func findBuriedNLJLegOrigin(
	outputLayout values.OrdinalLayout,
	outputSource values.QuantifiedObjectValue,
	legs []nljChildLeg,
) (outputSourceOrigin, bool, error) {
	// No child binds the source itself: it is buried in a null-supplying
	// leg's retained box (the layout inherited the leg's edge for it), and
	// the child that implements the box need not keep it as its own row.
	// Its presence is the leg's.
	buriedIn := -1
	for legIndex, leg := range legs {
		if leg.alias.IsZero() || !leg.nullSupplying {
			continue
		}
		var legWindow values.QuantifiedObjectValue
		for _, source := range outputLayout.WindowSources() {
			if source.Correlation() == leg.alias {
				legWindow = source
				break
			}
		}
		if legWindow == nil {
			continue
		}
		within, withinErr := values.LayoutWindowWithin(outputLayout, outputSource, legWindow)
		if withinErr != nil {
			return outputSourceOrigin{}, false, fmt.Errorf("nested-loop join buried output source: %w", withinErr)
		}
		if !within {
			continue
		}
		if buriedIn >= 0 {
			return outputSourceOrigin{}, false, layoutBindingError(values.LayoutInvalidWindow,
				"two nested-loop join legs enclose one retained output source")
		}
		buriedIn = legIndex
	}
	if buriedIn < 0 {
		return outputSourceOrigin{}, false, nil
	}
	return outputSourceOrigin{topLegAlias: legs[buriedIn].alias}, true, nil
}

// legFieldName names a leg's merged column: a bare-scalar UNNEST element (a
// 1-field `_0` row — `t.arr AS X`) is renamed to its AS alias so a downstream
// BARE read of the alias ("X") resolves to the element directly. Any other field
// keeps its own name.
func legFieldName(fieldName, alias string, legWidth int) string {
	if alias != "" && legWidth == 1 && fieldName == values.OrdinalFieldName(0) {
		return unicode15.ToUpperString(alias)
	}
	return fieldName
}

// concatLegPositionals builds a leg-windowed merged PositionalRow by CONCATENATING
// two leg rows' own Positionals (parallel construction). The merged Type carries
// top-level Legs [outerAlias@[0,Wo),
// innerAlias@[Wo,Wo+Wi)] so a qualified reference ("LA.K") binds its leg's window
// (rowLegsBinder / legWindowBinder); a bare read reads its baked slot. Nested Legs (a leg
// that is itself a merge) are preserved, the inner leg's shifted by Wo. Returns nil
// when either leg lacks a Positional. This is the merge for a NON-build join
// cursor (the nljCursor's build path OVERWRITES .Positional when a gated join
// has an ordinal seed).
func concatLegPositionals(outer, inner *PositionalRow, outerAlias, innerAlias values.CorrelationIdentifier) *PositionalRow {
	if outer == nil || inner == nil || outer.Type == nil || inner.Type == nil {
		return nil
	}
	nOuter := len(outer.Type.Fields)
	nInner := len(inner.Type.Fields)
	fields := make([]values.Field, 0, nOuter+nInner)
	for i, f := range outer.Type.Fields {
		fields = append(fields, values.Field{Name: legFieldName(f.Name, outerAlias.Name(), nOuter), FieldType: f.FieldType, Ordinal: i})
	}
	for i, f := range inner.Type.Fields {
		fields = append(fields, values.Field{Name: legFieldName(f.Name, innerAlias.Name(), nInner), FieldType: f.FieldType, Ordinal: nOuter + i})
	}
	slots := make([]any, 0, len(outer.Slots)+len(inner.Slots))
	slots = append(slots, outer.Slots...)
	slots = append(slots, inner.Slots...)
	legs := make([]values.RecordTypeLeg, 0, len(outer.Type.Legs)+len(inner.Type.Legs)+2)
	if !outerAlias.IsZero() {
		legs = append(legs, values.NewRecordTypeLeg(values.LegKindFlatRun, outerAlias, outerAlias.Name(), 0, nOuter))
	}
	if !innerAlias.IsZero() {
		legs = append(legs, values.NewRecordTypeLeg(values.LegKindFlatRun, innerAlias, innerAlias.Name(), nOuter, nInner))
	}
	legs = append(legs, outer.Type.Legs...)
	for _, lg := range inner.Type.Legs {
		// The leg's identity is CARRIED, not re-minted: only its offset moves,
		// because the inner row's slots now start at nOuter.
		legs = append(legs, values.NewRecordTypeLeg(lg.Kind, lg.Alias, lg.Name, lg.Start+nOuter, lg.Width))
	}
	return &PositionalRow{Type: &values.RecordType{Fields: fields, Legs: legs}, Slots: slots}
}

func mergeRows(outer, inner QueryResult, outerAlias, innerAlias values.CorrelationIdentifier) QueryResult {
	// A join merge's output IS a leg-windowed PositionalRow — the two
	// leg rows' own Positionals concatenated (concatLegPositionals), with leg windows
	// so a qualified read (`A.ID`) resolves to its leg and a bare read by name-in-row.
	return QueryResult{
		Positional: concatLegPositionals(outer.Positional, inner.Positional, outerAlias, innerAlias),
		Record:     outer.Record,
		PrimaryKey: outer.PrimaryKey,
	}
}

// spansFromMergedLegs derives the per-leg windows from a merged row's own leg
// metadata (RecordType.Legs), for the non-build join-predicate path: each leg's
// alias maps to its window [Start, Start+Width) with the sub-slice of fields as
// its leg type, so a QOV(leg).col reference resolves leg-locally over the flat
// merged row.
func spansFromMergedLegs(pos *PositionalRow) []legSpan {
	if pos == nil || pos.Type == nil {
		return nil
	}
	spans := make([]legSpan, 0, len(pos.Type.Legs))
	for _, leg := range pos.Type.Legs {
		end := leg.Start + leg.Width
		if leg.Start < 0 || end > len(pos.Type.Fields) {
			continue
		}
		legFields := make([]values.Field, leg.Width)
		for i := 0; i < leg.Width; i++ {
			f := pos.Type.Fields[leg.Start+i]
			f.Ordinal = i
			legFields[i] = f
		}
		if h := values.Census(); h != nil {
			// The re-mint's delta: what the leg's identity WAS at construction
			// against what re-minting its text would have produced.
			h.LegIdentityLeg(leg)
		}
		spans = append(spans, legSpan{
			// The leg's identity is CARRIED, not re-minted from leg.Name. Re-minting
			// gave the span a second spelling of the same leg — the producer chose one
			// identifier and the consumer manufactured another from its text — and a
			// second spelling is how an alias-qualified read binds a different leg's
			// slots than the one it names.
			Alias: leg.Alias,
			// The KIND is carried from the leg table for the same reason the Alias
			// is: this span is a reading of a leg somebody else described, not a
			// fresh description.
			Kind:    leg.Kind,
			LegType: &values.RecordType{Fields: legFields},
			Offset:  leg.Start,
			Width:   leg.Width,
		})
	}
	return spans
}

// passesJoinPredicatesLegs evaluates a join's residual predicates against the
// merged leg-windowed positional row, with the ordinal-build leg bindings.
// legs nil (the non-build merged-row path)
// resolves through the merged row's leg windows (spansFromMergedLegs below).
// legs non-nil (an ordinal-build cursor) evaluates the predicates against a
// RowEvalContext carrying the DIRECT per-leg bindings (the cursor's pre-built
// twoLegBinder — predicates need no windows at build; legs are
// PRE-adapted, one small binder per pair, never a map or a re-adaptation): a
// lazy leg reference QOV(leg).col resolves leg-relative against the adapted leg
// row (correct even for the second leg), a BAKED one by its baked ordinal, an
// outer correlation via the binder's base, and a qualified read ("A.ID", a flat
// FieldValue) resolves via the merged row's leg windows.
// legs is the CONCRETE *twoLegBinder (not the CorrelationBinder interface) so
// the cursor's `var pair *twoLegBinder` typed-nil passes as a genuine nil —
// an interface-typed param would make a typed-nil non-nil and route the
// non-build path through a nil binder.
func passesJoinPredicatesLegs(combined QueryResult, preds []predicates.QueryPredicate, evalCtx *EvaluationContext, legs *twoLegBinder) (bool, error) {
	if len(preds) == 0 {
		return true, nil
	}
	// Resolve join/EXISTS predicates against the merged row's ordinal
	// Positional. A build-active cursor supplies a
	// per-leg binder (legs); a non-build merge derives leg windows from the merged
	// row's own leg metadata (Type.Legs) so a QOV(leg).col — e.g. QOV(B).ID —
	// resolves to leg B's window instead of first-matching leg A's bare "ID" on the
	// flat merged row. An outer correlation / param / scalar subquery resolves
	// through evalCtx.
	var rowCtx any
	switch {
	case legs != nil:
		rc := &values.RowEvalContext{Positional: combined.Positional, Correlations: legs}
		if evalCtx != nil {
			rc.Binder = evalCtx
			rc.ScalarSubqueries = evalCtx.scalarSubqueries
			rc.Clock = evalCtx
			if len(evalCtx.quantifiedBindings) > 0 {
				rc.Objects = legs
			}
		}
		rowCtx = rc
	case combined.Positional != nil && combined.Positional.Type != nil && len(combined.Positional.Type.Legs) > 0:
		rowCtx = legWindowRowContext(combined.Positional, evalCtx, spansFromMergedLegs(combined.Positional))
	default:
		rc := &values.RowEvalContext{Positional: combined.Positional}
		if evalCtx != nil {
			rc.Correlations = evalCtx
			rc.Binder = evalCtx
			rc.ScalarSubqueries = evalCtx.scalarSubqueries
			rc.Clock = evalCtx
		}
		rowCtx = rc
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
}
