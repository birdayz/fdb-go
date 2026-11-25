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

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// --- cursor-side build wiring ------------------------------------------------

// rcOutputType derives an RC's OUTPUT row type: a RAW *RecordType (duplicate
// names allowed and preserved verbatim — positional access is by ordinal) with
// one field per RC field: the RC field's name, the field Value's flowed type,
// ordinal = position. For the pristine ordinal join SEED this equals
// ordinalJoinSpans' mergedType (each field is a baked leg reference and its
// Type() is the leg column's type); for a FOLDED result value (the pure-wrapper
// merge's projection RC — baked refs mixed with computed values/constants) it
// is the projection's output row type, which has no leg windows but is still
// the single authoritative type of the built positional row.
func rcOutputType(rc *values.RecordConstructorValue) *values.RecordType {
	fields := make([]values.Field, len(rc.Fields))
	for i, f := range rc.Fields {
		var ft values.Type = values.UnknownType
		if f.Value != nil {
			ft = f.Value.Type()
		}
		fields[i] = values.Field{Name: f.Name, FieldType: ft, Ordinal: i}
	}
	// The built row type carries leg boundaries — each
	// baked leg run plus every BURIED leg a clustered box leg's type records
	// (RecordType.Legs) — so a buried-leg reference ("B.BID") binds its OWN
	// window through the row's Legs metadata (rowLegsBinder) on rows that
	// build positional-only (the clustered null-supplying pad).
	var legs []values.RecordTypeLeg
	lastCorr := ""
	for i, f := range rc.Fields {
		fv, isFV := values.AsFieldValue(f.Value)
		if !isFV {
			lastCorr = ""
			continue
		}
		qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue())
		if !isQOV {
			lastCorr = ""
			continue
		}
		corr := qov.Correlation().Name()
		if corr == lastCorr {
			continue
		}
		lastCorr = corr
		if rt, isRT := qov.FlowedType().(*values.RecordType); isRT {
			if len(rt.Legs) > 0 {
				// A clustered box run: its name is the rightmost LEAF's (the
				// sourceBinding convention), which the buried bounds already
				// carry at the leaf's own offset — a run-level entry under
				// that name would shadow it with the WHOLE concat (the
				// RIGHT-box collision: "D.ID" first-matched the box run and
				// read the null-supplying leg's slot). Subs only.
				for _, sub := range rt.Legs {
					legs = append(legs, values.NewRecordTypeLeg(
						// A REBASE carries the KIND as it carries the Alias. A rebase that
						// re-mints a kind is the same defect class as one that re-mints an
						// identity: the sub-leg did not change shape, only offset.
						sub.Kind, sub.Alias, sub.Name, i+sub.Start, sub.Width,
					))
				}
			} else {
				legs = append(legs, values.NewRecordTypeLeg(
					// rcOutputType's top-level box RUN.
					values.LegKindFlatRun, qov.Correlation(), corr, i, len(rt.Fields),
				))
			}
		}
	}
	return &values.RecordType{Fields: fields, Legs: legs}
}

// legTypesFromResultValue includes whole-object reads inside computed values,
// such as EXISTS, as well as field reads. All references to a leg must agree.
func legTypesFromResultValue(rv values.Value) (map[values.CorrelationIdentifier]*values.RecordType, map[values.CorrelationIdentifier]values.Type, error) {
	legs := make(map[values.CorrelationIdentifier]*values.RecordType)
	var raw map[values.CorrelationIdentifier]values.Type
	var err error
	values.WalkValue(rv, func(n values.Value) bool {
		qov, isQOV := values.AsQuantifiedObjectValue(n)
		if !isQOV {
			return true
		}
		typ := qov.Type()
		rt, isRT := typ.(*values.RecordType)
		if !isRT {
			if raw == nil {
				raw = make(map[values.CorrelationIdentifier]values.Type)
			}
			raw[qov.Correlation()] = typ
			return true
		}
		prev, seen := legs[qov.Correlation()]
		if !seen {
			legs[qov.Correlation()] = rt
			return true
		}
		if len(prev.Fields) != len(rt.Fields) && err == nil {
			err = fmt.Errorf("leg %s carries DIVERGENT types (%d vs %d fields) across the "+
				"result value's references — all references must copy the one "+
				"planner-constructed typed QOV (planner bug; malformed plan)",
				qov.Correlation(), len(prev.Fields), len(rt.Fields))
		}
		return true
	})
	if err != nil {
		return nil, nil, err
	}
	return legs, raw, nil
}

