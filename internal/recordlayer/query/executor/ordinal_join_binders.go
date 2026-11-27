// Portions derived from FoundationDB Record Layer (RecordQueryFlatMapPlan.java,
// ImplementNestedLoopJoinRule.java, FieldValue.java, PartitionSelectRule.java,
// and others),
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"fmt"

	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
)

// outputSourcePresenceBinder translates only the physical presence question
// for a proof-backed retained source to the selected child leg which supplied
// it. It is deliberately not an evaluation binder: the value is read from its
// exact output window after the row has been built. Presence instead follows
// the leg and, for a nested null-supplying source, the child row's exact match
// state. A matched all-NULL object remains distinct from an absent object.
type outputSourcePresenceBinder struct {
	base    values.CorrelationBinder
	origins map[values.CorrelationIdentifier]outputSourceOrigin
	err     error
}

func (b *outputSourcePresenceBinder) GetCorrelationBinding(
	id values.CorrelationIdentifier,
) (any, bool) {
	if b == nil || b.base == nil {
		return nil, false
	}
	if origin, ok := b.origins[id]; ok {
		value, present := b.base.GetCorrelationBinding(origin.topLegAlias)
		if !present {
			b.err = layoutBindingError(values.LayoutPresenceMissing,
				"retained output source origin leg is absent")
			return nil, false
		}
		if value == nil || origin.childSource == nil {
			return value, true
		}
		row, ok := value.(*PositionalRow)
		if !ok || row == nil || row.Layout == nil {
			b.err = layoutBindingError(values.LayoutRuntimeShape,
				"retained output source origin is not an exact positional row")
			return nil, false
		}
		childNullSupplying, childErr := values.LayoutWindowNullSupplying(
			row.Layout, origin.childSource,
		)
		if childErr != nil {
			b.err = fmt.Errorf("retained output source child layout: %w", childErr)
			return nil, false
		}
		if childNullSupplying != origin.childNullSupplying {
			b.err = layoutBindingError(values.LayoutInvalidWindow,
				"retained output source null-supplying proof disagrees with the child layout")
			return nil, false
		}
		if !origin.childNullSupplying {
			return value, true
		}
		matched, matchErr := values.OrdinalWindowMatchState(
			row.Layout, row.LayoutPresence, origin.childSource,
		)
		if matchErr != nil {
			b.err = fmt.Errorf("retained output source match state: %w", matchErr)
			return nil, false
		}
		if !matched {
			return nil, true
		}
		return value, true
	}
	return b.base.GetCorrelationBinding(id)
}

// twoLegBinder is the NLJ cursor's per-pair leg binder: exactly the join's
// two legs, pre-adapted rows plugged in per candidate pair — no map, no
// re-adaptation (the inner rows are a FIXED slice adapted once at cursor
// construction; the outer is adapted once per outer-row advance — per-pair
// adapter work would be a structural perf regression). A nil row IS the
// deliberately-NULL leg (LEFT/FULL padding): (nil, true). Non-leg correlations
// delegate to base.
// Scalar legs retain their transport rows but bind the datum inside them.
type twoLegBinder struct {
	outerID, innerID values.CorrelationIdentifier
	outer, inner     values.OrdinalRow
	outerType        values.Type
	innerType        values.Type
	base             values.CorrelationBinder
}

func (b *twoLegBinder) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	switch id {
	case b.outerID:
		if b.outer == nil {
			return nil, true // the NULL leg
		}
		if b.outerType != nil && b.outerType.Code() != values.TypeCodeRecord {
			return b.outer.Get(0)
		}
		return b.outer, true
	case b.innerID:
		if b.inner == nil {
			return nil, true // the NULL leg
		}
		if b.innerType != nil && b.innerType.Code() != values.TypeCodeRecord {
			return b.inner.Get(0)
		}
		return b.inner, true
	}
	if b.base != nil {
		return b.base.GetCorrelationBinding(id)
	}
	return nil, false
}

