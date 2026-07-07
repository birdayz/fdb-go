package query

// RFC-173 W4c — white-box pins for the single-source lateral-unnest ordinal
// seed. The ordinal-vs-name-model choice is invisible in EXPLAIN (both flow the
// same rows), so these prove the SEED SHAPE directly; the FDB e2e
// (array_unnest_ordinality_fdb_test.go) proves execution + shadowing +
// qualified-ref resolution + struct/positional authority.

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

// TestRFC173W4c_UnnestSeed_NonOrdinality pins the NO-AT seed: a MIXED RC — the
// outer leg as baked ofOrdinal references, then the element as a DIRECT
// QuantifiedObjectValue (Java's isPrimitive() branch; ofOrdinal over a scalar
// throws). NOT the name-model anchored record, and NOT run through
// AssertOrdinalJoinSeed (the bare-QOV element legitimately fails its
// frontier-pin check).
func TestRFC173W4c_UnnestSeed_NonOrdinality(t *testing.T) {
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
	if rc.AnchoredJoin {
		t.Fatal("W4c seed must be the ORDINAL seed, not the name-model anchored record")
	}
	outerType := tr.ordinalLegType(outer)
	wantOuter := len(outerType.Fields)
	if len(rc.Fields) != wantOuter+1 {
		t.Fatalf("seed has %d fields, want %d (outer %d cols + 1 element)", len(rc.Fields), wantOuter+1, wantOuter)
	}
	// Every outer field is a baked ofOrdinal.
	for i := 0; i < wantOuter; i++ {
		fv, ok := rc.Fields[i].Value.(*values.FieldValue)
		if !ok || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
			t.Fatalf("outer field %d is %T, want a baked frontier-pinned ofOrdinal", i, rc.Fields[i].Value)
		}
	}
	// The LAST field is the element: a DIRECT bare QOV over the inner correlation
	// (a whole-object leg — Java's primitive branch), NOT a baked FieldValue.
	last := rc.Fields[len(rc.Fields)-1]
	qov, ok := last.Value.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("element field is %T, want a DIRECT *QuantifiedObjectValue (Java's isPrimitive branch)", last.Value)
	}
	if qov.Correlation != innerCorr {
		t.Errorf("element QOV correlation = %s, want %s (the unnest AS alias)", qov.Correlation, innerCorr)
	}
	if last.Name != "X" {
		t.Errorf("element field name = %q, want X (the AS alias — the output column)", last.Name)
	}
}

// TestRFC173W4c_UnnestSeed_WithOrdinality pins the AS+AT seed: a FULL all-baked
// 2-leg seed — the outer leg run then the element (ofOrdinal 0) and ordinal
// (ofOrdinal 1) over an inner leg type NAMED BY THE AS/AT ALIASES (so the
// coexistence leg-window resolves QOV(alias).<AS|AT>). Runs AssertOrdinalJoinSeed
// inside the builder (a malformed seed panics at construction).
func TestRFC173W4c_UnnestSeed_WithOrdinality(t *testing.T) {
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
	if rc.AnchoredJoin {
		t.Fatal("W4c with-ordinality seed must be the ORDINAL seed, not anchored")
	}
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
	efv, ok := elem.Value.(*values.FieldValue)
	if !ok || efv.Resolved == nil || !efv.Resolved.FrontierPinned {
		t.Fatalf("element field is %T, want a baked ofOrdinal", elem.Value)
	}
	iqov, ok := efv.Child.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("element ofOrdinal child is %T, want the inner *QuantifiedObjectValue", efv.Child)
	}
	legType, ok := iqov.Type().(*values.RecordType)
	if !ok || len(legType.Fields) != 2 || legType.Fields[0].Name != "X" || legType.Fields[1].Name != "O" {
		t.Fatalf("inner leg type = %v, want a 2-field record named [X O] (the AS/AT aliases)", iqov.Type())
	}
	// The ordinal column is INT NOT NULL (the 1-based position).
	ofv := ord.Value.(*values.FieldValue)
	if ofv.Typ.IsNullable() {
		t.Errorf("ordinal (AT) column must be NOT NULL, got %s", ofv.Typ)
	}
}

