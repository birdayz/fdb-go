// Portions derived from FoundationDB Record Layer (RecordQueryFlatMapPlan.java,
// ImplementNestedLoopJoinRule.java, FieldValue.java, PartitionSelectRule.java,
// and others),
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import "fdb.dev/internal/recordlayer/query/plan/cascades/values"

// legWindowRow is a leg-relative view over the join's merged positional row:
// leg ordinal i reads merged slot Offset+i. A correlated baked reference
// (FieldValue(QOV(leg), col) carrying a LEG-LOCAL ordinal) bound to a leg of a
// merged/concatenated row reads its source's own slot through this window —
// Java's rewire-by-ordinal semantics for the two-level NLJ→FlatMap lowering.
//
// It implements values.OrdinalRow (Get leg-relative), so it slots into the
// existing evaluateCorrelated binder arm with no new eval arm, and a miss
// stays loud (the (nil,false) return becomes an OrdinalResolutionError in
// evaluateOrdinal). TypeNames feeds that error's diagnostics via the
// values.ordinalRowNames optional interface, reporting the LEG's columns
// (what this window actually exposes).
type legWindowRow struct {
	parent  values.OrdinalRow
	legType *values.RecordType
	// parentType is the LAZY alternative to legType, for producers that already
	// hold the MERGED row's type and would otherwise have to slice a fresh
	// per-leg *RecordType out of it just to fill legType.
	//
	// legType is read by exactly one thing — TypeNames, which feeds the column
	// names into an OrdinalResolutionError. That is an ERROR path. Building a
	// []values.Field copy and a *RecordType per leg per OUTER ROW to service it
	// is the whole cost of a diagnostic that almost never renders, so a producer
	// may hand over the merged type and the window slices the names on demand
	// instead. legType still wins when set: producers that genuinely have a
	// per-leg type (the leg spans derived from an ordinal join RC) pass it, and
	// its fields are the leg's own re-ordinalized ones.
	parentType *values.RecordType
	offset     int
	width      int
	// siblingLegs marks a window whose merged row bound TWO OR MORE legs — the
	// box-gather shape, as opposed to a merged row carrying a single leg.
	//
	// Stamped by the producer, which is the only place the row's leg COUNT is in
	// hand: a reader holds one window and cannot tell a lone leg from one of
	// several, and the aliases it could ask about collide across queries. It is
	// set only while the leg-identity census gate is on, because its sole consumer
	// is that census's activation criterion.
	siblingLegs bool
	// shadowsExisting marks a window that DISPLACED a binding the incoming context
	// already carried for the same alias, and shadowed holds what it displaced.
	//
	// This is the structural marker for "a second resolution route exists": when a
	// window shadows, the alias was already resolvable WITHOUT the binder. The
	// binder's shadowing semantics are documented in DIVERGENCES.md.
	//
	// Recorded only while the leg-identity census gate is on; the map probe is not
	// something the per-outer-row path should pay for in production.
	shadowsExisting bool
	shadowed        any
	// misaimed marks a window whose span was deliberately moved off its own leg by
	// misaimMergedLegWindows, so a read that resolves to it is reading the WRONG
	// slots. Set only under EvaluationContext.WithMergedLegWrongWindows, which is
	// itself honoured only while the leg-identity census gate is on.
	//
	// It travels on the window rather than being inferred at the read site because
	// the reader holds one window and no leg table: it cannot tell a window aimed at
	// its own leg from one aimed at its sibling's, and that distinction is the whole
	// content of the wrong-window instrument's engagement floor.
	misaimed bool
}

// Get reads the leg-relative ordinal: merged slot offset+ord. Out-of-range leg
// ordinals return (nil, false) — evaluateOrdinal turns that into a loud
// values.OrdinalResolutionError, never a silent NULL.
func (w *legWindowRow) Get(ord int) (any, bool) {
	if ord < 0 || ord >= w.width {
		return nil, false
	}
	return w.parent.Get(w.offset + ord)
}