// GetQuantifiedBinding preserves the pair-local leg precedence of the legacy
// correlation binder in the exact namespace. A same-alias QOV with another
// exact type is a foreign owner and cannot fall through to the ambient context.
func (b *twoLegBinder) GetQuantifiedBinding(view values.QuantifiedObjectValue) (any, bool, error) {
	exact, ok := values.AsQuantifiedObjectValue(view)
	if !ok || exact == nil {
		return nil, false, layoutBindingError(values.CorrelationForeignValue, "join-pair lookup QOV is not exact")
	}
	switch exact.Correlation() {
	case b.outerID:
		if b.outerType == nil || !values.FlowedRowShapeEquals(exact, b.outerType) {
			return nil, false, layoutBindingError(values.CorrelationTypeConflict,
				fmt.Sprintf("outer join-pair lookup %s: read as %s, local leg %s",
					exact.Correlation().Name(), values.DescribeType(exact.FlowedType()),
					values.DescribeType(b.outerType)))
		}
		bound, present := b.GetCorrelationBinding(b.outerID)
		return bound, present, nil
	case b.innerID:
		if b.innerType == nil || !values.FlowedRowShapeEquals(exact, b.innerType) {
			return nil, false, layoutBindingError(values.CorrelationTypeConflict,
				fmt.Sprintf("inner join-pair lookup %s: read as %s, local leg %s",
					exact.Correlation().Name(), values.DescribeType(exact.FlowedType()),
					values.DescribeType(b.innerType)))
		}
		bound, present := b.GetCorrelationBinding(b.innerID)
		return bound, present, nil
	}
	return (&evaluationObjectBinder{base: b.base}).GetQuantifiedBinding(view)
}

func (b *twoLegBinder) IsExplicitNullQuantifiedBinding(view values.QuantifiedObjectValue) (bool, error) {
	exact, ok := values.AsQuantifiedObjectValue(view)
	if !ok || exact == nil {
		return false, layoutBindingError(values.CorrelationForeignValue, "join-pair absence QOV is not exact")
	}
	switch exact.Correlation() {
	case b.outerID:
		if b.outerType == nil || !values.FlowedRowShapeEquals(exact, b.outerType) {
			return false, layoutBindingError(values.CorrelationTypeConflict,
				"outer join-pair absence type disagrees with the local exact leg")
		}
		return b.outer == nil, nil
	case b.innerID:
		if b.innerType == nil || !values.FlowedRowShapeEquals(exact, b.innerType) {
			return false, layoutBindingError(values.CorrelationTypeConflict,
				"inner join-pair absence type disagrees with the local exact leg")
		}
		return b.inner == nil, nil
	}
	return (&evaluationObjectBinder{base: b.base}).IsExplicitNullQuantifiedBinding(view)
}

// buildLegBinder is the BUILD-time correlation binder: DIRECT per-leg bindings
// (predicates and result-value evaluation need no windows at
// build — each leg binds to its OWN leg-local row, so both baked (leg ordinal)
// and lazy (leg-relative resolveOrdinal) references read the right slot, even
// for the second leg). A key PRESENT with a nil value is the deliberately-NULL
// leg: GetCorrelationBinding returns (nil, true) and the baked node's
// `return bound, nil` arm yields NULL. Anything else
// delegates to base (outer correlations; nil base = unbound).
//
// The map-based binder is used ONLY for flatMap's one-shot evaluate (one
// binder per EMITTED row — no per-candidate cost there); the NLJ's per-pair
// hot path uses the fixed twoLegBinder.
type buildLegBinder struct {
	legs map[values.CorrelationIdentifier]values.OrdinalRow
	// raw carries bare-QOV non-record legs (the unnest element) bound to
	// their WHOLE flowed value — QOV(inner).Evaluate returns the scalar/struct
	// itself, not a positional row. A key present with a nil value is that leg's
	// NULL binding.
	raw  map[values.CorrelationIdentifier]any
	base values.CorrelationBinder
}

// GetCorrelationBinding implements values.CorrelationBinder.
func (b *buildLegBinder) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	if v, present := b.raw[id]; present {
		return v, true // raw leg — the whole flowed value (nil = NULL leg)
	}
	if row, present := b.legs[id]; present {
		if row == nil {
			return nil, true // NULL leg — untyped nil, the sanctioned nil-binding
		}
		return row, true
	}
	if b.base != nil {
		return b.base.GetCorrelationBinding(id)
	}
	return nil, false
}