// ordinalJoinBuild is the per-cursor ordinal-BUILD state, computed ONCE at
// cursor construction (detection is the structural
// ContainsBakedOrdinal probe on the plan's result value — emergent from the
// representation, never a per-plan flag). Enabled marks the cursor as an ordinal build site: its
// emitted rows carry the positional row evaluated from the RC with per-leg
// bindings — the row model's single authority.
type ordinalJoinBuild struct {
	// Enabled is true iff the result value contains a baked ordinal reference
	// (values.ContainsBakedOrdinal, deep). A nil/lazy result value yields a nil
	// *ordinalJoinBuild instead — use the nil-safe enabled().
	Enabled bool
	// RC is the result value as the RC the build evaluates per-field. Nil
	// exactly when Bare is set.
	RC *values.RecordConstructorValue
	// Bare is the result value when it is NOT an RC — a single baked value the
	// select flows as its whole output (PartitionSelectRule's single-live-lower
	// row translated onto a merge quantifier: `ofOrdinal(QOV(merge), i)`). The
	// built row is that ONE value's evaluation, not an assembled slot list, so
	// there is no OutputType and no leg window to publish: a row-shaped result
	// flows through as itself (the merged leg row it names, windows and all) and
	// a scalar wraps into a 1-slot row. Mutually exclusive with RC.
	Bare values.Value
	// OutputType is the built positional row's single authoritative type
	// (rcOutputType; == ordinalJoinSpans' mergedType for the pristine seed).
	OutputType *values.RecordType
	// OutputLayout is the selected immutable physical address space attached to
	// every row this build emits. It replaces RecordType.Legs as runtime QOV
	// binding authority; folded/partial sources are simply absent windows.
	OutputLayout values.OrdinalLayout
	// OutputLayoutFromPlan records that OutputLayout is the selected plan's
	// admitted physical contract rather than the executor's legacy derivation
	// from RC. configureNullSupplying must preserve that exact contract: it may
	// contain proof-backed scalar windows which are not syntactically visible in
	// RC and therefore cannot be reconstructed here.
	OutputLayoutFromPlan bool
	// OutputSourceOrigins maps a proof-backed output-window source to the exact
	// NLJ leg and exact selected-child source which supplied it. It is used only
	// to derive null-extension presence: a present child row proves an ordinary
	// source matched, while a child source which was itself null-supplying must
	// propagate that child's exact per-row match state.
	OutputSourceOrigins map[values.CorrelationIdentifier]outputSourceOrigin
	// Spans + WindowsOK: the decline-only leg-window eligibility probe
	// (ordinalJoinSpans — pristine seed only). WindowsOK false for a folded
	// RV: the output is a plain projection row, downstream gets no windows.
	Spans     []legSpan
	WindowsOK bool
	// LegTypes are the leg types recovered from the RV's baked references —
	// the adapter's leg types when the RV is folded and Spans are unavailable.
	LegTypes map[values.CorrelationIdentifier]*values.RecordType
	// RawLegs bind a non-record QOV to its datum, not its one-slot transport row.
	RawLegs map[values.CorrelationIdentifier]values.Type
	// Clock supplies the statement-stable CURRENT_TIMESTAMP-family instant
	// to the build's row evaluation, set from the constructing cursor's
	// EvaluationContext. Without it a CURRENT_* value folded into the baked
	// result value would evaluate against RowEvalContext's wall-clock
	// fallback and drift across rows.
	Clock values.StatementClock
}

