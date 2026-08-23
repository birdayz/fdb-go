package query

// White-box pins for the single-source lateral-unnest ordinal seed. The
// ordinal-vs-name-model choice is invisible in EXPLAIN (both flow the same
// rows), so these prove the SEED SHAPE directly; the FDB array-unnest
// ordinality test proves execution + shadowing + qualified-ref resolution +
// struct/positional authority.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// unnestFor builds a LogicalUnnest over the given outer table with the given
// AS/AT aliases (empty = omitted).
func unnestFor(table, asAlias, atAlias string) *logical.LogicalUnnest {
	return &logical.LogicalUnnest{Segments: []string{table, "ARR"}, Alias: asAlias, AtAlias: atAlias}
}

// TestUnnestSeed_NonOrdinality pins the NO-AT seed: a MIXED RC — the outer
// leg as baked ofOrdinal references, then the element as a DIRECT
// QuantifiedObjectValue (Java's isPrimitive() branch; ofOrdinal over a scalar
// throws). NOT the name-model anchored record, and NOT run through
// AssertOrdinalJoinSeed (the bare-QOV element legitimately fails its
// frontier-pin check).
func TestUnnestSeed_NonOrdinality(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	outer := scan("Customer", "c")
	outerCorr := values.NamedCorrelationIdentifier("c")
	innerCorr := values.NamedCorrelationIdentifier("X")

	seed := tr.unnestOrdinalSeed(outer, outerCorr, innerCorr, unnestFor("CUSTOMER", "X", ""), values.NotNullLong)
	if seed == nil {
		t.Fatal("single-source non-ordinality unnest must ordinalize, got nil (declined)")
	}
	rc, ok := seed.(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("seed = %T, want *RecordConstructorValue", seed)
	}
	outerType := tr.ordinalLegType(outer)
	wantOuter := len(outerType.Fields)
	if len(rc.Fields) != wantOuter+1 {
		t.Fatalf("seed has %d fields, want %d (outer %d cols + 1 element)", len(rc.Fields), wantOuter+1, wantOuter)
	}
	// Every outer field is a baked ofOrdinal.
	for i := 0; i < wantOuter; i++ {
		fv, ok := values.AsFieldValue(rc.Fields[i].Value)
		if !ok || !fv.Path().IsFrontierPinned() {
			t.Fatalf("outer field %d is %T, want a baked frontier-pinned ofOrdinal", i, rc.Fields[i].Value)
		}
	}
	// The LAST field is the element: a DIRECT bare QOV over the inner correlation
	// (a whole-object leg — Java's primitive branch), NOT a baked FieldValue.
	last := rc.Fields[len(rc.Fields)-1]
	qov, ok := values.AsQuantifiedObjectValue(last.Value)
	if !ok {
		t.Fatalf("element field is %T, want a DIRECT *QuantifiedObjectValue (Java's isPrimitive branch)", last.Value)
	}
	if qov.Correlation() != innerCorr {
		t.Errorf("element QOV correlation = %s, want %s (the unnest AS alias)", qov.Correlation(), innerCorr)
	}
	if last.Name != "X" {
		t.Errorf("element field name = %q, want X (the AS alias — the output column)", last.Name)
	}
}