// correlationBase adapts an *EvaluationContext into a CorrelationBinder,
// avoiding the typed-nil-interface trap (a nil *EvaluationContext stored in a
// non-nil interface would be called with a nil receiver).
func correlationBase(ec *EvaluationContext) values.CorrelationBinder {
	if ec == nil {
		return nil
	}
	return ec
}

// rowLegsBinder resolves a correlation to its LEG WINDOW derived from the
// row's OWN Type.Legs — the runtime half of the quantifier-addressed
// reference model: a SOURCE-RELATIVE baked reference
// (FieldValue{QOV(leg), col, Resolved: source ordinal}) evaluated over a
// merged/concatenated row binds its leg's window, where the source-relative
// ordinal reads the source's own slot (legWindowRow.Get = parent.Get(offset+i)
// — Java's quantifier binding, ordinal within the quantifier's row). The
// window authority is the row TYPE's leg boundaries (concatLegPositionals /
// ordinalLegType stamp them at row construction), NOT a plan-shape probe — so
// every merged row serves its legs uniformly, gated joins and leg-concat
// fallback merges alike. Aliases not in the row's legs delegate to base (an
// outer correlation); a miss with no base stays unbound (loud downstream).
type rowLegsBinder struct {
	base values.CorrelationBinder
	row  *PositionalRow
}

// GetCorrelationBinding implements values.CorrelationBinder. Two legs of one row
// can carry the SAME identity only if a producer minted it twice, which SameLeg
// cannot happen across the deliberately case-disjoint alias namespaces; the loop
// therefore takes the first identity match and stops. This is NOT the retired
// name lookup's first-match rule wearing a different field — it selects on the
// leg's IDENTITY, which is why a duplicate here would be a producer bug rather
// than an ordinary ambiguity to resolve.
func (b *rowLegsBinder) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	if b.row != nil && b.row.Type != nil {
		for _, leg := range b.row.Type.Legs {
			if h := values.Census(); h != nil {
				// RETIRED PREDICATE: `leg.Name == id.Name()` — exact on the leg's text
				// against the correlation's own spelling. Recording it lets the census
				// measure whether the conversion changed this binder's ANSWER, rather
				// than only whether the pair it now compares folds equal.
				h.LegIdentityConversion(values.LegSiteRowLegsBinder,
					leg.Alias, id, leg.Name == id.Name())
				h.LegIdentityLeg(leg)
			}
			// The leg's IDENTITY decides, through the one comparison every identity
			// proof routes through. Not its Name: matching text here would be an
			// identity claim made by string equality, and the alias namespaces are
			// deliberately case-DISJOINT precisely so that claim cannot be forged.
			if !values.SameLeg(leg.Alias, id) {
				continue
			}
			end := leg.Start + leg.Width
			if leg.Start < 0 || end > len(b.row.Type.Fields) {
				// Malformed bounds: skip — a whole-row fallback would silently
				// misread another leg's slots; the unbound miss stays loud at
				// the evaluateCorrelated tail.
				break
			}
			sub := &values.RecordType{Nullable: b.row.Type.Nullable, Fields: b.row.Type.Fields[leg.Start:end]}
			return &legWindowRow{parent: b.row, legType: sub, offset: leg.Start, width: leg.Width}, true
		}
	}
	if b.base != nil {
		return b.base.GetCorrelationBinding(id)
	}
	return nil, false
}

// Join output replaces the row correlations, not the statement's constants,
// parameters or scalar subqueries (Java's FlatMap uses context.withBinding).
func joinBuildRowContext(bindings values.CorrelationBinder, clock values.StatementClock) *values.RowEvalContext {
	row := &values.RowEvalContext{}
	for base := bindings; base != nil; {
		switch b := base.(type) {
		case *EvaluationContext:
			row.Binder = b
			row.ScalarSubqueries = b.scalarSubqueries
			base = nil
		case *buildLegBinder:
			base = b.base
		case *twoLegBinder:
			base = b.base
		default:
			base = nil
		}
	}
	row.Correlations = bindings
	row.Clock = clock
	return row
}