type outputSourceOrigin struct {
	topLegAlias values.CorrelationIdentifier
	// childSource is nil for a source buried in the leg's retained box that the
	// selected child does not bind on its own; its presence is the leg's.
	childSource        values.QuantifiedObjectValue
	childNullSupplying bool
}

// newOrdinalJoinBuild probes a join plan's result value at cursor
// construction. nil (disabled) when rv is nil or carries no baked ordinal —
// the non-build cursor path (identity pass-through / fold).
//
// Two ENABLED shapes, discriminated by the result value's own structure:
//
//   - an RC — the leg-concat seed or a folded projection: the built row is one
//     slot per RC field, with leg windows when the seed is pristine;
//   - a BARE (non-RC) baked value — the select flows ONE value as its whole
//     output: the built row is that value's evaluation (see Bare and
//     evaluateOrdinalJoinBareRow). Java imposes no RC requirement at any point;
//     ImplementNestedLoopJoinRule.java:187,201,214 hand
//     selectExpression.getResultValue() to RecordQueryFlatMapPlan verbatim in
//     all three arms, and PartitionSelectRule.java:281,319 mints exactly this
//     shape — a single-live-lower select flows one leg's whole row, and a later
//     positional-merge round translates that bare QOV into
//     `ofOrdinal(QOV(merge), i)`.
//
// The bare arm must stay ENABLED and must not be turned back into either an
// error or a decline. Both were MEASURED on the 3-way comma join with equijoin
// predicates and a projected EXISTS
// (sqldriver.TestFDB_CommaJoin3ProjectedExistsWithEquijoins, which carries the
// three correct rows): erroring fails the query outright, and declining to the
// non-build path executes and returns ZERO rows, because that path binds the
// outer by name rather than adapting it to the merge layout the inner's
// pushed-down SARGs read by ordinal, then wraps the flowed leg row in a 1-slot
// scalar row.
func newOrdinalJoinBuild(rv values.Value, preds []predicates.QueryPredicate) (*ordinalJoinBuild, error) {
	return newOrdinalJoinBuildWithOutputLayout(rv, preds, nil, nil, nil)
}

