package query

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// unnestOrdinalSeed builds the ORDINAL result-value seed for a SINGLE-SOURCE
// lateral unnest (`FROM t, t.arr AS x [AT ord]`, RFC-173 W4c), replacing the
// name-model buildUnnestResultValue so the seed survives the S4 name-model
// deletion. Emitted ONLY when the OUTER leg is a SINGLE SOURCE
// (clusterArity(j.Left)==1 — the caller's gate): a multi-source outer
// (`FROM A, B, A.arr AS x`) stays name-model — that is W5.
//
// Mirrors Java's LogicalOperator.generateCorrelatedFieldAccess three-way branch
// (LogicalOperator.java:318-353), the ordinal-form spec:
//
//   - OUTER leg: ofOrdinal(QOV(outer), i) per column — the same ordinal leg
//     W4b/W4-LEFT bake (ordinalLegType carries the scan RecordName so a
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
// Returns nil (DECLINE → the caller falls back to buildUnnestResultValue) when
// the outer leg is untranslatable, or for the degenerate no-AS/no-AT shape.
func (t *cascadesTranslator) unnestOrdinalSeed(
	outer logical.LogicalOperator,
	outerCorr, innerCorr values.CorrelationIdentifier,
	u *logical.LogicalUnnest,
	elementType values.Type,
) values.Value {
	outerType := t.ordinalLegType(outer)
	if outerType == nil || len(outerType.Fields) == 0 {
		return nil // decline → name-model fallback
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

// unnestBakedRootCollection builds the BAKED fused collection for a SINGLE-SOURCE
// MULTI-SEGMENT lateral unnest (`FROM t, t.rec.arr AS x`, RFC-173 unnest-residual
// class 2) under the ORDINAL seed. The name-keyed arrayValue the name-model path
// uses (`FieldValue{QOV(outer), Resolved:[{rec,-1},{arr,-1}]}`) does NOT descend
// under the ordinal-seed birth: the outer row is ORDINAL-addressed, so the
// ordinal-birth resolver applies all name accessors flat against the outer row
// and fails ("field NARR not resolvable ... ordinal -1"). The ordinal form bakes
// the ROOT struct column positionally — ofOrdinal(QOV(outer, outerType),
// FieldIndex(rec)) — and rides the remaining segments as a NAME-addressed FUSED
// suffix that descends the struct VALUE through FieldValue's proto-message arm
// (independent of the positional birth). This is the single-leg case of the
// gathered path's owner-window bake (rfc173_w5_unnest_gather.go): owner == the
// outer scan itself, leafOffset 0, leafTyp == the scan's own type.
//
// Returns nil (DECLINE → the caller keeps the name-model builder) when the outer
// leg is untranslatable or the root struct field is absent.
func (t *cascadesTranslator) unnestBakedRootCollection(
	outer logical.LogicalOperator,
	outerCorr values.CorrelationIdentifier,
	u *logical.LogicalUnnest,
	fieldName string,
	elementType values.Type,
) values.Value {
	outerType := t.ordinalLegType(outer)
	if outerType == nil || len(outerType.Fields) == 0 {
		return nil
	}
	// The ROOT column of the collection path is the FIRST field segment
	// (Segments[0] is the source alias; Segments[1] is the struct column). The
	// remaining segments ride as the fused suffix.
	rootField := strings.ToUpper(u.Segments[1])
	arrIdx, found := outerType.FieldIndex(rootField)
	if !found {
		return nil
	}
	outerQOV := values.NewQuantifiedObjectValueOfType(outerCorr, outerType)
	collection, err := values.NewFieldValueOfOrdinal(outerQOV, arrIdx)
	if err != nil {
		return nil
	}
	suffix := make([]values.ResolvedAccessor, 0, len(u.Segments)-2)
	for _, seg := range u.Segments[2:] {
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

// unnestSeedInnerFields builds the unnest INNER leg's seed fields — the W4c
// three-way branch (Java LogicalOperator.java:318-353 in ordinal form), shared
// by the single-source binary seed above and the W5 gathered N-way seed:
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
		// so the coexistence leg-window resolution — legWindowRow.GetByName keys on
		// the leg TYPE names — resolves a downstream `QOV(<alias>).<AS|AT>`
		// projection/predicate reference (the AS/AT aliases are the columns' OUTPUT
		// names, what an upper references). The Explode still flows `_0`/`_1`; the
		// birth's adaptLegPositional binds that ordinality Datum to this alias-named
		// leg by OrdinalFieldName per slot. AT-only leaves the element slot named
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