// TestRFC173W4c_UnnestSeed_ATOnly pins the AT-only seed (no AS): the outer leg
// run then ONLY the ordinal (ofOrdinal 1) — the element is discarded (no AS
// binds it), so the inner run is partial and the seed skips AssertOrdinalJoinSeed.
func TestRFC173W4c_UnnestSeed_ATOnly(t *testing.T) {
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

// TestRFC173W4c_UnnestBakedRootCollection_MultiSegment pins the unnest-residual
// class-2 fused baked collection (`FROM t, t.rec.arr AS x`, len(Segments)>2): the
// collection ROOT is baked POSITIONALLY as ofOrdinal over the outer leg type, and
// the remaining segments ride as a NAME-addressed suffix. This is what lets the
// single-source multi-segment unnest ORDINALIZE — the name-keyed arrayValue does
// NOT descend under the ordinal-seed birth (proven by the FDB class-2 subtest,
// which fails the moment the guard is relaxed WITHOUT this bake: "field NARR not
// resolvable in the runtime row, ordinal -1"). Without ordinalizing this class it
// stays name-model, blocking the S4 cap. The bake itself is array-agnostic (the
// classifier validates arrayness upstream), so Order.flower (a nested struct)
// exercises the root-bake + suffix-descent shape directly.
func TestRFC173W4c_UnnestBakedRootCollection_MultiSegment(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	outer := scan("Order", "o")
	outerCorr := values.NamedCorrelationIdentifier("o")
	// Order.flower is a nested struct (Flower{type, color}); descend to `type`.
	u := &logical.LogicalUnnest{Segments: []string{"ORDER", "FLOWER", "TYPE"}, Alias: "X"}

	coll := tr.unnestBakedRootCollection(outer, outerCorr, u, "TYPE", values.NotNullLong)
	if coll == nil {
		t.Fatal("multi-segment single-source unnest must bake a fused collection, got nil (declined)")
	}
	fv, ok := coll.(*values.FieldValue)
	if !ok {
		t.Fatalf("baked collection = %T, want *FieldValue", coll)
	}
	if fv.Resolved == nil || !fv.Resolved.FrontierPinned {
		t.Fatal("baked collection root must be a frontier-pinned ofOrdinal (positional), not a name read")
	}
	if len(fv.Resolved.Accessors) != 2 {
		t.Fatalf("baked collection has %d accessors, want 2 (ofOrdinal root + name suffix)", len(fv.Resolved.Accessors))
	}
	// Root accessor: baked POSITIONAL (ordinal >= 0). Suffix: NAME-addressed (-1).
	if fv.Resolved.Accessors[0].Ordinal < 0 {
		t.Errorf("root accessor ordinal = %d, want >= 0 (baked positional)", fv.Resolved.Accessors[0].Ordinal)
	}
	if fv.Resolved.Accessors[1].Field != "TYPE" || fv.Resolved.Accessors[1].Ordinal != -1 {
		t.Errorf("suffix accessor = {%q, %d}, want {TYPE, -1} (name-addressed struct descent)",
			fv.Resolved.Accessors[1].Field, fv.Resolved.Accessors[1].Ordinal)
	}
	// The child is the outer QOV carrying the outer LEG TYPE, so the root ordinal
	// resolves positionally against the ordinal-seed birth row.
	qov, ok := fv.Child.(*values.QuantifiedObjectValue)
	if !ok || qov.Correlation != outerCorr {
		t.Fatalf("baked collection child = %T, want the outer *QuantifiedObjectValue %s", fv.Child, outerCorr)
	}
	if _, isRT := qov.Type().(*values.RecordType); !isRT {
		t.Fatalf("outer QOV must carry the leg RecordType for positional resolution, got %T", qov.Type())
	}
}

// TestRFC173W4c_UnnestBakedRootCollection_DeclineUntranslatable pins the bake's
// DECLINE: a catalog-free outer has no derivable leg type, so the bake returns
// nil and the caller keeps the name-model builder (which owns the name-keyed
// collection).
func TestRFC173W4c_UnnestBakedRootCollection_DeclineUntranslatable(t *testing.T) {
	t.Parallel()
	tr := &cascadesTranslator{} // no md → ordinalLegType nil
	coll := tr.unnestBakedRootCollection(scan("Order", "o"),
		values.NamedCorrelationIdentifier("o"),
		&logical.LogicalUnnest{Segments: []string{"ORDER", "FLOWER", "TYPE"}, Alias: "X"},
		"TYPE", values.NotNullLong)
	if coll != nil {
		t.Fatalf("untranslatable outer (no metadata) must DECLINE the bake to nil, got %T", coll)
	}
}

// TestRFC173W4c_UnnestSeed_DeclineUntranslatable pins the DECLINE path: a
// nil-metadata (catalog-free) outer has no derivable leg columns, so the seed
// declines to nil and the caller falls back to the name model.
func TestRFC173W4c_UnnestSeed_DeclineUntranslatable(t *testing.T) {
	t.Parallel()
	tr := &cascadesTranslator{} // no md → ordinalLegType nil
	seed := tr.unnestOrdinalSeed(scan("Customer", "c"),
		values.NamedCorrelationIdentifier("c"), values.NamedCorrelationIdentifier("X"),
		unnestFor("CUSTOMER", "X", ""), values.NotNullLong)
	if seed != nil {
		t.Fatalf("untranslatable outer (no metadata) must DECLINE to nil (name-model fallback), got %T", seed)
	}
}