// TestUnnestSeed_WithOrdinality pins the AS+AT seed: a FULL all-baked
// 2-leg seed — the outer leg run then the element (ofOrdinal 0) and ordinal
// (ofOrdinal 1) over an inner leg type NAMED BY THE AS/AT ALIASES (so the
// executor's leg-window resolution can address QOV(alias).<AS|AT>). Runs
// AssertOrdinalJoinSeed inside the builder (a malformed seed panics at
// construction).
func TestUnnestSeed_WithOrdinality(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	outer := scan("Customer", "c")
	outerCorr := values.NamedCorrelationIdentifier("c")
	innerCorr := values.NamedCorrelationIdentifier("X")

	seed := tr.unnestOrdinalSeed(outer, outerCorr, innerCorr, unnestFor("CUSTOMER", "X", "O"), values.NotNullLong)
	if seed == nil {
		t.Fatal("single-source with-ordinality unnest must ordinalize, got nil")
	}
	rc := seed.(*values.RecordConstructorValue)
	// A full all-baked seed passes the ordinal-join contract.
	values.AssertOrdinalJoinSeed(rc)

	outerType := tr.ordinalLegType(outer)
	wantOuter := len(outerType.Fields)
	if len(rc.Fields) != wantOuter+2 {
		t.Fatalf("seed has %d fields, want %d (outer %d + element + ordinal)", len(rc.Fields), wantOuter+2, wantOuter)
	}
	elem := rc.Fields[wantOuter]
	ord := rc.Fields[wantOuter+1]
	if elem.Name != "X" || ord.Name != "O" {
		t.Errorf("inner field names = (%q,%q), want (X, O) — the AS/AT aliases", elem.Name, ord.Name)
	}
	// Both inner fields are baked ofOrdinal over the SAME inner QOV, whose leg
	// type is named by the AS/AT aliases (X, O) — the leg-window resolution key.
	efv, ok := values.AsFieldValue(elem.Value)
	if !ok || !efv.Path().IsFrontierPinned() {
		t.Fatalf("element field is %T, want a baked ofOrdinal", elem.Value)
	}
	iqov, ok := values.AsQuantifiedObjectValue(efv.ChildValue())
	if !ok {
		t.Fatalf("element ofOrdinal child is %T, want the inner *QuantifiedObjectValue", efv.ChildValue())
	}
	legType, ok := iqov.FlowedType().(*values.RecordType)
	if !ok || len(legType.Fields) != 2 || legType.Fields[0].Name != "X" || legType.Fields[1].Name != "O" {
		t.Fatalf("inner leg type = %v, want a 2-field record named [X O] (the AS/AT aliases)", iqov.FlowedType())
	}
	// An emitted Explode row is present, so the 1-based AT ordinal remains INT
	// NOT NULL through exact FieldValue result-type derivation.
	ofv := exactTestFieldView(t, ord.Value)
	if ofv.ResultType().Code() != values.TypeCodeInt || ofv.ResultType().IsNullable() {
		t.Errorf("accessed ordinal (AT) column = %s, want INT NOT NULL", ofv.ResultType())
	}
}

// TestUnnestSeed_ATOnly pins the AT-only seed (no AS): the outer leg
// run then ONLY the ordinal (ofOrdinal 1) — the element is discarded (no AS
// binds it), so the inner run is partial and the seed skips AssertOrdinalJoinSeed.
func TestUnnestSeed_ATOnly(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	outer := scan("Customer", "c")
	outerCorr := values.NamedCorrelationIdentifier("c")
	innerCorr := values.NamedCorrelationIdentifier("O")

	seed := tr.unnestOrdinalSeed(outer, outerCorr, innerCorr, unnestFor("CUSTOMER", "", "O"), values.NotNullLong)
	if seed == nil {
		t.Fatal("AT-only unnest must ordinalize, got nil")
	}
	rc := seed.(*values.RecordConstructorValue)
	outerType := tr.ordinalLegType(outer)
	if len(rc.Fields) != len(outerType.Fields)+1 {
		t.Fatalf("AT-only seed has %d fields, want %d (outer + ordinal only, no element)", len(rc.Fields), len(outerType.Fields)+1)
	}
	last := rc.Fields[len(rc.Fields)-1]
	if last.Name != "O" {
		t.Errorf("AT-only last field = %q, want O (the ordinal); the element must NOT be exposed", last.Name)
	}
}

