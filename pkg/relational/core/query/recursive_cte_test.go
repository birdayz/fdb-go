package query

// Recursive-CTE truth pins. A join over a
// RECURSIVE-CTE REFERENCE (the cteExprScope temp-table scan) gates
// ordinal: the reference is an ordinal-eligible arity-1
// leaf whose seed leg type comes from cteColumnsScope. These pins turn that
// from drift-prone folklore (two header comments once claimed the class was a
// name-model survivor) into asserted behavior, both directions.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

func TestRecursiveRefJoinGatesOrdinal(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)
	// The recursive-branch translation state: the reference pre-registered as
	// a temp-table scan (presence keys the gate arm) with its OUTPUT columns.
	tr.cteExprScope["R"] = nil
	tr.cteColumnsScope["R"] = []values.Field{
		{Name: "RID", FieldType: values.NotNullLong, Ordinal: 0},
	}

	j := logical.NewJoin(scan("R", "r"), scan("SRC", "s"), logical.JoinInner, "")
	d := tr.ordinalWedgeGateDecide(j)
	if !d.Gated || d.Arity != 2 {
		t.Fatalf("recursive-reference join gate = %+v, want Gated arity 2", d)
	}

	// The seed's reference leg is TYPED from cteColumnsScope.
	legs := tr.legsOfGatedJoin(j)
	rv, _ := tr.buildOrdinalJoinResultValue(legs)
	if rv == nil {
		t.Fatal("the recursive-reference join must build the ordinal seed")
	}
	rc := rv.(*values.RecordConstructorValue)
	if rc.Fields[0].Name != "RID" {
		t.Fatalf("seed leads with %q, want the reference leg's cteColumnsScope column RID", rc.Fields[0].Name)
	}
	fv := rc.Fields[0].Value.(*values.FieldValue)
	qov := fv.Child.(*values.QuantifiedObjectValue)
	if !strings.EqualFold(qov.Correlation.Name(), "r") {
		t.Fatalf("reference-leg QOV correlates to %s, want r", qov.Correlation)
	}

	// The OTHER direction: the recursive DEFINITION node in leg position
	// stays poison — defensively; the shape is production-unreachable
	// (derived-table WITH parses to 42F01 "table does not exist").
	recDef := logical.NewCTE("R2", scan("SRC", "s2"), scan("R2", "d"), true)
	jDef := logical.NewJoin(recDef, scan("AUX", "x"), logical.JoinInner, "")
	if dd := tr.ordinalWedgeGateDecide(jDef); dd.Gated {
		t.Fatal("a recursive DEFINITION node in leg position must stay poison (defensive; production-unreachable)")
	}
}

// The recursive-CTE leg remap's dotted arm fires from the STRUCTURAL
// classification (the projected value was a plain FieldValue — its rendered
// name is an identifier by construction), never from the string's shape. A
// string-grammar discriminator here misread computed renderings twice
// (review findings): "(B.ID + 1)" split into the garbage correlation "(B",
// and a float literal's "1.5" — digit-only segments pass an [A-Z0-9_] test —
// into QOV("1"). Structurally classified, a computed value's dots are never
// qualifiers regardless of rendering shape.
func TestRemapComputedRenderingNotSplit(t *testing.T) {
	t.Parallel()
	// "A.B" at index 1 is an ALIAS-derived name (a quoted alias may legally
	// contain a dot — one identifier, never qualifier syntax): verbatim
	// false, must NOT split into QOV("A") (review finding, provenance not
	// value type).
	names := []string{"(B.ID + 1)", "A.B", "1.5", "B.ID", "PLAIN"}
	verbatimField := []bool{false, false, false, true, true}
	vals := recursiveRemapValues(names, verbatimField, true, false)

	// Non-verbatim names (expression rendering, dotted quoted alias, float
	// literal): NOT split — a resolved-ordinal read named by the full
	// rendering, no QOV child.
	for _, i := range []int{0, 1, 2} {
		fv, ok := vals[i].(*values.FieldValue)
		if !ok || fv.Child != nil {
			t.Fatalf("computed rendering %q = %#v, want a flat FieldValue (no QOV split)", names[i], vals[i])
		}
		if fv.Field != names[i] {
			t.Fatalf("computed rendering read name = %q, want the full rendering %q", fv.Field, names[i])
		}
		if fv.Resolved == nil {
			t.Fatalf("computed rendering %q under ordinalReads must carry the resolved ordinal", names[i])
		}
	}

	// A plain unaliased FieldValue's genuinely-dotted lazy name: the QOV
	// read stays.
	fv3, ok := vals[3].(*values.FieldValue)
	if !ok || fv3.Child == nil || fv3.Field != "ID" {
		t.Fatalf("qualified reference = %#v, want QOV(B).ID", vals[3])
	}

	// Bare column: resolved-ordinal read.
	fv4, ok := vals[4].(*values.FieldValue)
	if !ok || fv4.Child != nil || fv4.Field != "PLAIN" || fv4.Resolved == nil {
		t.Fatalf("bare column = %#v, want resolved-ordinal PLAIN", vals[4])
	}

	// The fallback path (nil classification — logical column names,
	// identifiers by construction): dotted names still split.
	fb := recursiveRemapValues([]string{"B.ID"}, nil, false, false)
	fv, ok := fb[0].(*values.FieldValue)
	if !ok || fv.Child == nil || fv.Field != "ID" {
		t.Fatalf("fallback qualified reference = %#v, want QOV(B).ID", fb[0])
	}
}