// newOrdinalJoinBuildWithOutputLayout is the selected-plan counterpart of
// newOrdinalJoinBuild. The plan-owned layout is authoritative because it can
// carry exact retained-source proofs which are no longer present as QOV nodes
// in rv. sourceOrigins is an equally exact construction-time proof used only
// for null-supplying window presence; both inputs are defensively admitted and
// copied before the cursor retains them.
func newOrdinalJoinBuildWithOutputLayout(
	rv values.Value,
	preds []predicates.QueryPredicate,
	outputLayout values.OrdinalLayout,
	sourceOrigins map[values.CorrelationIdentifier]outputSourceOrigin,
	legAliases []values.CorrelationIdentifier,
) (*ordinalJoinBuild, error) {
	// Two build triggers:
	//   - a FrontierPinned baked reference anywhere in the RV (the flat
	//     seed, its folds, and the post-translation MIXED upper shape whose
	//     fields are ofOrdinal-over-innerMerge alongside bare leg QOVs);
	//   - the positional-merge RC (ALL fields bare `_i`-named QOVs —
	//     the lowest merge level carries no baked refs at all, but its rows
	//     must build positional: the level above reads them by ordinal).
	if rv == nil {
		return nil, nil
	}
	rc, isRC := rv.(*values.RecordConstructorValue)
	// Plain concatenation retains child windows. Computed or nested slots instead
	// require result evaluation, as in Java's RecordQueryFlatMapPlan.executePlan.
	planBackedRC := outputLayout != nil && isRC
	// A single leg object is not the merged row; only the latter is concatenation.
	notAConcatenation := false
	if qov, ok := values.AsQuantifiedObjectValue(rv); ok {
		notAConcatenation = namesOneLeg(qov.Correlation(), legAliases)
	}

	if !values.ContainsBakedOrdinal(rv) && !values.IsPositionalMergeRC(rv) && !planBackedRC && !notAConcatenation {
		return nil, nil
	}
	if !isRC {
		// A BARE (non-RC) baked result value: the select's output is not an
		// assembled row but a SINGLE flowed value read out of a leg. Java's
		// RecordQueryFlatMapPlan has no RC requirement at all — it evaluates
		// selectExpression.getResultValue() against the bound legs whatever its
		// shape (ImplementNestedLoopJoinRule.java:187,201,214 pass the select's
		// result value verbatim in all three arms) — and PartitionSelectRule
		// legitimately MINTS this shape: its single-live-lower arm flows one
		// leg's whole row (`quantifier.getFlowedObjectValue()`,
		// PartitionSelectRule.java:281), and a later positional-merge round
		// translates that bare QOV onto the merge quantifier
		// (`resultValue.translateCorrelations(translationMap)`, :319), leaving
		// `ofOrdinal(QOV(merge), i)` — a bare baked reference — as the upper
		// select's whole result value.
		//
		// The build stays ENABLED for it, and that is the load-bearing part:
		// DECLINING (returning a nil build) routes the cursor down the non-build
		// path, which binds the outer by name instead of adapting it to the merge
		// layout the inner's pushed-down SARGs read by ordinal, and wraps the
		// flowed row in a 1-slot scalar row. Measured on the 3-way comma join
		// with equijoin predicates and a projected EXISTS: declining executes and
		// returns ZERO rows where three are correct. Enabled, the legs bind
		// exactly as for an RC and the one value is evaluated against them.
		// The width-agreement assert runs on THIS path too — it lives inside
		// legTypesFromResultValue, so the Bare arm gets it without a copy of the RC
		// arm's loop. It used to be RC-only, which left the arm that has exactly one
		// leg-type source with no check on that source at all.
		bareLegTypes, rawLegTypes, err := legTypesFromResultValue(rv)
		if err != nil {
			return nil, err
		}
		bare := &ordinalJoinBuild{Enabled: true, Bare: rv, LegTypes: bareLegTypes, RawLegs: rawLegTypes}
		widenLegTypesFromPredicates(bare.LegTypes, preds)
		return bare, nil
	}
	spans, _, windowsOK := ordinalJoinSpans(rc)
	// LegTypes come from the RESULT VALUE *and* the join PREDICATES: a
	// folded projection RV can DROP a leg entirely while a
	// baked cross-leg ON predicate still references it — collecting from the
	// RV alone left the dropped leg typeless, and the leg
	// adapted to a ZERO-WIDTH binding that blew up the predicate
	// (loud OrdinalResolutionError, "row columns []") on a legitimate plan.
	legTypes, rawLegs, err := legTypesFromResultValue(rc)
	if err != nil {
		return nil, err
	}
	widenLegTypesFromPredicates(legTypes, preds)
	outputType := rcOutputType(rc)
	derivedLayout, err := values.NewFlatOrdinalLayoutForRetainedResult(rc, nil)
	if err != nil {
		return nil, fmt.Errorf("ordinal join output layout: %w", err)
	}
	fromPlan := outputLayout != nil
	if !fromPlan && len(sourceOrigins) > 0 {
		return nil, layoutBindingError(values.LayoutSourceNotProvided,
			"nested-loop join source origins require a selected output layout")
	}
	if fromPlan {
		if err := validateSelectedOrdinalJoinLayout(outputLayout, outputType, sourceOrigins); err != nil {
			return nil, err
		}
		derivedLayout = outputLayout
	}
	retainedOrigins := make(map[values.CorrelationIdentifier]outputSourceOrigin, len(sourceOrigins))
	for source, origin := range sourceOrigins {
		retainedOrigins[source] = origin
	}
	build := &ordinalJoinBuild{
		Enabled:              true,
		RC:                   rc,
		OutputType:           outputType,
		OutputLayout:         derivedLayout,
		OutputLayoutFromPlan: fromPlan,
		OutputSourceOrigins:  retainedOrigins,
		Spans:                spans,
		WindowsOK:            windowsOK,
		LegTypes:             legTypes,
		RawLegs:              rawLegs,
	}
	// Check result and predicate types against the retained seed windows.
	if err := build.assertLegTypeSourcesAgree(); err != nil {
		return nil, err
	}
	return build, nil
}

