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

	"fdb.dev/internal/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/internal/recordlayer/query/plan/plans"
)

// widenLegTypesFromPredicates adds the leg types the join PREDICATES carry to
// an already-collected map. A folded projection RV can DROP a leg entirely
// while a baked cross-leg ON predicate still references it; collecting from the
// RV alone leaves the dropped leg typeless, and it then adapts to a ZERO-WIDTH
// binding that blows up the predicate. FIRST-WINS rather than a divergence
// assert: this is the widening SOURCE, and the RV-side collection the caller
// already ran is the one that asserts width agreement.
func widenLegTypesFromPredicates(legTypes map[values.CorrelationIdentifier]*values.RecordType, preds []predicates.QueryPredicate) {
	for _, p := range preds {
		predicates.ReplaceValues(p, func(v values.Value) values.Value {
			fv, isFV := values.AsFieldValue(v)
			if !isFV || fv.Path() == nil {
				return v
			}
			if qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue()); isQOV {
				if rt, isRT := qov.Type().(*values.RecordType); isRT {
					if _, seen := legTypes[qov.Correlation()]; !seen {
						legTypes[qov.Correlation()] = rt
					}
				}
			}
			return v
		})
	}
}

// widenLegTypesFromPlan widens LegTypes with every BAKED leg reference found
// in a physical plan tree's predicate surfaces — PredicatesFilter/Filter
// predicates and scan/index comparison operands. The correlated implementation
// pushes the join's baked ON references INTO the inner plan as SARGs and
// residual filters, so a folded result value that DROPPED a leg leaves the
// build typeless for it even though the inner plan still references it — the
// untyped leg then adapts to a
// zero-width binding and dies loudly on a legitimate plan. Called by
// newFlatMapCursorForPlan with the inner plan; the NLJ path gets the same widening
// directly from its predicate list in newOrdinalJoinBuild.
//
// The walk exists only because a folded RV can drop a leg the plan still
// references. Its
// exact-set plan arms fail SAFE: a missed predicate surface leaves the leg
// typeless → loud zero-width death, never silent.
//
// Multiple sources (RV, join preds, pushed SARGs) can each carry a leg's
// type; this is CONFLICT-IMPOSSIBLE, not precedence — every baked reference
// is a copy of the ONE seed-constructed typed QOV, and every transformation
// preserves marker and type. The width-divergence error below asserts that
// load-bearing invariant: a violation is a malformed plan (planner bug) and
// fails the query, not the process. The walk callback has no error channel,
// so the divergence is captured and returned after the walk.
func (b *ordinalJoinBuild) widenLegTypesFromPlan(plan plans.RecordQueryPlan) error {
	if !b.enabled() || plan == nil {
		return nil
	}
	var divergence error
	// The walk continues widening LegTypes after a capture; harmless — the
	// caller (newFlatMapCursorForPlan) discards the whole build on error.
	walkBakedRefs(plan, func(v values.Value) values.Value {
		fv, isFV := values.AsFieldValue(v)
		if !isFV || fv.Path() == nil {
			return v
		}
		if qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue()); isQOV {
			// Reserved current names the local physical row phase of whichever
			// operator owns this predicate/SARG. A nested inner plan can contain
			// several such phases with legitimately different widths; none is a
			// FlatMap leg identity. Folding them into LegTypes under the one
			// `_current` key creates a false divergence and can never help the leg
			// adapter, whose lookups are by the FlatMap's named outer/inner aliases.
			if qov.Correlation() == values.CurrentCorrelation() {
				return v
			}
			if rt, isRT := qov.Type().(*values.RecordType); isRT {
				if prev, seen := b.LegTypes[qov.Correlation()]; !seen {
					b.LegTypes[qov.Correlation()] = rt
				} else if len(prev.Fields) != len(rt.Fields) && divergence == nil {
					divergence = fmt.Errorf("leg %s carries DIVERGENT baked types (%d vs %d fields) across the RV/predicate/SARG sources — all baked references must copy the one seed-constructed typed QOV (planner bug; malformed plan)", qov.Correlation(), len(prev.Fields), len(rt.Fields))
				}
			}
		}
		return v
	})
	if divergence != nil {
		return divergence
	}
	// A leg this walk ADDED can newly collide with the seed leg window, so the
	// two-source agreement is re-asserted here rather than only at construction.
	// The walk never overwrites an existing entry (the arm above reports instead),
	// so a leg that agreed before still agrees.
	return b.assertLegTypeSourcesAgree()
}