// nestedArrayUnnestFixture supplies a real exact nested-array path without
// depending on a catalog descriptor having one. The projected leg flows one
// column N whose value is RECORD<ARR ARRAY<LONG NOT NULL>>, so both the root
// and suffix can be resolved exactly and the collection's final type agrees
// with the unnest element type.
func nestedArrayUnnestFixture(t testing.TB) (logical.LogicalOperator, *logical.LogicalUnnest, values.Type) {
	t.Helper()
	elementType := values.NotNullLong
	arrayType := values.NewArrayType(true, elementType)
	nestedType := &values.RecordType{Fields: []values.Field{
		{Name: "ARR", Ordinal: 0, FieldType: arrayType},
	}}
	sourceType := &values.RecordType{Fields: []values.Field{
		{Name: "N", Ordinal: 0, FieldType: nestedType},
	}}
	outer := &logical.LogicalProject{
		Input:           scan("Customer", "src"),
		Projections:     []string{"N"},
		ProjectedValues: []values.Value{exactTestField(t, exactTestQOV(t, "SRC", sourceType), 0)},
	}
	u := &logical.LogicalUnnest{Segments: []string{"D", "N", "ARR"}, Alias: "X"}
	return outer, u, elementType
}

// TestUnnestBakedRootCollection_MultiSegment pins the multi-segment fused
// baked collection (`FROM t, t.rec.arr AS x`, len(Segments)>2): the
// collection ROOT is baked POSITIONALLY as ofOrdinal over the outer leg
// type, and the remaining segment is an exactly resolved fused suffix. This
// is what lets the single-source multi-segment unnest ORDINALIZE — the
// collection must carry the positional seed-purpose authority at its root,
// while exact type descent determines the suffix ordinal.
func TestUnnestBakedRootCollection_MultiSegment(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	outer, u, elementType := nestedArrayUnnestFixture(t)
	outerCorr := values.NamedCorrelationIdentifier("D")

	coll := tr.unnestBakedRootCollection(outer, outerCorr, u, "ARR", elementType, 1, -1)
	if coll == nil {
		t.Fatal("multi-segment single-source unnest must bake a fused collection, got nil (declined)")
	}
	fv, ok := values.AsFieldValue(coll)
	if !ok {
		t.Fatalf("baked collection = %T, want *FieldValue", coll)
	}
	if !fv.Path().IsFrontierPinned() {
		t.Fatal("baked collection root must be a frontier-pinned ofOrdinal (positional), not a name read")
	}
	if fv.Path().Len() != 2 {
		t.Fatalf("baked collection has %d accessors, want 2 (ofOrdinal root + exact suffix)", fv.Path().Len())
	}
	root, rootOK := fv.Path().Accessor(0)
	leaf, leafOK := fv.Path().Accessor(1)
	if !rootOK || root.Ordinal() < 0 {
		t.Errorf("root accessor = %v, want a non-negative exact ordinal", root)
	}
	leafName, named := leaf.DisplayName()
	if !leafOK || !named || leafName != "ARR" || leaf.Ordinal() != 0 {
		t.Errorf("suffix accessor = {%q, %d}, want exact ARR ordinal 0", leafName, leaf.Ordinal())
	}
	// The child is the outer QOV carrying the outer LEG TYPE, so the root ordinal
	// resolves positionally against the ordinal-seed build row.
	qov, ok := values.AsQuantifiedObjectValue(fv.ChildValue())
	if !ok || qov.Correlation() != outerCorr {
		t.Fatalf("baked collection child = %T, want the outer *QuantifiedObjectValue %s", fv.ChildValue(), outerCorr)
	}
	if _, isRT := qov.FlowedType().(*values.RecordType); !isRT {
		t.Fatalf("outer QOV must carry the leg RecordType for positional resolution, got %T", qov.FlowedType())
	}
}