func validateSelectedOrdinalJoinLayout(
	outputLayout values.OrdinalLayout,
	outputType *values.RecordType,
	sourceOrigins map[values.CorrelationIdentifier]outputSourceOrigin,
) error {
	if err := values.ValidateOrdinalLayoutAdmission(outputLayout); err != nil {
		return fmt.Errorf("ordinal join selected output layout: %w", err)
	}
	if outputLayout.CarrierKind() != values.OrdinalCarrierRecord ||
		outputLayout.Carrier() == nil ||
		!values.PhysicalCarrierType(outputLayout).Equals(outputType) {
		return layoutBindingError(values.LayoutCarrierMismatch,
			"selected nested-loop join layout disagrees with the result program")
	}
	declared := make(map[values.CorrelationIdentifier]struct{})
	for _, source := range outputLayout.WindowSources() {
		declared[source.Correlation()] = struct{}{}
	}
	for source, origin := range sourceOrigins {
		if source.IsZero() || origin.topLegAlias.IsZero() {
			return layoutBindingError(values.CorrelationZero,
				"nested-loop join output-source origin contains a zero correlation")
		}
		if origin.childSource != nil && origin.childSource.Correlation() != source {
			return layoutBindingError(values.CorrelationForeignValue,
				"nested-loop join output-source origin names a different child source")
		}
		if _, ok := declared[source]; !ok {
			return layoutBindingError(values.LayoutSourceNotProvided,
				"nested-loop join source origin is absent from the selected output layout")
		}
	}
	return nil
}

// namesOneLeg reports whether corr is one of this join's own leg aliases.
//
// The distinction it draws is what keeps the build triggers above from firing on
// the ordinary case: a correlation naming a LEG means the result value reads
// that leg specifically, while a correlation standing for the MERGED row is
// exactly what plain child concatenation produces.
func namesOneLeg(corr values.CorrelationIdentifier, legAliases []values.CorrelationIdentifier) bool {
	for _, leg := range legAliases {
		if !leg.IsZero() && corr == leg {
			return true
		}
	}
	return false
}

// configureNullSupplying rebuilds the output layout with explicit per-row
// presence for the named join legs. The exact QOV roots are discovered from
// the admitted result program; a missing/differently-typed root is a malformed
// plan, never a reason to infer presence from all-NULL field values.
func (b *ordinalJoinBuild) configureNullSupplying(correlations ...values.CorrelationIdentifier) error {
	if b == nil || b.RC == nil || len(correlations) == 0 {
		return nil
	}
	if b.OutputLayoutFromPlan {
		// RecordQueryNestedLoopJoinPlan constructed this exact layout with the
		// join type in hand, including every admitted source's null-supplying
		// bit. Re-deriving from RC would silently erase proof-only scalar
		// sources. Per-row presence for such sources is derived from their exact
		// child origins in evaluateBound.
		return nil
	}
	wanted := make(map[values.CorrelationIdentifier]struct{}, len(correlations))
	for _, correlation := range correlations {
		wanted[correlation] = struct{}{}
	}
	found := make(map[values.CorrelationIdentifier]values.QuantifiedObjectValue, len(correlations))
	var conflict error
	values.WalkValue(b.RC, func(value values.Value) bool {
		qov, ok := values.AsQuantifiedObjectValue(value)
		if !ok {
			return true
		}
		if _, needed := wanted[qov.Correlation()]; !needed {
			return true
		}
		if previous, exists := found[qov.Correlation()]; exists && !values.FlowedTypesEqual(previous, qov) {
			conflict = fmt.Errorf("null-supplying leg %s has conflicting exact types", qov.Correlation())
			return false
		}
		found[qov.Correlation()] = qov
		return true
	})
	if conflict != nil {
		return conflict
	}
	nullSources := make([]values.QuantifiedObjectValue, 0, len(correlations))
	for _, correlation := range correlations {
		qov, exists := found[correlation]
		if !exists {
			// A completely projected-away leg needs no output window or presence.
			continue
		}
		nullSources = append(nullSources, qov)
	}
	layout, err := values.NewFlatOrdinalLayoutForRetainedResult(b.RC, nullSources)
	if err != nil {
		return fmt.Errorf("ordinal join null-supplying layout: %w", err)
	}
	b.OutputLayout = layout
	return nil
}