// TypeNames returns the LEG type's column names — the diagnostics the
// values.OrdinalResolutionError enrichment (values.ordinalRowNames) reads via
// optional-interface assertion.
func (w *legWindowRow) TypeNames() []string {
	if w.legType != nil {
		return typeFieldNames(w.legType)
	}
	// The lazy form: slice this window's names out of the MERGED type. Bounds
	// are re-checked rather than assumed — this runs while building an error
	// message, and a diagnostic that panics loses the error it was describing.
	if w.parentType == nil {
		return nil
	}
	end := w.offset + w.width
	if w.offset < 0 || w.width < 0 || end > len(w.parentType.Fields) {
		return nil
	}
	out := make([]string, w.width)
	for i := range out {
		out[i] = w.parentType.Fields[w.offset+i].Name
	}
	return out
}

// legWindowBinder is the correlation binder for uppers over the ordinal join:
// a reference to a leg alias is bound to that leg's window over the merged
// row, anything else delegates to base. This is the runtime half of Java's
// rewire-by-ordinal semantics for Go's two-level NLJ→FlatMap lowering: a
// baked QOV(leg).col reference carries a LEG-LOCAL ordinal, and the binder
// supplies the LEG's own slice of the merged row for that ordinal to read.
type legWindowBinder struct {
	base  values.CorrelationBinder
	spans []legSpan
	row   values.OrdinalRow
}

// exactLocalBinding resolves one declared leg window and returns the exact
// record type which owns that window. The boolean means the correlation is
// locally claimed; a claimed malformed window is an error and must not fall
// through to an enclosing binding with the same alias.
func (b *legWindowBinder) exactLocalBinding(
	id values.CorrelationIdentifier,
) (values.OrdinalRow, *values.RecordType, bool, error) {
	for _, s := range b.spans {
		if !values.SameLeg(s.Alias, id) {
			continue
		}
		if s.LegType != nil {
			if w, ok := buriedLegWindow(b.row, s, id); ok {
				return w, w.legType, true, nil
			}
		}
		if s.LegType == nil || s.Offset < 0 || s.Width < 0 || s.Width != len(s.LegType.Fields) {
			return nil, nil, true, layoutBindingError(values.LayoutRuntimeShape,
				"local leg window has no exact record type or invalid bounds")
		}
		return &legWindowRow{parent: b.row, legType: s.LegType, offset: s.Offset, width: s.Width}, s.LegType, true, nil
	}
	for _, s := range b.spans {
		if s.LegType == nil {
			continue
		}
		for _, buried := range s.LegType.Legs {
			if !values.SameLeg(buried.Alias, id) {
				continue
			}
			w, ok := buriedLegWindow(b.row, s, id)
			if !ok {
				return nil, nil, true, layoutBindingError(values.LayoutRuntimeShape,
					"buried local leg window has invalid bounds")
			}
			return w, w.legType, true, nil
		}
	}
	return nil, nil, false, nil
}

// GetQuantifiedBinding gives exact QOV reads the same local-leg precedence as
// GetCorrelationBinding, while rejecting a same-alias foreign exact type
// before it can borrow either this row or an enclosing binding.
func (b *legWindowBinder) GetQuantifiedBinding(view values.QuantifiedObjectValue) (any, bool, error) {
	exact, ok := values.AsQuantifiedObjectValue(view)
	if !ok || exact == nil {
		return nil, false, layoutBindingError(values.CorrelationForeignValue, "leg-window lookup QOV is not exact")
	}
	window, declared, claimed, err := b.exactLocalBinding(exact.Correlation())
	if err != nil {
		return nil, false, err
	}
	if claimed {
		if declared == nil || !values.FlowedRowShapeEquals(exact, declared) {
			return nil, false, layoutBindingError(values.CorrelationTypeConflict,
				"leg-window lookup type disagrees with the local exact leg")
		}
		return window, true, nil
	}
	return (&evaluationObjectBinder{base: b.base}).GetQuantifiedBinding(view)
}