// walkBakedRefs walks a physical plan tree's predicate surfaces —
// PredicatesFilter/Filter predicates and scan/index comparison operands —
// applying collect to every value found there. It is the ONE baked-reference
// plan walk (a single derivation path), shared by
// widenLegTypesFromPlan (the build-side type widening, width-divergence error
// captured in its collector) and probeOuterBakedType (the
// disabled-build probe).
//
// RecordQueryNestedLoopJoinPlan also implements GetPredicates but is
// DELIBERATELY omitted: a baked-ref-bearing NLJ does not appear inside a
// gated FlatMap's inner plan (an existential inner is never itself consumed
// as a gated join leg).
// A missed predicate surface fails SAFE for the BUILD consumer only: a
// typeless leg dies loudly at zero width downstream. It does NOT fail safe for
// the PROBE consumer — a probe negative is indistinguishable from "this inner
// plan has no baked references", which licenses name-keyed binding. So the arm
// set has to cover every surface a SARG can be pushed onto, which is why the
// covering scan is enumerated below rather than left to child descent.
func walkBakedRefs(plan plans.RecordQueryPlan, collect func(values.Value) values.Value) {
	collectComparison := func(c *predicates.Comparison) {
		if c != nil && c.Operand != nil {
			values.Replace(c.Operand, collect)
		}
	}
	var walk func(p plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		if p == nil {
			return
		}
		switch t := p.(type) {
		case *plans.RecordQueryPredicatesFilterPlan:
			for _, pr := range t.GetPredicates() {
				predicates.ReplaceValues(pr, collect)
			}
		case *plans.RecordQueryFilterPlan:
			for _, pr := range t.GetPredicates() {
				predicates.ReplaceValues(pr, collect)
			}
		// The three SARGable leaves, read through the one accessor they share.
		// A COVERING scan belongs in this set because the access path emits
		// Fetch(Covering(IndexScan)) for EVERY index-backed access (RFC-220), so
		// it is the ordinary shape here, not an exotic one — and it holds its
		// index scan as a FIELD, so the generic child descent below never
		// reaches the operands. Its omission would NOT fail safe the way a
		// missed predicate surface does: for the probe, a nil outer type reads
		// as "no baked references at all" and licenses the identity
		// pass-through to bind by NAME, which is the exact state the probe
		// exists to prevent. The covering plan's GetScanComparisons delegates to
		// the inner scan, whose physical range it shares.
		case *plans.RecordQueryScanPlan, *plans.RecordQueryIndexPlan, *plans.RecordQueryCoveringIndexPlan:
			sargable, ok := t.(interface {
				GetScanComparisons() []*predicates.ComparisonRange
			})
			if !ok {
				break
			}
			for _, cr := range sargable.GetScanComparisons() {
				if cr.IsEquality() {
					collectComparison(cr.GetEqualityComparison())
				} else if cr.IsInequality() {
					for _, c := range cr.GetInequalityComparisons() {
						collectComparison(c)
					}
				}
			}
		}
		for _, ch := range p.GetChildren() {
			walk(ch)
		}
	}
	walk(plan)
}

