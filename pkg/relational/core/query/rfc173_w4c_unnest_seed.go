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

	withOrdinality := u.AtAlias != ""
	fullBakedSeed := false
	if withOrdinality {
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
		innerType := values.NewRecordType("", true, []values.Field{
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
				return nil
			}
			fields = append(fields, values.RecordConstructorField{Name: strings.ToUpper(u.Alias), Value: elemFV})
			fullBakedSeed = true // element+ordinal cover the whole inner leg
		}
		ordFV, err := values.NewFieldValueOfOrdinal(innerQOV, 1)
		if err != nil {
			return nil
		}
		fields = append(fields, values.RecordConstructorField{Name: strings.ToUpper(u.AtAlias), Value: ordFV})
	} else {
		// The element is the whole flowed object — Java's primitive branch.
		if u.Alias == "" {
			return nil // neither AS nor AT: no bindable unnest column — name-model
		}
		elementValue := values.NewQuantifiedObjectValueOfType(innerCorr, elementType)
		fields = append(fields, values.RecordConstructorField{Name: strings.ToUpper(u.Alias), Value: elementValue})
	}

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
