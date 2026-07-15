package query

import (
	"strings"

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
//     executor birth (a bare-QOV non-record leg — see the ordinal-birth binder).
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
	outerQOV := values.NewQuantifiedObjectValueOfType(outerCorr, outerType)
	for i := range outerType.Fields {
		fv, err := values.NewFieldValueOfOrdinal(outerQOV, i)
		if err != nil {
			return nil // decline
		}
		fields = append(fields, values.RecordConstructorField{Name: fv.Field, Value: fv})
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

// unnestBakedRootCollection builds the BAKED fused collection for a lateral
// unnest under the ORDINAL seed, rooting the Explode's array Value at a
// POSITIONAL column of the outer's ordinal leg type and riding the remaining
// segments as a NAME-addressed FUSED suffix. Two callers, distinguished by
// rootSegmentIndex — the segment that names the ROOT array column:
//
//   - SINGLE-SOURCE MULTI-SEGMENT (`FROM t, t.rec.arr AS x`, unnest-
//     residual class 2): rootSegmentIndex 1 — Segments[0] is the source alias,
//     Segments[1] the struct column; suffix Segments[2:] descend it. owner ==
//     the outer scan itself.
//   - CHAINED (`FROM t, t.arr AS x, x.sub AS y`, unnest-residual class 4):
//     rootSegmentIndex 0 — Segments[0] is the OWNER ALIAS `x`, a COLUMN of the
//     first link's ordinal leg type (the element of the prior Explode);
//     suffix Segments[1:] (`sub…`) descend it.
//
// The name-keyed arrayValue the name-model path uses (`FieldValue{QOV(outer),
// Resolved:[{rec,-1},{arr,-1}]}`) does NOT descend under the ordinal-seed birth:
// the outer row is ORDINAL-addressed, so the ordinal-birth resolver applies all
// name accessors flat against the outer row and fails ("field NARR not resolvable
// ... ordinal -1"). The ordinal form bakes the ROOT column positionally —
// ofOrdinal(QOV(outer, outerType), FieldIndex(root)) — and rides the remaining
// segments as a NAME-addressed FUSED suffix that descends the struct VALUE
// through FieldValue's proto-message arm (independent of the positional birth).
// This is the single-leg case of the gathered unnest cluster's owner-window
// bake: owner == the outer itself, leafOffset 0.
//
// Returns nil (DECLINE → the caller keeps the name-model builder) when the outer
// leg is untranslatable or the root column is absent.
func (t *cascadesTranslator) unnestBakedRootCollection(
	outer logical.LogicalOperator,
	outerCorr values.CorrelationIdentifier,
	u *logical.LogicalUnnest,
	fieldName string,
	elementType values.Type,
	rootSegmentIndex int,
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
	if rootSegmentIndex < 0 || rootSegmentIndex >= len(u.Segments) {
		return nil
	}
	// The ROOT column of the collection path (the remaining segments ride as the
	// fused suffix, Segments[rootSegmentIndex+1:]). Resolve the ROOT INDEX two ways:
	//   - single-source root (a struct column of the outer SCAN): explicitRootIdx
	//     < 0 → resolve by NAME (Segments[rootSegmentIndex]). Unshadowable — the
	//     outer scan's own columns.
	//   - CHAINED owner-alias root (the first link's ELEMENT): the caller passes
	//     explicitRootIdx (the element's slot). A NAME lookup here would pick an
	//     OUTER column that SHADOWS the alias — the outer columns precede the
	//     element in the merged row, so FieldIndex returns the wrong (outer)
	//     occurrence and the Explode roots at the wrong column. Use the slot.
	var arrIdx int
	if explicitRootIdx >= 0 {
		if explicitRootIdx >= len(outerType.Fields) {
			return nil
		}
		arrIdx = explicitRootIdx
	} else {
		rootField := strings.ToUpper(u.Segments[rootSegmentIndex])
		idx, found := outerType.FieldIndex(rootField)
		if !found {
			return nil
		}
		arrIdx = idx
	}
	outerQOV := values.NewQuantifiedObjectValueOfType(outerCorr, outerType)
	collection, err := values.NewFieldValueOfOrdinal(outerQOV, arrIdx)
	if err != nil {
		return nil
	}
	suffix := make([]values.ResolvedAccessor, 0, len(u.Segments)-rootSegmentIndex-1)
	for _, seg := range u.Segments[rootSegmentIndex+1:] {
		// NAME-addressed struct descent (FieldValue's proto-message arm); ordinal
		// is the LOUD sentinel -1 — a struct materializes as a proto message, not
		// a positional row, so the ordinal is never consulted.
		suffix = append(suffix, values.ResolvedAccessor{Field: strings.ToUpper(seg), Ordinal: -1})
	}
	fused := collection.Resolved.WithSuffix(&values.FieldPath{Accessors: suffix})
	// The fused node advertises the ARRAY type for the Explode (the classifier's
	// proto-derived element type is authoritative); set it directly in the rebuild
	// rather than carry the root ofOrdinal's struct type and overwrite.
	return &values.FieldValue{
		Field:    strings.ToUpper(fieldName),
		Typ:      values.NewArrayType(true, elementType),
		Child:    collection.Child,
		Resolved: fused,
	}
}

// unnestSeedInnerFields builds the unnest INNER leg's seed fields — the
// three-way branch (Java LogicalOperator.java:318-353 in ordinal form), shared
// by the single-source binary seed above and the gathered N-way seed:
//
//   - WITH ORDINALITY (AT): {element=ofOrdinal(inner,0) [when AS binds it],
//     ordinal=ofOrdinal(inner,1)} over the alias-named 2-field leg type;
//     fullBaked reports whether the pair covers the whole inner run (AS+AT);
//   - WITHOUT: the element is the WHOLE flowed object — a direct inner QOV
//     (the mixed-RC form whose leg binds RAW at the executor birth).
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
		// Explode still flows `_0`/`_1`; the birth's bindLeg binds that ordinality
		// row to this alias-named leg strictly by position (element slot 0,
		// ordinal slot 1). AT-only leaves the element slot named
		// `_0` — unreferenced, since without an AS the element binds to nothing.
		elemName := strings.ToUpper(u.Alias)
		if elemName == "" {
			elemName = values.OrdinalFieldName(0)
		}
		// NOT record-level nullable: the unnest inner leg always produces a
		// row per element (this is not an outer join's null-supplying side),
		// and ofOrdinal inherits the record's nullability into its column
		// types per Java's FieldValue.computeResultType — a nullable marker
		// here would flip the AT ordinal column (INT NOT NULL, the 1-based
		// position) to nullable in the flowed metadata.
		innerType := values.NewRecordType("", false, []values.Field{
			{Name: elemName, FieldType: elementType, Ordinal: 0},
			{Name: strings.ToUpper(u.AtAlias), FieldType: values.NotNullInt, Ordinal: 1},
		})
		innerQOV := values.NewQuantifiedObjectValueOfType(innerCorr, innerType)
		if u.Alias != "" {
			// AS binds the element to ordinal 0; naming the RC field by the AS
			// alias makes both the full inner leg run (0,1) and the result-set
			// column name correct.
			elemFV, err := values.NewFieldValueOfOrdinal(innerQOV, 0)
			if err != nil {
				return nil, false, false
			}
			fields = append(fields, values.RecordConstructorField{Name: strings.ToUpper(u.Alias), Value: elemFV})
			fullBaked = true // element+ordinal cover the whole inner leg
		}
		ordFV, err := values.NewFieldValueOfOrdinal(innerQOV, 1)
		if err != nil {
			return nil, false, false
		}
		fields = append(fields, values.RecordConstructorField{Name: strings.ToUpper(u.AtAlias), Value: ordFV})
		return fields, fullBaked, true
	}
	// The element is the whole flowed object — Java's primitive branch.
	if u.Alias == "" {
		return nil, false, false // neither AS nor AT: no bindable unnest column
	}
	elementValue := values.NewQuantifiedObjectValueOfType(innerCorr, elementType)
	fields = append(fields, values.RecordConstructorField{Name: strings.ToUpper(u.Alias), Value: elementValue})
	return fields, false, true
}