// probeOuterBakedType is the DISABLED-BUILD probe: a
// FlatMap whose own result value builds no ordinal state (the identity-RV
// existential FlatMap — WHERE-EXISTS over a gated join) can still carry BAKED
// FrontierPinned references to the OUTER alias inside its inner plan (the
// correlated implementation pushes the join's ON references down as SARGs and
// residual filters). Those references resolve by ordinal and die loudly on a
// name-keyed binding (BakedNameContextError), so the cursor must bind
// the outer positionally, adapted to the probed type's layout. The probe
// recovers the outer's typed RecordType
// from those references through the shared walker; the width-divergence error
// asserts the same one-seed invariant widenLegTypesFromPlan pins.
func probeOuterBakedType(plan plans.RecordQueryPlan, outerAlias values.CorrelationIdentifier) (*values.RecordType, error) {
	var found *values.RecordType
	// The walk callback has no error channel; a width divergence — a
	// malformed plan (planner bug) that must fail the query, not the
	// process — is captured and returned after the walk.
	var divergence error
	walkBakedRefs(plan, func(v values.Value) values.Value {
		fv, isFV := values.AsFieldValue(v)
		if !isFV || fv.Path() == nil || !fv.Path().IsFrontierPinned() {
			return v
		}
		qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue())
		if !isQOV || qov.Correlation() != outerAlias {
			return v
		}
		if rt, isRT := qov.Type().(*values.RecordType); isRT {
			if found == nil {
				found = rt
			} else if len(found.Fields) != len(rt.Fields) && divergence == nil {
				divergence = fmt.Errorf("outer %s carries DIVERGENT baked types (%d vs %d fields) across the inner plan's references — all baked references must copy the one seed-constructed typed QOV (planner bug; malformed plan)", outerAlias, len(found.Fields), len(rt.Fields))
			}
		}
		return v
	})
	return found, divergence
}

// legRows adapts the two join legs into the BUILD-time binding map: alias →
// values.OrdinalRow via adaptLegPositional (layout-matching legs flow through;
// other layouts gather into the leg type). A NIL QueryResult pointer is the
// NULL leg (LEFT/FULL null padding): its alias maps to nil, PRESENT — the
// binder then returns (nil, true) and the leg's baked references evaluate to
// NULL (the null extension falls out of evaluation).
func (b *ordinalJoinBuild) legRows(outerAlias, innerAlias values.CorrelationIdentifier, outer, inner *QueryResult) (map[values.CorrelationIdentifier]values.OrdinalRow, map[values.CorrelationIdentifier]any, error) {
	legs := make(map[values.CorrelationIdentifier]values.OrdinalRow, 2)
	raw := make(map[values.CorrelationIdentifier]any)
	if err := b.bindLeg(legs, raw, outerAlias, outer); err != nil {
		return nil, nil, err
	}
	if err := b.bindLeg(legs, raw, innerAlias, inner); err != nil {
		return nil, nil, err
	}
	return legs, raw, nil
}

func (b *ordinalJoinBuild) bindLeg(legs map[values.CorrelationIdentifier]values.OrdinalRow, raw map[values.CorrelationIdentifier]any, id values.CorrelationIdentifier, qr *QueryResult) error {
	var scalar bool
	if qr != nil {
		var err error
		scalar, err = isBareScalarRow(qr.Positional)
		if err != nil {
			return err
		}
	}
	// FirstOrDefault's empty record arm carries an exact-width shell so layout
	// validation remains possible, with LayoutPresence stating that the WHOLE
	// quantified object is absent. Bind that object as NULL before any scalar
	// unwrap or record adaptation. Inferring presence from the non-nil shell
	// makes EXISTS over an empty record subplan unconditionally true; inferring
	// absence from nil slots would misclassify a matched all-NULL record.
	if qr != nil && qr.Positional != nil {
		_, explicitAbsent, err := qr.Positional.wholeObjectBinding()
		if err != nil {
			return err
		}
		if explicitAbsent {
			if _, isRaw := b.RawLegs[id]; isRaw {
				raw[id] = nil
			} else {
				legs[id] = nil
			}
			return nil
		}
	}
	// A RAW leg (a bare-QOV non-record unnest element) binds its whole flowed
	// scalar — never adapted to a (non-record → empty) OrdinalRow. The element
	// flows as a 1-slot `_0` PositionalRow (scalarPositionalRow), so bind the
	// UNWRAPPED scalar, which QOV(inner).Evaluate flows as the value. A nil pointer
	// is the deliberately-NULL leg: raw nil (→ NULL).
	if _, isRaw := b.RawLegs[id]; isRaw {
		switch {
		case qr == nil || qr.Positional == nil:
			// A nil / positional-less inner is the deliberately-NULL leg (→ NULL);
			// bind an UNTYPED nil, never a typed-nil *PositionalRow.
			raw[id] = nil
		case scalar:
			raw[id] = qr.Positional.Slots[0]
		default:
			raw[id] = qr.Positional
		}
		return nil
	}
	if qr == nil {
		legs[id] = nil // the deliberately-NULL leg: present, bound to nil
		return nil
	}
	// A leg whose flowed value is NOT A RECORD binds its DATUM, not the one-slot
	// carrier around it — on EITHER side of the FlatMap.
	//
	// Java decides this from the VALUE's own static result type, never from
	// which side of the join the binding came from:
	// QuantifiedObjectValue.eval (QuantifiedObjectValue.java:82-95) is
	// `isRelation() -> obj`, `isRecord() -> getMessage()`, else
	// `binding.getDatum()`. The two FlatMap bindings are the IDENTICAL call
	// (RecordQueryFlatMapPlan.java:135 and :140 — `withBinding(CORRELATION,
	// quantifier.getAlias(), result)`), so there is no side for a rule to key on.
	//
	// The carrier kind distinguishes a datum envelope from a genuine record.
	// Anonymous one-field records may have the SAME _0 title and still bind
	// whole; the checked discriminator above never uses that title.

	// The consequence when the carrier is bound instead: the quantifier object
	// is non-null whatever it holds, so ExistsValue.eval
	// (ExistsValue.java:98-100, `getChild().eval() != null`) can no longer see
	// an empty subplan. Measured before the unwrap existed: `SELECT a.id, EXISTS
	// (SELECT 1 FROM e WHERE e.c = b.c) FROM a JOIN b ON … JOIN q ON …` over an
	// EMPTY `e` answered TRUE for every row, while the same query correlated to
	// the FIRST leg (which lands on the sibling non-build path, which already
	// unwrapped) answered FALSE.
	if scalar {
		raw[id] = qr.Positional.Slots[0]
		return nil
	}
	row, err := adaptLegPositional(*qr, b.legType(id), id)
	if err != nil {
		return err
	}
	legs[id] = row
	return nil
}