// TestUnnestBakedRootCollection_SuffixTakesTheRowsSpelling pins the fused
// SUFFIX against the case gap the fixture above cannot express: there both the
// segment and the nested field are spelled ARR, so any rule at all resolves it.
//
// The real shape does not agree. A path segment arrives normalized at the parse
// boundary — `d.n.arr` folds to ARR — while the nested record it descends is
// named by whichever authority built it, and a base table's is the DESCRIPTOR,
// which for a hand-written .proto spells the field `arr`. The descent matches
// names EXACTLY, so a request carrying the SEGMENT's spelling finds nothing and
// the whole unnest declines to ordinalize; the request has to carry the FIELD's
// spelling instead.
//
// The assertion is on the leaf accessor's name and not only on "it resolved":
// the name is the evidence that the request was minted from the row rather than
// from the reference.
func TestUnnestBakedRootCollection_SuffixTakesTheRowsSpelling(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)

	elementType := values.NotNullLong
	// The nested record carries the descriptor's spelling; the OUTER column
	// keeps the exact spelling so this pin isolates the SUFFIX step — a
	// mismatch at the root would decline before the suffix is ever built.
	nestedType := &values.RecordType{Fields: []values.Field{
		{Name: "arr", Ordinal: 0, FieldType: values.NewArrayType(true, elementType)},
	}}
	sourceType := &values.RecordType{Fields: []values.Field{
		{Name: "N", Ordinal: 0, FieldType: nestedType},
	}}
	outer := &logical.LogicalProject{
		Input:           scan("Customer", "src"),
		Projections:     []string{"N"},
		ProjectedValues: []values.Value{exactTestField(t, exactTestQOV(t, "SRC", sourceType), 0)},
	}
	u := &logical.LogicalUnnest{Segments: []string{"D", "N", "ARR"}, Alias: "X"}
	outerCorr := values.NamedCorrelationIdentifier("D")

	coll := tr.unnestBakedRootCollection(outer, outerCorr, u, "ARR", elementType, 1, -1)
	if coll == nil {
		t.Fatal("a folded segment over a descriptor-spelled nested field must still bake the fused collection, got nil (declined)")
	}
	fv, ok := values.AsFieldValue(coll)
	if !ok {
		t.Fatalf("baked collection = %T, want an admitted exact FieldValue", coll)
	}
	if got := fv.Path().Len(); got != 2 {
		t.Fatalf("baked collection has %d accessors, want 2 (ofOrdinal root + exact suffix)", got)
	}
	leaf, leafOK := fv.Path().Accessor(1)
	leafName, named := leaf.DisplayName()
	if !leafOK || !named || leafName != "arr" || leaf.Ordinal() != 0 {
		t.Fatalf("suffix accessor = {%q, %d}, want {\"arr\", 0} — the ROW's spelling, not the segment's",
			leafName, leaf.Ordinal())
	}
}

// TestUnnestBakedRootCollection_DeclineUntranslatable pins the bake's
// DECLINE: a catalog-free outer has no derivable leg type, so the bake returns
// nil and the caller keeps the name-model builder (which owns the name-keyed
// collection).
func TestUnnestBakedRootCollection_DeclineUntranslatable(t *testing.T) {
	t.Parallel()
	tr := &cascadesTranslator{} // no md → ordinalLegType nil
	coll := tr.unnestBakedRootCollection(scan("Order", "o"),
		values.NamedCorrelationIdentifier("o"),
		&logical.LogicalUnnest{Segments: []string{"ORDER", "FLOWER", "TYPE"}, Alias: "X"},
		"TYPE", values.NotNullLong, 1, -1)
	if coll != nil {
		t.Fatalf("untranslatable outer (no metadata) must DECLINE the bake to nil, got %T", coll)
	}
}

// TestUnnestSeed_DeclineUntranslatable pins the DECLINE path: a
// nil-metadata (catalog-free) outer has no derivable leg columns, so the seed
// declines to nil and the caller falls back to the name model.
func TestUnnestSeed_DeclineUntranslatable(t *testing.T) {
	t.Parallel()
	tr := &cascadesTranslator{} // no md → ordinalLegType nil
	seed := tr.unnestOrdinalSeed(scan("Customer", "c"),
		values.NamedCorrelationIdentifier("c"), values.NamedCorrelationIdentifier("X"),
		unnestFor("CUSTOMER", "X", ""), values.NotNullLong)
	if seed != nil {
		t.Fatalf("untranslatable outer (no metadata) must DECLINE to nil (name-model fallback), got %T", seed)
	}
}