// legPhysicalOutputNames classifies the NAME'S PROVENANCE: only an UNALIASED
// plain FieldValue's name is its Field string verbatim (identifier by
// construction). An ALIASED FieldValue's name is the alias — one identifier,
// never qualifier syntax, and a quoted alias may legally contain a dot
// (`AS "A.B"` — splitting it manufactured QOV("A"); review finding).
func TestLegClassificationStructural(t *testing.T) {
	t.Parallel()
	lp := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{
			&values.FieldValue{Field: "B.ID", Typ: values.UnknownType},
			&values.FieldValue{Field: "ID", Typ: values.UnknownType},
			&values.ArithmeticValue{
				Op:    values.OpAdd,
				Left:  &values.FieldValue{Field: "ID", Typ: values.UnknownType},
				Right: &values.ConstantValue{Value: int64(1), Typ: values.NullableLong},
			},
			&values.ConstantValue{Value: 1.5, Typ: values.NullableDouble},
		},
		[]string{"", "A.B", "", ""},
		expressions.ForEachQuantifier(expressions.InitialOf(
			expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))),
	)
	names, verbatimField, fromProjection := legPhysicalOutputNames(lp, []string{"a", "b", "c", "d"})
	if !fromProjection {
		t.Fatal("projection-topped leg must classify fromProjection")
	}
	if len(names) != 4 || len(verbatimField) != 4 {
		t.Fatalf("names/classification arity = %d/%d, want 4/4", len(names), len(verbatimField))
	}
	if !verbatimField[0] || verbatimField[1] || verbatimField[2] || verbatimField[3] {
		t.Fatalf("classification = %v, want [true false false false] (unaliased FieldValue vs aliased/computed/literal)", verbatimField)
	}
	if names[1] != "A.B" {
		t.Fatalf("aliased column name = %q, want the alias A.B verbatim", names[1])
	}
}

// TestPositionalArmDecoupling pins the positional arm of
// recursiveRemapValues: it
// reads slot i by ORDINAL but DECOUPLES the emit name from the name-window read
// root. Field=BARE so ProjectionColumnName emits the temp-row key bare (RFC-130:
// a dotted "C.ID" key would double the wide-payload row); Resolved.Root().Field=
// the FULL qualified physName so the name-window fallback (Datum[nameReadRootKey])
// hits the body's qualified output key, matching the equivalent dotted-split
// read a name-model body would use.
func TestPositionalArmDecoupling(t *testing.T) {
	t.Parallel()
	vals := recursiveRemapValues([]string{"C.ID", "PAYLOAD"}, []bool{true, true}, true, true)

	// Qualified column: bare emit, FULL read root, ordinal 0.
	fv0, ok := vals[0].(*values.FieldValue)
	if !ok || fv0.Resolved == nil {
		t.Fatalf("positional arm col0 = %#v, want a resolved-ordinal FieldValue", vals[0])
	}
	if fv0.Field != "ID" {
		t.Fatalf("positional emit name = %q, want BARE ID (RFC-130: no dotted temp-row key)", fv0.Field)
	}
	if fv0.Resolved.Root().Field != "C.ID" {
		t.Fatalf("positional read root = %q, want the FULL qualified C.ID (name-window fallback key)", fv0.Resolved.Root().Field)
	}
	if fv0.Resolved.Root().Ordinal != 0 {
		t.Fatalf("positional ordinal = %d, want slot 0", fv0.Resolved.Root().Ordinal)
	}

	// Bare column: bare on both, ordinal 1 (no spurious qualifier manufactured).
	fv1, ok := vals[1].(*values.FieldValue)
	if !ok || fv1.Resolved == nil || fv1.Field != "PAYLOAD" || fv1.Resolved.Root().Field != "PAYLOAD" || fv1.Resolved.Root().Ordinal != 1 {
		t.Fatalf("positional arm col1 = %#v, want bare PAYLOAD ordinal 1", vals[1])
	}
}

