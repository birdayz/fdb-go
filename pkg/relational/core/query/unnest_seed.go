package query

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
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
// Resolved:[{rec,-1},{arr,-1}]}`) does NOT descend under the ordinal-seed build:
// the outer row is ORDINAL-addressed, so the ordinal-build resolver applies all
// name accessors flat against the outer row and fails ("field NARR not resolvable
// ... ordinal -1"). The ordinal form bakes the ROOT column positionally —
// ofOrdinal(QOV(outer, outerType), FieldIndexUnique(root)) — and rides the remaining
// segments as a NAME-addressed FUSED suffix that descends the struct VALUE
// through FieldValue's proto-message arm (independent of the positional build).
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
	//     explicitRootIdx (the element's slot). A NAME lookup here cannot reach the
	//     element: an OUTER column SHADOWS the alias, and the outer columns precede
	//     the element in the merged row, so the name matches TWO fields and the
	//     unique-match lookup declines — the Explode never roots. Use the slot.
	var arrIdx int
	if explicitRootIdx >= 0 {
		if explicitRootIdx >= len(outerType.Fields) {
			return nil
		}
		arrIdx = explicitRootIdx
	} else {
		idx, found := seedFieldIndex(outerType, u.Segments[rootSegmentIndex])
		if !found {
			return nil
		}
		arrIdx = idx
	}
	outerQOV, err := values.NewQuantifiedObjectValue(outerCorr, outerType)
	if err != nil {
		return nil
	}
	collection, err := resolveSeedCollection(outerQOV, arrIdx, u.Segments[rootSegmentIndex+1:])
	if err != nil {
		return nil
	}
	wantArray := values.NewArrayType(collection.Type().IsNullable(), elementType)
	if !collection.Type().Equals(wantArray) {
		return nil
	}
	return collection
}

// seedFieldIndex resolves ONE path segment against a seed row layout: the
// segment's EXACT spelling first, then a case-insensitive match against the
// layout's OWN spellings.
//
// A FROM-source path segment arrives already normalized by the parse capture —
// unquoted folded UPPER, quoted kept verbatim — while the layout it indexes
// carries whatever spelling its authority minted: a base table's row is named
// from the DESCRIPTOR, so a hand-written .proto contributes lower/snake names,
// and a derived source's row is named by its projection. Neither authority
// folds, so the reference and the layout can differ by case in EITHER
// direction, and only a case-insensitive pass spans both. Re-folding the
// SEGMENT spans one direction: it reaches `TAGS` from `tags` and never `tags`
// from `TAGS`, which is the direction every unquoted reference to a descriptor
// column takes.
//
// Exact first is what keeps a quoted identifier addressable — `"sk"` must reach
// the field literally named `sk` even when a sibling `SK` exists — and it is
// the same strict-then-relaxed order the semantic scope resolves references
// with. Both passes decline a name matching more than one field, so neither can
// first-match its way past an ambiguity.
func seedFieldIndex(rt *values.RecordType, segment string) (int, bool) {
	if rt == nil || segment == "" {
		return 0, false
	}
	if idx, ok := rt.FieldIndexUnique(segment); ok {
		return idx, true
	}
	idx, hits := 0, 0
	for i, f := range rt.Fields {
		if strings.EqualFold(f.Name, segment) {
			idx, hits = i, hits+1
		}
	}
	if hits != 1 {
		return 0, false
	}
	return idx, true
}

// resolveSeedCollection resolves an ordinal root plus a NAME-addressed suffix
// against a seed row.
func resolveSeedCollection(root values.Value, ordinal int, segments []string) (values.Value, error) {
	requests, err := seedFieldRequests(root, ordinal, segments)
	if err != nil {
		return nil, err
	}
	return values.ResolveOrdinalSeedAccess(root, ordinal, requests)
}

// seedFieldRequests spells the NAME-addressed suffix the way the ROW spells it.
//
// The descent resolves each request by EXACT name, so a request has to carry
// the layout's own spelling and not the reference's. Each segment is therefore
// resolved through seedFieldIndex against the type reached so far, and the
// request is built from the FIELD's name — the same relaxation the root segment
// gets, applied at the one place the descent cannot apply it itself.
//
// Walking segment by segment is required rather than convenient: the descent
// re-types on every step, so which field segment n+1 may name is only settled
// once segment n has chosen its own.
func seedFieldRequests(root values.Value, ordinal int, segments []string) ([]values.FieldRequest, error) {
	if len(segments) == 0 {
		return nil, nil
	}
	rowType, isRecord := root.Type().(*values.RecordType)
	if !isRecord || ordinal < 0 || ordinal >= len(rowType.Fields) {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"unnest seed root ordinal %d does not address a field of the flowed row", ordinal)
	}
	current := rowType.Fields[ordinal].FieldType
	out := make([]values.FieldRequest, 0, len(segments))
	for _, seg := range segments {
		record, stillRecord := current.(*values.RecordType)
		if !stillRecord {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"unnest path segment %q does not descend a record", seg)
		}
		idx, found := seedFieldIndex(record, seg)
		if !found {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"unnest path segment %q does not name exactly one field of the row it descends", seg)
		}
		request, err := values.FieldByName(record.Fields[idx].Name)
		if err != nil {
			return nil, err
		}
		out = append(out, request)
		current = record.Fields[idx].FieldType
	}
	return out, nil
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
		elemName := strings.ToUpper(u.Alias)
		if elemName == "" {
			elemName = values.OrdinalFieldName(0)
		}
		// Match the physical Explode WITH ORDINALITY carrier exactly. Each
		// emitted element/ordinal pair is a present row; an empty or NULL array
		// emits no row rather than a null-supplying row.
		innerType := values.NewRecordType("", false, []values.Field{
			{Name: elemName, FieldType: elementType, Ordinal: 0},
			{Name: strings.ToUpper(u.AtAlias), FieldType: values.NotNullInt, Ordinal: 1},
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
			fields = append(fields, values.RecordConstructorField{Name: strings.ToUpper(u.Alias), Value: elemFV})
			fullBaked = true // element+ordinal cover the whole inner leg
		}
		ordFV, err := values.ResolveOrdinalSeedField(innerQOV, 1)
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
	elementValue, err := values.NewQuantifiedObjectValue(innerCorr, elementType)
	if err != nil {
		return nil, false, false
	}
	fields = append(fields, values.RecordConstructorField{Name: strings.ToUpper(u.Alias), Value: elementValue})
	return fields, false, true
}
