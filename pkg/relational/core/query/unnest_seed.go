package query

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// unnestOrdinalSeed builds the ORDINAL result-value seed for a SINGLE-SOURCE
// lateral unnest (`FROM t, t.arr AS x [AT ord]`), replacing the retired
// name-model result-value builder. Emitted ONLY when the OUTER leg is a
// SINGLE SOURCE
// (clusterArity(j.Left)==1 — the caller's gate): a multi-source outer
// (`FROM A, B, A.arr AS x`) stays name-model — that is the gathered-cluster path.
//
// Mirrors Java's LogicalOperator.generateCorrelatedFieldAccess three-way branch
// (LogicalOperator.java:318-353), the ordinal-form spec:
//
//   - OUTER leg: ofOrdinal(QOV(outer), i) per column — the same ordinal leg
//     bake used elsewhere (ordinalLegType carries the scan RecordName so a
//     table-qualified outer reference resolves through the span type).
//   - WITH ORDINALITY (AT present): the Explode flows a genuine 2-field
//     {_0:element, _1:ordinal} record (ExplodeOrdinalityResultType), so bake
//     element=ofOrdinal(QOV(inner),0) and ordinal=ofOrdinal(QOV(inner),1) —
//     Java's ofOrdinalNumber(flowedObjectValue, 0/1). The RC fields are NAMED by
//     the user AS/AT aliases (not `_0`/`_1`) so the result-set columns and bare
//     references resolve. When an AS alias binds the element too, this is a full
//     all-baked 2-leg seed and runs AssertOrdinalJoinSeed; AT-only discards the
//     element (partial inner run) and skips the assert.
//   - WITHOUT ORDINALITY: the element is the WHOLE flowed object (a bare scalar,
//     or a struct map). Reference the inner QOV DIRECTLY — Java's isPrimitive()
//     branch, extended to struct elements per the whole-object ruling (ofOrdinal
//     over a scalar THROWS FIELD_ACCESS_INPUT_NON_RECORD_TYPE in both Java and
//     Go; a struct binds whole, matching ANSI/Postgres whole-composite UNNEST).
//     This yields a MIXED RC (baked outer run + a direct-QOV element field); the
//     bare-QOV field legitimately fails AssertOrdinalJoinSeed's frontier-pin
//     check, so the mixed shape skips the assert (a dedicated white-box
//     seed-shape pin replaces the tripwire). Its element leg binds RAW at the
//     executor build (a bare-QOV non-record leg — see the ordinal-build binder).
//
// Returns nil (DECLINE → the caller falls back to the name-model path) when
// the outer leg is untranslatable, or for the degenerate no-AS/no-AT shape.
func (t *cascadesTranslator) unnestOrdinalSeed(
	outer logical.LogicalOperator,
	outerCorr, innerCorr values.CorrelationIdentifier,
	u *logical.LogicalUnnest,
	elementType values.Type,
) values.Value {
	outerType := t.ordinalLegType(outer)
	if outerType == nil || len(outerType.Fields) == 0 {
		// A DERIVED-TABLE outer flows its projection's OUTPUT columns as a
		// positional row (see unnestBakedRootCollection) — derive that layout
		// so the seed's outer run and the collection bake share one type.
		if cols := t.derivedOutputColumns(outer); len(cols) > 0 {
			outerType = &values.RecordType{Fields: cols}
		} else {
			return nil // decline → name-model fallback
		}
	}

	var fields []values.RecordConstructorField

	// OUTER leg: ofOrdinal(QOV(outer, outerType), i), full leg run 0..n-1.
	outerQOV, err := values.NewQuantifiedObjectValue(outerCorr, outerType)
	if err != nil {
		return nil
	}
	for i := range outerType.Fields {
		resolved, err := values.ResolveOrdinalSeedField(outerQOV, i)
		if err != nil {
			return nil // decline
		}
		fv, ok := values.AsFieldValue(resolved)
		if !ok {
			return nil
		}
		fields = append(fields, values.RecordConstructorField{Name: fv.DisplayName(), Value: fv})
	}

	innerFields, fullBakedSeed, ok := unnestSeedInnerFields(innerCorr, u, elementType)
	if !ok {
		return nil // degenerate no-AS/no-AT shape — name-model
	}
	fields = append(fields, innerFields...)

	rc := values.NewRawRecordConstructorValue(fields...)
	if fullBakedSeed {
		// A full all-baked 2-leg seed (outer run + element+ordinal run): assert
		// the pristine ordinal-join shape. The mixed no-AT RC (direct-QOV
		// element) and the AT-only partial inner run legitimately carry a
		// non-frontier-pinned or partial-run field, so they skip the assert.
		values.AssertOrdinalJoinSeed(rc)
	}
	return rc
}