func (b *legWindowBinder) IsExplicitNullQuantifiedBinding(view values.QuantifiedObjectValue) (bool, error) {
	exact, ok := values.AsQuantifiedObjectValue(view)
	if !ok || exact == nil {
		return false, layoutBindingError(values.CorrelationForeignValue, "leg-window absence QOV is not exact")
	}
	_, declared, claimed, err := b.exactLocalBinding(exact.Correlation())
	if err != nil {
		return false, err
	}
	if claimed {
		if declared == nil || !values.FlowedRowShapeEquals(exact, declared) {
			return false, layoutBindingError(values.CorrelationTypeConflict,
				"leg-window absence type disagrees with the local exact leg")
		}
		return false, nil
	}
	return (&evaluationObjectBinder{base: b.base}).IsExplicitNullQuantifiedBinding(view)
}

// GetCorrelationBinding implements values.CorrelationBinder: a span alias gets
// its leg window over the merged row; any other alias delegates to base (nil
// base: unbound). Routing precedence (a source-relative baked QOV(leg).col
// carries a LEG-LOCAL ordinal, so the window must be the LEG's OWN slice):
//  1. a span alias match serves the BOX-LEAF sub-window when the span is a
//     clustered box named by its rightmost leaf (the whole-concat window would
//     misread a leg-local ordinal against an earlier buried leg's slots);
//  2. a BURIED leg inside any span serves its sub-window at the buried offset.
func (b *legWindowBinder) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	for _, s := range b.spans {
		// Exact, via the one identity comparison: both sides are correlation
		// IDENTIFIERS (canonical UPPER for user aliases, verbatim lowercase q$N for
		// machine mints). A fold here would let a quoted "q$5" user alias cross into
		// the machine namespace.
		if !values.SameLeg(s.Alias, id) {
			continue
		}
		if s.LegType != nil {
			if w, ok := buriedLegWindow(b.row, s, id); ok {
				return w, true
			}
		}
		return &legWindowRow{parent: b.row, legType: s.LegType, offset: s.Offset, width: s.Width}, true
	}
	// A BURIED leg inside a clustered box span — ordered AFTER the span
	// aliases (a top-level alias always wins its own name).
	for _, s := range b.spans {
		if s.LegType == nil {
			continue
		}
		if w, ok := buriedLegWindow(b.row, s, id); ok {
			return w, true
		}
	}
	if b.base != nil {
		return b.base.GetCorrelationBinding(id)
	}
	return nil, false
}

// buriedLegWindow serves the sub-window of the buried leg named `alias` within
// a clustered box span's flat concat (RecordType.Legs bounds), or ok=false.
// Malformed bounds are a MISS, never the run-wide window: a whole-concat read
// would silently serve another leg's slots.
//
// A miss is NOT uniformly loud downstream — the disposition depends on the
// reference kind that failed to bind (values.FieldValue.evaluate, the
// RowEvalContext arm):
//   - an UNPINNED leg-relative baked root over a multi-leg row: loud
//     (UnboundEvalContextError), because its ordinal addresses its source's own
//     window and a whole-row read would be a wrong-slot answer;
//   - a lazy (unbaked) reference with no positional row: loud, same error;
//   - a FRONTIER-PINNED baked root: NOT loud. It falls through to a positional
//     read against the whole merged row, which is correct precisely when the
//     ordinal already indexes that row (the plan-time-baked box case) and wrong
//     when it does not.
//
// So the miss path is correct-or-loud for everything except a frontier-pinned
// ref, whose correctness rests on the baker having pinned it against the row it
// is now read from. That is why the typed identity matters here: an unstated
// leg alias turns a bind into a miss, and for the pinned kind the miss is
// silent.
func buriedLegWindow(row values.OrdinalRow, s legSpan, alias values.CorrelationIdentifier) (*legWindowRow, bool) {
	for _, bl := range s.LegType.Legs {
		if h := values.Census(); h != nil {
			// RETIRED PREDICATE: `bl.Name == alias` — this function took the alias as a
			// STRING and compared it exactly against the buried leg's text.
			h.LegIdentityConversion(values.LegSiteBuriedLegWindow,
				bl.Alias, alias, bl.Name == alias.Name())
			h.LegIdentityLeg(bl)
		}
		if !values.SameLeg(bl.Alias, alias) {
			continue
		}
		end := bl.Start + bl.Width
		if bl.Start < 0 || end > len(s.LegType.Fields) {
			return nil, false
		}
		sub := &values.RecordType{Nullable: s.LegType.Nullable, Fields: s.LegType.Fields[bl.Start:end]}
		return &legWindowRow{parent: row, legType: sub, offset: s.Offset + bl.Start, width: bl.Width}, true
	}
	return nil, false
}