// TestRecursiveBodyGatesOrdinal is a structural sentinel. It runs the
// ACTUAL translateRecursiveCTE — exercising the
// lifted name-model blanket — and asserts the recursive PLAIN-JOIN body join
// GATES ordinal. This is the durable regression net: a silent revert
// that re-broadens the blanket forces inInnerCluster=true → the body join declines
// → this fails. The differential CANNOT catch that revert (it
// compares two EMISSION modes over the SHARED translator, so a name-model revert
// makes both modes agree and the differential stays green — a green-but-latent gap).
func TestRecursiveBodyGatesOrdinal(t *testing.T) {
	t.Parallel()
	// Recursive branch: SELECT c.order_id FROM Order c, WALK w  (plain inner join
	// over the CTE self-reference — the RFC-130 recursive-join-body shape).
	bodyJoin := inner(scan("Order", "c"), scan("WALK", "w"))
	rec := logical.NewProject(bodyJoin, []string{"ORDER_ID"}, nil)
	rec.ProjectedValues = []values.Value{&values.FieldValue{Field: "C.ORDER_ID", Typ: values.UnknownType}}
	// Seed: SELECT order_id FROM Order o.
	seed := logical.NewProject(scan("Order", "o"), []string{"ORDER_ID"}, nil)
	seed.ProjectedValues = []values.Value{&values.FieldValue{Field: "ORDER_ID", Typ: values.UnknownType}}
	cte := logical.NewCTE("WALK", logical.NewUnion([]logical.LogicalOperator{seed, rec}, false), scan("WALK", ""), true)

	tr := newGateTranslator(t)
	if tr.translateRecursiveCTE(cte) == nil {
		t.Fatalf("recursive CTE translation failed: %v", tr.translateErr)
	}

	// THE sentinel: the body join gated ordinal after the FULL run (blanket lifted).
	d, ok := tr.wedgeGate[bodyJoin]
	if !ok || !d.Gated {
		t.Fatalf("recursive plain-join body join = %+v (recorded=%v), want GATED after the full translateRecursiveCTE run", d, ok)
	}
	if !tr.recursiveBodyIsPositional(rec) {
		t.Fatal("recursiveBodyIsPositional must be TRUE for a plain-join recursive body")
	}
	// Gated ⟹ the seed took the ORDINAL branch (translateJoin's
	// buildOrdinalJoinResultValue), never the name-model buildJoinResultValue —
	// so the body RV is not AnchoredJoin. (The RV itself can't be rebuilt here:
	// translateRecursiveCTE deletes cteExprScope[cteName] on the way out, so the
	// self-reference leg no longer types. The Gated bit is the authoritative,
	// scope-independent proof.)

	// Negative control (surgical lift, value-level): a SCAN-only recursive body
	// (no join) has no gated join → stays name-model → NOT positional, so it keeps
	// the dotted-split arm. Proves the lift did not over-broaden to non-join bodies.
	scanBody := logical.NewProject(scan("WALK", "w2"), []string{"ORDER_ID"}, nil)
	scanBody.ProjectedValues = []values.Value{&values.FieldValue{Field: "ORDER_ID", Typ: values.UnknownType}}
	if tr.recursiveBodyIsPositional(scanBody) {
		t.Fatal("recursiveBodyIsPositional must be FALSE for a scan-only recursive body (name-model bodies keep the dotted-split arm)")
	}
}