// unnestBakedRootCollection rebinds the semantic collection's exact ordinal
// path onto the outer physical row. A single source starts at zero; a boxed
// source uses its owner window. A chained owner supplies explicitRootIdx,
// accounting for whether the preceding UNNEST flows its element directly or
// wraps it with AT. Diagnostic Segments never participate in this binding.
// Returns nil when no exact outer layout or owner window can be established.
func (t *cascadesTranslator) unnestBakedRootCollection(
	outer logical.LogicalOperator,
	outerCorr values.CorrelationIdentifier,
	u *logical.LogicalUnnest,
	explicitRootIdx int,
) values.Value {
	outerType := t.ordinalLegType(outer)
	if outerType == nil || len(outerType.Fields) == 0 {
		// A DERIVED-TABLE outer (`FROM (SELECT …) AS d, d.arr AS x`) is not a
		// scan/gated-box leg, so ordinalLegType declines — but it flows a
		// POSITIONAL row of its projection's OUTPUT columns, against which the
		// collection ordinal is sound. Derive that layout.
		if cols := t.derivedOutputColumns(outer); len(cols) > 0 {
			outerType = &values.RecordType{Fields: cols}
		} else {
			return nil
		}
	}
	owner, _, _ := boundUnnestCollection(u)
	if owner == nil {
		return nil
	}
	outerQOV, err := values.NewQuantifiedObjectValue(outerCorr, outerType)
	if err != nil {
		return nil
	}
	if explicitRootIdx >= 0 {
		prior := boundUnnestOwner(outer, u)
		if prior == nil {
			return nil
		}
		return resolveBoundSeedCollection(outerQOV, u, explicitRootIdx, prior.AtAlias == "")
	}
	if boundUnnestSingleSource(outer, u) {
		// Zero is valid only for the identified whole source. Optimizer arity
		// cannot establish ownership: a FULL box is one leg with several source
		// windows, and an unrelated source may have exactly the same row type.
		return resolveBoundSeedCollection(outerQOV, u, 0, false)
	}
	// A boxed outer carries every visible source as a distinct flat window.
	// Its quantifier is named after the rightmost source, but that does not
	// make the rightmost source start at ordinal zero.
	offset := -1
	source, ok := owner.FlowedType().(*values.RecordType)
	if !ok {
		return nil
	}
	for _, leg := range outerType.Legs {
		if !values.SameLeg(leg.Alias, owner.Correlation()) {
			continue
		}
		if offset >= 0 || leg.Kind != values.LegKindFlatRun || leg.Width != len(source.Fields) {
			return nil
		}
		offset = leg.Start
	}
	return resolveBoundSeedCollection(outerQOV, u, offset, false)
}

// unnestSeedInnerFields builds the unnest INNER leg's seed fields — the
// three-way branch (Java LogicalOperator.java:318-353 in ordinal form), shared
// by the single-source binary seed above and the gathered N-way seed:
//
//   - WITH ORDINALITY (AT): {element=ofOrdinal(inner,0) [when AS binds it],
//     ordinal=ofOrdinal(inner,1)} over the alias-named 2-field leg type;
//     fullBaked reports whether the pair covers the whole inner run (AS+AT);
//   - WITHOUT: the element is the WHOLE flowed object — a direct inner QOV
//     (the mixed-RC form whose leg binds RAW at the executor build).
//
// ok=false is the degenerate no-AS/no-AT shape (no bindable unnest column).
func unnestSeedInnerFields(
	innerCorr values.CorrelationIdentifier,
	u *logical.LogicalUnnest,
	elementType values.Type,
) (fields []values.RecordConstructorField, fullBaked, ok bool) {
	if u.AtAlias != "" {
		// The Explode flows {_0:element, _1:ordinal} — a genuine 2-field record
		// leg. NAME the leg type by the AS/AT ALIASES (not the Explode's `_0`/`_1`)
		// so a downstream `QOV(<alias>).<AS|AT>` projection/predicate reference
		// bakes its leg-local ordinal against the leg TYPE names (the AS/AT
		// aliases are the columns' OUTPUT names, what an upper references). The
		// Explode still flows `_0`/`_1`; the build's bindLeg binds that ordinality
		// row to this alias-named leg strictly by position (element slot 0,
		// ordinal slot 1). AT-only leaves the element slot named
		// `_0` — unreferenced, since without an AS the element binds to nothing.
		names := logical.UnnestOrdinalityNames(u.Alias, u.AtAlias)
		// Match the physical Explode WITH ORDINALITY carrier exactly. Each
		// emitted element/ordinal pair is a present row; an empty or NULL array
		// emits no row rather than a null-supplying row.
		innerType := values.NewRecordType("", false, []values.Field{
			{Name: names[0], FieldType: elementType, Ordinal: 0},
			{Name: names[1], FieldType: values.NotNullInt, Ordinal: 1},
		})
		innerQOV, err := values.NewQuantifiedObjectValue(innerCorr, innerType)
		if err != nil {
			return nil, false, false
		}
		if u.Alias != "" {
			// AS binds the element to ordinal 0; naming the RC field by the AS
			// alias makes both the full inner leg run (0,1) and the result-set
			// column name correct.
			elemFV, err := values.ResolveOrdinalSeedField(innerQOV, 0)
			if err != nil {
				return nil, false, false
			}
			fields = append(fields, values.RecordConstructorField{Name: names[0], Value: elemFV})
			fullBaked = true // element+ordinal cover the whole inner leg
		}
		ordFV, err := values.ResolveOrdinalSeedField(innerQOV, 1)
		if err != nil {
			return nil, false, false
		}
		fields = append(fields, values.RecordConstructorField{Name: names[1], Value: ordFV})
		return fields, fullBaked, true
	}
	// The element is the whole flowed object — Java's primitive branch.
	if u.Alias == "" {
		return nil, false, false // neither AS nor AT: no bindable unnest column
	}
	elementValue, err := values.NewQuantifiedObjectValue(innerCorr, elementType)
	if err != nil {
		return nil, false, false
	}
	fields = append(fields, values.RecordConstructorField{Name: u.Alias, Value: elementValue})
	return fields, false, true
}