// legWindowRowContext builds the downstream per-row eval context over the
// join's merged positional row: like EvaluationContext.RowContextPositional,
// but Correlations is the legWindowBinder — an upper's direct leg
// reference (FieldValue(QOV(leg), col), lazy or baked) resolves LEG-LOCALLY
// through its window, an outer correlation delegates to the base
// EvaluationContext, and only an unbound (join-quantifier) reference falls to
// the bare merged row. Applied UNCONDITIONALLY on windowed inputs — even with
// no param/subquery/outer binding in play — because the leg references NEED
// the Correlations bindings (the bare merged row misreads them leg-relative:
// a wrong-slot hazard).
func legWindowRowContext(pos values.OrdinalRow, ec *EvaluationContext, spans []legSpan) *values.RowEvalContext {
	legs := &legWindowBinder{base: correlationBase(ec), spans: spans, row: pos}
	rc := &values.RowEvalContext{
		Positional:   &spanAwareRow{parent: pos, spans: spans},
		Correlations: legs,
	}
	if ec != nil {
		rc.Binder = ec
		rc.ScalarSubqueries = ec.scalarSubqueries
		rc.Clock = ec
		// Installing an exact object binder changes QOV evaluation from
		// correlation-then-positional to exact-only. Do so only when there is an
		// ambient exact object to preserve; a current-only predicate deliberately
		// retains the positional fallback until its producer supplies an exact
		// current carrier handle.
		if len(ec.quantifiedBindings) > 0 {
			rc.Objects = legs
		}
	}
	return rc
}

// spanAwareRow is the merged positional row as the eval context's Positional:
// ordinal access passes through to the merged row untouched; MultiLeg exposes
// the leg-window structure to the values-side correlated fall-through guard
// (a source-relative baked ordinal must not read a multi-leg row bare — its
// ordinal addresses one leg's own window). Every column REFERENCE over the
// merged row resolves by plan-time-baked ordinal (a flat reference by its
// merged-row slot, a correlated leg reference through its leg window via the
// legWindowBinder Correlations) — there is no name-keyed read arm.
type spanAwareRow struct {
	parent values.OrdinalRow
	spans  []legSpan
}

func (r *spanAwareRow) Get(ord int) (any, bool) { return r.parent.Get(ord) }

// MultiLeg reports whether the merged row spans MORE than one leg window (or a
// single leg narrower than the row) — the values-side correlated fall-through
// guard's probe (see PositionalRow.MultiLeg).
func (r *spanAwareRow) MultiLeg() bool {
	if len(r.spans) == 0 {
		return false
	}
	if len(r.spans) == 1 && r.spans[0].Offset == 0 {
		if tn, ok := r.parent.(interface{ TypeNames() []string }); ok && r.spans[0].Width == len(tn.TypeNames()) {
			return false
		}
	}
	return true
}

// TypeNames feeds OrdinalResolutionError diagnostics (values.ordinalRowNames).
func (r *spanAwareRow) TypeNames() []string {
	if tn, ok := r.parent.(interface{ TypeNames() []string }); ok {
		return tn.TypeNames()
	}
	return nil
}