// evaluateLegs builds the positional row from already-adapted leg bindings —
// the SINGLE eval path (evaluateOrdinalJoinRow) under a buildLegBinder. Split
// from evaluate so the NLJ cursor can share one legRows adaptation between
// the predicate context and the build.
func (b *ordinalJoinBuild) evaluateLegs(legs map[values.CorrelationIdentifier]values.OrdinalRow, raw map[values.CorrelationIdentifier]any, base values.CorrelationBinder) (*PositionalRow, error) {
	return b.evaluateBound(&buildLegBinder{legs: legs, raw: raw, base: base})
}

// evaluateBound builds the positional row from any pre-built leg binder — the
// zero-rebuild path the NLJ cursor's per-pair twoLegBinder uses (no per-pair
// map, no per-pair re-adaptation).
func (b *ordinalJoinBuild) evaluateBound(bindings values.CorrelationBinder) (*PositionalRow, error) {
	if b.Bare != nil {
		return evaluateOrdinalJoinBareRow(b.Bare, bindings, b.Clock)
	}
	row, err := evaluateOrdinalJoinRow(b.RC, b.OutputType, bindings, b.Clock)
	if err != nil {
		return nil, err
	}
	if b.OutputLayout != nil {
		presenceBindings := bindings
		var sourcePresence *outputSourcePresenceBinder
		if len(b.OutputSourceOrigins) > 0 {
			sourcePresence = &outputSourcePresenceBinder{
				base: bindings, origins: b.OutputSourceOrigins,
			}
			presenceBindings = sourcePresence
		}
		presence, presenceErr := values.NewWindowMatchPresenceFromCorrelations(
			b.OutputLayout, presenceBindings,
		)
		if sourcePresence != nil && sourcePresence.err != nil {
			return nil, sourcePresence.err
		}
		if presenceErr != nil {
			return nil, presenceErr
		}
		row.Layout = b.OutputLayout
		row.LayoutPresence = presence
	}
	return row, nil
}

// evaluate is the one-shot build: adapt both legs (nil pointer = NULL leg),
// then evaluate the RC per-field into a PositionalRow under OutputType. base
// resolves outer correlations beyond the two legs (may be nil).
func (b *ordinalJoinBuild) evaluate(outerAlias, innerAlias values.CorrelationIdentifier, outer, inner *QueryResult, base values.CorrelationBinder) (*PositionalRow, error) {
	legs, raw, err := b.legRows(outerAlias, innerAlias, outer, inner)
	if err != nil {
		return nil, err
	}
	return b.evaluateLegs(legs, raw, base)
}