// enabled is the nil-safe Enabled read — a non-build cursor stores a nil
// *ordinalJoinBuild.
func (b *ordinalJoinBuild) enabled() bool { return b != nil && b.Enabled }

// legValueType is a raw leg's own type when the alias names one, else legType.
func (b *ordinalJoinBuild) legValueType(id values.CorrelationIdentifier) values.Type {
	if typ, raw := b.RawLegs[id]; raw {
		return typ
	}
	if typ := b.legType(id); typ != nil {
		return typ
	}
	return nil
}

// legType resolves the adapter's leg type for a leg alias.
//
// THE BAKED REFERENCES ARE THE AUTHORITY. LegTypes carries the types recovered
// from the RV's own baked leg references, widened from the join predicates and
// from the plan walk, so it is the type the expressions that will actually be
// EVALUATED were constructed against. Spans are the pristine-seed leg-window
// walk — a positional description of the seed RC, available only when
// WindowsOK, and invisible for a folded RV whose output is a plain projection
// row. So a span types a leg only when no baked reference names it.
//
// WHEN BOTH NAME A LEG THEY DESCRIBE ONE FLOWED ROW, so agreement is an
// invariant rather than a coincidence — which means the order below decides
// nothing on a well-formed plan. A disagreement is a malformed plan, not a
// preference to resolve, and it is rejected by assertLegTypeSourcesAgree at
// BUILD time rather than here: the two sources are fixed for the cursor's
// lifetime, so re-deciding per row would pay for the check on every row of
// every join and still have nowhere to report it from (pairBinder has no error
// return). Checked once at the boundary, this stays a plain accessor.
func (b *ordinalJoinBuild) legType(id values.CorrelationIdentifier) *values.RecordType {
	if exact := b.LegTypes[id]; exact != nil {
		return exact
	}
	if b.WindowsOK {
		for _, s := range b.Spans {
			if s.Alias == id {
				return s.LegType
			}
		}
	}
	return nil
}

// assertLegTypeSourcesAgree rejects a build whose two leg-type sources describe
// one leg with different widths. It runs after every widening has been applied,
// so LegTypes is final.
//
// Without it, legType's precedence silently picks a winner and the leg is
// adapted to a width the other half of the plan does not expect — the failure
// then surfaces far downstream as an ordinal that resolves into the wrong
// column, or not at all. This is the same class as the two DIVERGENT reports
// legTypesFromResultValue and widenLegTypesFromPlan already make, and it is
// worded the same way so all three read as one rule.
func (b *ordinalJoinBuild) assertLegTypeSourcesAgree() error {
	if !b.WindowsOK {
		// Spans describe the pristine seed only; a folded RV has no second
		// source to disagree with.
		return nil
	}
	for _, s := range b.Spans {
		exact := b.LegTypes[s.Alias]
		if exact == nil || s.LegType == nil {
			continue
		}
		if len(exact.Fields) != len(s.LegType.Fields) {
			return fmt.Errorf("leg %s carries DIVERGENT types (%d vs %d fields) between the RV's baked references and the seed leg window — both describe the same flowed row (planner bug; malformed plan)",
				s.Alias, len(exact.Fields), len(s.LegType.Fields))
		}
	}
	return nil
}
